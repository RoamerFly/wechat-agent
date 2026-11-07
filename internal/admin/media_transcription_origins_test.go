package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func boundTranscriptionInvocation() pluginhost.Invocation {
	return pluginhost.Invocation{
		Account:       "test",
		Thread:        "host:synthetic-origin",
		Requester:     "administrator",
		Purpose:       "synthetic media transcription test",
		RequestKey:    "synthetic-origin-request",
		Authorization: "synthetic-origin-authorization",
		SourceVersions: map[string]int{
			"synthetic-source": 7,
		},
	}
}

func TestBoundMediaTranscriptionOriginRequiresProofAndRechecks(t *testing.T) {
	f, _, _, ctx := transcriptionFixture(t, false)
	if _, err := f.manager.newBoundMediaTranscriptionPolicy(ctx, syntheticWAV(), pluginhost.Invocation{}, 1, func(context.Context) error { return nil }); err == nil {
		t.Fatal("bare WAV acquired a media transcription policy")
	}
	valid := true
	p, err := f.manager.newBoundMediaTranscriptionPolicy(ctx, syntheticWAV(), boundTranscriptionInvocation(), 7, func(context.Context) error {
		if !valid {
			return errors.New("synthetic origin revoked")
		}
		return nil
	})
	if err != nil || p.receivedSharePolicy != nil || p.invocation().Purpose != "synthetic media transcription test" || p.invocation().Thread != "host:synthetic-origin" {
		t.Fatal("bound origin was converted to an incoming message", err, p)
	}
	job, capability := transcriptionJob(p)
	calls := 0
	p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"output":{"text":"synthetic bound transcript"}}`), nil
	}
	if _, err = p.Service(ctx, job, capability, "wechat/model_request", transcriptionRequest(p)); err != nil || calls != 1 {
		t.Fatal("bound origin did not reach exactly one provider attempt", err, calls)
	}
	if _, err = p.Service(ctx, job, capability, "wechat/model_request", transcriptionRequest(p)); err == nil || calls != 1 {
		t.Fatal("bound origin resubmitted a provider request", err, calls)
	}
	valid = false
	result, _ := json.Marshal(map[string]any{"ok": true, "text": "synthetic bound transcript", "status": "transcribed", "sources": []mediaAudioReference{p.Reference()}, "coverage": "provided_audio_only", "untrusted_external_content": true})
	if _, err = p.Validate(ctx, job, capability, result); err == nil {
		t.Fatal("revoked bound origin accepted provider result")
	}

	// A completed or queued non-incoming origin is just as invalidated by an
	// administrator ASR configuration change as an incoming one.
	p2, err := f.manager.newBoundMediaTranscriptionPolicy(ctx, syntheticWAV(), boundTranscriptionInvocation(), 7, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	job2, capability2 := transcriptionJob(p2)
	installed, err := p2.store.Installed(ctx, "media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p2.store.SetEnabled(ctx, installed.Package.ID, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	configuration, err := p2.store.Configuration(ctx, "media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p2.store.SaveConfiguration(ctx, "media-understanding", pluginhost.ConfigUpdate{Revision: configuration.Revision, Values: map[string]json.RawMessage{
		"vision_endpoint": json.RawMessage(`"https://provider.example/chat/completions"`),
		"vision_model":    json.RawMessage(`"synthetic-model"`),
		"asr_endpoint":    json.RawMessage(`"https://provider.example/api/v1/services/aigc/multimodal-generation/generation"`),
		"asr_model":       json.RawMessage(`"changed-asr-model"`),
	}, Secrets: map[string]string{"asr_key": "synthetic-asr-key"}}); err != nil {
		t.Fatal(err)
	}
	installed, err = p2.store.Installed(ctx, "media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p2.store.SetEnabled(ctx, installed.Package.ID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	if err = p2.Check(ctx, job2, capability2); err == nil {
		t.Fatal("changed ASR configuration accepted by bound origin")
	}
}

func TestIncomingMediaTranscriptionRangeSuffixBindsCurrentInvocation(t *testing.T) {
	f, n, policy, ctx := transcriptionFixture(t, false)
	oldJob, capability := transcriptionJob(policy)
	// This is the same shape used by video_inspect: a silent range can produce
	// byte-identical WAV data, but each requested time range remains a distinct
	// idempotency operation.
	policy.appendRequestSuffix("0", "30")
	currentJob, _ := transcriptionJob(policy)
	if currentJob.Context.RequestKey == oldJob.Context.RequestKey {
		t.Fatal("range suffix did not change the incoming transcription request key")
	}
	if err := policy.Check(ctx, currentJob, capability); err != nil {
		t.Fatal("current ranged incoming transcription job was rejected", err)
	}
	if err := policy.Check(ctx, oldJob, capability); err == nil {
		t.Fatal("pre-range job remained valid after the requested range changed")
	}

	other, err := f.manager.newMediaTranscriptionPolicy(ctx, n.msg, policy.WAV())
	if err != nil {
		t.Fatal(err)
	}
	other.appendRequestSuffix("30", "30")
	otherJob, _ := transcriptionJob(other)
	if otherJob.Context.RequestKey == currentJob.Context.RequestKey {
		t.Fatal("two requested ranges with identical WAV bytes collapsed into one request key")
	}
	if err := other.Check(ctx, otherJob, capability); err != nil {
		t.Fatal("second ranged incoming transcription job was rejected", err)
	}
}

// This opt-in test traverses the official isolated media package through the
// Controller. Both provider callbacks are synthetic: no provider, media
// download, device, or WeChat interaction occurs.
func TestLiveAdminMediaTranscriptionOriginIsolation(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in isolated plugin with synthetic provider only")
	}
	python, err := filepath.Abs(python)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	if err != nil {
		t.Fatal(err)
	}
	options := pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}
	f, ctx, speech, _ := adminSpeechFixture(t)
	store := speech.store
	prepared, err := store.Prepare(ctx, "../../plugins/media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(ctx, prepared.Token, prepared.Digest, prepared.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SaveConfiguration(ctx, "media-understanding", pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{
		"asr_endpoint": json.RawMessage(`"https://provider.example/api/v1/services/aigc/multimodal-generation/generation"`),
		"asr_model":    json.RawMessage(`"synthetic-asr"`),
	}, Secrets: map[string]string{"asr_key": "synthetic-asr-key"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetEnabled(ctx, "media-understanding", installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	controller, err := pluginruntime.NewController(store, pluginruntime.InstalledExecutor(store, options))
	if err != nil {
		t.Fatal(err)
	}
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	t.Cleanup(func() { _ = controller.Close() })
	speechCalls := 0
	speech.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		speechCalls++
		return json.RawMessage(`{"model":"synthetic-tts","output":{"audio":{"url":"https://audio.example/admin-generated.wav"}}}`), nil
	}
	if _, err = speech.synthesize(ctx); err != nil || speechCalls != 1 {
		t.Fatal("admin speech origin was not completed", err, speechCalls)
	}
	p, err := f.manager.newAdminMediaTranscriptionPolicy(ctx, speech, syntheticWAV())
	if err != nil {
		t.Fatal(err)
	}
	if p.receivedSharePolicy != nil || p.invocation().Thread != "host:admin-speech-test" || p.invocation().Requester != "administrator" || p.invocation().Purpose != "admin speech ASR test" {
		t.Fatal("admin ASR origin impersonated a received message", p.invocation())
	}
	asrCalls := 0
	p.post = func(_ context.Context, _ string, _ map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
		asrCalls++
		if bytes.Contains(body, []byte(`$media_ref`)) || !bytes.Contains(body, []byte("data:audio/wav;base64,")) {
			t.Fatal("isolated ASR callback was not materialized from the bound WAV")
		}
		return json.RawMessage(`{"output":{"text":"synthetic administrator transcript"}}`), nil
	}
	text, err := p.transcribe(ctx)
	if err != nil || text != "synthetic administrator transcript" || asrCalls != 1 || p.CompletedJob().State != "completed" {
		t.Fatal("isolated administrator ASR origin failed", text, err, asrCalls, p.CompletedJob())
	}
	if err = p.checkCompleted(ctx); err != nil {
		t.Fatal("completed administrator transcript lost its origin proof", err)
	}
}
