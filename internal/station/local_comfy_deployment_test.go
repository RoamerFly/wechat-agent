package station

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"wechat-bot/internal/pluginhost"
)

const comfyDeploymentRoute = "/api/local/project-tools/comfyui/deployment"

func comfyDeploymentFixture(t *testing.T) (*localHomeFixture, localComfyRegistration) {
	t.Helper()
	f := newLocalHomeFixture(t, filepath.Join(t.TempDir(), "host"), filepath.Join(t.TempDir(), "home"))
	f.login()
	if code, _ := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != 200 {
		t.Fatalf("initialize: %d", code)
	}
	_, _, registration := writeLocalComfyFixture(t)
	manifest, err := os.ReadFile(registration.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	registration.Manifest = filepath.Join(f.dir, "approved-comfy-manifest.json")
	if err = os.WriteFile(registration.Manifest, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	writeLocalComfyChatWorkflow(t, registration.Workflows)
	return f, registration
}

func comfyDeploymentInput(revision string, registration localComfyRegistration) map[string]any {
	return map[string]any{"revision": revision, "approve_execution": true, "configuration": registration}
}

func TestLocalComfyDeploymentOwnerCASAndRestart(t *testing.T) {
	f, registration := comfyDeploymentFixture(t)
	if code, view := f.request("GET", comfyDeploymentRoute, nil, false); code != 200 || view["revision"] != "unconfigured" || view["configuration"] != nil {
		t.Fatalf("initial view: %d %#v", code, view)
	}
	input := comfyDeploymentInput("unconfigured", registration)
	if code, _ := f.request("PUT", comfyDeploymentRoute, input, false); code != http.StatusForbidden {
		t.Fatalf("CSRF bypass: %d", code)
	}
	input["approve_execution"] = false
	if code, _ := f.request("PUT", comfyDeploymentRoute, input, true); code != 400 {
		t.Fatalf("missing approval accepted: %d", code)
	}
	input["approve_execution"] = true
	code, saved := f.request("PUT", comfyDeploymentRoute, input, true)
	if code != 200 || saved["configured"] != true || saved["read_only"] != false || saved["revision"] == "unconfigured" {
		t.Fatalf("save: %d %#v", code, saved)
	}
	if info, err := f.handler.chat.projectTools.comfy.Info(t.Context(), "dafeiyu"); err != nil || info.Running || info.Busy {
		t.Fatalf("setup started a backend or failed admission: %#v %v", info, err)
	}
	path := filepath.Join(f.dir, localComfyRegistrationFile)
	before, _ := os.ReadFile(path)
	if code, _ = f.request("PUT", comfyDeploymentRoute, input, true); code != 409 {
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
	if code, _ = next.request("GET", comfyDeploymentRoute, nil, false); code != 401 {
		t.Fatalf("deployment exposed without owner: %d", code)
	}
	next.login()
	if code, view := next.request("GET", comfyDeploymentRoute, nil, true); code != 200 || view["revision"] != saved["revision"] || view["configured"] != true {
		t.Fatalf("restart lost registration: %d %#v", code, view)
	}
}

func TestLocalComfyDeploymentInvalidResourcesPreservePriorRecord(t *testing.T) {
	f, registration := comfyDeploymentFixture(t)
	code, saved := f.request("PUT", comfyDeploymentRoute, comfyDeploymentInput("unconfigured", registration), true)
	if code != 200 {
		t.Fatalf("save: %d %#v", code, saved)
	}
	path := filepath.Join(f.dir, localComfyRegistrationFile)
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
			code, view := f.request("PUT", comfyDeploymentRoute, comfyDeploymentInput(saved["revision"].(string), bad), true)
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

func TestLocalComfyDeploymentRepairsCorruptRecordWithExactRevision(t *testing.T) {
	f, registration := comfyDeploymentFixture(t)
	path := filepath.Join(f.dir, localComfyRegistrationFile)
	if err := os.WriteFile(path, []byte(`{"old":"incomplete`), 0600); err != nil {
		t.Fatal(err)
	}
	code, old := f.request("GET", comfyDeploymentRoute, nil, true)
	if code != 200 || old["configured"] != false || old["configuration"] != nil || old["revision"] == "unconfigured" {
		t.Fatalf("corrupt record cannot be repaired: %d %#v", code, old)
	}
	if code, _ := f.request("PUT", comfyDeploymentRoute, comfyDeploymentInput("unconfigured", registration), true); code != 409 {
		t.Fatalf("corrupt record overwritten without exact version: %d", code)
	}
	if code, view := f.request("PUT", comfyDeploymentRoute, comfyDeploymentInput(old["revision"].(string), registration), true); code != 200 || view["configured"] != true {
		t.Fatalf("repair failed: %d %#v", code, view)
	}
}

func TestLocalComfyIdleRangePreservesDefaultsAndRejectsDurationOverflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("64-bit runtime duration range")
	}
	data, home, registration := writeLocalComfyFixture(t)
	registration.IdleSeconds = 0
	if registration.idleTimeout() != localComfyDefaultIdle {
		t.Fatal("zero must retain the documented default")
	}
	registration.IdleSeconds = int(localComfyMaxIdleSeconds)
	if err := registration.validate(data, home); err != nil || registration.idleTimeout() <= 0 {
		t.Fatalf("valid duration was limited or overflowed: %v", err)
	}
	registration.IdleSeconds++
	if err := registration.validate(data, home); err == nil {
		t.Fatal("overflowing idle duration accepted")
	}
}

func TestLocalComfyDeploymentCannotReplaceEnabledOrUnfinishedWork(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows project-tool installation")
	}
	f, registration := comfyDeploymentFixture(t)
	_, saved := f.request("PUT", comfyDeploymentRoute, comfyDeploymentInput("unconfigured", registration), true)
	setUpLocalComfyChat(t, f, f.handler.chat.projectTools.comfy)
	input := comfyDeploymentInput(saved["revision"].(string), registration)
	if code, _ := f.request("PUT", comfyDeploymentRoute, input, true); code != 409 {
		t.Fatalf("enabled deployment changed: %d", code)
	}
	_, home := f.request("GET", "/api/local/home", nil, true)
	host, err := f.handler.chat.projectTools.forFish(home["fish_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := host.store.Installed(t.Context(), localComfyPackage)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := host.store.Configuration(t.Context(), localComfyPackage)
	if err != nil {
		t.Fatal(err)
	}
	_, created, err := host.store.CreateJob(t.Context(), pluginhost.Job{
		Plugin: localComfyPackage, Version: installed.Package.Version, PackageDigest: installed.Digest, ConfigRevision: strconv.Itoa(configuration.Revision),
		Tool: localComfyStartTool, Arguments: json.RawMessage(`{}`),
		Context: pluginhost.Invocation{Account: host.store.Account(), Thread: "local", Requester: "owner", Purpose: "synthetic setup guard", RequestKey: "unfinished-comfy", SourceVersions: map[string]int{"synthetic": 1}},
	})
	if err != nil || !created {
		t.Fatalf("create pending job: %v", err)
	}
	if _, err = host.store.SetEnabled(t.Context(), localComfyPackage, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	if code, view := f.request("GET", comfyDeploymentRoute, nil, true); code != 200 || view["read_only"] != true || !strings.Contains(view["reason"].(string), "未完成任务") {
		t.Fatalf("pending task not protected: %d %#v", code, view)
	}
	if code, _ := f.request("PUT", comfyDeploymentRoute, input, true); code != 409 {
		t.Fatalf("pending deployment changed: %d", code)
	}
}
