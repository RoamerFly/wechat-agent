package station

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func TestLocalYuEDeploymentComputesOmittedPinsAndPrivateManifest(t *testing.T) {
	f, registration := yueDeploymentFixture(t)
	registration.WorkflowPins, registration.FFmpegSHA256, registration.Manifest = nil, "", ""
	code, saved := f.request("PUT", yueDeploymentRoute, yueDeploymentInput("unconfigured", registration), true)
	if code != 200 {
		t.Fatalf("save selected files: %d %#v", code, saved)
	}
	var actual localYuERegistration
	raw, err := os.ReadFile(filepath.Join(f.dir, localYuERegistrationFile))
	if err != nil || json.Unmarshal(raw, &actual) != nil {
		t.Fatalf("read saved registration: %v", err)
	}
	if len(actual.WorkflowPins) != 2 || actual.FFmpegSHA256 == "" || !localComfyInside(actual.Manifest, f.dir) {
		t.Fatalf("missing computed approvals: %#v", actual)
	}
	if _, err := localYuELoadManifest(actual); err != nil {
		t.Fatal(err)
	}
	if info, err := f.handler.chat.projectTools.yue.Info(t.Context(), "dafeiyu"); err != nil || info.Running {
		t.Fatalf("setup started service: %#v %v", info, err)
	}
	before, _ := filepath.Glob(filepath.Join(f.dir, "station-yue-manifest-*.json"))
	registration.Device = "invalid"
	if code, _ := f.request("PUT", yueDeploymentRoute, yueDeploymentInput(saved["revision"].(string), registration), true); code != 400 {
		t.Fatalf("invalid setup accepted: %d", code)
	}
	after, _ := filepath.Glob(filepath.Join(f.dir, "station-yue-manifest-*.json"))
	if len(before) != len(after) {
		t.Fatal("failed setup left an orphaned approval manifest")
	}
	if current, _ := os.ReadFile(filepath.Join(f.dir, localYuERegistrationFile)); !bytes.Equal(current, raw) {
		t.Fatal("invalid setup replaced saved registration")
	}
}

func TestLocalYuEDeploymentExplicitPinsRemainPromises(t *testing.T) {
	for _, kind := range []string{"workflow", "ffmpeg", "invalid_graph"} {
		t.Run(kind, func(t *testing.T) {
			f, registration := yueDeploymentFixture(t)
			if kind == "ffmpeg" {
				registration.FFmpegSHA256 = strings.Repeat("0", 64)
			} else {
				name := filepath.Join(registration.Workflows, "audio_yue2_text_to_music.json")
				if err := os.WriteFile(name, []byte(`{"private":"do-not-disclose"}`), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "invalid_graph" {
					registration.WorkflowPins = nil
				}
			}
			registration.Manifest = ""
			code, out := f.request("PUT", yueDeploymentRoute, yueDeploymentInput("unconfigured", registration), true)
			if code != 400 {
				t.Fatalf("bad pin/graph accepted: %d %#v", code, out)
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), registration.Runtime) || strings.Contains(string(raw), "do-not-disclose") {
				t.Fatal("private resource disclosed")
			}
			if _, err := os.Stat(filepath.Join(f.dir, localYuERegistrationFile)); !os.IsNotExist(err) {
				t.Fatal("failed setup wrote registration")
			}
			left, _ := filepath.Glob(filepath.Join(f.dir, "station-yue-manifest-*.json"))
			if len(left) != 0 {
				t.Fatal("failed setup left manifest")
			}
		})
	}
}

func TestLocalYuEDeploymentActiveAndRunningProtectCorruptRegistration(t *testing.T) {
	f, registration := yueDeploymentFixture(t)
	s := f.handler.chat.projectTools
	release := s.markActive(localYuEPackage)
	if code, out := f.request("GET", yueDeploymentRoute, nil, true); code != 200 || out["read_only"] != true {
		t.Fatalf("active tool unprotected: %d %#v", code, out)
	}
	if code, _ := f.request("PUT", yueDeploymentRoute, yueDeploymentInput("unconfigured", registration), true); code != 409 {
		t.Fatalf("active tool deployment modified: %d", code)
	}
	release()
	backend := s.yue.(*localYuEService)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, home := f.request("GET", "/api/local/home", nil, true)
	id := localYuEServiceID(home["fish_id"].(string))
	_, err = backend.managed.services.Start(t.Context(), id, pluginruntime.ManagedServiceSpec{
		OwnerPlugin: localYuEServiceOwner, Binding: "synthetic", Executable: executable,
		Arguments: []string{"-test.run=^TestLocalYuEDeploymentProcessHelper$"}, Directory: t.TempDir(),
		Environment: append(os.Environ(), "AHS_YUE_DEPLOYMENT_HELPER=1"), LogPath: filepath.Join(t.TempDir(), "service.log"),
		StopInput: "stop\n", StopTimeout: time.Second, Check: func(context.Context) error { return nil },
	}, func(_ context.Context, start func() error) error { return start() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Stop(context.Background()) })
	path := filepath.Join(f.dir, localYuERegistrationFile)
	corrupt := []byte(`{"broken":`)
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	code, out := f.request("GET", yueDeploymentRoute, nil, true)
	if code != 200 || out["read_only"] != true {
		t.Fatalf("running service hidden by broken registration: %d %#v", code, out)
	}
	if code, _ := f.request("PUT", yueDeploymentRoute, yueDeploymentInput(out["revision"].(string), registration), true); code != 409 {
		t.Fatalf("running service replaced: %d", code)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, corrupt) {
		t.Fatal("running service registration replaced")
	}
}

func TestLocalYuEDeploymentProcessHelper(t *testing.T) {
	if os.Getenv("AHS_YUE_DEPLOYMENT_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if scanner.Text() == "stop" {
			return
		}
	}
}

func TestLocalYuEDeploymentWriteFailureKeepsPriorGeneration(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows file sharing semantics")
	}
	f, registration := yueDeploymentFixture(t)
	code, saved := f.request("PUT", yueDeploymentRoute, yueDeploymentInput("unconfigured", registration), true)
	if code != 200 {
		t.Fatalf("initial save: %d %#v", code, saved)
	}
	path := filepath.Join(f.dir, localYuERegistrationFile)
	prior, _ := os.ReadFile(path)
	hold, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	registration.Manifest = ""
	code, out := f.request("PUT", yueDeploymentRoute, yueDeploymentInput(saved["revision"].(string), registration), true)
	if code != 503 {
		t.Fatalf("write failure not surfaced: %d %#v", code, out)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, prior) {
		t.Fatal("write failure replaced prior registration")
	}
	left, _ := filepath.Glob(filepath.Join(f.dir, "station-yue-manifest-*.json"))
	if len(left) != 0 {
		t.Fatal("write failure left candidate manifest")
	}
}

const yueDeploymentRoute = "/api/local/project-tools/yue/deployment"

func yueDeploymentFixture(t *testing.T) (*localHomeFixture, localYuERegistration) {
	t.Helper()
	f := newLocalHomeFixture(t, filepath.Join(t.TempDir(), "host"), filepath.Join(t.TempDir(), "home"))
	f.login()
	if code, _ := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != 200 {
		t.Fatalf("initialize: %d", code)
	}
	_, _, registration := writeLocalYuEFixture(t)
	manifest, err := os.ReadFile(registration.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	registration.Manifest = filepath.Join(f.dir, "approved-yue-manifest.json")
	if err = os.WriteFile(registration.Manifest, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	for mode, name := range localYuEDeploymentTemplates() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "components", "yue2", "app", "workflows_api", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(registration.Workflows, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
		registration.WorkflowPins[string(mode)] = localYuESHA(raw)
	}
	return f, registration
}

func yueDeploymentInput(revision string, registration localYuERegistration) map[string]any {
	return map[string]any{"revision": revision, "approve_execution": true, "configuration": registration}
}

func TestLocalYuEDeploymentOwnerCASAndRestart(t *testing.T) {
	f, registration := yueDeploymentFixture(t)
	if code, view := f.request("GET", yueDeploymentRoute, nil, false); code != 200 || view["revision"] != "unconfigured" || view["configuration"] != nil {
		t.Fatalf("initial view: %d %#v", code, view)
	}
	input := yueDeploymentInput("unconfigured", registration)
	if code, _ := f.request("PUT", yueDeploymentRoute, input, false); code != http.StatusForbidden {
		t.Fatalf("CSRF bypass: %d", code)
	}
	input["approve_execution"] = false
	if code, _ := f.request("PUT", yueDeploymentRoute, input, true); code != 400 {
		t.Fatalf("missing approval accepted: %d", code)
	}
	input["approve_execution"] = true
	code, saved := f.request("PUT", yueDeploymentRoute, input, true)
	if code != 200 || saved["configured"] != true || saved["read_only"] != false || saved["revision"] == "unconfigured" {
		t.Fatalf("save: %d %#v", code, saved)
	}
	if info, err := f.handler.chat.projectTools.yue.Info(t.Context(), "dafeiyu"); err != nil || info.Running || info.Busy {
		t.Fatalf("setup started a backend or failed admission: %#v %v", info, err)
	}
	path := filepath.Join(f.dir, localYuERegistrationFile)
	before, _ := os.ReadFile(path)
	if code, _ = f.request("PUT", yueDeploymentRoute, input, true); code != 409 {
		t.Fatalf("stale edit accepted: %d", code)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("stale edit changed registration")
	}
	f.server.Close()
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	next := newLocalHomeFixture(t, f.dir, f.root)
	if code, _ = next.request("GET", yueDeploymentRoute, nil, false); code != 401 {
		t.Fatalf("deployment exposed without owner: %d", code)
	}
	next.login()
	if code, view := next.request("GET", yueDeploymentRoute, nil, true); code != 200 || view["revision"] != saved["revision"] || view["configured"] != true {
		t.Fatalf("restart lost registration: %d %#v", code, view)
	}
}

func TestLocalYuEDeploymentInvalidResourcesPreservePriorRecord(t *testing.T) {
	f, registration := yueDeploymentFixture(t)
	code, saved := f.request("PUT", yueDeploymentRoute, yueDeploymentInput("unconfigured", registration), true)
	if code != 200 {
		t.Fatalf("save: %d %#v", code, saved)
	}
	path := filepath.Join(f.dir, localYuERegistrationFile)
	before, _ := os.ReadFile(path)
	for _, kind := range []string{"home_overlap", "missing_entry", "changed_manifest", "bad_workflows"} {
		t.Run(kind, func(t *testing.T) {
			bad := registration
			switch kind {
			case "home_overlap":
				bad.Models = f.root
			case "missing_entry":
				bad.Entry = "missing.py"
			case "changed_manifest":
				bad.Manifest = filepath.Join(f.dir, "invalid-manifest.json")
				if err := os.WriteFile(bad.Manifest, []byte(`{"private":"do-not-disclose"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "bad_workflows":
				bad.Workflows = t.TempDir()
			}
			code, view := f.request("PUT", yueDeploymentRoute, yueDeploymentInput(saved["revision"].(string), bad), true)
			if code != 400 {
				t.Fatalf("invalid registration accepted: %d %#v", code, view)
			}
			raw, _ := json.Marshal(view)
			if strings.Contains(string(raw), "do-not-disclose") || strings.Contains(string(raw), bad.Runtime) {
				t.Fatal("validation disclosed private details")
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
				t.Fatal("failed validation changed registration")
			}
		})
	}
}

func TestLocalYuEDeploymentRepairsCorruptRecordWithExactRevision(t *testing.T) {
	f, registration := yueDeploymentFixture(t)
	path := filepath.Join(f.dir, localYuERegistrationFile)
	if err := os.WriteFile(path, []byte(`{"old":"incomplete`), 0600); err != nil {
		t.Fatal(err)
	}
	code, old := f.request("GET", yueDeploymentRoute, nil, true)
	if code != 200 || old["configured"] != false || old["configuration"] != nil || old["revision"] == "unconfigured" {
		t.Fatalf("corrupt record cannot be repaired: %d %#v", code, old)
	}
	if code, _ := f.request("PUT", yueDeploymentRoute, yueDeploymentInput("unconfigured", registration), true); code != 409 {
		t.Fatalf("corrupt record overwritten without exact version: %d", code)
	}
	if code, view := f.request("PUT", yueDeploymentRoute, yueDeploymentInput(old["revision"].(string), registration), true); code != 200 || view["configured"] != true {
		t.Fatalf("repair failed: %d %#v", code, view)
	}
}

func TestLocalYuEDeploymentCannotReplaceEnabledOrUnfinishedWork(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows project-tool installation")
	}
	f, registration := yueDeploymentFixture(t)
	_, saved := f.request("PUT", yueDeploymentRoute, yueDeploymentInput("unconfigured", registration), true)
	catalog, _ := filepath.Abs(filepath.Join("..", "..", "plugins"))
	f.handler.chat.projectTools.options.CatalogDirectory = catalog
	if code, out := f.request("PUT", "/api/local/project-tools/yue", map[string]any{"action": "install", "approve_artifacts": true}, true); code != 200 {
		t.Fatalf("install: %d %#v", code, out)
	}
	installedView := localProjectToolByID(t, f, localYuEPackage)
	if code, out := f.request("PUT", "/api/local/project-tools/yue", map[string]any{"action": "enable", "revision": localProjectToolRevision(t, installedView)}, true); code != 200 {
		t.Fatalf("enable: %d %#v", code, out)
	}
	input := yueDeploymentInput(saved["revision"].(string), registration)
	if code, _ := f.request("PUT", yueDeploymentRoute, input, true); code != 409 {
		t.Fatalf("enabled deployment changed: %d", code)
	}
	_, home := f.request("GET", "/api/local/home", nil, true)
	host, err := f.handler.chat.projectTools.forFish(home["fish_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := host.store.Installed(t.Context(), localYuEPackage)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := host.store.Configuration(t.Context(), localYuEPackage)
	if err != nil {
		t.Fatal(err)
	}
	_, created, err := host.store.CreateJob(t.Context(), pluginhost.Job{
		Plugin: localYuEPackage, Version: installed.Package.Version, PackageDigest: installed.Digest, ConfigRevision: strconv.Itoa(configuration.Revision),
		Tool: localYuEStartTool, Arguments: json.RawMessage(`{}`),
		Context: pluginhost.Invocation{Account: host.store.Account(), Thread: "local", Requester: "owner", Purpose: "synthetic setup guard", RequestKey: "unfinished-comfy", SourceVersions: map[string]int{"synthetic": 1}},
	})
	if err != nil || !created {
		t.Fatalf("create pending job: %v", err)
	}
	if _, err = host.store.SetEnabled(t.Context(), localYuEPackage, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	if code, view := f.request("GET", yueDeploymentRoute, nil, true); code != 200 || view["read_only"] != true || !strings.Contains(view["reason"].(string), "未完成任务") {
		t.Fatalf("pending task not protected: %d %#v", code, view)
	}
	if code, _ := f.request("PUT", yueDeploymentRoute, input, true); code != 409 {
		t.Fatalf("pending deployment changed: %d", code)
	}
}
