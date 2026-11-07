package stationyue

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ValidateTemplate checks an owner-selected API graph without starting a
// runtime. Live object_info and native parameter bounds are still checked by
// Compile at execution time.
func ValidateTemplate(source []byte, mode Mode) error {
	if mode != ModeOriginal && mode != ModeCover {
		return errors.New("stationyue: invalid_mode")
	}
	if len(source) == 0 || len(source) > 1<<20 {
		return errors.New("stationyue: approved_template_invalid_size")
	}
	var graph map[string]node
	dec := json.NewDecoder(bytes.NewReader(source))
	dec.UseNumber()
	if err := dec.Decode(&graph); err != nil {
		return errors.New("stationyue: template_invalid_json")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("stationyue: template_invalid_json")
	}
	return validateGraph(graph, mode)
}
