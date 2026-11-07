package stationyue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileOriginalAppliesNativeRecipe(t *testing.T) {
	dir := writeTemplates(t)
	c := compilerFor(t, dir)
	min, guidance := 42.0, 3.5
	result, err := c.Compile(Request{Mode: ModeOriginal, Style: "female vocals; lo-fi", Lyrics: "hello", DurationSeconds: 50, MinDurationSeconds: &min, TextGuidanceScale: &guidance, Seed: 7, PlanningMode: "melody", Sampling: map[string]map[string]any{"render": {"steps": 12}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.SaveNodeID != "10" || result.SourceSHA256 == result.EffectiveSHA256 || result.Effective.MinDurationSeconds != 42 || result.Effective.Sampling["render"]["steps"] != 12 {
		t.Fatalf("unexpected result: %#v", result)
	}
	var g map[string]node
	if err := json.Unmarshal(result.Graph, &g); err != nil {
		t.Fatal(err)
	}
	if g["10"].Inputs["filename_prefix"] != "frozen" {
		t.Fatal("compiler changed output node")
	}
	if g["33:25"].Inputs["max_duration"] != float64(50) || g["33:34"].Inputs["seed"] != float64(7) || g["33:31"].Inputs["value"] != true {
		t.Fatalf("missing patches: %#v", g["33:25"].Inputs)
	}
	if got := g["33:25"].Inputs["mode"]; got != "melody" {
		t.Fatalf("mode=%v", got)
	}
}

func TestCompileCoverAllowsHostBasenameOnly(t *testing.T) {
	dir := writeTemplates(t)
	c := compilerFor(t, dir)
	ref := strings.Repeat("a", 64)
	result, err := c.Compile(Request{Mode: ModeCover, Style: "ballad", Lyrics: "words", DurationSeconds: 80, Seed: 9, ReferenceBasename: "host-reference.wav", ReferenceSHA256: ref})
	if err != nil {
		t.Fatal(err)
	}
	var g map[string]node
	_ = json.Unmarshal(result.Graph, &g)
	if g["45"].Inputs["audio"] != "host-reference.wav" || g["33:41"].Inputs["mode"] != "melody" || result.Effective.ReferenceMode != "melody" || !strings.Contains(g["33:25"].Inputs["style"].(string), "Faithful cover") {
		t.Fatalf("cover graph wrong: %#v", g)
	}
	_, err = c.Compile(Request{Mode: ModeCover, Style: "x", Lyrics: "x", DurationSeconds: 80, Seed: 1, ReferenceBasename: "../secret.wav", ReferenceSHA256: ref})
	if err == nil || !strings.Contains(err.Error(), "invalid_cover_reference") {
		t.Fatalf("unsafe path accepted: %v", err)
	}
}

func TestRefusesUnapprovedTemplateAndNativeBounds(t *testing.T) {
	dir := writeTemplates(t)
	c := compilerFor(t, dir)
	_, err := c.Compile(Request{Mode: ModeOriginal, Style: "x", Lyrics: "x", DurationSeconds: 20, Seed: 1, Sampling: map[string]map[string]any{"render": {"steps": 999}}})
	if err == nil || !strings.Contains(err.Error(), "sampling_outside_native_spec:render.steps") {
		t.Fatalf("wanted bounds rejection, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audio_yue2_text_to_music.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = c.Compile(Request{Mode: ModeOriginal, Style: "x", Lyrics: "x", DurationSeconds: 20, Seed: 1})
	if err == nil || !strings.Contains(err.Error(), "template_hash_not_approved") {
		t.Fatalf("modified source accepted: %v", err)
	}
}

func TestValidateDuration(t *testing.T) {
	ok, err := ValidateDuration(10.5, 10, 9, nil)
	if err != nil || !ok.Accepted {
		t.Fatalf("codec tolerance: %#v %v", ok, err)
	}
	bad, _ := ValidateDuration(10.51, 10, 9, nil)
	if bad.Accepted {
		t.Fatal("overlong accepted")
	}
	frame := 0.04
	ok, err = ValidateDuration(10.53, 10, 9, &frame)
	if err != nil || !ok.Accepted {
		t.Fatalf("frame tolerance %#v %v", ok, err)
	}
}

func TestCheckedInApprovedExportsHaveExpectedNativeShape(t *testing.T) {
	root := filepath.Join("..", "..", "components", "yue2", "app", "workflows_api")
	for _, item := range []struct {
		mode Mode
		name string
	}{{ModeOriginal, "audio_yue2_text_to_music.json"}, {ModeCover, "audio_yue2_music_cover.json"}} {
		b, err := os.ReadFile(filepath.Join(root, item.name))
		if err != nil {
			t.Fatalf("checked-in export %s: %v", item.name, err)
		}
		var graph map[string]node
		if err := json.Unmarshal(b, &graph); err != nil {
			t.Fatalf("%s invalid JSON: %v", item.name, err)
		}
		if err := validateGraph(graph, item.mode); err != nil {
			t.Fatalf("%s rejected: %v", item.name, err)
		}
	}
}

func compilerFor(t *testing.T, dir string) Compiler {
	t.Helper()
	pinned := map[Mode]string{}
	for _, pair := range []struct {
		m Mode
		n string
	}{{ModeOriginal, "audio_yue2_text_to_music.json"}, {ModeCover, "audio_yue2_music_cover.json"}} {
		b, err := os.ReadFile(filepath.Join(dir, pair.n))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(b)
		pinned[pair.m] = hex.EncodeToString(h[:])
	}
	return Compiler{WorkflowDir: dir, PinnedTemplateSHA256: pinned, ObjectInfo: withControls(t, objectInfo)}
}

func withControls(t *testing.T, raw string) json.RawMessage {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	input := data["YuE2GenerateMusic"].(map[string]any)["input"].(map[string]any)
	required := input["required"].(map[string]any)
	required["max_duration"] = []any{"FLOAT", map[string]any{"min": .04, "max": 900}}
	input["optional"] = map[string]any{"cfg_scale": []any{"FLOAT", map[string]any{"min": 0, "max": 100}}}
	result, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func writeTemplates(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	original := `{"10":{"class_type":"SaveAudioAdvanced","inputs":{"filename_prefix":"frozen","format":"FLAC"}},"33:25":{"class_type":"YuE2GenerateMusic","inputs":{"max_duration":30,"cfg_scale":1,"temperature":0.8,"top_p":0.9,"top_k":50,"repetition_penalty":1,"mode":"full"}},"33:34":{"class_type":"SeedNode","inputs":{"seed":0}},"33:8":{"class_type":"KSampler","inputs":{"steps":10,"cfg":2,"sampler_name":"euler","scheduler":"normal","denoise":1}},"33:24":{"class_type":"YuE2GenerateABC","inputs":{"max_abc_tokens":100,"temperature":0.8,"top_p":0.9,"top_k":50,"repetition_penalty":1,"penalty_window":10,"mode":"full"}},"33:31":{"class_type":"PrimitiveBoolean","inputs":{"value":true}},"33:36":{"class_type":"PrimitiveStringMultiline","inputs":{"value":""}},"33:37":{"class_type":"PrimitiveStringMultiline","inputs":{"value":""}}}`
	cover := `{"10":{"class_type":"SaveAudioAdvanced","inputs":{"filename_prefix":"frozen","format":"FLAC"}},"33:25":{"class_type":"YuE2GenerateMusic","inputs":{"max_duration":30,"cfg_scale":2,"temperature":0.8,"top_p":0.9,"top_k":50,"repetition_penalty":1,"mode":"reference","style":"","lyrics":""}},"33:34":{"class_type":"SeedNode","inputs":{"seed":0}},"33:8":{"class_type":"KSampler","inputs":{"steps":10,"cfg":2,"sampler_name":"euler","scheduler":"normal","denoise":1}},"45":{"class_type":"LoadAudio","inputs":{"audio":""}},"33:41":{"class_type":"SheetSage2AudioToABC","inputs":{"mode":"reference"}}}`
	if err := os.WriteFile(filepath.Join(dir, "audio_yue2_text_to_music.json"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audio_yue2_music_cover.json"), []byte(cover), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const objectInfo = `{"YuE2GenerateMusic":{"input":{"required":{"temperature":["FLOAT",{"min":0,"max":2}],"top_p":["FLOAT",{"min":0,"max":1}],"top_k":["INT",{"min":1,"max":100}],"repetition_penalty":["FLOAT",{"min":0,"max":2}]}}},"YuE2GenerateABC":{"input":{"required":{"max_abc_tokens":["INT",{"min":1,"max":1000}],"temperature":["FLOAT",{"min":0,"max":2}],"top_p":["FLOAT",{"min":0,"max":1}],"top_k":["INT",{"min":1,"max":100}],"repetition_penalty":["FLOAT",{"min":0,"max":2}],"penalty_window":["INT",{"min":1,"max":100}]}}},"KSampler":{"input":{"required":{"steps":["INT",{"min":1,"max":100}],"cfg":["FLOAT",{"min":0,"max":20}],"sampler_name":[["euler","dpm"],{}],"scheduler":[["normal","karras"],{}],"denoise":["FLOAT",{"min":0,"max":1}]}}}}`
