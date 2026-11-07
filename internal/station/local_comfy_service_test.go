package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"wechat-bot/internal/comfyui"
)

func TestLocalComfyInfoLoadsPinnedPrivateDeployment(t *testing.T) {
	data, home, registration := writeLocalComfyFixture(t)
	service := newLocalComfyServiceAt(data, home)
	info, err := service.Info(context.Background(), "dafeiyu")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Binding == "" || info.WorkflowRoot != registration.Workflows || info.Running || info.Busy {
		t.Fatalf("unexpected deployment: %#v", info)
	}
	if _, err := os.Stat(filepath.Join(data, "station-comfyui", localComfyFishHash("dafeiyu"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Info must not create a service scratch directory: %v", err)
	}
}

func TestLocalComfyUseRecoveryNeverAdoptsLoopbackListener(t *testing.T) {
	data, home, registration := writeLocalComfyFixture(t)
	service := newLocalComfyServiceAt(data, home)
	manifest, err := localComfyLoadManifest(registration)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(registration.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	called := false
	err = service.Use(context.Background(), "dafeiyu", localComfyBinding(registration, manifest), false, func(localComfyClient) error {
		called = true
		return nil
	})
	if !errors.Is(err, errLocalComfyNotRunning) || called {
		t.Fatalf("unowned listener must not be adopted, err=%v called=%v", err, called)
	}
}

func TestLocalComfyJournalPersistsUnknownUntilTerminal(t *testing.T) {
	data, home, registration := writeLocalComfyFixture(t)
	service := newLocalComfyServiceAt(data, home)
	manifest, err := localComfyLoadManifest(registration)
	if err != nil {
		t.Fatal(err)
	}
	binding := localComfyBinding(registration, manifest)
	jobID := uuid.NewString()
	if err := service.recordJob("dafeiyu", binding, jobID, "unknown"); err != nil {
		t.Fatalf("record unknown: %v", err)
	}
	path := filepath.Join(data, "station-comfyui", localComfyFishHash("dafeiyu"), "jobs.json")
	dataBytes, err := os.ReadFile(path)
	if err != nil || string(dataBytes) == "" || !containsLocalComfyState(dataBytes, jobID, "unknown") {
		t.Fatalf("unknown job missing: %v %s", err, string(dataBytes))
	}
	if err := service.recordJob("dafeiyu", binding, jobID, "completed"); err != nil {
		t.Fatalf("record completed: %v", err)
	}
	dataBytes, err = os.ReadFile(path)
	if err != nil || !containsLocalComfyState(dataBytes, jobID, "completed") {
		t.Fatalf("terminal job missing: %v %s", err, string(dataBytes))
	}
}

// TestLocalComfyOwnedBackend is intentionally opt-in. It launches the audited
// portable installation with CPU EmptyImage only, uses a unique loopback port,
// and leaves both the source/runtime and production data untouched.
func TestLocalComfyOwnedBackend(t *testing.T) {
	root := t.TempDir()
	data, workflows := filepath.Join(root, "data"), filepath.Join(root, "workflows")
	for _, directory := range []string{data, workflows} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	service := configureLocalComfyPortable(t, data, workflows)
	info, err := service.Info(context.Background(), "dafeiyu")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := comfyui.NewJobRef("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "9")
	if err != nil {
		t.Fatal(err)
	}
	graph := []byte(`{"1":{"class_type":"EmptyImage","inputs":{"width":32,"height":24,"batch_size":2,"color":16711680}},"9":{"class_type":"SaveImage","inputs":{"images":["1",0],"filename_prefix":"owner-prefix"}}}`)
	sum := sha256.Sum256(graph)
	prepared, err := comfyui.Prepare(graph, hex.EncodeToString(sum[:]), ref)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	err = service.Use(ctx, "dafeiyu", info.Binding, true, func(client localComfyClient) error {
		if err := client.Submit(ctx, prepared); err != nil {
			return err
		}
		for {
			status, err := client.Status(ctx, ref)
			if err != nil {
				return err
			}
			switch status.State {
			case comfyui.Completed:
				if status.ImageCount != 2 {
					return errors.New("unexpected image count")
				}
				for index := 0; index < status.ImageCount; index++ {
					if _, err := client.FetchPNG(ctx, ref, index); err != nil {
						return err
					}
				}
				return client.MarkSettled(ctx, ref)
			case comfyui.Failed, comfyui.Cancelled:
				return errors.New("ComfyUI EmptyImage job did not complete")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	registration, _, err := service.loadRegistration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	connection, dialErr := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(registration.Port), time.Second)
	if dialErr == nil {
		connection.Close()
		t.Fatal("owned ComfyUI listener remained reachable after Close")
	}
}

// configureLocalComfyPortable registers an opt-in installed portable for an
// isolated test. The manifest is written to the supplied host data directory;
// the real portable directory stays read-only. Tests in this package can call
// Info to obtain the resulting exact deployment binding.
func configureLocalComfyPortable(t *testing.T, dataDir, workflows string) *localComfyService {
	t.Helper()
	runtime := os.Getenv("AHS_TEST_COMFY_PORTABLE")
	if runtime == "" {
		t.Skip("set AHS_TEST_COMFY_PORTABLE to run an owned ComfyUI CPU test")
	}
	runtime = filepath.Clean(runtime)
	python := filepath.Join(runtime, "python_embeded", "python.exe")
	entry := filepath.Join(runtime, "ComfyUI", "main.py")
	frontend := filepath.Join(runtime, "python_embeded", "Lib", "site-packages", "comfyui_frontend_package", "static")
	for _, path := range []string{python, entry} {
		if !localComfySafeRegular(path) {
			t.Fatalf("test portable file unavailable: %s", path)
		}
	}
	if !localComfyServiceSafeDirectory(frontend) {
		t.Fatal("test portable frontend unavailable")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	models := filepath.Join(filepath.Dir(dataDir), "empty-models")
	if err := os.MkdirAll(models, 0700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dataDir, "station-manifest.json")
	if err := writeLocalComfyManifest(manifestPath, runtime, filepath.Join("python_embeded", "python.exe"), filepath.Join("ComfyUI", "main.py")); err != nil {
		t.Fatal(err)
	}
	registration := localComfyRegistration{SchemaVersion: 1, Enabled: true, Runtime: runtime, Python: filepath.Join("python_embeded", "python.exe"), Entry: filepath.Join("ComfyUI", "main.py"), Frontend: filepath.Join("python_embeded", "Lib", "site-packages", "comfyui_frontend_package", "static"), Manifest: manifestPath, Models: models, Workflows: workflows, Port: freeLocalComfyPort(t), Device: "cpu", IdleSeconds: 60}
	raw, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, localComfyRegistrationFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return newLocalComfyServiceAt(dataDir, filepath.Join(filepath.Dir(dataDir), "agent-home"))
}

func containsLocalComfyState(raw []byte, id, state string) bool {
	var jobs map[string]struct {
		State string `json:"state"`
	}
	return json.Unmarshal(raw, &jobs) == nil && jobs[id].State == state
}

func writeLocalComfyFixture(t *testing.T) (string, string, localComfyRegistration) {
	t.Helper()
	root := t.TempDir()
	data, home, runtime := filepath.Join(root, "data"), filepath.Join(root, "home"), filepath.Join(root, "runtime")
	for _, directory := range []string{data, home, runtime, filepath.Join(runtime, "ComfyUI"), filepath.Join(runtime, "frontend"), filepath.Join(root, "models"), filepath.Join(root, "workflows")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{"python.exe": []byte("fixture python"), filepath.Join("ComfyUI", "main.py"): []byte("fixture main")}
	manifestFiles := make(map[string]string, len(files))
	for relative, content := range files {
		path := filepath.Join(runtime, filepath.FromSlash(relative))
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		manifestFiles[relative] = hex.EncodeToString(sum[:])
	}
	manifest, err := json.Marshal(localComfyManifest{SchemaVersion: 1, Policy: "station-comfyui-v1", Files: manifestFiles})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(data, "station-manifest.json")
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	port := freeLocalComfyPort(t)
	registration := localComfyRegistration{SchemaVersion: 1, Enabled: true, Runtime: runtime, Python: "python.exe", Entry: filepath.Join("ComfyUI", "main.py"), Frontend: "frontend", Manifest: manifestPath, Models: filepath.Join(root, "models"), Workflows: filepath.Join(root, "workflows"), Port: port, Device: "cpu"}
	raw, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, localComfyRegistrationFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return data, home, registration
}

func writeLocalComfyManifest(path, runtime string, names ...string) error {
	files := make(map[string]string, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(runtime, name))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[name] = hex.EncodeToString(sum[:])
	}
	raw, err := json.Marshal(localComfyManifest{SchemaVersion: 1, Policy: "station-comfyui-v1", Files: files})
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func freeLocalComfyPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
