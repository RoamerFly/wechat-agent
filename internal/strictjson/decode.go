package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
)

func strictJSON(d *json.Decoder, depth int) (any, error) {
	if depth > 16 {
		return nil, errors.New("JSON nesting too deep")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := t.(json.Delim); ok {
		switch delimiter {
		case '{':
			out := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				name, ok := k.(string)
				if !ok {
					return nil, errors.New("invalid key")
				}
				if _, exists := out[name]; exists {
					return nil, errors.New("duplicate key")
				}
				v, e := strictJSON(d, depth+1)
				if e != nil {
					return nil, e
				}
				out[name] = v
			}
			_, err = d.Token()
			return out, err
		case '[':
			out := []any{}
			for d.More() {
				v, e := strictJSON(d, depth+1)
				if e != nil {
					return nil, e
				}
				out = append(out, v)
			}
			_, err = d.Token()
			return out, err
		default:
			return nil, errors.New("invalid delimiter")
		}
	}
	return t, nil
}

// Decode rejects duplicate keys, excessive nesting and trailing values.
func Decode(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, e := strictJSON(d, 0)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return v, nil
}

// Matches compares a strict JSON value against an expected value.
func Matches(raw json.RawMessage, expected any) bool {
	actual, e := Decode(raw)
	if e != nil {
		return false
	}
	b, _ := json.Marshal(expected)
	want, e := Decode(b)
	return e == nil && reflect.DeepEqual(actual, want)
}

// DecodeKnownFields decodes one JSON value and rejects unknown struct fields.
func DecodeKnownFields(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
