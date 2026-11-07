package station

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"wechat-bot/internal/strictjson"
)

func TestLocalComfyWorkflowPreparePatchesOnlyRegisteredValues(t *testing.T) {
	root := t.TempDir()
	graph := []byte(`{"1":{"class_type":"EmptyImage","inputs":{"width":32,"height":24,"batch_size":1,"color":0}},"2":{"class_type":"CLIPTextEncode","inputs":{"text":"old","clip":["3",0]}},"3":{"class_type":"KSampler","inputs":{"seed":7,"model":["4",0],"positive":["2",0]}},"9":{"class_type":"SaveImage","inputs":{"images":["1",0],"filename_prefix":"owner-prefix"}}}`)
	workflow := writeLocalComfyWorkflow(t, root, graph, `[{"id":"width","label":"Width","kind":"integer","minimum":16,"maximum":512,"node":"1","input":"width"},{"id":"seed","label":"Seed","kind":"integer","minimum":0,"maximum":9223372036854775807,"node":"3","input":"seed"},{"id":"text","label":"Prompt","kind":"string","max_length":120,"node":"2","input":"text"}]`)
	prepared, digest, err := workflow.prepare(map[string]json.RawMessage{"width": json.RawMessage(`64`), "seed": json.RawMessage(`42`), "text": json.RawMessage(`"blue fish"`)})
	if err != nil || digest != localComfyDigest(prepared) {
		t.Fatalf("prepare: digest=%q err=%v", digest, err)
	}
	decoded, err := strictjson.Decode(prepared)
	if err != nil {
		t.Fatal(err)
	}
	items := decoded.(map[string]any)
	if got := items["1"].(map[string]any)["inputs"].(map[string]any)["width"].(json.Number).String(); got != "64" {
		t.Fatalf("width=%q", got)
	}
	if got := items["3"].(map[string]any)["inputs"].(map[string]any)["seed"].(json.Number).String(); got != "42" {
		t.Fatalf("seed=%q", got)
	}
	if got := items["2"].(map[string]any)["inputs"].(map[string]any)["text"]; got != "blue fish" {
		t.Fatalf("text=%#v", got)
	}
	if got := items["9"].(map[string]any)["inputs"].(map[string]any)["filename_prefix"]; got != "owner-prefix" {
		t.Fatalf("unregistered field changed: %#v", got)
	}
	view := workflow.view()
	if view.ID != "smoke" || view.Fingerprint == "" || len(view.Parameters) != 3 {
		t.Fatalf("unsafe/missing view: %#v", view)
	}
}

func TestLocalComfyWorkflowRejectsUnknownOrOutOfRangeWithoutClamping(t *testing.T) {
	root := t.TempDir()
	graph := []byte(`{"1":{"class_type":"EmptyImage","inputs":{"width":32,"height":24,"batch_size":1,"color":0}},"9":{"class_type":"SaveImage","inputs":{"images":["1",0],"filename_prefix":"owner-prefix"}}}`)
	workflow := writeLocalComfyWorkflow(t, root, graph, `[{"id":"width","label":"Width","kind":"integer","minimum":16,"maximum":512,"node":"1","input":"width"}]`)
	for name, values := range map[string]map[string]json.RawMessage{
		"unknown":    {"model": json.RawMessage(`"untrusted.safetensors"`)},
		"overflow":   {"width": json.RawMessage(`513`)},
		"wrong_type": {"width": json.RawMessage(`"512"`)},
	} {
		if _, _, err := workflow.prepare(values); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestLocalComfyWorkflowRegistryRefusesUnsafeTargetAndExcessOutputs(t *testing.T) {
	root := t.TempDir()
	graph := []byte(`{"1":{"class_type":"EmptyImage","inputs":{"width":32,"height":24,"batch_size":1,"color":0}},"9":{"class_type":"SaveImage","inputs":{"images":["1",0],"filename_prefix":"owner-prefix"}}}`)
	writeLocalComfyRegistry(t, root, graph, `[{"id":"bad","label":"Bad","kind":"string","max_length":80,"node":"9","input":"filename_prefix"}]`)
	if _, err := loadLocalComfyWorkflows(root); err == nil {
		t.Fatal("SaveImage path parameter accepted")
	}
	writeLocalComfyRegistry(t, root, graph, `[{"id":"batch","label":"Batch","kind":"integer","minimum":1,"maximum":17,"node":"1","input":"batch_size"}]`)
	if _, err := loadLocalComfyWorkflows(root); err == nil {
		t.Fatal("more than 16 output parameter accepted")
	}
}

func TestLocalComfyWorkflowRegistryDetectsGraphChange(t *testing.T) {
	root := t.TempDir()
	graph := []byte(`{"1":{"class_type":"EmptyImage","inputs":{"width":32,"height":24,"batch_size":1,"color":0}},"9":{"class_type":"SaveImage","inputs":{"images":["1",0],"filename_prefix":"owner-prefix"}}}`)
	workflow := writeLocalComfyWorkflow(t, root, graph, `[]`)
	if err := os.WriteFile(filepath.Join(root, "smoke.json"), append(graph, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workflow.prepare(nil); err == nil {
		t.Fatal("changed approved graph accepted")
	}
}

func writeLocalComfyWorkflow(t *testing.T, root string, graph []byte, parameters string) localComfyWorkflow {
	t.Helper()
	writeLocalComfyRegistry(t, root, graph, parameters)
	workflows, err := loadLocalComfyWorkflows(root)
	if err != nil {
		t.Fatal(err)
	}
	return workflows[0]
}

func writeLocalComfyRegistry(t *testing.T, root string, graph []byte, parameters string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "smoke.json"), graph, 0600); err != nil {
		t.Fatal(err)
	}
	registry := `{"schema_version":1,"comfyui_version":"0.37.2","workflows":[{"id":"smoke","label":"Offline smoke","graph":"smoke.json","sha256":"` + localComfyDigest(graph) + `","save_node":"9","parameters":` + parameters + `}]}`
	if err := os.WriteFile(filepath.Join(root, localComfyWorkflowRegistryFile), []byte(registry), 0600); err != nil {
		t.Fatal(err)
	}
}
