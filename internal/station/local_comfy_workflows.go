package station

// This file is deliberately a small, host-only approval boundary.  The Agent
// gets a workflow ID plus scalar values; it never gets a Comfy graph, a model
// name, a node ID, a file name, or a path to choose from.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/strictjson"
)

const (
	localComfyWorkflowRegistryFile = "registry.json"
	localComfyWorkflowMaxJSON      = 4 << 20
	localComfyWorkflowMaxOutputs   = 16
)

var localComfyWorkflowID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var localComfyNodeID = regexp.MustCompile(`^[A-Za-z0-9:_-]{1,96}$`)
var localComfyInputID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
var localComfyGraphFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.json$`)

// localComfyWorkflow is a registered, immutable recipe.  The unexported
// graphPath and parameter targets are intentionally absent from tool views.
type localComfyWorkflow struct {
	ID          string
	Label       string
	Version     string
	Digest      string
	SaveNode    string
	Fingerprint string
	graphPath   string
	parameters  []localComfyWorkflowParameter
}

type localComfyWorkflowParameter struct {
	ID        string
	Label     string
	Kind      string
	Required  bool
	Min       *float64
	Max       *float64
	MaxLength int
	Choices   []string
	node      string
	input     string
}

// localComfyWorkflowView is safe to put in project-tool discovery metadata.
// It omits node/input bindings, raw graph data, paths, and model references.
type localComfyWorkflowView struct {
	ID          string                            `json:"id"`
	Label       string                            `json:"label"`
	Version     string                            `json:"comfyui_version"`
	Fingerprint string                            `json:"fingerprint"`
	Parameters  []localComfyWorkflowParameterView `json:"parameters"`
}

type localComfyWorkflowParameterView struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Kind      string   `json:"kind"`
	Required  bool     `json:"required,omitempty"`
	Min       *float64 `json:"minimum,omitempty"`
	Max       *float64 `json:"maximum,omitempty"`
	MaxLength int      `json:"max_length,omitempty"`
	Choices   []string `json:"enum,omitempty"`
}

type localComfyWorkflowRegistry struct {
	SchemaVersion int                        `json:"schema_version"`
	ComfyVersion  string                     `json:"comfyui_version"`
	Workflows     []localComfyWorkflowRecord `json:"workflows"`
}

type localComfyWorkflowRecord struct {
	ID         string                              `json:"id"`
	Label      string                              `json:"label"`
	Graph      string                              `json:"graph"`
	SHA256     string                              `json:"sha256"`
	SaveNode   string                              `json:"save_node"`
	Parameters []localComfyWorkflowParameterRecord `json:"parameters"`
}

type localComfyWorkflowParameterRecord struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Kind      string   `json:"kind"`
	Required  bool     `json:"required,omitempty"`
	Minimum   *float64 `json:"minimum,omitempty"`
	Maximum   *float64 `json:"maximum,omitempty"`
	MaxLength int      `json:"max_length,omitempty"`
	Enum      []string `json:"enum,omitempty"`
	Node      string   `json:"node"`
	Input     string   `json:"input"`
}

// loadLocalComfyWorkflows reads a host-private, version-pinned registration.
// root is supplied by the host deployment and must never point into Agent home.
func loadLocalComfyWorkflows(root string) ([]localComfyWorkflow, error) {
	if !localComfySafeDirectory(root) {
		return nil, errors.New("station_comfy_workflow_registry_unavailable")
	}
	raw, err := localComfyReadRegular(filepath.Join(root, localComfyWorkflowRegistryFile), localComfyWorkflowMaxJSON)
	if err != nil {
		return nil, errors.New("station_comfy_workflow_registry_unavailable")
	}
	var registry localComfyWorkflowRegistry
	if err := localComfyStrictDecode(raw, &registry); err != nil || registry.SchemaVersion != 1 || registry.ComfyVersion != comfyui.ReferenceVersion || len(registry.Workflows) == 0 || len(registry.Workflows) > 64 {
		return nil, errors.New("station_comfy_workflow_registry_invalid")
	}
	workflows := make([]localComfyWorkflow, 0, len(registry.Workflows))
	seen := map[string]bool{}
	for _, record := range registry.Workflows {
		if seen[record.ID] {
			return nil, errors.New("station_comfy_workflow_registry_invalid")
		}
		workflow, err := localComfyBuildWorkflow(root, registry.ComfyVersion, record)
		if err != nil {
			return nil, err
		}
		seen[workflow.ID] = true
		workflows = append(workflows, workflow)
	}
	sort.Slice(workflows, func(i, j int) bool { return workflows[i].ID < workflows[j].ID })
	return workflows, nil
}

func localComfyBuildWorkflow(root, version string, record localComfyWorkflowRecord) (localComfyWorkflow, error) {
	if !localComfyWorkflowID.MatchString(record.ID) || !safeRVCDisplay(record.Label, 96) || !localComfyGraphFile.MatchString(record.Graph) ||
		!validLocalRVCHex(record.SHA256, 64) || !localComfyNodeID.MatchString(record.SaveNode) || len(record.Parameters) > 32 {
		return localComfyWorkflow{}, errors.New("station_comfy_workflow_registry_invalid")
	}
	graphPath := filepath.Join(root, record.Graph)
	if filepath.Dir(graphPath) != root {
		return localComfyWorkflow{}, errors.New("station_comfy_workflow_registry_invalid")
	}
	raw, err := localComfyReadRegular(graphPath, localComfyWorkflowMaxJSON)
	if err != nil || localComfyDigest(raw) != record.SHA256 {
		return localComfyWorkflow{}, errors.New("station_comfy_workflow_approval_invalid")
	}
	graph, err := localComfyDecodeGraph(raw)
	if err != nil || !localComfySaveNodeValid(graph, record.SaveNode) || !localComfyOutputBounded(graph) {
		return localComfyWorkflow{}, errors.New("station_comfy_workflow_approval_invalid")
	}
	parameters := make([]localComfyWorkflowParameter, 0, len(record.Parameters))
	seenID, seenTarget := map[string]bool{}, map[string]bool{}
	for _, item := range record.Parameters {
		parameter, err := localComfyBuildParameter(graph, item)
		if err != nil || seenID[parameter.ID] || seenTarget[parameter.node+"\x00"+parameter.input] {
			return localComfyWorkflow{}, errors.New("station_comfy_workflow_registry_invalid")
		}
		seenID[parameter.ID] = true
		seenTarget[parameter.node+"\x00"+parameter.input] = true
		parameters = append(parameters, parameter)
	}
	if !localComfyOutputBoundedWithParameters(graph, parameters) {
		return localComfyWorkflow{}, errors.New("station_comfy_workflow_registry_invalid")
	}
	workflow := localComfyWorkflow{ID: record.ID, Label: record.Label, Version: version, Digest: record.SHA256, SaveNode: record.SaveNode, graphPath: graphPath, parameters: parameters}
	workflow.Fingerprint = localComfyFingerprint(workflow)
	return workflow, nil
}

func localComfyWorkflowViews(workflows []localComfyWorkflow) []localComfyWorkflowView {
	views := make([]localComfyWorkflowView, 0, len(workflows))
	for _, workflow := range workflows {
		views = append(views, workflow.view())
	}
	return views
}

func (w localComfyWorkflow) view() localComfyWorkflowView {
	view := localComfyWorkflowView{ID: w.ID, Label: w.Label, Version: w.Version, Fingerprint: w.Fingerprint, Parameters: make([]localComfyWorkflowParameterView, 0, len(w.parameters))}
	for _, p := range w.parameters {
		view.Parameters = append(view.Parameters, localComfyWorkflowParameterView{ID: p.ID, Label: p.Label, Kind: p.Kind, Required: p.Required, Min: p.Min, Max: p.Max, MaxLength: p.MaxLength, Choices: append([]string(nil), p.Choices...)})
	}
	return view
}

// prepare patches only host-approved scalar locations and returns raw Comfy
// API graph JSON.  Digest identifies the immutable approved source graph.
func (w localComfyWorkflow) prepare(inputs map[string]json.RawMessage) ([]byte, string, error) {
	raw, err := localComfyReadRegular(w.graphPath, localComfyWorkflowMaxJSON)
	if err != nil || localComfyDigest(raw) != w.Digest {
		return nil, "", errors.New("station_comfy_workflow_approval_changed")
	}
	graph, err := localComfyDecodeGraph(raw)
	if err != nil || !localComfySaveNodeValid(graph, w.SaveNode) {
		return nil, "", errors.New("station_comfy_workflow_approval_changed")
	}
	if inputs == nil {
		inputs = map[string]json.RawMessage{}
	}
	allowed := make(map[string]localComfyWorkflowParameter, len(w.parameters))
	for _, parameter := range w.parameters {
		allowed[parameter.ID] = parameter
	}
	for id := range inputs {
		if _, ok := allowed[id]; !ok {
			return nil, "", fmt.Errorf("station_comfy_parameter_unknown:%s", id)
		}
	}
	for _, parameter := range w.parameters {
		rawValue, exists := inputs[parameter.ID]
		if !exists {
			if parameter.Required {
				return nil, "", fmt.Errorf("station_comfy_parameter_required:%s", parameter.ID)
			}
			continue
		}
		value, err := localComfyParameterValue(parameter, rawValue)
		if err != nil {
			return nil, "", fmt.Errorf("station_comfy_parameter_invalid:%s", parameter.ID)
		}
		node, _ := graph[parameter.node].(map[string]any)
		nodeInputs, _ := node["inputs"].(map[string]any)
		if node == nil || nodeInputs == nil || !localComfySafeBuiltinInput(node["class_type"], parameter.input, parameter.Kind) {
			return nil, "", errors.New("station_comfy_workflow_approval_changed")
		}
		nodeInputs[parameter.input] = value
	}
	if !localComfyOutputBounded(graph) {
		return nil, "", errors.New("station_comfy_parameter_output_limit")
	}
	patched, err := json.Marshal(graph)
	if err != nil || len(patched) == 0 || len(patched) > localComfyWorkflowMaxJSON {
		return nil, "", errors.New("station_comfy_workflow_encode_failed")
	}
	// `Digest` is verified before mutation.  The returned digest binds this
	// exact, typed patch for comfyui.Prepare; Fingerprint keeps the immutable
	// owner approval/spec identity in the durable host job.
	return patched, localComfyDigest(patched), nil
}

func localComfyBuildParameter(graph map[string]any, item localComfyWorkflowParameterRecord) (localComfyWorkflowParameter, error) {
	p := localComfyWorkflowParameter{ID: item.ID, Label: item.Label, Kind: item.Kind, Required: item.Required, Min: item.Minimum, Max: item.Maximum, MaxLength: item.MaxLength, Choices: append([]string(nil), item.Enum...), node: item.Node, input: item.Input}
	if !localComfyWorkflowID.MatchString(p.ID) || !safeRVCDisplay(p.Label, 96) || !localComfyNodeID.MatchString(p.node) || !localComfyInputID.MatchString(p.input) || !localComfySafeBuiltinInput(localComfyClass(graph, p.node), p.input, p.Kind) {
		return localComfyWorkflowParameter{}, errors.New("invalid parameter")
	}
	if (p.Min != nil && (!localComfyFinite(*p.Min))) || (p.Max != nil && (!localComfyFinite(*p.Max))) || (p.Min != nil && p.Max != nil && *p.Min > *p.Max) {
		return localComfyWorkflowParameter{}, errors.New("invalid range")
	}
	if p.Kind == "string" {
		if p.MaxLength < 1 || p.MaxLength > 8192 || p.Min != nil || p.Max != nil {
			return localComfyWorkflowParameter{}, errors.New("invalid string")
		}
		for _, choice := range p.Choices {
			if !safeRVCDisplay(choice, p.MaxLength) {
				return localComfyWorkflowParameter{}, errors.New("invalid enum")
			}
		}
	} else if p.Kind == "integer" || p.Kind == "number" {
		if p.MaxLength != 0 || len(p.Choices) != 0 {
			return localComfyWorkflowParameter{}, errors.New("invalid numeric")
		}
		if p.Kind == "integer" && ((p.Min != nil && math.Trunc(*p.Min) != *p.Min) || (p.Max != nil && math.Trunc(*p.Max) != *p.Max)) {
			return localComfyWorkflowParameter{}, errors.New("invalid integer range")
		}
	} else if p.Kind == "boolean" {
		if p.Min != nil || p.Max != nil || p.MaxLength != 0 || len(p.Choices) != 0 {
			return localComfyWorkflowParameter{}, errors.New("invalid boolean")
		}
	} else {
		return localComfyWorkflowParameter{}, errors.New("invalid kind")
	}
	node, _ := graph[p.node].(map[string]any)
	nodeInputs, _ := node["inputs"].(map[string]any)
	if node == nil || nodeInputs == nil {
		return localComfyWorkflowParameter{}, errors.New("missing target")
	}
	if _, exists := nodeInputs[p.input]; !exists {
		return localComfyWorkflowParameter{}, errors.New("missing target input")
	}
	return p, nil
}

func localComfyParameterValue(p localComfyWorkflowParameter, raw json.RawMessage) (any, error) {
	decoded, err := strictjson.Decode(raw)
	if err != nil {
		return nil, err
	}
	switch p.Kind {
	case "string":
		value, ok := decoded.(string)
		if !ok || !safeRVCDisplay(value, p.MaxLength) {
			return nil, errors.New("bad string")
		}
		if len(p.Choices) > 0 && !localComfyContains(p.Choices, value) {
			return nil, errors.New("not enum")
		}
		return value, nil
	case "boolean":
		value, ok := decoded.(bool)
		if !ok {
			return nil, errors.New("bad boolean")
		}
		return value, nil
	case "integer":
		number, ok := decoded.(json.Number)
		if !ok || strings.ContainsAny(number.String(), ".eE") {
			return nil, errors.New("bad integer")
		}
		value, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil || !localComfyInRange(float64(value), p.Min, p.Max) {
			return nil, errors.New("out of range")
		}
		return value, nil
	case "number":
		number, ok := decoded.(json.Number)
		if !ok {
			return nil, errors.New("bad number")
		}
		value, err := strconv.ParseFloat(number.String(), 64)
		if err != nil || !localComfyFinite(value) || !localComfyInRange(value, p.Min, p.Max) {
			return nil, errors.New("out of range")
		}
		return value, nil
	}
	return nil, errors.New("bad kind")
}

func localComfyDecodeGraph(raw []byte) (map[string]any, error) {
	value, err := strictjson.Decode(raw)
	graph, ok := value.(map[string]any)
	if err != nil || !ok {
		return nil, errors.New("graph invalid")
	}
	return graph, nil
}
func localComfyClass(graph map[string]any, node string) any {
	item, _ := graph[node].(map[string]any)
	return item["class_type"]
}
func localComfySaveNodeValid(graph map[string]any, id string) bool {
	item, _ := graph[id].(map[string]any)
	inputs, _ := item["inputs"].(map[string]any)
	_, images := inputs["images"]
	return item != nil && item["class_type"] == "SaveImage" && inputs != nil && images
}
func localComfySafeBuiltinInput(class any, input, kind string) bool {
	switch class {
	case "CLIPTextEncode":
		return input == "text" && kind == "string"
	case "KSampler":
		return input == "seed" && kind == "integer"
	case "EmptyLatentImage":
		return (input == "width" || input == "height" || input == "batch_size") && kind == "integer"
	case "EmptyImage":
		return (input == "width" || input == "height" || input == "batch_size" || input == "color") && kind == "integer"
	}
	return false
}
func localComfyOutputBounded(graph map[string]any) bool {
	for _, item := range graph {
		node, _ := item.(map[string]any)
		inputs, _ := node["inputs"].(map[string]any)
		if node["class_type"] == "EmptyImage" || node["class_type"] == "EmptyLatentImage" {
			count, ok := localComfyInteger(inputs["batch_size"])
			if !ok || count < 1 || count > localComfyWorkflowMaxOutputs {
				return false
			}
		}
	}
	return true
}

func localComfyInteger(value any) (int64, bool) {
	switch value := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(value.String(), 10, 64)
		return parsed, err == nil
	case int64:
		return value, true
	case int:
		return int64(value), true
	}
	return 0, false
}
func localComfyOutputBoundedWithParameters(graph map[string]any, params []localComfyWorkflowParameter) bool {
	for _, p := range params {
		if p.input == "batch_size" {
			if p.Max == nil || *p.Max > localComfyWorkflowMaxOutputs {
				return false
			}
		}
	}
	return localComfyOutputBounded(graph)
}
func localComfyInRange(value float64, min, max *float64) bool {
	return (min == nil || value >= *min) && (max == nil || value <= *max)
}
func localComfyFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func localComfyContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
func localComfyDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func localComfyFingerprint(w localComfyWorkflow) string {
	type canonical struct {
		ID, Label, Version, Digest, SaveNode string
		Parameters                           []localComfyWorkflowParameter
	}
	raw, _ := json.Marshal(canonical{w.ID, w.Label, w.Version, w.Digest, w.SaveNode, w.parameters})
	return localComfyDigest(raw)
}
func localComfyStrictDecode(raw []byte, out any) error {
	value, err := strictjson.Decode(raw)
	if err != nil {
		return err
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(normalized)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) == nil {
		return errors.New("trailing json")
	}
	return nil
}
func localComfySafeDirectory(path string) bool {
	info, err := os.Lstat(path)
	return filepath.IsAbs(path) && filepath.Clean(path) == path && err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && localComfyLinkFree(path)
}
func localComfyReadRegular(path string, limit int) ([]byte, error) {
	if limit <= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("regular file required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !localComfyLinkFree(path) {
		return nil, errors.New("regular file required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(data) > limit {
		return nil, errors.New("file limit")
	}
	return data, nil
}
func localComfyLinkFree(path string) bool {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return true
		}
	}
}
