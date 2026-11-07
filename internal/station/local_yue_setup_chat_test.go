package station

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

// Keep real deployment admission in this replay. Only GPU inference is a
// synthetic backend; completed output still passes the actual audio decoder.
type setupReplayYuE struct {
	*syntheticYuE
	registration *localYuEService
}

func (s *setupReplayYuE) Info(ctx context.Context, fish string) (localYuEDeployment, error) {
	d, err := s.registration.Info(ctx, fish)
	if err == nil {
		s.deploy = d
	}
	return d, err
}

func TestLocalYuEDeploymentToChatAndVerifiedArtifact(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("local FFmpeg unavailable")
	}
	ffmpeg, err = filepath.Abs(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	f, configuration := yueDeploymentFixture(t)
	configuration.Manifest, configuration.FFmpeg, configuration.FFmpegSHA256 = "", ffmpeg, ""
	configuration.WorkflowPins = nil
	code, saved := f.request("PUT", yueDeploymentRoute, yueDeploymentInput("unconfigured", configuration), true)
	if code != 200 || saved["configured"] != true {
		t.Fatalf("save: %d %#v", code, saved)
	}
	service := f.handler.chat.projectTools.yue.(*localYuEService)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	if info, err := service.Info(t.Context(), "dafeiyu"); err != nil || info.Running {
		t.Fatalf("saving must not run inference: %+v %v", info, err)
	}
	file := filepath.Join(t.TempDir(), "synthetic.flac")
	if output, err := exec.Command(ffmpeg, "-v", "error", "-nostdin", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000:duration=1", "-c:a", "flac", file).CombinedOutput(); err != nil {
		t.Fatalf("synthetic audio: %v %.200s", err, output)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	backend := &setupReplayYuE{registration: service, syntheticYuE: &syntheticYuE{flac: comfyui.FLAC{Data: raw, SHA256: sha256Hex(raw), Filename: "fixture.flac"}}}
	f.handler.chat.projectTools.yue = backend
	f.handler.chat.projectTools.options.CatalogDirectory, _ = filepath.Abs(filepath.Join("..", "..", "plugins"))
	const toolRoute = "/api/local/project-tools/yue"
	if code, out := f.request("PUT", toolRoute, map[string]any{"action": "install", "approve_artifacts": true}, true); code != 200 {
		t.Fatal("install", code, out)
	}
	installed := localProjectToolByID(t, f, localYuEPackage)
	if code, out := f.request("PUT", toolRoute, map[string]any{"action": "enable", "revision": localProjectToolRevision(t, installed)}, true); code != 200 {
		t.Fatal("enable", code, out)
	}
	if backend.starts != 0 {
		t.Fatal("enabling started inference eagerly")
	}
	var jobID string
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return &syntheticObservationModel{call: func(_ context.Context, _ string, history []map[string]string, _ []mcptools.Tool) (string, error) {
			calls++
			switch calls {
			case 1:
				return localToolCall("discover_tools", map[string]any{"query": "project_yue"}), nil
			case 2:
				return localToolCall(localYuEStartTool, map[string]any{"mode": "original", "style": "folk", "lyrics": "[verse]\nSynthetic song.", "duration_seconds": 1}), nil
			case 3:
				ids := localPublicationJobIDs(localModelHistory(history))
				if len(ids) != 1 {
					t.Fatal("missing music task", localModelHistory(history))
				}
				jobID = ids[0]
				backend.state = comfyui.Completed
				return localToolCall(localYuEJobTool, map[string]any{"job_id": jobID, "action": "status"}), nil
			default:
				text := localModelHistory(history)
				if !strings.Contains(text, "artifact_id") {
					t.Fatal("verified product missing from continuation", text)
				}
				for _, private := range []string{configuration.Runtime, configuration.Workflows, ffmpeg} {
					if strings.Contains(text, private) {
						t.Fatal("deployment path entered model history")
					}
				}
				return `{"action":"reply","messages":["合成测试作品已完成。"]}`, nil
			}
		}}, nil
	}
	if code, out := localChatSend(f, "yue-setup-replay", "创作一段合成测试音乐"); code != 200 || out["status"] != "completed" {
		t.Fatal("chat", code, out)
	}
	identity, _ := f.handler.home.identity.Read()
	host, err := f.handler.chat.projectTools.forFish(identity.FishID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := host.store.Job(t.Context(), jobID)
	if err != nil || job.State != "completed" || len(job.Artifacts) != 1 || backend.submits != 1 {
		t.Fatalf("result: %+v %v, submissions %d", job, err, backend.submits)
	}
	if !bytes.Equal(localArtifactDownload(t, f, job.Artifacts[0]), raw) {
		t.Fatal("download differs from verified synthetic output")
	}
	if code, out := localChatSend(f, "yue-setup-replay", "创作一段合成测试音乐"); code != 200 || out["status"] != "completed" || backend.submits != 1 {
		t.Fatal("replayed request duplicated generation", code, out)
	}
}
