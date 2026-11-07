package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func audioObservationInvocation() pluginhost.Invocation {
	return pluginhost.Invocation{Account: "test", Thread: "host:synthetic-audio-origin", Requester: "synthetic-approved-requester", Purpose: "observe selected audio samples", RequestKey: "synthetic-audio-origin", Authorization: "synthetic-current-authority", EvidenceEpoch: "synthetic-source-epoch", SourceVersions: map[string]int{"synthetic-audio-source": 7}}
}

func audioObservationFixture(t *testing.T) (*webFixture, *mediaAudioObservationPolicy, *bool) {
	t.Helper()
	f := setup(t)
	store, err := f.manager.packageStore()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.Prepare(t.Context(), "../../plugins/media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	i, err := store.Install(t.Context(), prepared.Token, prepared.Digest, prepared.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveConfiguration(t.Context(), i.Package.ID, pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{"audio_review_endpoint": json.RawMessage(`"https://audio-provider.example/compatible-mode/v1/chat/completions"`), "audio_review_model": json.RawMessage(`"synthetic-audio-model"`), "max_tokens": json.RawMessage(`1120`)}, Secrets: map[string]string{"audio_review_key": "synthetic-audio-key"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetEnabled(t.Context(), i.Package.ID, i.Revision, true); err != nil {
		t.Fatal(err)
	}
	// This generic adapter delegates authority to the approved origin. It must
	// not impose conversation-send permissions, ASR or a visual model on it.
	f.manager.config.Enabled = false
	f.manager.config.Companion.Permissions.SendVoice = false
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.ASREnabled = false
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	valid := true
	p, err := f.manager.newBoundMediaAudioObservationPolicy(t.Context(), syntheticWAV(), audioObservationInvocation(), 7, func(ctx context.Context) error {
		if !valid {
			return errors.New("synthetic audio origin revoked")
		}
		return ctx.Err()
	}, "Describe only the supplied audio", 17, 7200)
	if err != nil {
		t.Fatal(err)
	}
	return f, p, &valid
}

func audioObservationJob(p *mediaAudioObservationPolicy) (pluginhost.Job, pluginhost.Capability) {
	return pluginhost.Job{Plugin: "media-understanding", Tool: "media.listen", Version: p.version, PackageDigest: p.digest, ConfigRevision: strconv.Itoa(p.configRevision), Context: p.invocation()}, pluginhost.Capability{ID: "media.listen", Permissions: []string{"artifact.read", "model.invoke"}}
}

func audioObservationRequest(p *mediaAudioObservationPolicy) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"route": "audio-review", "body": p.requestBody(false), "max_bytes": 1 << 20, "timeout_seconds": p.timeoutSeconds})
	return raw
}

func audioObservationResult(p *mediaAudioObservationPolicy, status, text string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"ok": true, "status": status, "observation": text, "sources": []mediaAudioReference{p.ref}, "coverage": "provided_audio_only", "untrusted_external_content": true})
	return raw
}

func TestMediaAudioObservationExactRequestSingleAttemptAndPrivateModelOpinion(t *testing.T) {
	f, p, _ := audioObservationFixture(t)
	job, cap := audioObservationJob(p)
	calls := 0
	p.post = func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
		calls++
		if endpoint != "https://audio-provider.example/compatible-mode/v1/chat/completions" || headers["Authorization"] != "Bearer synthetic-audio-key" || timeout != 7200*time.Second || limit != 1<<20 || bytes.Contains(body, []byte("$media_ref")) {
			t.Fatal("unbound audio observation transport", endpoint, timeout, limit)
		}
		var actual map[string]any
		if json.Unmarshal(body, &actual) != nil {
			t.Fatal("invalid bound body")
		}
		expected := map[string]any{"model": "synthetic-audio-model", "stream": true, "enable_thinking": false, "max_tokens": 17, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Describe only the supplied audio"}, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(p.wav), "format": "wav"}}}}}}
		if !sameShareBody(body, expected) {
			t.Fatal("wrong materialized audio body", string(body))
		}
		return json.RawMessage(`{"model":"provider-audio-revision","choices":[{"message":{"content":"Synthetic audio observation","reasoning_content":"private reasoning sentinel"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`), nil
	}
	request := audioObservationRequest(p)
	for _, change := range [][2]string{{"synthetic-audio-model", "other-model"}, {"Describe only the supplied audio", "another prompt"}, {`"stream":true`, `"stream":false`}, {`"enable_thinking":false`, `"enable_thinking":true`}, {`"max_tokens":17`, `"max_tokens":18`}, {`"format":"wav"`, `"format":"mp3"`}, {p.ref.SHA, strings.Repeat("f", 64)}, {`"timeout_seconds":7200`, `"timeout_seconds":180`}, {`"max_bytes":1048576`, `"max_bytes":0`}, {`"max_bytes":1048576`, `"max_bytes":2097153`}} {
		bad := bytes.Replace(request, []byte(change[0]), []byte(change[1]), 1)
		if bytes.Equal(bad, request) {
			t.Fatal("test mutation did not change request", change)
		}
		if _, err := p.Service(t.Context(), job, cap, "wechat/model_request", bad); err == nil || calls != 0 {
			t.Fatal("changed audio request reached provider", change, err, calls)
		}
	}
	if _, err := p.Service(t.Context(), job, cap, "wechat/model_request", request); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Service(t.Context(), job, cap, "wechat/model_request", request); err == nil || calls != 1 {
		t.Fatal("audio observation resubmitted", err, calls)
	}
	verified, err := p.Validate(t.Context(), job, cap, audioObservationResult(p, "observed", "Synthetic audio observation"))
	if err != nil || bytes.Contains(verified.Result, []byte("Synthetic audio observation")) || !bytes.Contains(verified.Private, []byte("Synthetic audio observation")) || bytes.Contains(verified.Private, []byte("private reasoning sentinel")) || !bytes.Contains(verified.Private, []byte(`"voice_identity_evidence":"model_opinion_only"`)) || !bytes.Contains(verified.Private, []byte(`"provider_model":"provider-audio-revision"`)) {
		t.Fatal("audio observation lost bounded private provenance", err, string(verified.Result), string(verified.Private))
	}
	u := f.manager.billing.PurposeTotals("plugin_media_audio_observation")
	if u.Requests != 1 || u.Reported != 1 || u.Input != 11 || u.Output != 5 {
		t.Fatal("audio observation provider usage lost", u)
	}
}

func TestMediaAudioObservationBindsProviderMessageFinishAndSources(t *testing.T) {
	for _, tc := range []struct {
		name, provider, status, text string
		want                         bool
	}{
		{"stop", `{"choices":[{"message":{"content":" actual samples "},"finish_reason":"stop"}]}`, "observed", "actual samples", true},
		{"length_partial", `{"choices":[{"message":{"content":"partial samples"},"finish_reason":"length"}]}`, "partial", "partial samples", true},
		{"length_not_complete", `{"choices":[{"message":{"content":"partial samples"},"finish_reason":"length"}]}`, "observed", "partial samples", false},
		{"invented_text", `{"choices":[{"message":{"content":"actual samples"},"finish_reason":"stop"}]}`, "observed", "invented samples", false},
		{"tool_call", `{"choices":[{"message":{"content":"actual samples","tool_calls":[{"id":"fake-tool"}]},"finish_reason":"stop"}]}`, "observed", "actual samples", false},
		{"function_call", `{"choices":[{"message":{"content":"actual samples","function_call":{"name":"fake"}},"finish_reason":"stop"}]}`, "observed", "actual samples", false},
		{"null_tool_call", `{"choices":[{"message":{"content":"actual samples","tool_calls":null},"finish_reason":"stop"}]}`, "observed", "actual samples", false},
		{"provider_error", `{"error":{"code":"failed"},"choices":[{"message":{"content":"actual samples"},"finish_reason":"stop"}]}`, "observed", "actual samples", false},
		{"unfinished", `{"choices":[{"message":{"content":"actual samples"},"finish_reason":null}]}`, "observed", "actual samples", false},
		{"duplicate_choices", `{"choices":[],"choices":[{"message":{"content":"actual samples"},"finish_reason":"stop"}]}`, "observed", "actual samples", false},
		{"extra_choice", `{"choices":[{"message":{"content":"actual samples"},"finish_reason":"stop"},{"message":{"content":"other samples"},"finish_reason":"stop"}]}`, "observed", "actual samples", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, _ := audioObservationFixture(t)
			job, cap := audioObservationJob(p)
			p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
				return json.RawMessage(tc.provider), nil
			}
			if _, err := p.Service(t.Context(), job, cap, "wechat/model_request", audioObservationRequest(p)); err != nil {
				t.Fatal(err)
			}
			_, err := p.Validate(t.Context(), job, cap, audioObservationResult(p, tc.status, tc.text))
			if (err == nil) != tc.want {
				t.Fatal("provider evidence mismatch", tc.name, err)
			}
		})
	}
}

func TestMediaAudioObservationHonorsResponseLimitAndRejectsPluginUsageClaims(t *testing.T) {
	f, p, _ := audioObservationFixture(t)
	job, cap := audioObservationJob(p)
	p.post = func(_ context.Context, _ string, _ map[string]string, _ json.RawMessage, limit int64, _ time.Duration) (json.RawMessage, error) {
		if limit != 2<<20 {
			t.Fatal("explicit response limit was clamped", limit)
		}
		return json.RawMessage(`{"choices":[{"message":{"content":"synthetic"},"finish_reason":"stop"}]}`), nil
	}
	request := bytes.Replace(audioObservationRequest(p), []byte(`"max_bytes":1048576`), []byte(`"max_bytes":2097152`), 1)
	if _, err := p.Service(t.Context(), job, cap, "wechat/model_request", request); err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	_ = json.Unmarshal(audioObservationResult(p, "observed", "synthetic"), &candidate)
	candidate["usage"] = map[string]any{"prompt_tokens": 999999, "completion_tokens": 999999}
	raw, _ := json.Marshal(candidate)
	if _, err := p.Validate(t.Context(), job, cap, raw); err == nil {
		t.Fatal("plugin-created usage claims were accepted")
	}
	usage := f.manager.billing.PurposeTotals("plugin_media_audio_observation")
	if usage.Requests != 1 || usage.Reported != 0 || usage.Input != 0 || usage.Output != 0 {
		t.Fatal("provider-unreported audio usage became fabricated counts", usage)
	}
}

func TestMediaAudioObservationAuthorityConfigPackageAndCancellationRechecked(t *testing.T) {
	for _, change := range []string{"origin", "account", "thread", "requester", "source", "config", "disabled", "package", "cancelled_context", "job_cancel"} {
		t.Run(change, func(t *testing.T) {
			_, p, valid := audioObservationFixture(t)
			job, cap := audioObservationJob(p)
			ctx := t.Context()
			switch change {
			case "origin":
				*valid = false
			case "account":
				job.Context.Account = "other-account"
			case "thread":
				job.Context.Thread = "other-thread"
			case "requester":
				job.Context.Requester = "other-requester"
			case "source":
				job.Context.SourceVersions["synthetic-audio-source"]++
			case "config", "disabled":
				i, _ := p.store.Installed(ctx, job.Plugin)
				if _, err := p.store.SetEnabled(ctx, job.Plugin, i.Revision, false); err != nil {
					t.Fatal(err)
				}
				if change == "config" {
					_, err := p.store.SaveConfiguration(ctx, job.Plugin, pluginhost.ConfigUpdate{Revision: p.configRevision, Values: map[string]json.RawMessage{"audio_review_endpoint": json.RawMessage(`"https://other-provider.example/chat/completions"`), "audio_review_model": json.RawMessage(`"other-model"`)}})
					if err != nil {
						t.Fatal(err)
					}
				}
			case "package":
				job.PackageDigest = strings.Repeat("f", 64)
			case "cancelled_context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "job_cancel":
				job.Arguments = p.arguments
				var err error
				job, _, err = p.store.CreateJob(ctx, job)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = p.store.RequestJobCancel(ctx, job.ID, job.Revision); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
				calls++
				return nil, nil
			}
			if _, err := p.Service(ctx, job, cap, "wechat/model_request", audioObservationRequest(p)); err == nil || calls != 0 {
				t.Fatal("revoked audio observation reached provider", change, err, calls)
			}
			if _, err := p.Validate(ctx, job, cap, audioObservationResult(p, "observed", "synthetic")); err == nil {
				t.Fatal("revoked audio result accepted", change)
			}
		})
	}
}

func TestMediaAudioObservationUncertainTransportNoRetryAndPostCallRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(strconv.FormatBool(revoke), func(t *testing.T) {
			f, p, valid := audioObservationFixture(t)
			job, cap := audioObservationJob(p)
			calls := 0
			p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
				calls++
				if revoke {
					*valid = false
					return json.RawMessage(`{"choices":[{"message":{"content":"synthetic"},"finish_reason":"stop"}]}`), nil
				}
				return nil, errors.New("synthetic uncertain stream")
			}
			for i := 0; i < 2; i++ {
				if _, err := p.Service(t.Context(), job, cap, "wechat/model_request", audioObservationRequest(p)); err == nil {
					t.Fatal("uncertain/revoked audio attempt returned usable evidence")
				}
			}
			if calls != 1 || f.manager.billing.PurposeTotals("plugin_media_audio_observation").Requests != 1 || f.manager.billing.PurposeTotals("plugin_media_audio_observation").Reported != 0 {
				t.Fatal("uncertain audio usage was replayed or reported as zero", calls)
			}
			if _, err := p.Validate(t.Context(), job, cap, audioObservationResult(p, "observed", "synthetic")); err == nil {
				t.Fatal("uncertain/revoked stream produced verified observation")
			}
		})
	}
}

func TestMediaAudioObservationInputLimitsDefaultsAndFrozenOrigin(t *testing.T) {
	f, p, _ := audioObservationFixture(t)
	inv := audioObservationInvocation()
	wav := syntheticWAV()
	bound, err := f.manager.newBoundMediaAudioObservationPolicy(t.Context(), wav, inv, 7, func(context.Context) error { return nil }, "Audio only", 0, 0)
	if err != nil || bound.tokens != 1120 || bound.timeoutSeconds != 90 {
		t.Fatal("configured defaults lost", err)
	}
	unicodeBound, err := f.manager.newBoundMediaAudioObservationPolicy(t.Context(), syntheticWAV(), audioObservationInvocation(), 7, func(context.Context) error { return nil }, strings.Repeat("听", 8192), 50000, 86400)
	if err != nil || unicodeBound.tokens != 50000 || unicodeBound.timeoutSeconds != 86400 {
		t.Fatal("Unicode boundary or explicit provider parameters were clamped", err)
	}
	inv.SourceVersions["synthetic-audio-source"] = 8
	wav[0] = 'X'
	if bound.invocation().SourceVersions["synthetic-audio-source"] != 7 || bound.wav[0] != 'R' {
		t.Fatal("caller mutation changed bound origin")
	}
	for _, tc := range []struct {
		name            string
		wav             []byte
		prompt          string
		tokens, timeout int
	}{
		{"over_3mib", make([]byte, (3<<20)+1), "audio", 1, 1},
		{"invalid_wav", []byte("not audio"), "audio", 1, 1},
		{"empty_prompt", syntheticWAV(), " ", 1, 1},
		{"over_unicode_prompt", syntheticWAV(), strings.Repeat("听", 8193), 1, 1},
		{"negative_tokens", syntheticWAV(), "audio", -1, 1},
		{"negative_timeout", syntheticWAV(), "audio", 1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.manager.newBoundMediaAudioObservationPolicy(t.Context(), tc.wav, audioObservationInvocation(), 7, func(context.Context) error { return nil }, tc.prompt, tc.tokens, tc.timeout); err == nil {
				t.Fatal("invalid audio observation input accepted")
			}
		})
	}
	if _, err = f.manager.newBoundMediaAudioObservationPolicy(t.Context(), syntheticWAV(), pluginhost.Invocation{}, 7, func(context.Context) error { return nil }, "audio", 1, 1); err == nil {
		t.Fatal("bare WAV gained audio observation authority")
	}
	if _, err = f.manager.newBoundMediaAudioObservationPolicy(t.Context(), syntheticWAV(), audioObservationInvocation(), 7, nil, "audio", 1, 1); err == nil {
		t.Fatal("missing origin proof accepted")
	}
	i, _ := p.store.Installed(t.Context(), "media-understanding")
	if _, err = p.store.SetEnabled(t.Context(), i.Package.ID, i.Revision, false); err != nil {
		t.Fatal(err)
	}
	if _, err = p.store.SaveConfiguration(t.Context(), i.Package.ID, pluginhost.ConfigUpdate{Revision: p.configRevision, Values: map[string]json.RawMessage{"vision_endpoint": json.RawMessage(`"https://vision.example/chat/completions"`), "vision_model": json.RawMessage(`"synthetic-visual"`)}, Secrets: map[string]string{"vision_key": "synthetic-vision-key"}, ClearSecrets: []string{"audio_review_key"}}); err != nil {
		t.Fatal(err)
	}
	i, _ = p.store.Installed(t.Context(), i.Package.ID)
	if _, err = p.store.SetEnabled(t.Context(), i.Package.ID, i.Revision, true); err != nil {
		t.Fatal(err)
	}
	if f.manager.mediaAudioObservationConfigured(t.Context()) {
		t.Fatal("visual model/key silently became an audio observation route")
	}
	if _, err = f.manager.newBoundMediaAudioObservationPolicy(t.Context(), syntheticWAV(), audioObservationInvocation(), 7, func(context.Context) error { return nil }, "audio", 1, 1); err == nil {
		t.Fatal("unconfigured audio model fell back to visual/legacy key")
	}
}

func TestLiveMediaAudioObservationPrivateCheckpointAndRevocation(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in isolated audio observation with synthetic provider")
	}
	python, _ = filepath.Abs(python)
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	f, p, valid := audioObservationFixture(t)
	controller, err := pluginruntime.NewController(p.store, pluginruntime.InstalledExecutor(p.store, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
	if err != nil {
		t.Fatal(err)
	}
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	t.Cleanup(func() { _ = controller.Close() })
	calls := 0
	p.post = func(_ context.Context, _ string, _ map[string]string, body json.RawMessage, _ int64, timeout time.Duration) (json.RawMessage, error) {
		calls++
		if !sameShareBody(body, p.requestBody(true)) || timeout != 7200*time.Second {
			t.Fatal("isolated plugin changed bound body/timeout")
		}
		return json.RawMessage(`{"model":"synthetic-reported-model","choices":[{"message":{"content":"synthetic listening observation"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":4}}`), nil
	}
	result, err := p.observe(t.Context())
	if err != nil || calls != 1 || result.Status != "observed" || result.Text != "synthetic listening observation" || result.Model != "synthetic-audio-model" || result.ProviderModel != "synthetic-reported-model" || result.VoiceEvidence != "model_opinion_only" || p.completedJob.State != "completed" {
		t.Fatal("isolated audio observation contract", result, err, calls)
	}
	if err = p.checkCompleted(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := p.store.Jobs(t.Context(), "media-understanding", 0, 8)
	if err != nil || len(page.Items) != 1 || bytes.Contains(page.Items[0].Result, []byte("synthetic listening observation")) || bytes.Contains(page.Items[0].Result, []byte("prompt_tokens")) {
		t.Fatal("raw provider content entered public checkpoint", page, err)
	}
	*valid = false
	if err = p.checkCompleted(t.Context()); err == nil {
		t.Fatal("completed listening observation outlived origin authority")
	}
	if _, err = p.store.PrivateJobResult(t.Context(), p.completedJob, func(ctx context.Context, job pluginhost.Job) error { return p.Check(ctx, job, pluginhost.Capability{}) }); err == nil {
		t.Fatal("revoked listening observation remained readable")
	}
}
