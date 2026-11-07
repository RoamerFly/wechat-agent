package devicebroker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

type nativeIOPolicy struct {
	call              devicespec.IOCall
	operation, source string
	invocation        pluginhost.Invocation
	raw               json.RawMessage
	alive             bool
}

func (p *nativeIOPolicy) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !p.alive || j.Tool != p.operation || j.Plugin != "mobile-control" || !ioSame(ioJSON(j.Context), ioJSON(p.invocation)) {
		return errors.New("synthetic source revoked")
	}
	return nil
}
func (p *nativeIOPolicy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	if method != "wechat/device_io" {
		return nil, errors.New("unexpected host service")
	}
	if err := p.Check(ctx, j, cap); err != nil {
		return nil, err
	}
	var r struct {
		Operation  string          `json:"operation"`
		Source     string          `json:"source_version"`
		Step       string          `json:"step"`
		Device     string          `json:"device_id"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Operation != p.operation || r.Source != p.source {
		return nil, errToolDenied
	}
	return p.call.Service(devicespec.WithIOAuthority(ctx, func(ctx context.Context) error { return p.Check(ctx, j, cap) }), r.Step, r.Device, r.Parameters)
}
func (p *nativeIOPolicy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	if err := p.Check(ctx, j, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var r struct {
		Operation string          `json:"operation"`
		Source    string          `json:"source_version"`
		Result    json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Operation != p.operation || r.Source != p.source {
		return pluginruntime.VerifiedResult{}, errToolDenied
	}
	value, err := p.call.Complete(devicespec.WithIOAuthority(ctx, func(ctx context.Context) error { return p.Check(ctx, j, cap) }), r.Result)
	if err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	p.raw = value
	return pluginruntime.VerifiedResult{Result: ioJSON(map[string]any{"native_result_sha256": ioSHA(value), "raw_bytes": len(value)})}, nil
}

// A single official isolated main.py runs the real packaged high-level native
// logic. Only fixed host ADB results are synthetic. No second bridge starts.
func TestLiveNativeIO(t *testing.T) {
	if os.Getenv("WECHAT_STREAM_SANDBOX_TEST") != "1" {
		t.Skip("opt-in project-local single-process native protocol")
	}
	python, err := filepath.Abs("../../dist/windows-native/runtimes/windows-python-cli/python.exe")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	if err != nil {
		t.Fatal(err)
	}
	store, err := pluginhost.OpenStore(t.TempDir(), "account_1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prepared, err := store.Prepare(t.Context(), "../../plugins/mobile-control")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(t.Context(), prepared.Token, prepared.Digest, prepared.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	installed, err = store.SetEnabled(t.Context(), installed.Package.ID, installed.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := pluginruntime.NewController(store, pluginruntime.InstalledExecutor(store, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if err = controller.Start(t.Context(), "mobile-control", installed.Revision); err != nil {
		t.Fatal(err)
	}
	o, paths, actions, records := mockIOOptions(t)
	o.Python = python
	o.PackageDirectory, err = store.PackageDirectory(installed.Digest)
	if err != nil {
		t.Fatal(err)
	}
	compat := filepath.Join(paths.workspace, "compat.py")
	if err = os.WriteFile(compat, []byte("# never executed"), 0600); err != nil {
		t.Fatal(err)
	}
	o.CompatibilityEntry = compat
	o.Legacy = legacyConfig(compat, paths.config, paths.storage)
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	callIndex := 0
	invoke := func(operation, name string, request map[string]any) json.RawMessage {
		t.Helper()
		callIndex++
		call, err := b.Begin(t.Context(), name, ioJSON(request))
		if err != nil {
			t.Fatal(err)
		}
		defer call.Close()
		source := strings.Repeat("a", 64)
		p := &nativeIOPolicy{call: call, operation: operation, source: source, alive: true, invocation: pluginhost.Invocation{Account: "account_1", Thread: "synthetic-device", Requester: "synthetic-owner", Purpose: "offline native protocol", RequestKey: ioID("native-"), Authorization: source}}
		result, err := controller.Invoke(t.Context(), pluginruntime.CallRequest{Plugin: "mobile-control", Capability: operation, Context: p.invocation, Arguments: ioJSON(map[string]any{"source_version": source, "request": request})}, p)
		if err != nil || result.Job.State != "completed" || len(p.raw) == 0 {
			t.Fatal("official single-process native protocol failed", operation, err, result.Job)
		}
		return p.raw
	}
	listed := invoke("device.list", "devices", map[string]any{})
	if !strings.Contains(string(listed), `"state":"device"`) || strings.Contains(string(listed), "emulator-5554") {
		t.Fatal("list leaked serial or lost fixed host state", string(listed))
	}
	observed := invoke("device.observe", "observe", map[string]any{"device_id": "allowed"})
	var envelope struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(observed, &envelope)
	var snapshot struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(envelope.Text), &snapshot)
	if snapshot.ID == "" || len(observed) < 9<<20 {
		t.Fatal("large host-private observation lost")
	}
	request := map[string]any{"device_id": "allowed", "action": "tap", "request_key": "original-native-action", "observation_id": snapshot.ID, "x": 1, "y": 2}
	for n := 0; n < 2; n++ {
		raw := invoke("device.act", "act", request)
		if !strings.Contains(string(raw), `"submitted"`) {
			t.Fatal(string(raw))
		}
	}
	if *actions != 1 {
		t.Fatal("same real native journal action replayed", *actions)
	}
	recording := invoke("device.record", "act", map[string]any{"device_id": "allowed", "action": "capture", "request_key": "original-native-record", "observation_id": snapshot.ID, "seconds": 2})
	var record struct {
		Result struct {
			Artifact map[string]any `json:"artifact"`
		} `json:"result"`
	}
	_ = json.Unmarshal(recording, &record)
	if *records != 1 || record.Result.Artifact["id"] == nil {
		t.Fatal("recording protocol incomplete", string(recording))
	}
	chunk := invoke("device.read_artifact", "read_artifact", map[string]any{"device_id": "allowed", "artifact_id": record.Result.Artifact["id"], "request_key": "original-native-record", "offset": 3 << 20, "bytes": 7 << 20})
	if !strings.Contains(string(chunk), `"eof":true`) {
		t.Fatal("artifact page was clamped")
	}
}

// The synthetic prelude is indexed into a temporary copy, never the official
// package. It fails before protocol startup if LPAC receives ambient secrets
// or can open the unmounted host file; the rest is the actual native package.
func TestLiveNativeIOIsolationAndCancellation(t *testing.T) {
	if os.Getenv("WECHAT_STREAM_SANDBOX_TEST") != "1" {
		t.Skip("opt-in project-local native isolation and cancellation")
	}
	pack, err := pluginhost.Verify("../../plugins/mobile-control")
	if err != nil {
		t.Fatal(err)
	}
	code := t.TempDir()
	outside := filepath.Join(t.TempDir(), "unmounted-synthetic-private.txt")
	if err := os.WriteFile(outside, []byte("synthetic host-private data"), 0600); err != nil {
		t.Fatal(err)
	}
	for name := range pack.Files {
		raw, err := os.ReadFile(filepath.Join("../../plugins/mobile-control", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if name == pack.Runtime.Entrypoint {
			prefix := "import os\nassert 'DEVICEBROKER_HOST_SECRET' not in os.environ\ntry:\n    open(" + string(ioJSON(outside)) + ", 'rb').read()\nexcept OSError:\n    pass\nelse:\n    raise RuntimeError('unmounted_host_file_exposed')\n"
			raw = append([]byte(prefix), raw...)
		}
		target := filepath.Join(code, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, 0600); err != nil {
			t.Fatal(err)
		}
		pack.Files[name] = ioSHA(raw)
	}
	if err := os.WriteFile(filepath.Join(code, "plugin.json"), ioJSON(pack), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVICEBROKER_HOST_SECRET", "synthetic-not-inherited")
	python, _ := filepath.Abs("../../dist/windows-native/runtimes/windows-python-cli/python.exe")
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	store, err := pluginhost.OpenStore(t.TempDir(), "account_1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prepared, err := store.Prepare(t.Context(), code)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(t.Context(), prepared.Token, prepared.Digest, prepared.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	installed, err = store.SetEnabled(t.Context(), "mobile-control", installed.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := pluginruntime.NewController(store, pluginruntime.InstalledExecutor(store, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if err := controller.Start(t.Context(), "mobile-control", installed.Revision); err != nil {
		t.Fatal(err)
	}
	o, paths, _, _ := mockIOOptions(t)
	o.Python = python
	o.PackageDirectory, err = store.PackageDirectory(installed.Digest)
	if err != nil {
		t.Fatal(err)
	}
	compat := filepath.Join(paths.workspace, "compat.py")
	if err := os.WriteFile(compat, []byte("# only a fixed legacy resource proof"), 0600); err != nil {
		t.Fatal(err)
	}
	o.CompatibilityEntry = compat
	o.Legacy = legacyConfig(compat, paths.config, paths.storage)
	block := false
	base := o.Runner
	o.Runner = func(ctx context.Context, client string, args []string, limit int64, timeout time.Duration) ([]byte, error) {
		if block {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return base(ctx, client, args, limit, timeout)
	}
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	invoke := func(ctx context.Context) error {
		call, err := b.Begin(ctx, "observe", ioJSON(map[string]any{"device_id": "allowed"}))
		if err != nil {
			return err
		}
		defer call.Close()
		source := strings.Repeat("b", 64)
		p := &nativeIOPolicy{call: call, operation: "device.observe", source: source, alive: true, invocation: pluginhost.Invocation{Account: "account_1", Thread: "synthetic-device", Requester: "synthetic-owner", Purpose: "offline isolation proof", RequestKey: ioID("native-"), Authorization: source}}
		_, err = controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: "mobile-control", Capability: p.operation, Context: p.invocation, Arguments: ioJSON(map[string]any{"source_version": source, "request": map[string]any{"device_id": "allowed"}})}, p)
		return err
	}
	if err := invoke(t.Context()); err != nil {
		t.Fatal("indexed isolated native logic or host-private boundary failed", err)
	}
	block = true
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := invoke(ctx); err == nil || time.Since(started) > 8*time.Second {
		t.Fatal("cancel did not close native isolated invocation", err, time.Since(started))
	}
	free, err := b.Begin(t.Context(), "observe", ioJSON(map[string]any{"device_id": "allowed"}))
	if err != nil {
		t.Fatal("cancelled confirmed IO retained physical lease", err)
	}
	_ = free.Close()
	if err := b.Close(); err != nil {
		t.Fatal("native IO close failed", err)
	}
}
