package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
	"wechat-bot/internal/mcptools"
)

// Validate the schema subset used by our own built-ins. External MCP tools
// retain their server-owned schemas and are not rewritten by this adapter.
type inputSchema struct {
	Type       any                    `json:"type"`
	Properties map[string]inputSchema `json:"properties"`
	Required   []string               `json:"required"`
	Enum       []any                  `json:"enum"`
	Additional any                    `json:"additionalProperties"`
	Items      *inputSchema           `json:"items"`
	Minimum    *float64               `json:"minimum"`
	Maximum    *float64               `json:"maximum"`
	MinLength  *int                   `json:"minLength"`
	MaxLength  *int                   `json:"maxLength"`
	MaxItems   *int                   `json:"maxItems"`
}

func argumentError(path, message string) error { return mcptools.ArgumentError(path, message) }

func parameterKey(key string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' {
			return -1
		}
		return unicode.ToLower(r)
	}, key)
}
func (p *permittedTools) normalizeToolArgs(name string, args json.RawMessage) (json.RawMessage, error) {
	if !utf8.Valid(args) {
		return nil, argumentError("$", "参数必须为有效UTF-8")
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	d := json.NewDecoder(bytes.NewReader(args))
	d.UseNumber()
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return nil, argumentError("$", "需要JSON对象，不能是null、数组或字符串")
	}
	object := map[string]any{}
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return nil, argumentError("$", "JSON格式无效")
		}
		k, ok := key.(string)
		if !ok {
			return nil, argumentError("$", "字段名无效")
		}
		if _, exists := object[k]; exists {
			return nil, argumentError("$", "字段重复，未选择其中任何值")
		}
		var value any
		if e = d.Decode(&value); e != nil {
			return nil, argumentError(k, "JSON格式无效")
		}
		object[k] = value
	}
	if token, e = d.Token(); e != nil || token != json.Delim('}') {
		return nil, argumentError("$", "JSON对象未完整结束")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, argumentError("$", "JSON对象后存在额外内容")
	}
	var schema *inputSchema
	for _, tool := range append(p.localTools(), mcptools.BuiltinTools()...) {
		if tool.Name == name {
			var s inputSchema
			if json.Unmarshal(tool.InputSchema, &s) == nil {
				schema = &s
			}
			break
		}
	}
	if schema != nil {
		normalized, e := normalizeSchemaValue(object, *schema, "$")
		if e != nil {
			return nil, e
		}
		return json.Marshal(normalized)
	}
	// Preserve external argument spelling and types; only root JSON validity
	// has been checked, so their complete server-side schema stays authoritative.
	return args, nil
}
func normalizeSchemaValue(value any, s inputSchema, path string) (any, error) {
	typeName, _ := s.Type.(string)
	if text, ok := value.(string); ok {
		if typeName == "integer" || typeName == "number" {
			trimmed := strings.TrimSpace(text)
			var number json.Number
			if json.Unmarshal([]byte(trimmed), &number) == nil {
				if f, e := number.Float64(); e == nil && (typeName != "integer" || math.Trunc(f) == f) {
					value = number
				}
			}
		} else if typeName == "boolean" && (text == "true" || text == "false") {
			value = text == "true"
		}
	}
	if number, ok := value.(json.Number); ok && typeName == "integer" {
		r, ok := new(big.Rat).SetString(number.String())
		if !ok || !r.IsInt() {
			return nil, argumentError(path, "需要整数")
		}
		value = json.Number(r.Num().String())
	}
	match := func(kind string) bool {
		switch kind {
		case "object":
			_, ok := value.(map[string]any)
			return ok
		case "array":
			_, ok := value.([]any)
			return ok
		case "string":
			_, ok := value.(string)
			return ok
		case "boolean":
			_, ok := value.(bool)
			return ok
		case "number", "integer":
			n, ok := value.(json.Number)
			if !ok {
				return false
			}
			f, e := n.Float64()
			return e == nil && (kind != "integer" || math.Trunc(f) == f)
		case "null":
			return value == nil
		}
		return true
	}
	if typeName != "" && !match(typeName) {
		return nil, argumentError(path, "类型应为"+typeName)
	}
	if types, ok := s.Type.([]any); ok {
		valid := false
		for _, t := range types {
			if k, ok := t.(string); ok && match(k) {
				valid = true
			}
		}
		if !valid {
			return nil, argumentError(path, "类型不匹配")
		}
	}
	if text, ok := value.(string); ok {
		length := utf8.RuneCountInString(text)
		if s.MinLength != nil && length < *s.MinLength {
			return nil, argumentError(path, "文本太短")
		}
		if s.MaxLength != nil && length > *s.MaxLength {
			return nil, argumentError(path, "文本超过该字段长度范围")
		}
	}
	if number, ok := value.(json.Number); ok {
		f, _ := number.Float64()
		if (s.Minimum != nil && f < *s.Minimum) || (s.Maximum != nil && f > *s.Maximum) {
			return nil, argumentError(path, "数值超出该字段范围")
		}
	}
	if len(s.Enum) > 0 {
		encoded, _ := json.Marshal(value)
		valid := false
		for _, option := range s.Enum {
			b, _ := json.Marshal(option)
			valid = valid || bytes.Equal(encoded, b)
		}
		if !valid {
			return nil, argumentError(path, "不是允许的枚举值")
		}
	}
	if items, ok := value.([]any); ok {
		if s.MaxItems != nil && len(items) > *s.MaxItems {
			return nil, argumentError(path, "列表项目过多")
		}
		if s.Items != nil {
			for i, item := range items {
				v, e := normalizeSchemaValue(item, *s.Items, fmt.Sprintf("%s[%d]", path, i))
				if e != nil {
					return nil, e
				}
				items[i] = v
			}
		}
	}
	if object, ok := value.(map[string]any); ok {
		unknown := false
		next := map[string]any{}
		keys := make([]string, 0, len(object))
		for k := range object {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, original := range keys {
			key := original
			property, exists := s.Properties[key]
			if !exists {
				for canonical, field := range s.Properties {
					if parameterKey(original) == parameterKey(canonical) {
						key, property, exists = canonical, field, true
						break
					}
				}
			}
			if _, duplicate := next[key]; duplicate {
				return nil, argumentError(path+"."+key, "别名与标准字段同时存在，存在歧义")
			}
			unknown = unknown || (!exists && s.Additional == false)
			v := object[original]
			if exists {
				var e error
				v, e = normalizeSchemaValue(v, property, path+"."+key)
				if e != nil {
					return nil, e
				}
			}
			next[key] = v
		}
		for _, required := range s.Required {
			if _, ok := next[required]; !ok {
				return nil, argumentError(path+"."+required, "缺少必填字段")
			}
		}
		if unknown {
			return nil, argumentError(path, "存在未定义字段，请按工具schema传参")
		}
		return next, nil
	}
	return value, nil
}
