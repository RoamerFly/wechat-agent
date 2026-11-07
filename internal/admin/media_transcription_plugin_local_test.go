package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
)

func TestMediaTranscriptionLocalServiceBindsProfileAudioAndSingleAttempt(t *testing.T) {
	f, n, old, ctx := transcriptionFixture(t, false)
	installed, err := old.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	installed, err = old.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, expected := writeRegisteredLocalASRProfile(t, f.manager)
	cfg, err := old.store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]json.RawMessage{}
	for key, value := range cfg.Values {
		values[key] = append(json.RawMessage(nil), value...)
	}
	delete(values, "asr_endpoint")
	delete(values, "asr_model")
	values["asr_backend"] = json.RawMessage(`"local"`)
	values["local_asr_profile_id"] = mustJSONRaw(t, expected.Initializer.ProfileID)
	if _, err = old.store.SaveConfiguration(ctx, localasr.PluginID, pluginhost.ConfigUpdate{Revision: cfg.Revision, Values: values, ClearSecrets: []string{"asr_key"}}); err != nil {
		t.Fatal(err)
	}
	installed, err = old.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	p, err := f.manager.newMediaTranscriptionPolicy(ctx, n.msg, old.WAV())
	if err != nil {
		t.Fatal("local policy was not bound to registered profile", err)
	}
	profileID, resourceVersion, _ := p.LocalProfile()
	if p.Backend() != "local" || profileID != expected.Initializer.ProfileID || resourceVersion != expected.ResourceVersion {
		t.Fatal("local policy was not bound to registered profile", p.Backend(), profileID, resourceVersion)
	}
	calls := 0
	p.runLocal = func(_ context.Context, got pluginhost.Package, grant pluginhost.Grant, profile localasr.Profile, wav []byte, timeout int) (string, string, error) {
		calls++
		if got.ID != localasr.PluginID || grant.Account != n.msg.AccountID || profile.Metadata() != expected.Initializer || string(wav) != string(old.WAV()) || timeout != 42 {
			t.Fatal("local runner received changed host binding")
		}
		return "transcribed", "local synthetic transcript", nil
	}
	j, cap := transcriptionJob(p)
	request := func(profile, version string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"profile_id": profile, "resource_version": version, "audio": p.Reference(), "timeout_seconds": 42})
		return raw
	}
	if _, err := p.Service(ctx, j, cap, "wechat/local_transcription", request(profileID, "wrong")); err == nil || calls != 0 || p.Requested() {
		t.Fatal("changed profile reached local runner", err, calls)
	}
	result, err := p.Service(ctx, j, cap, "wechat/local_transcription", request(profileID, resourceVersion))
	if err != nil || calls != 1 {
		t.Fatal("local runner was not invoked exactly once", err, calls)
	}
	var serviceReply struct {
		State string `json:"state"`
		Text  string `json:"text"`
	}
	if json.Unmarshal(result, &serviceReply) != nil || serviceReply.State != "transcribed" || serviceReply.Text != "local synthetic transcript" {
		t.Fatal("local service response changed", string(result))
	}
	if _, err := p.Service(ctx, j, cap, "wechat/local_transcription", request(profileID, resourceVersion)); err == nil || calls != 1 {
		t.Fatal("local ASR attempt replayed", err, calls)
	}
	raw, _ := json.Marshal(map[string]any{"ok": true, "status": "transcribed", "text": "local synthetic transcript", "sources": []mediaAudioReference{p.Reference()}, "coverage": "provided_audio_only", "untrusted_external_content": true})
	verified, err := p.Validate(ctx, j, cap, raw)
	if err != nil || string(verified.Private) == "" || string(verified.Result) == "" || f.manager.billing.Totals().Requests != 0 {
		t.Fatal("local transcription result was not isolated from provider billing", err, string(verified.Result), f.manager.billing.Totals())
	}
	var public map[string]any
	if json.Unmarshal(verified.Result, &public) != nil || public["usage_source"] != "local_runtime" {
		t.Fatal("local result mislabeled as provider usage", string(verified.Result))
	}
}

func TestMediaTranscriptionLocalFailureIsNotResubmitted(t *testing.T) {
	f, n, old, ctx := transcriptionFixture(t, false)
	_, _, expected := writeRegisteredLocalASRProfile(t, f.manager)
	installed, err := old.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := old.store.Configuration(ctx, localasr.PluginID)
	values := map[string]json.RawMessage{}
	for key, value := range cfg.Values {
		values[key] = append(json.RawMessage(nil), value...)
	}
	values["asr_backend"] = json.RawMessage(`"local"`)
	values["local_asr_profile_id"] = mustJSONRaw(t, expected.Initializer.ProfileID)
	if _, err := old.store.SaveConfiguration(ctx, localasr.PluginID, pluginhost.ConfigUpdate{Revision: cfg.Revision, Values: values}); err != nil {
		t.Fatal(err)
	}
	installed, err = old.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	p, err := f.manager.newMediaTranscriptionPolicy(ctx, n.msg, old.WAV())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	p.runLocal = func(context.Context, pluginhost.Package, pluginhost.Grant, localasr.Profile, []byte, int) (string, string, error) {
		calls++
		return "", "", errors.New("synthetic local failure")
	}
	j, cap := transcriptionJob(p)
	profileID, resourceVersion, _ := p.LocalProfile()
	raw, _ := json.Marshal(map[string]any{"profile_id": profileID, "resource_version": resourceVersion, "audio": p.Reference(), "timeout_seconds": 42})
	for range 2 {
		if _, err := p.Service(ctx, j, cap, "wechat/local_transcription", raw); err == nil {
			t.Fatal("synthetic failure lost")
		}
	}
	if calls != 1 || !p.Requested() {
		t.Fatal("failed local ASR was retried", calls, p.Requested())
	}
	verified, err := p.Validate(ctx, j, cap, json.RawMessage(`{"ok":false,"error":"host_service_denied_or_failed"}`))
	if err == nil || verified.Failed {
		t.Fatal("unknown local failure was treated as terminal instead of needing review", err, verified)
	}
	_ = f
}

func TestMediaTranscriptionLocalValidateRechecksRegisteredResources(t *testing.T) {
	for _, kind := range []string{"vad", "sdk", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			_, _, policy, ctx, expected := configuredLocalTranscriptionPolicy(t)
			job, capability := transcriptionJob(policy)
			policy.runLocal = func(context.Context, pluginhost.Package, pluginhost.Grant, localasr.Profile, []byte, int) (string, string, error) {
				tamperRegisteredLocalASRResource(t, expected, kind)
				return "transcribed", "synthetic transcript", nil
			}
			profileID, resourceVersion, _ := policy.LocalProfile()
			request, _ := json.Marshal(map[string]any{"profile_id": profileID, "resource_version": resourceVersion, "audio": policy.Reference(), "timeout_seconds": 42})
			if _, err := policy.Service(ctx, job, capability, "wechat/local_transcription", request); err != nil {
				t.Fatal("synthetic local execution did not reach the post-run recheck", err)
			}
			result, _ := json.Marshal(map[string]any{"ok": true, "status": "transcribed", "text": "synthetic transcript", "sources": []mediaAudioReference{policy.Reference()}, "coverage": "provided_audio_only", "untrusted_external_content": true})
			if _, err := policy.Validate(ctx, job, capability, result); err == nil {
				t.Fatal("local result survived a changed registered resource")
			}
		})
	}
}

func TestMediaTranscriptionLocalCompletedResultRechecksRegisteredResources(t *testing.T) {
	for _, kind := range []string{"vad", "sdk", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			_, _, policy, ctx, expected := configuredLocalTranscriptionPolicy(t)
			job, capability := transcriptionJob(policy)
			job.Arguments = policy.Arguments()
			created, createdNew, err := policy.store.CreateJob(ctx, job)
			if err != nil || !createdNew {
				t.Fatal("failed to persist synthetic local ASR job", err, createdNew)
			}
			claimed, err := policy.store.ClaimJob(ctx, created.ID, created.Revision)
			if err != nil {
				t.Fatal(err)
			}
			completed, err := policy.store.CompleteJobWithPrivate(ctx, claimed.ID, claimed.Revision, nil, json.RawMessage(`{"ok":true,"status":"transcribed"}`), json.RawMessage(`{"text":"synthetic transcript"}`))
			if err != nil {
				t.Fatal(err)
			}
			tamperRegisteredLocalASRResource(t, expected, kind)
			if err = policy.Check(ctx, completed, pluginhost.Capability{}); err == nil {
				t.Fatal("completed local transcript survived a changed registered resource")
			}
			if _, err = policy.store.PrivateJobResult(ctx, completed, func(ctx context.Context, current pluginhost.Job) error { return policy.Check(ctx, current, capability) }); err == nil {
				t.Fatal("private local transcript was readable after registered resource changed")
			}
		})
	}
}

func configuredLocalTranscriptionPolicy(t *testing.T) (*webFixture, *bindingChannelsNative, *mediaTranscriptionPolicy, context.Context, localASRTestProfile) {
	t.Helper()
	f, n, old, ctx := transcriptionFixture(t, false)
	installed, err := old.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	_, _, expected := writeRegisteredLocalASRProfile(t, f.manager)
	configuration, err := old.store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]json.RawMessage, len(configuration.Values)+1)
	for key, value := range configuration.Values {
		values[key] = append(json.RawMessage(nil), value...)
	}
	delete(values, "asr_endpoint")
	delete(values, "asr_model")
	values["asr_backend"] = json.RawMessage(`"local"`)
	values["local_asr_profile_id"] = mustJSONRaw(t, expected.Initializer.ProfileID)
	if _, err = old.store.SaveConfiguration(ctx, localasr.PluginID, pluginhost.ConfigUpdate{Revision: configuration.Revision, Values: values, ClearSecrets: []string{"asr_key"}}); err != nil {
		t.Fatal(err)
	}
	installed, err = old.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	policy, err := f.manager.newMediaTranscriptionPolicy(ctx, n.msg, old.WAV())
	if err != nil {
		t.Fatal(err)
	}
	return f, n, policy, ctx, expected
}

func tamperRegisteredLocalASRResource(t *testing.T, profile localASRTestProfile, kind string) {
	t.Helper()
	path, contents := profile.VAD, []byte("tampered-vad")
	switch kind {
	case "sdk":
		path, contents = profile.SDKFile, []byte("tampered-sdk")
	case "manifest":
		path, contents = profile.Manifest, []byte(`{"id":"changed"}`)
	case "vad":
	default:
		t.Fatalf("unknown registered local ASR resource %q", kind)
	}
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
}
