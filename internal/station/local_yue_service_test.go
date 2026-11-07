package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"wechat-bot/internal/stationyue"
)

func TestLocalYuEInfoLoadsDedicatedPinnedDeployment(t *testing.T) {
	data, home, registration := writeLocalYuEFixture(t)
	service := newLocalYuEServiceAt(data, home)
	info, err := service.Info(context.Background(), "dafeiyu")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Binding == "" || info.WorkflowRoot != registration.Workflows || info.FFmpegPath != registration.FFmpeg || info.Running || info.Busy {
		t.Fatalf("unexpected deployment: %#v", info)
	}
	if info.Pins[stationyue.ModeOriginal] != registration.WorkflowPins[string(stationyue.ModeOriginal)] || info.Pins[stationyue.ModeCover] != registration.WorkflowPins[string(stationyue.ModeCover)] {
		t.Fatalf("workflow pins lost: %#v", info.Pins)
	}
	if _, err := os.Stat(filepath.Join(data, "station-yue", localYuEFishHash("dafeiyu"))); !os.IsNotExist(err) {
		t.Fatalf("Info must not create a YuE scratch directory: %v", err)
	}
}

func TestLocalYuERejectsWorkflowOrFFmpegChanges(t *testing.T) {
	data, home, registration := writeLocalYuEFixture(t)
	service := newLocalYuEServiceAt(data, home)
	if err := os.WriteFile(filepath.Join(registration.Workflows, "audio_yue2_music_cover.json"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Info(context.Background(), "dafeiyu"); err == nil {
		t.Fatal("changed workflow must not be accepted")
	}
	// Restore a clean fixture, then prove the externally configured FFmpeg pin
	// participates in admission as well as in the durable binding.
	data, home, registration = writeLocalYuEFixture(t)
	if err := os.WriteFile(registration.FFmpeg, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newLocalYuEServiceAt(data, home).Info(context.Background(), "dafeiyu"); err == nil {
		t.Fatal("changed ffmpeg must not be accepted")
	}
}

func TestLocalYuEUsesIndependentServiceIdentityAndSafeArguments(t *testing.T) {
	_, _, r := writeLocalYuEFixture(t)
	if localYuEServiceID("dafeiyu") == localComfyServiceID("dafeiyu") || localYuEFishHash("dafeiyu") == localComfyFishHash("dafeiyu") {
		t.Fatal("YuE must not share the image service identity or scratch key")
	}
	args := localYuEArguments(r, filepath.Join(r.Runtime, r.Entry), filepath.Join(r.Runtime, r.Frontend), t.TempDir())
	if !containsLocalYuEArgument(args, "--disable-all-custom-nodes") || containsLocalYuEArgument(args, "--enable-custom-nodes") {
		t.Fatalf("YuE must run its built-in nodes without third-party custom nodes: %v", args)
	}
}

func TestLocalYuEJournalKeepsSubmissionUntilSettled(t *testing.T) {
	data, home, registration := writeLocalYuEFixture(t)
	service := newLocalYuEServiceAt(data, home)
	manifest, err := localYuELoadManifest(registration)
	if err != nil {
		t.Fatal(err)
	}
	jobID := uuid.NewString()
	if err := service.recordJob("dafeiyu", localYuEBinding(registration, manifest), jobID, "submitted"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "station-yue", localYuEFishHash("dafeiyu"), "jobs.json")
	raw, err := os.ReadFile(path)
	if err != nil || !containsLocalComfyState(raw, jobID, "submitted") {
		t.Fatalf("submission journal missing: %v %s", err, raw)
	}
	if err := service.recordJob("dafeiyu", localYuEBinding(registration, manifest), jobID, "delivered"); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil || !containsLocalComfyState(raw, jobID, "delivered") {
		t.Fatalf("settlement journal missing: %v %s", err, raw)
	}
}

func containsLocalYuEArgument(args []string, want string) bool {
	for _, value := range args {
		if value == want {
			return true
		}
	}
	return false
}

func writeLocalYuEFixture(t *testing.T) (string, string, localYuERegistration) {
	t.Helper()
	root := t.TempDir()
	data, home, runtime := filepath.Join(root, "data"), filepath.Join(root, "home"), filepath.Join(root, "runtime")
	models, workflows, tools := filepath.Join(root, "models"), filepath.Join(root, "workflows"), filepath.Join(root, "tools")
	for _, directory := range []string{data, home, runtime, filepath.Join(runtime, "ComfyUI"), filepath.Join(runtime, "frontend"), models, workflows, tools} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{"python.exe": []byte("fixture python"), filepath.Join("ComfyUI", "main.py"): []byte("fixture main")}
	manifestFiles := make(map[string]string, len(files))
	for relative, content := range files {
		path := filepath.Join(runtime, relative)
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		manifestFiles[relative] = localYuESHA(content)
	}
	manifestPath := filepath.Join(data, "station-yue-manifest.json")
	manifest, _ := json.Marshal(localYuEManifest{SchemaVersion: 1, Policy: "station-yue-v1", Files: manifestFiles})
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	pins := make(map[string]string, 2)
	for mode, name := range map[stationyue.Mode]string{stationyue.ModeOriginal: "audio_yue2_text_to_music.json", stationyue.ModeCover: "audio_yue2_music_cover.json"} {
		content := []byte(`{"fixture":"` + string(mode) + `"}`)
		if err := os.WriteFile(filepath.Join(workflows, name), content, 0600); err != nil {
			t.Fatal(err)
		}
		pins[string(mode)] = localYuESHA(content)
	}
	ffmpeg := filepath.Join(tools, "ffmpeg.exe")
	ffmpegBytes := []byte("fixture ffmpeg")
	if err := os.WriteFile(ffmpeg, ffmpegBytes, 0600); err != nil {
		t.Fatal(err)
	}
	r := localYuERegistration{SchemaVersion: 1, Enabled: true, Runtime: runtime, Python: "python.exe", Entry: filepath.Join("ComfyUI", "main.py"), Frontend: "frontend", Manifest: manifestPath, Models: models, Workflows: workflows, WorkflowPins: pins, FFmpeg: ffmpeg, FFmpegSHA256: localYuESHA(ffmpegBytes), Port: 36123, Device: "cpu"}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, localYuERegistrationFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return data, home, r
}

func localYuESHA(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }

func TestLocalYuEIdleDurationCannotOverflow(t *testing.T) {
	maximum := localComfyMaxIdleSeconds
	if int64(int(maximum+1)) != maximum+1 {
		t.Skip("duration boundary needs a 64-bit int")
	}
	data, home, registration := writeLocalYuEFixture(t)
	for _, seconds := range []int64{0, maximum, maximum + 1} {
		registration.IdleSeconds = int(seconds)
		err := registration.validate(data, home)
		if (err != nil) != (seconds > maximum) {
			t.Fatalf("idle_seconds=%d: %v", seconds, err)
		}
		if err == nil && registration.idleTimeout() <= 0 {
			t.Fatal("accepted idle timeout became nonpositive")
		}
	}
}
