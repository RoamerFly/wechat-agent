package stationyue

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

type fieldSpec struct {
	Type     string
	Min, Max *float64
	Enum     map[string]bool
}
type groupSpec map[string]fieldSpec
type runtimeSpec struct {
	Groups             map[string]groupSpec
	Duration, Guidance fieldSpec
}

var groups = map[string]struct {
	Node, Kind string
	Keys       []string
}{"semantic": {"33:25", "YuE2GenerateMusic", []string{"temperature", "top_p", "top_k", "repetition_penalty"}}, "render": {"33:8", "KSampler", []string{"steps", "cfg", "sampler_name", "scheduler", "denoise"}}, "score": {"33:24", "YuE2GenerateABC", []string{"max_abc_tokens", "temperature", "top_p", "top_k", "repetition_penalty", "penalty_window"}}}

type objectNode struct {
	Input struct {
		Required map[string]json.RawMessage `json:"required"`
		Optional map[string]json.RawMessage `json:"optional"`
	} `json:"input"`
}

func parseObjectInfo(raw json.RawMessage, graph map[string]node, mode Mode) (runtimeSpec, error) {
	if len(raw) == 0 {
		return runtimeSpec{}, errors.New("stationyue: missing_live_object_info")
	}
	var info map[string]objectNode
	if err := json.Unmarshal(raw, &info); err != nil {
		return runtimeSpec{}, fmt.Errorf("stationyue: invalid_live_object_info: %w", err)
	}
	result := runtimeSpec{Groups: map[string]groupSpec{}}
	for name, group := range groups {
		if _, active := graph[group.Node]; !active {
			continue
		}
		meta, ok := info[group.Kind]
		if !ok {
			return runtimeSpec{}, fmt.Errorf("stationyue: native_node_not_advertised:%s", group.Kind)
		}
		fields := groupSpec{}
		for _, key := range group.Keys {
			spec, ok := inputFrom(meta, key)
			if !ok {
				return runtimeSpec{}, fmt.Errorf("stationyue: native_input_not_advertised:%s.%s", name, key)
			}
			fields[key] = spec
		}
		result.Groups[name] = fields
	}
	meta := info["YuE2GenerateMusic"]
	var ok bool
	if result.Duration, ok = inputFrom(meta, "max_duration"); !ok || result.Duration.Type != "FLOAT" {
		return runtimeSpec{}, errors.New("stationyue: native_input_not_advertised:duration")
	}
	if result.Guidance, ok = inputFrom(meta, "cfg_scale"); !ok || result.Guidance.Type != "FLOAT" {
		return runtimeSpec{}, errors.New("stationyue: native_input_not_advertised:guidance")
	}
	return result, nil
}
func inputFrom(info objectNode, key string) (fieldSpec, bool) {
	if raw, ok := info.Input.Required[key]; ok {
		return parseInput(raw)
	}
	if raw, ok := info.Input.Optional[key]; ok {
		return parseInput(raw)
	}
	return fieldSpec{}, false
}
func parseInput(raw json.RawMessage) (fieldSpec, bool) {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return fieldSpec{}, false
	}
	var typ string
	if json.Unmarshal(parts[0], &typ) == nil {
		var opt struct {
			Min *float64 `json:"min"`
			Max *float64 `json:"max"`
		}
		if len(parts) < 2 || json.Unmarshal(parts[1], &opt) != nil || opt.Min == nil || opt.Max == nil || (typ != "FLOAT" && typ != "INT") {
			return fieldSpec{}, false
		}
		return fieldSpec{Type: typ, Min: opt.Min, Max: opt.Max}, true
	}
	var choices []string
	if json.Unmarshal(parts[0], &choices) == nil && len(choices) > 0 {
		e := map[string]bool{}
		for _, x := range choices {
			e[x] = true
		}
		return fieldSpec{Type: "COMBO", Enum: e}, true
	}
	return fieldSpec{}, false
}
func resolveSampling(r Request, spec runtimeSpec, graph map[string]node) (map[string]map[string]any, error) {
	if r.Sampling == nil {
		r.Sampling = map[string]map[string]any{}
	}
	for g := range r.Sampling {
		if _, ok := spec.Groups[g]; !ok {
			return nil, fmt.Errorf("stationyue: invalid_sampling_group:%s", g)
		}
	}
	if r.PlanningMode == "off" && r.Sampling["score"] != nil {
		return nil, errors.New("stationyue: score_sampling_requires_active_planning")
	}
	out := map[string]map[string]any{}
	for g, fields := range spec.Groups {
		if g == "score" && r.PlanningMode == "off" {
			continue
		}
		over := r.Sampling[g]
		if over == nil {
			over = map[string]any{}
		}
		for key := range over {
			if _, ok := fields[key]; !ok {
				return nil, fmt.Errorf("stationyue: invalid_sampling_parameter:%s.%s", g, key)
			}
		}
		vals := map[string]any{}
		for key, f := range fields {
			v, supplied := over[key]
			if !supplied {
				v = graph[groupNode(g)].Inputs[key]
			}
			if err := validateValue(v, f); err != nil {
				if supplied {
					return nil, fmt.Errorf("stationyue: sampling_outside_native_spec:%s.%s", g, key)
				}
				return nil, fmt.Errorf("stationyue: template_default_outside_native_spec:%s.%s", g, key)
			}
			vals[key] = v
		}
		out[g] = vals
	}
	return out, nil
}
func validateValue(v any, s fieldSpec) error {
	if s.Type == "COMBO" {
		x, ok := v.(string)
		if !ok || !s.Enum[x] {
			return errors.New("invalid_enum")
		}
		return nil
	}
	if _, bad := v.(bool); bad {
		return errors.New("bool")
	}
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case float32:
		n = float64(x)
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	case json.Number:
		var err error
		n, err = x.Float64()
		if err != nil {
			return err
		}
	default:
		return errors.New("not_number")
	}
	if !finite(n) || n < *s.Min || n > *s.Max || (s.Type == "INT" && math.Trunc(n) != n) {
		return errors.New("outside")
	}
	return nil
}
