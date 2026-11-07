package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
)

func TestRetiredDeviceControlFreezesQueuesAndPreservesHistoryAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	native := &fakeNative{}
	legacy, err := NewManager(dir, "", native)
	if err != nil {
		t.Fatal(err)
	}
	openManager := legacy
	defer func() {
		if openManager != nil {
			_ = openManager.Close()
		}
	}()
	ctx := t.Context()
	installed := installMobilePackageForManager(t, legacy)
	store, err := legacy.packageStore()
	if err != nil {
		t.Fatal(err)
	}
	staleInstall, err := store.Prepare(ctx, "../../plugins/mobile-control")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	recording := []byte("synthetic old recording")
	picture := []byte("\x89PNG\r\n\x1a\n")
	active := mobile.Task{ID: "synthetic-active", RequestHash: "synthetic-request", DeviceID: "synthetic-device",
		Goal: "synthetic historical task", State: "running", Result: "last verified observation",
		ObservationID: "obs-" + strings.Repeat("a", 32), Created: now, Updated: now,
		Source:    mobile.Source{Account: native.Account(), Thread: "synthetic-chat", Sender: "synthetic-owner"},
		Artifacts: []mobile.Artifact{{ID: "synthetic-recording", Kind: "recording", MIME: "video/mp4", Bytes: len(recording), SHA256: namedSpeechSHA(recording)}},
		Events:    []mobile.Event{{At: now, State: "running", Summary: "existing historical event"}},
	}
	finished := active
	finished.ID, finished.State = "synthetic-done", "completed"
	for path, data := range map[string][]byte{"observations/" + active.ObservationID + ".png": picture, "recordings/synthetic-recording.mp4": recording} {
		path = filepath.Join(dir, "mobile", filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	settings := mobile.Defaults()
	settings.Enabled = true
	records, err := mobileStateRecords(settings, map[string]mobile.Task{active.ID: active, finished.ID: finished}, store.Account())
	if err != nil {
		t.Fatal(err)
	}
	manifest := mobileImportManifest{Version: 1, SourceHash: namedSpeechSHA([]byte("synthetic state source")), TaskCount: 2}
	records["manifest"], _ = json.Marshal(manifest)
	records[mobileWorkflowKey(active.ID)] = json.RawMessage(`{"state":{"observation_only":true},"claim":"synthetic-claimed-effect","fact":{"effect_id":"synthetic-effect"}}`)
	// The archive retains checkpoints from unknown workflow versions as
	// evidence, without needing a planner that understands their format.
	records[mobileWorkflowKey(finished.ID)] = json.RawMessage(`{"future_workflow":{"effect":17}}`)
	if _, err := store.InitializeHostState(ctx, mobileDevicePluginID, mobileStateNamespace, manifest.SourceHash, records); err != nil {
		t.Fatal(err)
	}
	agent := agentTask{ID: "synthetic-followup", DeviceTask: active.ID, Account: native.Account(), Thread: active.Source.Thread,
		Sender: active.Source.Sender, Title: "synthetic followup", State: "waiting", LastResult: "synthetic prior result"}
	if err := legacy.agent.task(agent); err != nil {
		t.Fatal(err)
	}
	makeJob := func(key string) pluginhost.Job {
		t.Helper()
		job, _, err := store.CreateJob(ctx, pluginhost.Job{Plugin: mobileDevicePluginID, Version: installed.Package.Version,
			PackageDigest: installed.Digest, ConfigRevision: "0", Tool: mobileWorkflowCapability, Arguments: json.RawMessage(`{}`),
			Context: pluginhost.Invocation{Account: store.Account(), Thread: "synthetic-chat", Requester: "synthetic-owner", Purpose: "synthetic", RequestKey: key}})
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	waiting, dispatched := makeJob("before-dispatch"), makeJob("after-dispatch")
	dispatched, err = store.ClaimJob(ctx, dispatched.ID, dispatched.Revision)
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err = store.AcknowledgeJob(ctx, dispatched.ID, dispatched.Revision, "synthetic-device-receipt")
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	openManager = nil

	var revision int64
	for i := range 2 {
		m, err := NewManagerWithRetiredDeviceControl(dir, "", native)
		if err != nil {
			t.Fatal(err)
		}
		openManager = m
		store, err := m.packageStore()
		if err != nil || m.mobile.InitializationError() != nil || m.mobile.Settings().Enabled || m.mobile.Busy() {
			t.Fatalf("archive did not load safely: %v", err)
		}
		snapshot, err := store.ReadHostState(ctx, mobileDevicePluginID, mobileStateNamespace)
		if err != nil || i == 1 && snapshot.Revision != revision {
			t.Fatalf("restart rewrote retired host state: %d vs %d, %v", snapshot.Revision, revision, err)
		}
		revision = snapshot.Revision
		stopped, ok := m.mobile.Get(active.ID)
		if !ok || stopped.State != "stopped" || stopped.Reason != mobileRetirementReason || stopped.Result != active.Result ||
			!reflect.DeepEqual(stopped.Events, active.Events) || !reflect.DeepEqual(stopped.Artifacts, active.Artifacts) || stopped.Source != active.Source {
			t.Fatalf("historical task changed unexpectedly: %+v", stopped)
		}
		if done, ok := m.mobile.Get(finished.ID); !ok || !reflect.DeepEqual(done, finished) {
			t.Fatalf("terminal task was rewritten: %+v", done)
		}
		for _, id := range []string{active.ID, finished.ID} {
			if !bytes.Equal(snapshot.Records[mobileWorkflowKey(id)], records[mobileWorkflowKey(id)]) {
				t.Fatal("the original effect claim or evidence was rewritten")
			}
		}
		if after, err := store.Installed(ctx, mobileDevicePluginID); err != nil || after.Enabled || after.Removed || after.Digest != installed.Digest {
			t.Fatalf("original package history not retained disabled: %+v %v", after, err)
		}
		if j, err := store.Job(ctx, waiting.ID); err != nil || j.State != "cancelled" {
			t.Fatalf("unsubmitted job not cancelled: %+v %v", j, err)
		}
		if j, err := store.Job(ctx, dispatched.ID); err != nil || !j.CancelRequested || j.State != dispatched.State || j.RemoteID != dispatched.RemoteID {
			t.Fatalf("dispatched job lost original receipt/status: %+v %v", j, err)
		}
		if tsk, ok := m.agent.lookupTask(agent.ID, agent.Account, agent.Thread); !ok || tsk.State != "stopped" || !strings.HasPrefix(tsk.LastResult, agent.LastResult) || m.agentTaskActive(companion.Input{Account: agent.Account, Thread: agent.Thread, TaskID: agent.ID}) {
			t.Fatalf("old Agent continuation remained active: %+v", tsk)
		}
		if _, err := m.mobile.Start(active.Source, active.DeviceID, active.Goal); !errors.Is(err, errDeviceServiceRetired) {
			t.Fatalf("archive allowed new device work: %v", err)
		}
		if i == 0 {
			h, err := NewHandler(m, "test-password-at-least-24-bytes", "")
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(h)
			defer srv.Close()
			jar, _ := cookiejar.New(nil)
			f := &webFixture{t: t, manager: m, server: srv, client: &http.Client{Jar: jar}, native: native, dir: dir}
			screenshotPath := "/api/mobile/tasks/" + active.ID + "/screenshot/" + active.ObservationID
			if status, _, _ := f.request("GET", screenshotPath, nil, false, false); status != http.StatusUnauthorized {
				t.Fatalf("history leaked before login: %d", status)
			}
			f.login()
			if status, body, _ := f.request("GET", "/api/status", nil, false, false); status != 200 || !bytes.Contains(body, []byte(`"device_control_retired":true`)) {
				t.Fatalf("Windows management status did not declare retired device control: %d %s", status, body)
			}
			for _, call := range []struct {
				method, route string
				body          any
				status        int
			}{
				{"POST", "/api/mobile/tasks", map[string]any{}, http.StatusNotFound},
				{"GET", "/api/mobile/devices", nil, http.StatusNotFound},
				{"POST", "/api/plugin-packages/mobile-control/runtime", map[string]any{"action": "start"}, http.StatusGone},
				{"POST", "/api/plugin-packages/install", map[string]any{"token": staleInstall.Token, "digest": staleInstall.Digest, "confirm": true}, http.StatusGone},
				{"POST", "/api/agent/tasks/" + agent.ID, map[string]any{"action": "resume"}, http.StatusGone},
			} {
				if status, body, _ := f.request(call.method, call.route, call.body, true, true); status != call.status {
					t.Fatalf("retired %s %s: %d %s", call.method, call.route, status, body)
				}
			}
			if status, body, _ := f.request("GET", "/api/plugin-packages/mobile-control", nil, false, false); status != 200 || !bytes.Contains(body, []byte(`"archive":true`)) || bytes.Contains(body, []byte("tasks.cancel")) {
				t.Fatalf("archive not readable or offered control actions: %d %s", status, body)
			}
			if status, body, _ := f.request("GET", screenshotPath, nil, false, false); status != 200 || !bytes.Equal(body, picture) {
				t.Fatalf("historical screenshot unavailable: %d", status)
			}
			if status, body, _ := f.request("GET", "/api/mobile/tasks/"+active.ID+"/recording/synthetic-recording", nil, false, false); status != 200 || !bytes.Equal(body, recording) {
				t.Fatalf("historical recording unavailable: %d", status)
			}
			srv.Close()
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		openManager = nil
	}
}

func TestRetiredDeviceControlReadsLegacySnapshotWithoutOverwritingIt(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "valid"
		if corrupt {
			name = "invalid"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sourceDir := filepath.Join(dir, "mobile")
			if err := os.MkdirAll(sourceDir, 0700); err != nil {
				t.Fatal(err)
			}
			task := mobile.Task{ID: "synthetic-old-task", RequestHash: "synthetic-old-request",
				Source: mobile.Source{Account: "test"}, State: "queued", Result: "old result"}
			raw, err := json.Marshal(map[string]mobile.Task{task.ID: task})
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				raw = []byte("synthetic invalid task ledger")
			}
			source := filepath.Join(sourceDir, "tasks.json")
			if err := os.WriteFile(source, raw, 0600); err != nil {
				t.Fatal(err)
			}
			m, err := NewManagerWithRetiredDeviceControl(dir, "", &fakeNative{})
			if err != nil {
				t.Fatal("an unreadable device archive must not block the account manager", err)
			}
			defer m.Close()
			loaded, ok := m.mobile.Get(task.ID)
			if corrupt {
				if ok || m.mobile.InitializationError() == nil {
					t.Fatal("unreadable archive was accepted")
				}
			} else if !ok || loaded.State != "stopped" || loaded.Result != task.Result {
				t.Fatalf("historical JSON was not retired safely: %+v", loaded)
			}
			if _, err := m.mobile.Devices(t.Context()); !errors.Is(err, errDeviceServiceRetired) {
				t.Fatalf("old archive enabled enumeration: %v", err)
			}
			after, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatalf("legacy JSON was overwritten: %v", err)
			}
		})
	}
}

func TestRetiredDeviceArchiveReadsWithoutImportingNewLegacyFiles(t *testing.T) {
	dir := t.TempDir()
	manager, err := NewManagerWithRetiredDeviceControl(dir, "", &fakeNative{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	legacyDir := filepath.Join(dir, "mobile")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	task := mobile.Task{ID: "synthetic-late-task", RequestHash: "synthetic-late-request",
		Source: mobile.Source{Account: manager.native.Account()}, State: "queued"}
	raw, err := json.Marshal(map[string]mobile.Task{task.ID: task})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(legacyDir, "tasks.json")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if tasks := manager.mobile.List(nil); len(tasks) != 0 || manager.mobile.InitializationError() != nil {
		t.Fatalf("archive read attempted to import data after startup: %+v", tasks)
	}
	store, err := manager.packageStore()
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadHostState(t.Context(), mobileDevicePluginID, mobileStateNamespace)
	if err != nil || state.Revision != 0 {
		t.Fatalf("read initialized the device namespace: %+v %v", state, err)
	}
}
