package admin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

// These opt-in tests run the official media-understanding package in its real
// isolated Python host. The ASR runner remains synthetic: the assertion is the
// cross-process configuration/resource contract, never a local SDK or model
// claim.
func TestLiveLocalMediaTranscriptionIsolationContract(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in official isolated media package with synthetic local ASR")
	}
	python, err := filepath.Abs(python)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	if err != nil {
		t.Fatal(err)
	}
	f, n, policy, ctx, expected := configuredLocalTranscriptionPolicy(t)
	controller, err := pluginruntime.NewController(policy.store, pluginruntime.InstalledExecutor(policy.store, pluginruntime.Options{
		Python:        python,
		Pythonw:       filepath.Join(filepath.Dir(python), "pythonw.exe"),
		WindowsHelper: helper,
		RuntimeRoots:  []string{filepath.Dir(python)},
		ResourceRoots: f.manager.localASRPluginResources,
		HostMetadata:  f.manager.localASRPluginMetadata,
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	calls := 0
	policy.runLocal = func(_ context.Context, got pluginhost.Package, grant pluginhost.Grant, profile localasr.Profile, wav []byte, timeout int) (string, string, error) {
		calls++
		if got.ID != localasr.PluginID || grant.Account != n.msg.AccountID || profile.Metadata() != expected.Initializer || string(wav) != string(policy.WAV()) || timeout < 1 {
			t.Fatal("isolated package callback lost its bound local ASR contract")
		}
		return "transcribed", "synthetic isolated local transcript", nil
	}
	text, err := policy.transcribe(ctx)
	if err != nil || text != "synthetic isolated local transcript" || calls != 1 || f.manager.billing.Totals().Requests != 0 || policy.CompletedJob().State != "completed" {
		t.Fatal("isolated local ASR did not complete without provider transport", text, err, calls, f.manager.billing.Totals(), policy.CompletedJob())
	}
	var private struct {
		Text   string `json:"text"`
		Status string `json:"status"`
	}
	raw, err := policy.store.PrivateJobResult(ctx, policy.CompletedJob(), func(ctx context.Context, job pluginhost.Job) error {
		return policy.Check(ctx, job, pluginhost.Capability{})
	})
	if err != nil || json.Unmarshal(raw, &private) != nil || private.Text != text || private.Status != "transcribed" {
		t.Fatal("isolated local ASR did not persist the verified private result", err, string(raw), private)
	}
}

func TestLiveLocalMediaTranscriptionIsolationRejectsMismatchedProfileMetadata(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in official isolated media package with synthetic local ASR")
	}
	python, err := filepath.Abs(python)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	if err != nil {
		t.Fatal(err)
	}
	f, _, policy, ctx, _ := configuredLocalTranscriptionPolicy(t)
	controller, err := pluginruntime.NewController(policy.store, pluginruntime.InstalledExecutor(policy.store, pluginruntime.Options{
		Python:        python,
		Pythonw:       filepath.Join(filepath.Dir(python), "pythonw.exe"),
		WindowsHelper: helper,
		RuntimeRoots:  []string{filepath.Dir(python)},
		ResourceRoots: f.manager.localASRPluginResources,
		HostMetadata: func(context.Context, pluginhost.Package, pluginhost.Grant) (map[string]json.RawMessage, error) {
			// Keep the shape trusted but use a nonce profile id that cannot match
			// the explicit local_asr_profile_id in the installed configuration.
			return map[string]json.RawMessage{"local_asr": json.RawMessage(`{"profile_id":"nonce-profile","resource_version":"synthetic"}`)}, nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	calls := 0
	policy.runLocal = func(context.Context, pluginhost.Package, pluginhost.Grant, localasr.Profile, []byte, int) (string, string, error) {
		calls++
		return "transcribed", "must not run", nil
	}
	if _, err = policy.transcribe(ctx); err == nil || calls != 0 {
		t.Fatal("mismatched profile metadata reached the host local-ASR callback", err, calls)
	}
}
