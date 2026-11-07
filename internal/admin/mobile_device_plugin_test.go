package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/adbserver"
	"wechat-bot/internal/devicebroker"
	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

type fakeMobileDeviceBroker struct {
	calls    int
	raw      string
	err      error
	replies  map[string]string
	response func(string, json.RawMessage) (string, error)
	binding  string
	closeErr error
	closes   int
}

type fakeMobileDeviceIOCall struct {
	broker    *fakeMobileDeviceBroker
	name      string
	request   json.RawMessage
	completed bool
}

func (b *fakeMobileDeviceBroker) Begin(_ context.Context, name string, raw json.RawMessage) (mobile.IOCall, error) {
	if !json.Valid(raw) {
		return nil, errors.New("unexpected native request")
	}
	return &fakeMobileDeviceIOCall{broker: b, name: name, request: append(json.RawMessage(nil), raw...)}, nil
}
func (c *fakeMobileDeviceIOCall) Service(_ context.Context, _, _ string, raw json.RawMessage) (json.RawMessage, error) {
	c.broker.calls++
	if !json.Valid(raw) || c.completed {
		return nil, errors.New("unexpected IO request")
	}
	return json.RawMessage(`{"host_ref":"synthetic-verified-ref"}`), c.broker.err
}
func (c *fakeMobileDeviceIOCall) Complete(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if c.completed || !sameJSON(raw, json.RawMessage(`{"host_ref":"synthetic-verified-ref"}`)) {
		return nil, errors.New("changed native result")
	}
	c.completed = true
	if c.broker.response != nil {
		value, err := c.broker.response(c.name, c.request)
		return json.RawMessage(value), err
	}
	if c.broker.replies != nil {
		value, ok := c.broker.replies[c.name]
		if !ok {
			return nil, errors.New("unexpected native request")
		}
		return json.RawMessage(value), c.broker.err
	}
	return json.RawMessage(c.broker.raw), c.broker.err
}
func (*fakeMobileDeviceIOCall) Close() error { return nil }
func (b *fakeMobileDeviceBroker) Close() error {
	b.closes++
	return b.closeErr
}
func (b *fakeMobileDeviceBroker) Binding() (string, error) {
	if b.binding == "" {
		return "synthetic-device-binding", nil
	}
	return b.binding, nil
}

func prepareSyntheticMobileDevicePackage(t *testing.T, store *pluginhost.Store) pluginhost.InstalledPackage {
	t.Helper()
	dir := t.TempDir()
	p := pluginhost.Package{SchemaVersion: 1, ID: mobileDevicePluginID, Name: "Synthetic mobile device", Version: "0.3.0-test", HostProtocol: 1, Status: "ready", Platforms: []string{runtime.GOOS}, Runtime: pluginhost.Runtime{Transport: "mcp-stdio", Entrypoint: "runtime/main.py"}, Permissions: []string{"device"}, Files: map[string]string{}}
	capabilities := make([]map[string]any, 0, len(mobileDeviceCapabilities))
	for _, id := range mobileDeviceCapabilities {
		capabilities = append(capabilities, map[string]any{"id": id, "implementation": "ready", "description": "synthetic", "permissions": []string{"device"}, "input_schema": map[string]any{"type": "object"}, "output_schema": map[string]any{"type": "object"}, "discoverable": true})
	}
	files := map[string]string{
		"config.schema.json": `{"type":"object","properties":{},"additionalProperties":false}`,
		"ui.schema.json":     `{}`,
		"capabilities.json":  string(mustJSON(map[string]any{"version": 1, "capabilities": capabilities})),
		"README.md":          "synthetic\n",
		"PERMISSIONS.md":     "synthetic\n",
		"MIGRATION.md":       "synthetic\n",
		"CHANGELOG.md":       "synthetic\n",
		"runtime/main.py":    "# synthetic\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		p.Files[name] = hex.EncodeToString(sum[:])
	}
	manifest, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "plugin.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := store.Prepare(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(t.Context(), prepared.Token, prepared.Digest, prepared.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SaveConfiguration(t.Context(), installed.Package.ID, pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{}}); err != nil {
		t.Fatal(err)
	}
	installed, err = store.SetEnabled(t.Context(), installed.Package.ID, installed.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	return installed
}

func mobileDevicePolicyFixture(t *testing.T) (*mobileDevicePluginBridge, *mobileDevicePluginPolicy, *fakeMobileDeviceBroker, pluginhost.Job, pluginhost.Capability) {
	t.Helper()
	f := setup(t)
	store, err := pluginhost.OpenStore(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	installed := prepareSyntheticMobileDevicePackage(t, store)
	f.manager.packages = store
	task := mobile.Task{ID: "task-device", RequestHash: "request-device", DeviceID: "phone-device", Goal: "检查页面", State: "running", Source: mobile.Source{Admin: true}, ObservationID: "obs-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	settings := mobile.Settings{Revision: 9, Enabled: true, Devices: []string{task.DeviceID}, Actions: []string{"tap", "launch", "open_url"}}
	f.manager.mobile = &mobilePlanDeviceStub{settings: settings, task: task}
	fake := &fakeMobileDeviceBroker{raw: `{"text":"{\"id\":\"obs-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"device_id\":\"phone-device\",\"package\":\"com.example.home\",\"width\":100,\"height\":200,\"allowed_packages\":[\"com.example.home\",\"com.browser\"],\"browsers\":[\"com.browser\"],\"web_hosts\":[\"example.test\"],\"nodes\":[],\"observed_at\":1.5,\"screenshot_sha256\":\"private\"}","media":[{"mime_type":"image/png","data_base64":"PRIVATE_PNG_BYTES"}]}`}
	bridge := &mobileDevicePluginBridge{manager: f.manager, store: store, installed: installed, task: task, settings: settings, brokerOpener: func(context.Context, mobile.IOOptions) (mobile.IOBroker, error) { return fake, nil }, observations: map[string]mobile.Observation{}, artifacts: map[string]mobile.Artifact{}}
	policy, err := bridge.newPolicy(t.Context(), "observe", json.RawMessage(`{"device_id":"phone-device"}`))
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := store.CreateJob(t.Context(), pluginhost.Job{Plugin: mobileDevicePluginID, Tool: policy.capability, Context: policy.invocation, Arguments: policy.arguments, Version: installed.Package.Version, PackageDigest: installed.Digest, ConfigRevision: "1"})
	if err != nil {
		t.Fatal(err)
	}
	job, err = store.ClaimJob(t.Context(), job.ID, job.Revision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = policy.closeIO() })
	return bridge, policy, fake, job, pluginhost.Capability{ID: policy.capability, Permissions: []string{"device"}}
}

func mobileDeviceFixtureResult(p *mobileDevicePluginPolicy) json.RawMessage {
	return mustJSON(map[string]any{"operation": p.capability, "source_version": p.sourceVersion, "result": json.RawMessage(`{"host_ref":"synthetic-verified-ref"}`)})
}

func TestMobileDevicePluginObserveRetainsActionScopeFromNativeMetadata(t *testing.T) {
	bridge, policy, fake, job, cap := mobileDevicePolicyFixture(t)
	request, _ := json.Marshal(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	if _, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request); err != nil || fake.calls != 1 {
		t.Fatalf("initial native observation failed: %v", err)
	}
	if _, err := policy.Validate(t.Context(), job, cap, mobileDeviceFixtureResult(policy)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"device_id":"phone-device","observation_id":"obs-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","request_key":"task-device-step-0","action":"launch","package":"com.browser"}`),
		json.RawMessage(`{"device_id":"phone-device","observation_id":"obs-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","request_key":"task-device-step-1","action":"open_url","package":"com.browser","url":"https://example.test/path"}`),
	} {
		if _, err := bridge.newPolicy(t.Context(), "act", raw); err != nil {
			t.Fatalf("native observation scope was lost for %s: %v", raw, err)
		}
	}
}

func TestMobileDevicePluginManagementListingUsesReadOnlyHostIdentity(t *testing.T) {
	bridge, _, _, _, _ := mobileDevicePolicyFixture(t)
	bridge.task = mobile.Task{Source: mobile.Source{Admin: true}}
	policy, err := bridge.newPolicy(t.Context(), "devices", json.RawMessage(`{}`))
	if err != nil || policy.invocation.Purpose == "" || policy.invocation.Thread != "host:mobile" || policy.invocation.Requester != "administrator" || len(policy.invocation.SourceVersions) != 0 {
		t.Fatal("management listing lost its narrow host authority", err)
	}
	if _, err = bridge.newPolicy(t.Context(), "observe", json.RawMessage(`{"device_id":"phone-device"}`)); err == nil {
		t.Fatal("management listing authority expanded to device observation")
	}
}

func TestMobileDevicePluginRetainsUnknownNativeOutcomeWithoutReplay(t *testing.T) {
	bridge, _, _, _, _ := mobileDevicePolicyFixture(t)
	request := map[string]any{"device_id": "phone-device", "observation_id": "obs-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "request_key": "task-device-step-0", "action": "tap", "x": float64(1), "y": float64(2)}
	bridge.observations["obs-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] = mobile.Observation{ID: "obs-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DeviceID: "phone-device", Width: 100, Height: 200}
	receipt, err := bridge.safeReceipt(bridge.task, "device.act", "act", json.RawMessage(`{"state":"unknown","result":{}}`), request)
	if err != nil || mobileReceiptStatus("device.act", receipt) != "unknown" {
		t.Fatalf("unknown native outcome was rewritten or rejected: receipt=%s err=%v", receipt, err)
	}
}

func TestMobileDevicePluginPolicyProjectsPrivateObserveReply(t *testing.T) {
	_, policy, fake, job, cap := mobileDevicePolicyFixture(t)
	if strings.Contains(string(policy.arguments), "PRIVATE_PNG_BYTES") {
		t.Fatal("native screenshot bytes leaked into plugin arguments")
	}
	request, _ := json.Marshal(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	result, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request)
	if err != nil || fake.calls != 1 {
		t.Fatalf("mediated observation failed: %v calls=%d", err, fake.calls)
	}
	if strings.Contains(string(result), "PRIVATE_PNG_BYTES") || strings.Contains(string(result), "screenshot_sha256") || strings.Contains(string(result), "observed_at") {
		t.Fatalf("private native observation leaked in receipt: %s", result)
	}
	verified, err := policy.Validate(t.Context(), job, cap, mobileDeviceFixtureResult(policy))
	if err != nil || strings.Contains(string(verified.Result), "PRIVATE_PNG_BYTES") || strings.Contains(string(verified.Private), "PRIVATE_PNG_BYTES") {
		t.Fatalf("receipt/private split invalid: result=%s private=%s err=%v", verified.Result, verified.Private, err)
	}
	var checkpoint mobileDeviceCheckpoint
	if json.Unmarshal(verified.Private, &checkpoint) != nil {
		t.Fatal("invalid private checkpoint")
	}
	retained, err := policy.readCheckpoint(t.Context(), job, checkpoint)
	if err != nil || !strings.Contains(string(retained), "PRIVATE_PNG_BYTES") {
		t.Fatal("host did not retain verified binary", err)
	}
	if _, err = policy.Service(t.Context(), job, cap, "wechat/device_io", request); err == nil || fake.calls != 1 {
		t.Fatal("completed/unknown native operation was replayed")
	}
}

func TestMobileDevicePluginPolicyRejectsRevokedSourceBeforeBroker(t *testing.T) {
	bridge, policy, fake, job, cap := mobileDevicePolicyFixture(t)
	stub := bridge.manager.mobile.(*mobilePlanDeviceStub)
	stub.task.Source.Admin = false
	request, _ := json.Marshal(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	if _, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request); err == nil || fake.calls != 0 {
		t.Fatalf("revoked source reached broker: err=%v calls=%d", err, fake.calls)
	}
}

func TestMobileDevicePluginPolicySnapshotRejectsTaskMutationBeforeBroker(t *testing.T) {
	bridge, policy, fake, job, cap := mobileDevicePolicyFixture(t)
	stub := bridge.manager.mobile.(*mobilePlanDeviceStub)
	stub.task.Steps++
	request, _ := json.Marshal(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	if _, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request); err == nil || fake.calls != 0 {
		t.Fatalf("changed durable task reached native broker: err=%v calls=%d", err, fake.calls)
	}
}

func TestMobileDevicePluginOldPolicyCannotFollowNewObservation(t *testing.T) {
	bridge, oldPolicy, fake, oldJob, cap := mobileDevicePolicyFixture(t)
	stub := bridge.manager.mobile.(*mobilePlanDeviceStub)
	stub.task.ObservationID = "obs-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := bridge.newPolicy(t.Context(), "observe", json.RawMessage(`{"device_id":"phone-device"}`)); err != nil {
		t.Fatalf("new operation could not bind the newer observation: %v", err)
	}
	request, _ := json.Marshal(map[string]any{"operation": oldPolicy.capability, "source_version": oldPolicy.sourceVersion, "device_id": oldPolicy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	if _, err := oldPolicy.Service(t.Context(), oldJob, cap, "wechat/device_io", request); err == nil || fake.calls != 0 {
		t.Fatalf("old policy followed newer bridge state: err=%v calls=%d", err, fake.calls)
	}
}

func TestMobileDevicePluginPolicyRejectsBindingDriftBeforeBroker(t *testing.T) {
	_, policy, fake, job, cap := mobileDevicePolicyFixture(t)
	fake.binding = "changed-device-binding"
	request, _ := json.Marshal(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	if _, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request); err == nil || fake.calls != 0 {
		t.Fatalf("changed native binding reached broker: err=%v calls=%d", err, fake.calls)
	}
}

func TestMobileDevicePluginPolicyRejectsPackageCodeDriftBeforeIO(t *testing.T) {
	bridge, policy, fake, job, cap := mobileDevicePolicyFixture(t)
	code, err := bridge.store.PackageDirectory(bridge.installed.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(code, "runtime", "main.py"), []byte("# changed package code"), 0600); err != nil {
		t.Fatal(err)
	}
	request := mustJSON(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	if _, err = policy.Service(t.Context(), job, cap, "wechat/device_io", request); err == nil || fake.calls != 0 {
		t.Fatal("changed installed package reached IO", err, fake.calls)
	}
}

func TestMobileDevicePluginBridgeRetainsFailedNativeCloseForRetry(t *testing.T) {
	bridge, _, fake, _, _ := mobileDevicePolicyFixture(t)
	fake.closeErr = errors.New("synthetic close failure")
	if err := bridge.Close(); err == nil || fake.closes != 1 {
		t.Fatalf("first close did not report native failure: err=%v closes=%d", err, fake.closes)
	}
	if _, err := bridge.Call(t.Context(), "observe", json.RawMessage(`{"device_id":"phone-device"}`)); err == nil || fake.calls != 0 {
		t.Fatalf("closed bridge accepted a new call: err=%v calls=%d", err, fake.calls)
	}
	fake.closeErr = nil
	if err := bridge.Close(); err != nil || fake.closes != 2 {
		t.Fatalf("retained native close was not retried: err=%v closes=%d", err, fake.closes)
	}
	if bridge.broker != nil {
		t.Fatal("successful retry retained closed broker")
	}
}

func TestMobileDevicePluginRejectsChangedIOEnvelopeAndRetiredCallback(t *testing.T) {
	_, policy, fake, job, cap := mobileDevicePolicyFixture(t)
	good := map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}}
	for _, change := range []map[string]any{
		{"operation": "device.act"}, {"source_version": "foreign"}, {"device_id": "foreign-device"}, {"step": "shell"}, {"parameters": nil}, {"argv": []string{"forbidden"}},
	} {
		value := map[string]any{}
		for key, item := range good {
			value[key] = item
		}
		for key, item := range change {
			value[key] = item
		}
		if _, err := policy.Service(t.Context(), job, cap, "wechat/device_io", mustJSON(value)); err == nil || fake.calls != 0 {
			t.Fatal("changed IO envelope reached native device", change, err, fake.calls)
		}
	}
	if _, err := policy.Service(t.Context(), job, cap, "wechat/device_request", mustJSON(good)); err == nil || fake.calls != 0 {
		t.Fatal("retired second-bridge callback remained available", err)
	}
}

func TestMobileDevicePluginRechecksSourceBetweenIOAndCompletion(t *testing.T) {
	bridge, policy, fake, job, cap := mobileDevicePolicyFixture(t)
	request := mustJSON(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	if _, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request); err != nil {
		t.Fatal(err)
	}
	bridge.manager.mobile.(*mobilePlanDeviceStub).task.Source.Admin = false
	if _, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request); err == nil || fake.calls != 1 {
		t.Fatal("revoked source issued another IO step", err, fake.calls)
	}
	if _, err := policy.Validate(t.Context(), job, cap, mobileDeviceFixtureResult(policy)); err == nil {
		t.Fatal("revoked source completed native result")
	}
}

func TestMobileDevicePluginRejectsFabricatedNativeCompletion(t *testing.T) {
	_, policy, _, job, cap := mobileDevicePolicyFixture(t)
	request := mustJSON(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	if _, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request); err != nil {
		t.Fatal(err)
	}
	forged := mustJSON(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "result": map[string]any{"host_ref": "fabricated"}})
	if _, err := policy.Validate(t.Context(), job, cap, forged); err == nil {
		t.Fatal("fabricated native completion was accepted")
	}
	if _, err := policy.Validate(t.Context(), job, cap, mobileDeviceFixtureResult(policy)); err == nil {
		t.Fatal("rejected completion retried the same native call")
	}
}

func TestMobileDevicePluginLargePrivateScreenshotUsesBoundFileReference(t *testing.T) {
	_, policy, fake, job, cap := mobileDevicePolicyFixture(t)
	var reply map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fake.raw), &reply); err != nil {
		t.Fatal(err)
	}
	reply["media"] = mustJSON([]map[string]string{{"mime_type": "image/png", "data_base64": strings.Repeat("A", 2<<20)}})
	fake.raw = string(mustJSON(reply))
	request := mustJSON(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
	if _, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request); err != nil {
		t.Fatal(err)
	}
	verified, err := policy.Validate(t.Context(), job, cap, mobileDeviceFixtureResult(policy))
	if err != nil || len(verified.Private) > 1024 || len(verified.Result) > 8192 {
		t.Fatal("large binary crossed bounded checkpoint", err, len(verified.Private), len(verified.Result))
	}
	var checkpoint mobileDeviceCheckpoint
	if err = json.Unmarshal(verified.Private, &checkpoint); err != nil {
		t.Fatal(err)
	}
	raw, err := policy.readCheckpoint(t.Context(), job, checkpoint)
	if err != nil || len(raw) < 2<<20 {
		t.Fatal("large private native reply was lost", err, len(raw))
	}
	if err = os.WriteFile(filepath.Join(policy.replyRoot(), job.ID, "reply.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = policy.readCheckpoint(t.Context(), job, checkpoint); err == nil {
		t.Fatal("tampered private binary checkpoint remained readable")
	}
}

// The official package runs through Controller and the platform sandbox. Only
// the fixed ADB runner and server lease are synthetic; no ADB binary, device,
// WeChat, model, network connection or legacy native child is started.
type mobileIOTestADBServer struct{ acquisitions int }

func (s *mobileIOTestADBServer) Acquire(context.Context, string) (adbserver.Lease, error) {
	s.acquisitions++
	return mobileIOTestADBLease{}, nil
}

type mobileIOTestADBLease struct{}

func (mobileIOTestADBLease) Endpoint() adbserver.Endpoint { return adbserver.DefaultEndpoint() }
func (mobileIOTestADBLease) Binding() string              { return "synthetic-admin-io-server" }
func (mobileIOTestADBLease) Close() error                 { return nil }

func TestLiveMobileDevicePluginContract(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in installed LPAC mobile IO contract")
	}
	python, _ = filepath.Abs(python)
	f := setup(t)
	store, err := f.manager.packageStore()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.Prepare(t.Context(), "../../plugins/mobile-control")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(t.Context(), prepared.Token, prepared.Digest, prepared.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetEnabled(t.Context(), installed.Package.ID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	task := mobile.Task{ID: "task-live-device", RequestHash: "request-live-device", DeviceID: "phone-device", Goal: "检查合成页面", State: "running", Source: mobile.Source{Admin: true}}
	settings := mobile.Settings{Revision: 5, Enabled: true, Devices: []string{task.DeviceID}, Actions: []string{"tap", "capture"}}
	options, settings := mobileDeviceIOResourceFixture(t, f.manager, settings, python)
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	options.WindowsHelper = helper
	rawOptions, _ := json.Marshal(options)
	t.Setenv("WECHAT_PLUGIN_RUNTIME_CONFIG", string(rawOptions))
	stub := &mobilePlanDeviceStub{settings: settings, task: task}
	f.manager.mobile = stub
	controller, err := pluginruntime.NewController(store, pluginruntime.InstalledExecutor(store, options))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	bridgeRaw, err := f.manager.newMobilePluginBridge(t.Context(), task, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer bridgeRaw.Close()
	img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	state := uint32(42)
	for index := range img.Pix {
		state = 1664525*state + 1013904223
		img.Pix[index] = byte(state >> 24)
	}
	var pngBuffer bytes.Buffer
	if err = png.Encode(&pngBuffer, img); err != nil {
		t.Fatal(err)
	}
	pngData := pngBuffer.Bytes()
	if len(pngData) < 1<<20 {
		t.Fatal("large screenshot fixture unexpectedly small", len(pngData))
	}
	video := append([]byte{0, 0, 0, 32, 'f', 't', 'y', 'p'}, bytes.Repeat([]byte("synthetic-mp4"), 8)...)
	server := &mobileIOTestADBServer{}
	calls, dispatches := 0, 0
	revokeNext := false
	bridgeRaw.(*mobileDevicePluginBridge).brokerOpener = func(ctx context.Context, input mobile.IOOptions) (mobile.IOBroker, error) {
		o := devicebroker.Options{IOOptions: input, ADBServer: server}
		o.Runner = func(ctx context.Context, path string, args []string, limit int64, timeout time.Duration) ([]byte, error) {
			calls++
			if path != filepath.Join(options.DeviceToolsRoots[0], "adb.exe") || len(args) < 7 || !reflect.DeepEqual(args[:6], []string{"-H", "127.0.0.1", "-P", "5037", "-s", "synthetic-serial-phone-device"}) || limit < 1 || timeout <= 0 {
				return nil, errors.New("unbound synthetic ADB request")
			}
			if revokeNext {
				stub.task.Source.Admin = false
				revokeNext = false
			}
			tail := args[6:]
			switch {
			case reflect.DeepEqual(tail, []string{"get-state"}):
				return []byte("device\n"), nil
			case reflect.DeepEqual(tail, []string{"shell", "dumpsys", "window"}):
				return []byte("mCurrentFocus=Window{synthetic com.example.home/.Main}"), nil
			case reflect.DeepEqual(tail, []string{"exec-out", "screencap", "-p"}):
				return append([]byte(nil), pngData...), nil
			case len(tail) == 4 && tail[0] == "shell" && tail[1] == "uiautomator" && tail[2] == "dump" && strings.HasPrefix(tail[3], "/sdcard/whale-ui-"):
				return nil, nil
			case len(tail) == 3 && tail[0] == "exec-out" && tail[1] == "cat" && strings.HasSuffix(tail[2], ".xml"):
				return []byte(`<hierarchy><node text="synthetic page" package="com.example.home" bounds="[0,0][10,10]" clickable="true"/></hierarchy>`), nil
			case len(tail) == 4 && tail[0] == "shell" && tail[1] == "rm" && tail[2] == "-f" && strings.HasPrefix(tail[3], "/sdcard/whale-"):
				return nil, nil
			case reflect.DeepEqual(tail, []string{"shell", "input", "tap", "1", "2"}):
				dispatches++
				return nil, nil
			case len(tail) == 9 && reflect.DeepEqual(tail[:8], []string{"shell", "screenrecord", "--size", "480x800", "--bit-rate", "256000", "--time-limit", "2"}) && strings.HasSuffix(tail[8], ".mp4"):
				dispatches++
				return nil, nil
			case len(tail) == 3 && tail[0] == "exec-out" && tail[1] == "cat" && strings.HasSuffix(tail[2], ".mp4"):
				return append([]byte(nil), video...), nil
			default:
				return nil, errors.New("unexpected synthetic fixed ADB operation")
			}
		}
		return devicebroker.OpenIO(ctx, o)
	}
	call := func(name string, request any) string {
		t.Helper()
		out, e := bridgeRaw.Call(t.Context(), name, mustJSON(request))
		if e != nil {
			page, _ := store.Jobs(t.Context(), mobileDevicePluginID, 0, 20)
			t.Fatalf("%s failed: %v native_calls=%d jobs=%#v", name, e, calls, page.Items)
		}
		return out
	}
	if server.acquisitions != 0 {
		t.Fatal("readiness acquired ADB")
	}
	call("devices", map[string]any{})
	observationRaw := call("observe", map[string]any{"device_id": task.DeviceID})
	var observed struct {
		Text  string
		Media []struct {
			Data string `json:"data_base64"`
		}
	}
	if json.Unmarshal([]byte(observationRaw), &observed) != nil || len(observed.Media) != 1 {
		t.Fatal("missing host screenshot")
	}
	decoded, e := base64.StdEncoding.DecodeString(observed.Media[0].Data)
	if e != nil || !bytes.Equal(decoded, pngData) {
		t.Fatal("large screenshot not reconstructed", e)
	}
	var observation mobile.Observation
	if json.Unmarshal([]byte(observed.Text), &observation) != nil || observation.ID == "" {
		t.Fatal("missing verified observation")
	}
	stub.task.ObservationID = observation.ID
	action := map[string]any{"device_id": task.DeviceID, "observation_id": observation.ID, "request_key": task.ID + "-step-0", "action": "tap", "x": 1, "y": 2}
	call("act", action)
	before := calls
	call("act", action)
	if calls != before || dispatches != 1 {
		t.Fatal("same action replayed IO", calls, before, dispatches)
	}
	record := call("act", map[string]any{"device_id": task.DeviceID, "observation_id": observation.ID, "request_key": task.ID + "-step-1", "action": "capture", "seconds": 2})
	var recorded struct {
		Result struct{ Artifact mobile.Artifact }
	}
	if json.Unmarshal([]byte(record), &recorded) != nil || recorded.Result.Artifact.ID == "" {
		t.Fatal("record artifact missing", record)
	}
	chunk := call("read_artifact", map[string]any{"device_id": task.DeviceID, "artifact_id": recorded.Result.Artifact.ID, "request_key": task.ID + "-step-1", "offset": 0})
	var read struct {
		Data string `json:"data_base64"`
	}
	if json.Unmarshal([]byte(chunk), &read) != nil {
		t.Fatal("artifact reply malformed")
	}
	decoded, e = base64.StdEncoding.DecodeString(read.Data)
	if e != nil || !bytes.Equal(decoded, video) {
		t.Fatal("private artifact bytes changed", e)
	}
	page, e := store.Jobs(t.Context(), mobileDevicePluginID, 0, 20)
	if e != nil {
		t.Fatal(e)
	}
	for _, job := range page.Items {
		if len(job.Result) > 64<<10 || bytes.Contains(job.Arguments, []byte("synthetic-serial")) {
			t.Fatal("raw device authority entered plugin arguments/checkpoint")
		}
	}
	before = calls
	revokeNext = true
	if _, e = bridgeRaw.Call(t.Context(), "observe", mustJSON(map[string]any{"device_id": task.DeviceID})); e == nil || calls != before+1 {
		t.Fatal("source revoked during one native read continued to another IO step", e, calls, before)
	}
	before = calls
	if _, e = bridgeRaw.Call(t.Context(), "observe", mustJSON(map[string]any{"device_id": task.DeviceID})); e == nil || calls != before {
		t.Fatal("revoked source reached ADB again", e)
	}
	if server.acquisitions < 1 || dispatches != 2 {
		t.Fatal("unexpected synthetic native lifecycle", server.acquisitions, dispatches)
	}
	if f.native.loginCalls.Load() != 0 {
		t.Fatal("offline contract touched native WeChat")
	}
}

func TestMobileDevicePluginPreservesSafeIOResourceLimits(t *testing.T) {
	for _, code := range []string{"device_artifact_page_limit", "device_io_response_limit"} {
		t.Run(code, func(t *testing.T) {
			_, policy, fake, job, cap := mobileDevicePolicyFixture(t)
			fake.err = errors.New(code)
			request := mustJSON(map[string]any{"operation": policy.capability, "source_version": policy.sourceVersion, "device_id": policy.task.DeviceID, "step": "profile", "parameters": map[string]any{}})
			_, err := policy.Service(t.Context(), job, cap, "wechat/device_io", request)
			var failure *RuntimeFailure
			if !errors.As(err, &failure) || failure.Code != code || !strings.Contains(failure.Message, "分小页继续") || !strings.Contains(failure.Continuation, "bytes") || !strings.Contains(failure.Continuation, "offset") {
				t.Fatal("actual IO limit lost its safe continuation", err)
			}
			var retained *RuntimeFailure
			if !errors.As(policy.ioFailureValue(), &retained) || retained.Code != code || fake.calls != 1 {
				t.Fatal("call-level boundary lost the verified resource diagnosis", policy.ioFailureValue(), fake.calls)
			}
		})
	}
}

func TestMobileDevicePluginIOFailuresNeverExposeRunnerDiagnostics(t *testing.T) {
	for _, test := range []struct {
		err   error
		stage string
	}{
		{errors.New(`device_artifact_page_limit: C:\private\secret token=synthetic-secret`), "device_io"},
		{&os.PathError{Op: "open", Path: `C:\private\secret`, Err: os.ErrPermission}, "filesystem"},
		{&os.PathError{Op: "open", Path: `C:\private\secret`, Err: os.ErrNotExist}, "resource"},
		{errors.New("device_broker_tool_not_permitted"), "authorization"},
	} {
		err := mobileDeviceIOFailure(test.err)
		var failure *RuntimeFailure
		if !errors.As(err, &failure) || failure.Code != "device_plugin_broker_unavailable" || failure.Stage != test.stage {
			t.Fatal("unverified runner diagnostic changed the safe code/stage", err)
		}
		_, safe := failure.SafeToolFailure()
		if strings.Contains(safe, "private") || strings.Contains(safe, "secret") || strings.Contains(safe, "token=") {
			t.Fatal("runner path or credential escaped safe IO failure", safe)
		}
	}
}
