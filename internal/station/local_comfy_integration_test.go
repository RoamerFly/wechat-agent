package station

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/fishidentity"
)

type syntheticLocalComfy struct {
	root      string
	fish      string
	prompt    comfyui.JobRef
	image     comfyui.Image
	state     comfyui.State
	submits   int
	starts    int
	fetches   int
	cancels   int
	stops     int
	lost      bool
	running   bool
	failFetch bool
}

func (s *syntheticLocalComfy) Info(_ context.Context, fish string) (localComfyDeployment, error) {
	if s.fish != "" && s.fish != fish {
		return localComfyDeployment{}, errors.New("wrong fixture scope")
	}
	return localComfyDeployment{Binding: "fixture-comfy-binding", WorkflowRoot: s.root, Running: s.running}, nil
}

func (s *syntheticLocalComfy) Use(_ context.Context, fish, binding string, start bool, fn func(localComfyClient) error) error {
	if fish != s.fish || binding != "fixture-comfy-binding" {
		return errors.New("wrong fixture scope")
	}
	if start && !s.running {
		s.starts++
		s.running = true
	}
	if !s.running {
		return errLocalComfyNotRunning
	}
	return fn(s)
}

func (s *syntheticLocalComfy) Check(context.Context) error { return nil }

func (s *syntheticLocalComfy) MarkSettled(context.Context, comfyui.JobRef) error { return nil }

func (s *syntheticLocalComfy) Submit(_ context.Context, p comfyui.Prepared) error {
	s.submits++
	s.prompt = p.Ref()
	s.state = comfyui.Running
	if s.lost {
		return comfyui.ErrSubmissionUnknown
	}
	return nil
}

func (s *syntheticLocalComfy) Status(_ context.Context, ref comfyui.JobRef) (comfyui.JobStatus, error) {
	if ref != s.prompt {
		return comfyui.JobStatus{}, errors.New("looked up a different prompt")
	}
	return comfyui.JobStatus{State: s.state, ImageCount: 2}, nil
}

func (s *syntheticLocalComfy) Cancel(_ context.Context, ref comfyui.JobRef) (bool, error) {
	if ref != s.prompt {
		return false, errors.New("cancelled a different prompt")
	}
	s.cancels++
	s.state = comfyui.Cancelled
	return true, nil
}

func (s *syntheticLocalComfy) FetchPNG(_ context.Context, ref comfyui.JobRef, index int) (comfyui.Image, error) {
	if ref != s.prompt || s.state != comfyui.Completed || index == 1 && s.failFetch {
		return comfyui.Image{}, errors.New("fixture image unavailable")
	}
	s.fetches++
	return s.image, nil
}

func (s *syntheticLocalComfy) Stop(context.Context) error {
	s.stops++
	s.running = false
	return nil
}

func (s *syntheticLocalComfy) Close(ctx context.Context) error { return s.Stop(ctx) }

func setUpLocalComfyChat(t *testing.T, f *localHomeFixture, backend localComfyBackend) {
	t.Helper()
	catalog, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	f.handler.chat.projectTools.options.CatalogDirectory = catalog
	f.handler.chat.projectTools.comfy = backend
	const route = "/api/local/project-tools/comfyui"
	if code, _ := f.request("PUT", route, map[string]any{"action": "install"}, true); code != http.StatusBadRequest {
		t.Fatalf("ComfyUI installed without approval: %d", code)
	}
	if code, out := f.request("PUT", route, map[string]any{"action": "install", "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatalf("install ComfyUI: %d %#v", code, out)
	}
	installed := localProjectToolByID(t, f, localComfyPackage)
	if code, out := f.request("PUT", route, map[string]any{"action": "enable", "revision": localProjectToolRevision(t, installed)}, true); code != http.StatusOK {
		t.Fatalf("enable ComfyUI: %d %#v", code, out)
	}
}

func requestLocalComfyJob(t *testing.T, f *localHomeFixture, requestID string) string {
	t.Helper()
	var jobID string
	jobPattern := regexp.MustCompile(`job_id[^a-f0-9]+([a-f0-9]{64})`)
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		round := 0
		return localModelFunc(func(_ context.Context, system string, turns []map[string]string) (string, error) {
			round++
			switch round {
			case 1:
				if !strings.Contains(system, localComfyStartTool) {
					return "", errors.New("ComfyUI missing from schema")
				}
				return localToolCall(localComfyListTool, map[string]any{}), nil
			case 2:
				if !strings.Contains(localModelHistory(turns), "Offline smoke") {
					return "", errors.New("approved workflow missing from discovery")
				}
				return localToolCall(localComfyStartTool, map[string]any{"workflow_id": "smoke", "inputs": map[string]int{"width": 48}}), nil
			default:
				matches := jobPattern.FindStringSubmatch(localModelHistory(turns))
				if len(matches) != 2 {
					return "", errors.New("ComfyUI job receipt missing")
				}
				jobID = matches[1]
				return `{"action":"reply","messages":["已登记任务。"]}`, nil
			}
		}), nil
	}
	if code, out := localChatSend(f, requestID, "生成两张离线图像"); code != http.StatusOK || out["status"] != "completed" || jobID == "" {
		t.Fatalf("ComfyUI submit chat: %d %#v job=%q", code, out, jobID)
	}
	return jobID
}

func requestLocalComfyStatus(t *testing.T, f *localHomeFixture, requestID, jobID, action string) []string {
	t.Helper()
	var ids []string
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		called := false
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			if !called {
				called = true
				return localToolCall(localComfyJobTool, map[string]string{"job_id": jobID, "action": action}), nil
			}
			ids = projectArtifactIDs(turns)
			return `{"action":"reply","messages":["已检查原任务。"]}`, nil
		}), nil
	}
	if code, out := localChatSend(f, requestID, "核对原 ComfyUI 任务"); code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("ComfyUI job chat: %d %#v", code, out)
	}
	return ids
}

func writeLocalComfyChatWorkflow(t *testing.T, root string) {
	t.Helper()
	graph := []byte(`{"1":{"class_type":"EmptyImage","inputs":{"width":32,"height":24,"batch_size":2,"color":16711680}},"9":{"class_type":"SaveImage","inputs":{"images":["1",0],"filename_prefix":"owner-prefix"}}}`)
	writeLocalComfyRegistry(t, root, graph, `[{"id":"width","label":"Width","kind":"integer","minimum":16,"maximum":512,"node":"1","input":"width"}]`)
}

func TestLocalComfyChatJobsAndArtifacts(t *testing.T) {
	f := localChatFixture(t)
	root := t.TempDir()
	writeLocalComfyChatWorkflow(t, root)
	fish, _, err := fishidentity.New(f.handler.home.dataDir).Bindings()
	if err != nil || fish == "" {
		t.Fatal("fixture fish missing", err)
	}
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 48, 24))); err != nil {
		t.Fatal(err)
	}
	backend := &syntheticLocalComfy{root: root, fish: fish, lost: true, image: comfyui.Image{
		Data: data.Bytes(), SHA256: localComfyDigest(data.Bytes()), Width: 48, Height: 24,
	}}
	setUpLocalComfyChat(t, f, backend)
	if backend.starts != 0 {
		t.Fatal("installation or discovery launched ComfyUI")
	}
	jobID := requestLocalComfyJob(t, f, "comfy-submit")
	if code, out := localChatSend(f, "comfy-submit", "生成两张离线图像"); code != http.StatusOK || out["reused"] != true || backend.submits != 1 {
		t.Fatalf("replayed chat resubmitted: %d %#v calls=%d", code, out, backend.submits)
	}
	backend.state = comfyui.Completed
	backend.failFetch = true
	if ids := requestLocalComfyStatus(t, f, "comfy-status-partial", jobID, "status"); len(ids) != 0 {
		t.Fatalf("uncommitted output was exposed: %v", ids)
	}
	artifactRoot := filepath.Join(f.handler.home.dataDir, "artifacts")
	entries, err := os.ReadDir(artifactRoot)
	if err != nil || len(entries) != 1 || !artifactID(entries[0].Name(), "work-") {
		t.Fatalf("expected one staged immutable copy: %v %v", entries, err)
	}
	if code, _ := f.request("GET", "/api/local/artifacts/"+entries[0].Name(), nil, true); code != http.StatusNotFound {
		t.Fatalf("uncommitted output readable: %d", code)
	}
	host, err := f.handler.chat.projectTools.forFish(fish)
	if err != nil {
		t.Fatal(err)
	}
	record, err := host.store.Job(context.Background(), jobID)
	if err != nil || record.State != "running" || record.RemoteID != backend.prompt.ID {
		t.Fatalf("lost receipt did not reconcile: %+v %v", record, err)
	}
	backend.failFetch = false
	ids := requestLocalComfyStatus(t, f, "comfy-status", jobID, "status")
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("expected two distinct artifact IDs, got %v", ids)
	}
	for _, id := range ids {
		if got := localArtifactDownload(t, f, id); !bytes.Equal(got, data.Bytes()) {
			t.Fatal("ComfyUI download differs from the verified result")
		}
	}
	second := requestLocalComfyJob(t, f, "comfy-submit-cancel")
	if second == jobID {
		t.Fatal("new request reused old job identity")
	}
	before := backend.fetches
	if ids := requestLocalComfyStatus(t, f, "comfy-cancel", second, "cancel"); len(ids) != 0 || backend.cancels != 1 || backend.fetches != before {
		t.Fatalf("cancel performed the wrong operation: ids=%v cancels=%d fetches=%d", ids, backend.cancels, backend.fetches)
	}
	if job, err := host.store.Job(context.Background(), second); err != nil || job.State != "cancelled" || !job.CancelRequested {
		t.Fatalf("cancellation was not durable: %+v %v", job, err)
	}
	third := requestLocalComfyJob(t, f, "comfy-submit-before-exit")
	backend.running = false
	starts := backend.starts
	if ids := requestLocalComfyStatus(t, f, "comfy-status-after-exit", third, "status"); len(ids) != 0 || backend.starts != starts {
		t.Fatalf("query started a new backend: ids=%v starts=%d", ids, backend.starts)
	}
	enabled := localProjectToolByID(t, f, localComfyPackage)
	if code, out := f.request("PUT", "/api/local/project-tools/comfyui", map[string]any{"action": "disable", "revision": localProjectToolRevision(t, enabled)}, true); code != http.StatusOK || backend.stops != 1 {
		t.Fatalf("ComfyUI disable: %d %#v stops=%d", code, out, backend.stops)
	}
	if got := localArtifactDownload(t, f, ids[0]); !bytes.Equal(got, data.Bytes()) {
		t.Fatal("disabling ComfyUI removed an already verified artifact")
	}
}

func TestLocalComfyOwnedWindowsThroughChat(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("AHS_TEST_COMFY_PORTABLE") == "" {
		t.Skip("set AHS_TEST_COMFY_PORTABLE to test the owned Windows CPU service")
	}
	f := localChatFixture(t)
	root := t.TempDir()
	writeLocalComfyChatWorkflow(t, root)
	backend := configureLocalComfyPortable(t, f.handler.home.dataDir, root)
	backend.editableRoot = f.handler.home.root
	setUpLocalComfyChat(t, f, backend)
	jobID := requestLocalComfyJob(t, f, "comfy-native-submit")
	var ids []string
	for i := 0; i < 12 && len(ids) == 0; i++ {
		ids = requestLocalComfyStatus(t, f, fmt.Sprintf("comfy-native-status-%d", i), jobID, "status")
		if len(ids) == 0 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("ComfyUI native batch: %v", ids)
	}
	for _, id := range ids {
		picture, err := png.Decode(bytes.NewReader(localArtifactDownload(t, f, id)))
		if err != nil || picture.Bounds().Dx() != 48 || picture.Bounds().Dy() != 24 {
			t.Fatalf("registered width override did not reach native ComfyUI: %v", err)
		}
		r, g, b, _ := picture.At(0, 0).RGBA()
		if r != 65535 || g != 0 || b != 0 {
			t.Fatalf("wrong native CPU output: %d %d %d", r, g, b)
		}
	}
	enabled := localProjectToolByID(t, f, localComfyPackage)
	if code, out := f.request("PUT", "/api/local/project-tools/comfyui", map[string]any{"action": "disable", "revision": localProjectToolRevision(t, enabled)}, true); code != http.StatusOK {
		t.Fatalf("stop owned ComfyUI: %d %#v", code, out)
	}
	fish, _, err := fishidentity.New(f.handler.home.dataDir).Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := backend.Info(context.Background(), fish); err != nil || info.Running {
		t.Fatalf("owned process still present after disable: %+v %v", info, err)
	}
}
