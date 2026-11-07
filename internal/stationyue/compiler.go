// Package stationyue compiles approved YuE2 API workflow exports into a
// deterministic native ComfyUI graph.  It deliberately does not start
// ComfyUI, access WeChat, select an RVC model, or accept arbitrary files.
package stationyue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"strings"

	"wechat-bot/internal/workspacefs"
)

type Mode string

const (
	ModeOriginal Mode = "original"
	ModeCover    Mode = "cover"
)

const faithfulCoverStyle = "Faithful cover: preserve the supplied recording's melody, rhythm, phrasing and arrangement; do not replace it with a newly composed song."

// Compiler only accepts workflow exports whose bytes match the host's trusted
// pinned SHA-256. ObjectInfo must be the live ComfyUI /object_info response;
// this prevents stale static limits from silently clamping native controls.
type Compiler struct {
	WorkflowDir          string
	PinnedTemplateSHA256 map[Mode]string
	ObjectInfo           json.RawMessage
}

type Request struct {
	Mode               Mode
	Style              string
	Lyrics             string
	DurationSeconds    float64
	MinDurationSeconds *float64
	TextGuidanceScale  *float64
	VocalGender        string // female or male; this is a text-style hint, not a voice model.
	Seed               int64
	PlanningMode       string // original: off|melody|full; cover: reference
	ReferenceMode      string // cover source analysis: melody|full (not a planning mode)
	ReferenceBasename  string // host-selected basename in ComfyUI input only; cover only.
	ReferenceSHA256    string // host provenance metadata, never a filesystem path.
	Sampling           map[string]map[string]any
}

type Effective struct {
	Mode               Mode                      `json:"mode"`
	DurationSeconds    float64                   `json:"duration_seconds"`
	MinDurationSeconds float64                   `json:"min_duration_seconds"`
	TextGuidanceScale  float64                   `json:"text_guidance_scale"`
	Seed               int64                     `json:"seed"`
	PlanningMode       string                    `json:"planning_mode"`
	ReferenceMode      string                    `json:"reference_mode,omitempty"`
	ReferenceBasename  string                    `json:"reference_basename,omitempty"`
	ReferenceSHA256    string                    `json:"reference_sha256,omitempty"`
	Sampling           map[string]map[string]any `json:"sampling"`
}

type Result struct {
	Graph           []byte    // JSON graph before transport patches its output destination.
	SourceSHA256    string    // approved template bytes.
	EffectiveSHA256 string    // canonical graph after this compiler's safe patches.
	SaveNodeID      string    // always "10".
	Effective       Effective `json:"effective"`
}

func (c Compiler) Compile(r Request) (Result, error) {
	if r.Mode != ModeOriginal && r.Mode != ModeCover {
		return Result{}, errors.New("stationyue: invalid_mode")
	}
	// Normalize only documented defaults before validation, so Result always
	// reports the actual graph choice rather than an implicit empty value.
	if r.VocalGender == "" {
		r.VocalGender = "female"
	}
	if r.PlanningMode == "" {
		if r.Mode == ModeCover {
			r.PlanningMode = "reference"
		} else {
			r.PlanningMode = "full"
		}
	}
	if r.Mode == ModeCover && r.ReferenceMode == "" {
		r.ReferenceMode = "melody"
	}
	if err := validateRequest(r); err != nil {
		return Result{}, err
	}
	name := "audio_yue2_text_to_music.json"
	if r.Mode == ModeCover {
		name = "audio_yue2_music_cover.json"
	}
	root, err := workspacefs.OpenPath(c.WorkflowDir)
	if err != nil {
		return Result{}, errors.New("stationyue: approved_template_directory_unavailable")
	}
	defer root.Close()
	f, err := workspacefs.OpenRegular(root, name)
	if err != nil {
		return Result{}, errors.New("stationyue: approved_template_unavailable")
	}
	source, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	f.Close()
	if err != nil || len(source) > 1<<20 {
		return Result{}, errors.New("stationyue: approved_template_invalid_size")
	}
	sourceHash := sha(source)
	pinned := c.PinnedTemplateSHA256[r.Mode]
	if !validHash(pinned) || !strings.EqualFold(sourceHash, pinned) {
		return Result{}, errors.New("stationyue: template_hash_not_approved")
	}
	var graph map[string]node
	dec := json.NewDecoder(strings.NewReader(string(source)))
	dec.UseNumber()
	if err := dec.Decode(&graph); err != nil {
		return Result{}, fmt.Errorf("stationyue: template JSON: %w", err)
	}
	if err := validateGraph(graph, r.Mode); err != nil {
		return Result{}, err
	}
	spec, err := parseObjectInfo(c.ObjectInfo, graph, r.Mode)
	if err != nil {
		return Result{}, err
	}
	if err := validateValue(r.DurationSeconds, spec.Duration); err != nil {
		return Result{}, errors.New("stationyue: duration_outside_native_spec")
	}
	effectiveSampling, err := resolveSampling(r, spec, graph)
	if err != nil {
		return Result{}, err
	}

	min := r.DurationSeconds * .9
	if r.MinDurationSeconds != nil {
		min = *r.MinDurationSeconds
	}
	if min < 0 || min > r.DurationSeconds || !finite(min) {
		return Result{}, errors.New("stationyue: invalid_min_duration_seconds")
	}
	guidance := 1.0
	if r.Mode == ModeCover {
		guidance = 2.0
	}
	if r.TextGuidanceScale != nil {
		guidance = *r.TextGuidanceScale
	}
	if err := validateValue(guidance, spec.Guidance); err != nil {
		return Result{}, errors.New("stationyue: guidance_outside_native_spec")
	}
	style := styleFor(r)
	graph["33:25"].Inputs["max_duration"] = r.DurationSeconds
	graph["33:25"].Inputs["cfg_scale"] = guidance
	graph["33:34"].Inputs["seed"] = r.Seed
	for group, values := range effectiveSampling {
		id := groupNode(group)
		n := graph[id]
		n.Inputs = merge(n.Inputs, values)
		graph[id] = n
	}
	if r.Mode == ModeOriginal {
		graph["33:36"].Inputs["value"] = style
		graph["33:37"].Inputs["value"] = r.Lyrics
		graph["33:31"].Inputs["value"] = r.PlanningMode != "off"
		if r.PlanningMode != "off" {
			graph["33:24"].Inputs["mode"] = r.PlanningMode
			graph["33:25"].Inputs["mode"] = r.PlanningMode
		}
	} else {
		graph["45"].Inputs["audio"] = r.ReferenceBasename
		graph["33:25"].Inputs["style"] = style
		graph["33:25"].Inputs["lyrics"] = r.Lyrics
		graph["33:25"].Inputs["mode"] = r.ReferenceMode
		graph["33:41"].Inputs["mode"] = r.ReferenceMode
	}
	data, err := json.Marshal(graph)
	if err != nil {
		return Result{}, fmt.Errorf("stationyue: marshal graph: %w", err)
	}
	return Result{Graph: data, SourceSHA256: sourceHash, EffectiveSHA256: sha(data), SaveNodeID: "10", Effective: Effective{r.Mode, r.DurationSeconds, min, guidance, r.Seed, r.PlanningMode, r.ReferenceMode, r.ReferenceBasename, r.ReferenceSHA256, effectiveSampling}}, nil
}

type node struct {
	ClassType string         `json:"class_type"`
	Inputs    map[string]any `json:"inputs"`
}

func validateRequest(r Request) error {
	if !finite(r.DurationSeconds) || r.DurationSeconds <= 0 {
		return errors.New("stationyue: invalid_duration_seconds")
	}
	if r.Seed < 0 || r.Seed > 2147483647 {
		return errors.New("stationyue: invalid_seed")
	}
	if len(r.Style) > 2000 || len(r.Lyrics) > 6000 || strings.ContainsRune(r.Style, 0) || strings.ContainsRune(r.Lyrics, 0) {
		return errors.New("stationyue: invalid_text")
	}
	if r.VocalGender != "female" && r.VocalGender != "male" {
		return errors.New("stationyue: invalid_vocal_gender")
	}
	if r.Mode == ModeCover {
		if !safeBasename(r.ReferenceBasename) || !validHash(r.ReferenceSHA256) {
			return errors.New("stationyue: invalid_cover_reference")
		}
		if r.PlanningMode != "reference" {
			return errors.New("stationyue: invalid_planning_mode")
		}
		if r.ReferenceMode != "melody" && r.ReferenceMode != "full" {
			return errors.New("stationyue: invalid_reference_mode")
		}
	} else {
		if r.ReferenceBasename != "" || r.ReferenceSHA256 != "" {
			return errors.New("stationyue: original_cannot_have_reference")
		}
		if r.PlanningMode != "off" && r.PlanningMode != "melody" && r.PlanningMode != "full" {
			return errors.New("stationyue: invalid_planning_mode")
		}
	}
	return nil
}

func validateGraph(g map[string]node, mode Mode) error {
	want := map[string]string{"10": "SaveAudioAdvanced", "33:25": "YuE2GenerateMusic", "33:34": "SeedNode", "33:8": "KSampler"}
	if mode == ModeOriginal {
		want["33:24"] = "YuE2GenerateABC"
		want["33:31"] = "PrimitiveBoolean"
		want["33:36"] = "PrimitiveStringMultiline"
		want["33:37"] = "PrimitiveStringMultiline"
	} else {
		want["45"] = "LoadAudio"
		want["33:41"] = "SheetSage2AudioToABC"
	}
	for id, class := range want {
		if n, ok := g[id]; !ok || n.ClassType != class || n.Inputs == nil {
			return fmt.Errorf("stationyue: untrusted_template_node:%s", id)
		}
	}
	return nil
}

func styleFor(r Request) string {
	gender := r.VocalGender
	if gender == "" {
		gender = "female"
	}
	style := strings.TrimSpace(r.Style)
	// Keep this conservative: remove only generic competing lead-singer labels.
	re := regexp.MustCompile(`(?i)\\b(male|female)\\s+(lead\\s+)?vocals?\\b`)
	style = strings.TrimSpace(re.ReplaceAllString(style, ""))
	style = strings.TrimSpace(style + "; " + gender + " lead vocal")
	if r.Mode == ModeCover {
		style += "; " + faithfulCoverStyle
	}
	return style
}

func merge(dst, src map[string]any) map[string]any {
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
func sha(b []byte) string     { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validHash(s string) bool { _, err := hex.DecodeString(s); return err == nil && len(s) == 64 }
func finite(v float64) bool   { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func safeBasename(s string) bool {
	return s != "" && filepath.Base(s) == s && !strings.ContainsAny(s, `\\/:`) && !strings.Contains(s, "..") && len(s) <= 160
}
func groupNode(group string) string {
	if group == "semantic" {
		return "33:25"
	}
	if group == "render" {
		return "33:8"
	}
	return "33:24"
}
