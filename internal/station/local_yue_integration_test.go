package station

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/stationyue"
)

type syntheticYuE struct {
	deploy  localYuEDeployment
	ref     comfyui.JobRef
	state   comfyui.State
	flac    comfyui.FLAC
	wav     []byte
	running bool
	lost    bool
	starts  int
	submits int
	uploads int
	fetches int
	cancels int
	stops   int
}

func (s *syntheticYuE) Info(context.Context, string) (localYuEDeployment, error) {
	return s.deploy, nil
}
func (s *syntheticYuE) Use(_ context.Context, _, binding string, start bool, fn func(localComfyAudioClient) error) error {
	if binding != s.deploy.Binding {
		return errors.New("wrong fixture binding")
	}
	if !s.running && start {
		s.running = true
		s.starts++
	}
	if !s.running {
		return errLocalComfyNotRunning
	}
	return fn(s)
}
func (s *syntheticYuE) Stop(context.Context) error {
	s.running = false
	s.stops++
	return nil
}
func (s *syntheticYuE) Close(ctx context.Context) error { return s.Stop(ctx) }
func (s *syntheticYuE) Check(context.Context) error     { return nil }
func (s *syntheticYuE) MarkSettled(context.Context, comfyui.JobRef) error {
	return nil
}
func (s *syntheticYuE) Status(context.Context, comfyui.JobRef) (comfyui.JobStatus, error) {
	return comfyui.JobStatus{}, errors.New("unexpected image status")
}
func (s *syntheticYuE) FetchPNG(context.Context, comfyui.JobRef, int) (comfyui.Image, error) {
	return comfyui.Image{}, errors.New("unexpected PNG")
}
func (s *syntheticYuE) UploadWAV(_ context.Context, ref comfyui.JobRef, raw []byte) (string, error) {
	s.uploads++
	if !bytes.Equal(raw, s.wav) {
		return "", errors.New("wrong reference WAV")
	}
	return comfyui.ReferenceWAVName(ref)
}
func (s *syntheticYuE) Submit(_ context.Context, prepared comfyui.Prepared) error {
	s.submits++
	s.ref, s.state = prepared.Ref(), comfyui.Running
	if s.lost {
		s.lost = false
		return comfyui.ErrSubmissionUnknown
	}
	return nil
}
func (s *syntheticYuE) AudioStatus(_ context.Context, ref comfyui.JobRef) (comfyui.AudioStatus, error) {
	if ref != s.ref {
		return comfyui.AudioStatus{}, errors.New("queried wrong original job")
	}
	return comfyui.AudioStatus{State: s.state, AudioCount: 1}, nil
}
func (s *syntheticYuE) FetchFLAC(_ context.Context, ref comfyui.JobRef) (comfyui.FLAC, error) {
	s.fetches++
	if ref != s.ref || s.state != comfyui.Completed {
		return comfyui.FLAC{}, errors.New("fetched wrong original job")
	}
	return s.flac, nil
}
func (s *syntheticYuE) Cancel(_ context.Context, ref comfyui.JobRef) (bool, error) {
	if ref != s.ref {
		return false, errors.New("cancelled wrong original job")
	}
	s.cancels++
	// Simulate a completion arriving just as cancellation is dispatched.
	s.state = comfyui.Completed
	return true, nil
}

func (s *syntheticYuE) YuERequiredNodeSchemas(context.Context) (json.RawMessage, error) {
	bounds := func(kind string, min, max float64) []any {
		return []any{kind, map[string]float64{"min": min, "max": max}}
	}
	semantic := map[string]any{"max_duration": bounds("FLOAT", .04, 900), "temperature": bounds("FLOAT", 0, 5),
		"top_p": bounds("FLOAT", .01, 1), "top_k": bounds("INT", 1, 32768), "repetition_penalty": bounds("FLOAT", .01, 10)}
	score := map[string]any{"max_abc_tokens": bounds("INT", 1, 30000), "temperature": bounds("FLOAT", 0, 5),
		"top_p": bounds("FLOAT", .01, 1), "top_k": bounds("INT", 1, 32768), "repetition_penalty": bounds("FLOAT", .01, 10),
		"penalty_window": bounds("INT", 0, 1000)}
	render := map[string]any{"steps": bounds("INT", 1, 100), "cfg": bounds("FLOAT", 0, 100), "denoise": bounds("FLOAT", 0, 1),
		"sampler_name": []any{[]string{"dpm_2"}}, "scheduler": []any{[]string{"sgm_uniform"}}}
	return json.Marshal(map[string]any{
		"YuE2GenerateMusic": map[string]any{"input": map[string]any{"required": semantic, "optional": map[string]any{"cfg_scale": bounds("FLOAT", 0, 100)}}},
		"YuE2GenerateABC":   map[string]any{"input": map[string]any{"required": score}},
		"KSampler":          map[string]any{"input": map[string]any{"required": render}},
	})
}

func yuEChatCall(t *testing.T, f *localHomeFixture, id, tool string, input map[string]any) string {
	t.Helper()
	var seen string
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		called := false
		return localModelFunc(func(_ context.Context, system string, turns []map[string]string) (string, error) {
			if !called {
				called = true
				if !strings.Contains(system, tool) {
					return "", errors.New("YuE tool missing from schema")
				}
				return localToolCall(tool, input), nil
			}
			seen = localModelHistory(turns)
			return `{"action":"reply","messages":["已检查 YuE 任务。"]}`, nil
		}), nil
	}
	if code, out := localChatSend(f, id, "处理合成音乐任务"); code != http.StatusOK || out["status"] != "completed" || seen == "" {
		t.Fatalf("YuE chat: %d %#v", code, out)
	}
	return seen
}

func TestLocalYuEChatClaimRecoveryCancellationAndDownload(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("local FFmpeg not installed")
	}
	ffmpeg, err = filepath.Abs(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "fixture.flac")
	if output, err := exec.Command(ffmpeg, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000:duration=1", "-c:a", "flac", file).CombinedOutput(); err != nil {
		t.Fatalf("synthetic FLAC: %v %.200s", err, output)
	}
	flac, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	workflowDir, _ := filepath.Abs(filepath.Join("..", "..", "components", "yue2", "app", "workflows_api"))
	pins := map[stationyue.Mode]string{}
	for mode, name := range map[stationyue.Mode]string{stationyue.ModeOriginal: "audio_yue2_text_to_music.json", stationyue.ModeCover: "audio_yue2_music_cover.json"} {
		raw, err := os.ReadFile(filepath.Join(workflowDir, name))
		if err != nil {
			t.Fatal(err)
		}
		pins[mode] = sha256Hex(raw)
	}
	backend := &syntheticYuE{deploy: localYuEDeployment{Binding: strings.Repeat("b", 64), WorkflowRoot: workflowDir, FFmpegPath: ffmpeg, Pins: pins},
		flac: comfyui.FLAC{Data: flac, SHA256: sha256Hex(flac), Filename: "song_00001.flac"}, wav: syntheticMonoWAV(t, 8000, 8000), lost: true}
	f := localChatFixture(t)
	catalog, _ := filepath.Abs(filepath.Join("..", "..", "plugins"))
	f.handler.chat.projectTools.options.CatalogDirectory = catalog
	f.handler.chat.projectTools.yue = backend
	const route = "/api/local/project-tools/yue"
	if code, _ := f.request("PUT", route, map[string]any{"action": "install"}, true); code != http.StatusBadRequest {
		t.Fatalf("YuE installed without approval: %d", code)
	}
	if code, out := f.request("PUT", route, map[string]any{"action": "install", "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatalf("install YuE: %d %#v", code, out)
	}
	installed := localProjectToolByID(t, f, localYuEPackage)
	if code, out := f.request("PUT", route, map[string]any{"action": "enable", "revision": localProjectToolRevision(t, installed)}, true); code != http.StatusOK {
		t.Fatalf("enable YuE: %d %#v", code, out)
	}
	if backend.starts != 0 {
		t.Fatal("enabling should be on demand")
	}
	_, home := f.request("GET", "/api/local/home", nil, true)
	const referenceFile = "Project Tools/synthetic-yue.wav"
	if err := os.WriteFile(filepath.Join(home["directory"].(string), referenceFile), backend.wav, 0600); err != nil {
		t.Fatal(err)
	}
	code, registered := f.request("POST", "/api/local/artifacts", map[string]string{
		"path": referenceFile, "expected_sha256": sha256Hex(backend.wav), "request_key": "yue-reference-fixture",
	}, true)
	if code != http.StatusOK {
		t.Fatalf("reference registration: %d %#v", code, registered)
	}
	referenceID := registered["artifact_id"].(string)
	submit := map[string]any{"mode": "cover", "style": "folk", "lyrics": "[verse]\nTest.", "duration_seconds": 1, "reference_artifact_id": referenceID}
	jobPattern := regexp.MustCompile(`job_id[^a-f0-9]+([a-f0-9]{64})`)
	match := jobPattern.FindStringSubmatch(yuEChatCall(t, f, "yue-submit", localYuEStartTool, submit))
	if len(match) != 2 || backend.submits != 1 || backend.uploads != 1 {
		t.Fatalf("submission receipts: %v submits=%d uploads=%d", match, backend.submits, backend.uploads)
	}
	jobID := match[1]
	fish, _, err := fishidentity.New(f.handler.home.dataDir).Bindings()
	if err != nil {
		t.Fatal(err)
	}
	host, err := f.handler.chat.projectTools.forFish(fish)
	if err != nil {
		t.Fatal(err)
	}
	job, err := host.store.Job(context.Background(), jobID)
	var receipt localYuESubmission
	if err != nil || job.State != "needs_review" || json.Unmarshal(job.Submission, &receipt) != nil ||
		receipt.Effective.ReferenceSHA256 != sha256Hex(backend.wav) || receipt.Effective.DurationSeconds != 1 {
		t.Fatalf("effective recipe missing after lost submission: %+v %v", receipt, err)
	}
	if code, result := localChatSend(f, "yue-submit", "处理合成音乐任务"); code != http.StatusOK || result["reused"] != true || backend.submits != 1 {
		t.Fatalf("duplicate request: %d %#v submits=%d", code, result, backend.submits)
	}
	backend.state = comfyui.Completed
	history := yuEChatCall(t, f, "yue-status", localYuEJobTool, map[string]any{"job_id": jobID, "action": "status"})
	artifacts := regexp.MustCompile(`work-[a-f0-9]{64}`).FindAllString(history, -1)
	var downloaded string
	for _, id := range artifacts {
		if id != referenceID {
			downloaded = id
		}
	}
	if downloaded == "" || !bytes.Equal(localArtifactDownload(t, f, downloaded), flac) {
		t.Fatal("verified FLAC missing from local chat download")
	}
	tools := projectFollowupFixtureTools(t, f, "yue-submit")
	view, waiting, err := tools.projectFollowupStatus(t.Context(), localYuEJobTool, jobID)
	if err != nil || waiting || view["state"] != "completed" || view["artifact_id"] != downloaded {
		t.Fatal("YuE background read did not reuse the verified output", view, waiting, err)
	}
	match = jobPattern.FindStringSubmatch(yuEChatCall(t, f, "yue-second", localYuEStartTool, map[string]any{
		"mode": "original", "style": "folk", "lyrics": "test", "duration_seconds": 1,
	}))
	if len(match) != 2 {
		t.Fatal("second job missing")
	}
	before := backend.fetches
	yuEChatCall(t, f, "yue-cancel", localYuEJobTool, map[string]any{"job_id": match[1], "action": "cancel"})
	cancelled, err := host.store.Job(context.Background(), match[1])
	if err != nil || !cancelled.CancelRequested || cancelled.State != "cancelled" || len(cancelled.Artifacts) != 0 || backend.cancels != 1 || backend.fetches != before {
		t.Fatalf("late result escaped cancellation: %+v %v", cancelled, err)
	}
	beforeSubmits := backend.submits
	badRecipe := yuEChatCall(t, f, "yue-invalid-recipe", localYuEStartTool, map[string]any{
		"mode": "original", "style": "folk", "lyrics": "test", "duration_seconds": 901,
	})
	match = jobPattern.FindStringSubmatch(badRecipe)
	if len(match) != 2 || !strings.Contains(badRecipe, "recipe_validation_failed") {
		t.Fatal("invalid recipe did not return its job and preparation error")
	}
	failed, err := host.store.Job(context.Background(), match[1])
	if err != nil || failed.State != "failed" || failed.Diagnostic == nil || failed.Diagnostic.Code != "recipe_validation_failed" || backend.submits != beforeSubmits {
		t.Fatalf("invalid recipe was not closed before dispatch: %+v %v", failed, err)
	}
	enabled := localProjectToolByID(t, f, localYuEPackage)
	if code, out := f.request("PUT", route, map[string]any{"action": "disable", "revision": localProjectToolRevision(t, enabled)}, true); code != http.StatusOK || backend.stops != 1 {
		t.Fatalf("disable YuE: %d %#v", code, out)
	}
	if !bytes.Equal(localArtifactDownload(t, f, downloaded), flac) {
		t.Fatal("disable changed previously verified song")
	}
}
