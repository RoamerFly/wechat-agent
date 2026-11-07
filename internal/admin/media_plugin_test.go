package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func mediaPolicyFixture(t *testing.T, remember bool) (*webFixture, *bindingChannelsNative, *mediaPluginPolicy, context.Context) {
	t.Helper()
	f := setup(t)
	s, err := pluginhost.OpenStore(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// Install the checked-in package unchanged.  Its optional visual/ASR routes
	// are independently configured below; fixtures must not promote a private
	// copy into a different package from the one users install.
	prepared, err := s.Prepare(t.Context(), "../../plugins/media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	i, err := s.Install(t.Context(), prepared.Token, prepared.Digest, prepared.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveConfiguration(t.Context(), prepared.Package.ID, pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{"vision_endpoint": json.RawMessage(`"https://provider.example/chat/completions"`), "vision_model": json.RawMessage(`"synthetic-model"`)}, Secrets: map[string]string{"vision_key": "synthetic-private-key"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetEnabled(t.Context(), prepared.Package.ID, i.Revision, true); err != nil {
		t.Fatal(err)
	}
	f.manager.packages = s
	f.manager.config.Enabled, f.manager.config.Behavior.ReadImages, f.manager.config.Behavior.ReadVideos = true, true, true
	f.manager.config.Companion.Permissions.Remember = remember
	n := &bindingChannelsNative{fakeNative: f.native, msg: linuxnative.Message{ID: "synthetic-image", AccountID: "test", ThreadID: "filehelper", SenderID: "synthetic-user", ServerID: "synthetic-server", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Direction: "incoming", Type: 3}}
	f.manager.native = n
	ctx := t.Context()
	if remember {
		f.manager.config.Companion.Permissions.Remember = true
		original := ""
		in := companion.Input{Channel: "wechat", Account: "test", Thread: n.msg.ThreadID, Sender: n.msg.SenderID, TraceID: n.msg.ID, MessageTime: n.msg.CreatedAt, OriginalText: &original, Text: "synthetic image", Source: &companion.SourceRef{InstanceID: f.manager.memory.SourceInstanceID(), MessageID: n.msg.ID, ServerID: n.msg.ServerID, Account: "test", Thread: n.msg.ThreadID, Sender: n.msg.SenderID, MessageType: 3, OccurredAt: n.msg.CreatedAt, Revision: 1}}
		if err = f.manager.memory.Observe(in, f.manager.config.Companion, time.Now()); err != nil {
			t.Fatal(err)
		}
		ref, ok := f.manager.memory.SourceForInput(in)
		if !ok {
			t.Fatal("missing source")
		}
		ctx = companion.WithMediaSource(ctx, *ref)
	}
	var buf bytes.Buffer
	if err = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	p, err := f.manager.newMediaPluginPolicy(ctx, n.msg, "Describe only this image", []deepseek.Image{{Data: buf.Bytes(), MIME: "image/png"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f, n, p, ctx
}

func mediaRequest(p *mediaPluginPolicy) json.RawMessage {
	content := []any{map[string]any{"type": "text", "text": p.Parameters().Prompt}}
	for _, ref := range p.Parameters().Refs {
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": map[string]any{"$media_ref": ref}, "detail": "auto"}})
	}
	body := map[string]any{"model": p.Parameters().Model, "messages": []any{map[string]any{"role": "system", "content": "Describe only provided pixels"}, map[string]any{"role": "user", "content": content}}, "max_tokens": p.Parameters().Tokens, "stream": false}
	raw, _ := json.Marshal(map[string]any{"route": "vision", "body": body, "max_bytes": 4096, "timeout_seconds": 10})
	return raw
}

const mediaReply = `{"choices":[{"message":{"content":"Synthetic pixels","reasoning_content":"hidden"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":40}}}`

func mediaJob(p *mediaPluginPolicy) (pluginhost.Job, pluginhost.Capability) {
	i, _ := p.store.Installed(context.Background(), "media-understanding")
	return pluginhost.Job{Plugin: i.Package.ID, Version: i.Package.Version, PackageDigest: i.Digest, ConfigRevision: "1", Tool: "media.observe", Context: p.Invocation()}, pluginhost.Capability{ID: "media.observe", Permissions: []string{"artifact.read", "model.invoke"}}
}

func TestMediaPluginSourceAndModelAuthority(t *testing.T) {
	for _, remember := range []bool{false, true} {
		t.Run(map[bool]string{true: "remembered", false: "ephemeral"}[remember], func(t *testing.T) {
			f, n, p, ctx := mediaPolicyFixture(t, remember)
			j, cap := mediaJob(p)
			calls := 0
			p.post = func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
				calls++
				if endpoint != "https://provider.example/chat/completions" || headers["Authorization"] != "Bearer synthetic-private-key" || !bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(p.Images()[0].Data))) || bytes.Contains(body, []byte("$media_ref")) {
					t.Fatal("unbound transport")
				}
				return json.RawMessage(mediaReply), nil
			}
			request := mediaRequest(p)
			for _, bad := range []json.RawMessage{bytes.Replace(request, []byte("synthetic-model"), []byte("other-model"), 1), bytes.Replace(request, []byte(p.Parameters().Refs[0].SHA), []byte(strings.Repeat("0", 64)), 1), bytes.Replace(request, []byte("image_0"), []byte("foreign"), 1)} {
				if _, err := p.Service(ctx, j, cap, "wechat/model_request", bad); err == nil {
					t.Fatal("modified request accepted")
				}
			}
			if calls != 0 {
				t.Fatal("invalid request billed")
			}
			if _, err := p.Service(ctx, j, cap, "wechat/model_request", request); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Service(ctx, j, cap, "wechat/model_request", request); err == nil || calls != 1 {
				t.Fatal("duplicate model call", err, calls)
			}
			result := map[string]any{"ok": true, "observation": "Synthetic pixels", "status": "observed", "sources": p.Parameters().Refs, "coverage": "provided_images_only", "untrusted_external_content": true, "audio_observed": false, "requested_model": p.Parameters().Model, "requested_max_tokens": p.Parameters().Tokens, "thinking": false, "reasoning_effort": nil, "provider_model": nil, "usage": map[string]any{"total_tokens": 999999}}
			raw, _ := json.Marshal(result)
			verified, err := p.Validate(ctx, j, cap, raw)
			if err != nil || !bytes.Contains(verified.Private, []byte("Synthetic pixels")) || bytes.Contains(verified.Result, []byte("Synthetic pixels")) {
				t.Fatal("private observation contract", err)
			}
			result["observation"] = "fabricated"
			raw, _ = json.Marshal(result)
			if _, err = p.Validate(ctx, j, cap, raw); err == nil {
				t.Fatal("fabricated observation accepted")
			}
			total := f.manager.billing.Totals()
			if total.Requests != 1 || total.Reported != 1 || total.Input != 100 || total.Output != 10 || total.CacheHit != 40 {
				t.Fatalf("provider billing: %+v", total)
			}
			n.msg.Body = "edited"
			if err = p.Check(ctx, j, cap); err == nil {
				t.Fatal("source edit accepted")
			}
			n.msg.Body = ""
			f.manager.config.Behavior.ReadImages = false
			if err = p.Check(ctx, j, cap); err == nil {
				t.Fatal("revoked read accepted")
			}
			f.manager.config.Behavior.ReadImages = true
			if remember {
				if err = f.manager.memory.Clear(companion.ScopeID("wechat", "test", "filehelper")); err != nil {
					t.Fatal(err)
				}
				if err = p.Check(ctx, j, cap); err == nil {
					t.Fatal("forgotten source accepted")
				}
			}
		})
	}
}

func TestMediaPluginRejectsScopeAndConfigChanges(t *testing.T) {
	_, _, p, ctx := mediaPolicyFixture(t, false)
	j, cap := mediaJob(p)
	foreign := j
	foreign.Context.Thread = "foreign-room"
	if err := p.Check(ctx, foreign, cap); err == nil {
		t.Fatal("cross-conversation request accepted")
	}
	foreign = j
	foreign.Context.Account = "foreign-account"
	if err := p.Check(ctx, foreign, cap); err == nil {
		t.Fatal("cross-account request accepted")
	}
	config, err := p.store.Configuration(ctx, j.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	i, err := p.store.Installed(ctx, j.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	i, err = p.store.SetEnabled(ctx, j.Plugin, i.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.store.SaveConfiguration(ctx, j.Plugin, pluginhost.ConfigUpdate{Revision: config.Revision, Values: map[string]json.RawMessage{"vision_model": json.RawMessage(`"changed-model"`), "vision_endpoint": json.RawMessage(`"https://provider.example/chat/completions"`)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.store.SetEnabled(ctx, j.Plugin, i.Revision, true); err != nil {
		t.Fatal(err)
	}
	if err = p.Check(ctx, j, cap); err == nil {
		t.Fatal("old configuration accepted")
	}
}

func TestMediaPluginDisabledBlocksImageAndVideoFallback(t *testing.T) {
	f, n, p, ctx := mediaPolicyFixture(t, false)
	i, _ := p.store.Installed(ctx, "media-understanding")
	if _, err := p.store.SetEnabled(ctx, i.Package.ID, i.Revision, false); err != nil {
		t.Fatal(err)
	}
	a := mediaUnderstanding{manager: f.manager}
	if _, err := a.observeImages(ctx, n.msg, p.Parameters().Prompt, p.Images()); err == nil || err.Error() != "media_plugin_disabled" {
		t.Fatal(err)
	}
	if _, err := a.observeVideo(ctx, n.msg, nil, videoSample{Seconds: 1}, false); err == nil || err.Error() != "media_plugin_disabled" {
		t.Fatal(err)
	}
}

func TestMediaPluginOwnsAllLegacyTranscriptionCallers(t *testing.T) {
	f, _, p, ctx := mediaPolicyFixture(t, false)
	// No audio/model/network is needed: configuration is checked before the
	// incoming-media reader can obtain private recording bytes.
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.ASREnabled = true
		legacySpeechFixture.ASRBackend = "local_then_bailian"
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	if _, err := (mediaUnderstanding{manager: f.manager}).recognizeVoice(ctx, linuxnative.Message{ID: "synthetic-voice", Type: 34}); err == nil || err.Error() != "media_transcription_configuration_required" {
		t.Fatal("legacy speech configuration bypassed media plugin readiness", err)
	}
	i, err := p.store.Installed(ctx, "media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.store.SetEnabled(ctx, i.Package.ID, i.Revision, false); err != nil {
		t.Fatal(err)
	}
	if _, err = (mediaUnderstanding{manager: f.manager}).recognizeVoice(ctx, linuxnative.Message{ID: "synthetic-voice", Type: 34}); err == nil || err.Error() != "media_plugin_disabled" {
		t.Fatal("disabled media plugin bypassed through legacy transcription", err)
	}
}

func TestMediaPluginFailedTransportNeverRetries(t *testing.T) {
	f, _, p, ctx := mediaPolicyFixture(t, false)
	j, cap := mediaJob(p)
	calls := 0
	p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		return nil, errors.New("synthetic disconnect")
	}
	for range 2 {
		if _, err := p.Service(ctx, j, cap, "wechat/model_request", mediaRequest(p)); err == nil {
			t.Fatal("transport error lost")
		}
	}
	if calls != 1 || f.manager.billing.Totals().Requests != 1 {
		t.Fatal("unknown outcome retried or uncounted", calls)
	}
}

func syntheticWAV() []byte {
	// Enough of a PCM WAV header for the transport gate. The adapter does not
	// pretend this proves audio decoding or provider recognition.
	b := make([]byte, 44)
	copy(b[0:], "RIFF")
	copy(b[8:], "WAVEfmt ")
	b[16], b[20], b[22], b[24], b[25], b[32], b[34] = 16, 1, 1, 0x80, 0x3e, 2, 16
	copy(b[36:], "data")
	return b
}

func transcriptionFixture(t *testing.T, remember bool) (*webFixture, *bindingChannelsNative, *mediaTranscriptionPolicy, context.Context) {
	t.Helper()
	f, n, visual, ctx := mediaPolicyFixture(t, false)
	n.msg.ID, n.msg.Type, n.msg.Body = "synthetic-voice", 34, ""
	if remember {
		f.manager.config.Companion.Permissions.Remember = true
		original := ""
		in := companion.Input{Channel: "wechat", Account: "test", Thread: n.msg.ThreadID, Sender: n.msg.SenderID, TraceID: n.msg.ID, MessageTime: n.msg.CreatedAt, OriginalText: &original, Text: "synthetic voice", Source: &companion.SourceRef{InstanceID: f.manager.memory.SourceInstanceID(), MessageID: n.msg.ID, ServerID: n.msg.ServerID, Account: "test", Thread: n.msg.ThreadID, Sender: n.msg.SenderID, MessageType: 34, OccurredAt: n.msg.CreatedAt, Revision: 1}}
		if err := f.manager.memory.Observe(in, f.manager.config.Companion, time.Now()); err != nil {
			t.Fatal(err)
		}
		ref, ok := f.manager.memory.SourceForInput(in)
		if !ok {
			t.Fatal("missing voice source")
		}
		ctx = companion.WithMediaSource(t.Context(), *ref)
	}
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.ASREnabled = true
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	i, err := visual.store.Installed(ctx, "media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	i, err = visual.store.SetEnabled(ctx, i.Package.ID, i.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := visual.store.Configuration(ctx, i.Package.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = visual.store.SaveConfiguration(ctx, i.Package.ID, pluginhost.ConfigUpdate{Revision: cfg.Revision, Values: map[string]json.RawMessage{
		"vision_endpoint": json.RawMessage(`"https://provider.example/chat/completions"`),
		"vision_model":    json.RawMessage(`"synthetic-model"`),
		"asr_endpoint":    json.RawMessage(`"https://provider.example/api/v1/services/aigc/multimodal-generation/generation"`),
		"asr_model":       json.RawMessage(`"synthetic-asr"`),
	}, Secrets: map[string]string{"asr_key": "synthetic-asr-key"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = visual.store.SetEnabled(ctx, i.Package.ID, i.Revision, true); err != nil {
		t.Fatal(err)
	}
	p, err := f.manager.newMediaTranscriptionPolicy(ctx, n.msg, syntheticWAV())
	if err != nil {
		t.Fatal(err)
	}
	return f, n, p, ctx
}

func transcriptionJob(p *mediaTranscriptionPolicy) (pluginhost.Job, pluginhost.Capability) {
	i, _ := p.store.Installed(context.Background(), "media-understanding")
	return pluginhost.Job{Plugin: i.Package.ID, Version: i.Package.Version, PackageDigest: i.Digest, ConfigRevision: strconv.Itoa(p.ConfigRevision()), Tool: "media.transcribe", Context: p.invocation()}, pluginhost.Capability{ID: "media.transcribe", Permissions: []string{"artifact.read", "model.invoke"}}
}

func transcriptionRequest(p *mediaTranscriptionPolicy) json.RawMessage {
	body := map[string]any{"model": "synthetic-asr", "input": map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": map[string]any{"$media_ref": p.Reference()}}}}}}}, "parameters": map[string]string{"format": "wav", "sample_rate": "16000"}}
	raw, _ := json.Marshal(map[string]any{"route": "transcription", "body": body, "max_bytes": 4096, "timeout_seconds": 15})
	return raw
}

func TestMediaTranscriptionSourceAndProviderAuthority(t *testing.T) {
	for _, remember := range []bool{false, true} {
		t.Run(map[bool]string{true: "remembered", false: "ephemeral"}[remember], func(t *testing.T) {
			f, n, p, ctx := transcriptionFixture(t, remember)
			j, cap := transcriptionJob(p)
			calls := 0
			p.post = func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
				calls++
				if endpoint != "https://provider.example/api/v1/services/aigc/multimodal-generation/generation" || headers["Authorization"] != "Bearer synthetic-asr-key" || bytes.Contains(body, []byte("$media_ref")) || !bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(syntheticWAV()))) {
					t.Fatal("unbound ASR transport")
				}
				return json.RawMessage(`{"output":{"text":"synthetic transcript"}}`), nil
			}
			bad := bytes.Replace(transcriptionRequest(p), []byte("synthetic-asr"), []byte("other"), 1)
			if _, err := p.Service(ctx, j, cap, "wechat/model_request", bad); err == nil || calls != 0 {
				t.Fatal("changed model reached ASR", err, calls)
			}
			if _, err := p.Service(ctx, j, cap, "wechat/model_request", transcriptionRequest(p)); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Service(ctx, j, cap, "wechat/model_request", transcriptionRequest(p)); err == nil || calls != 1 {
				t.Fatal("ASR duplicate", err, calls)
			}
			result := map[string]any{"ok": true, "text": "synthetic transcript", "status": "transcribed", "sources": []mediaAudioReference{p.Reference()}, "coverage": "provided_audio_only", "untrusted_external_content": true}
			raw, _ := json.Marshal(result)
			verified, err := p.Validate(ctx, j, cap, raw)
			if err != nil || !bytes.Contains(verified.Private, []byte("synthetic transcript")) || bytes.Contains(verified.Result, []byte("synthetic transcript")) {
				t.Fatal("private transcription", err)
			}
			result["text"] = "forged"
			raw, _ = json.Marshal(result)
			if _, err = p.Validate(ctx, j, cap, raw); err == nil {
				t.Fatal("forged ASR accepted")
			}
			n.msg.Body = "edited"
			if err = p.Check(ctx, j, cap); err == nil {
				t.Fatal("edited source accepted")
			}
			n.msg.Body = ""
			{
				legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
				legacySpeechFixture.ASREnabled = false
				writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
			}
			if err = p.Check(ctx, j, cap); err != nil {
				t.Fatal("installed ASR incorrectly depended on legacy switch", err)
			}
			f.manager.config.Enabled = false
			if err = p.Check(ctx, j, cap); err == nil {
				t.Fatal("agent authorization revocation accepted")
			}
		})
	}
}

func TestMediaTranscriptionNoWordsAndUncertainTransport(t *testing.T) {
	f, _, p, ctx := transcriptionFixture(t, false)
	j, cap := transcriptionJob(p)
	calls := 0
	p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"code":"ASR_RESPONSE_HAVE_NO_WORDS"}`), nil
	}
	if _, err := p.Service(ctx, j, cap, "wechat/model_request", transcriptionRequest(p)); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"ok": true, "text": "", "status": "no_recognizable_speech", "sources": []mediaAudioReference{p.Reference()}, "coverage": "provided_audio_only", "untrusted_external_content": true})
	verified, err := p.Validate(ctx, j, cap, raw)
	if err != nil || !bytes.Contains(verified.Private, []byte("no_recognizable_speech")) {
		t.Fatal(err)
	}
	if f.manager.billing.Totals().Requests != 1 {
		t.Fatal("ASR attempt not billed")
	}
	_ = calls
}

func TestMediaTranscriptionKnownProviderFailureIsTerminal(t *testing.T) {
	_, _, p, ctx := transcriptionFixture(t, false)
	j, cap := transcriptionJob(p)
	failure := json.RawMessage(`{"ok":false,"error":"asr_provider_failed"}`)
	if _, err := p.Validate(ctx, j, cap, failure); err == nil {
		t.Fatal("unknown transport was treated as terminal")
	}
	p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		return json.RawMessage(`{"code":"provider_rejected"}`), nil
	}
	if _, err := p.Service(ctx, j, cap, "wechat/model_request", transcriptionRequest(p)); err != nil {
		t.Fatal(err)
	}
	verified, err := p.Validate(ctx, j, cap, failure)
	if err != nil || !verified.Failed {
		t.Fatal("known provider failure was not terminal", err)
	}
}

func TestMediaTranscriptionUsageWithoutProviderSchemaStaysUnknown(t *testing.T) {
	f, _, p, ctx := transcriptionFixture(t, false)
	j, cap := transcriptionJob(p)
	p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		return json.RawMessage(`{"output":{"text":"synthetic"},"usage":{"input_tokens":7,"output_tokens":3}}`), nil
	}
	if _, err := p.Service(ctx, j, cap, "wechat/model_request", transcriptionRequest(p)); err != nil {
		t.Fatal(err)
	}
	total := f.manager.billing.Totals()
	if total.Requests != 1 || total.Reported != 0 || total.Input != 0 || total.Output != 0 {
		t.Fatalf("unrecognized ASR usage was misreported: %+v", total)
	}
}

func TestMediaTranscriptionVideoUsesVideoReadPermission(t *testing.T) {
	f, n, p, ctx := transcriptionFixture(t, false)
	n.msg.ID, n.msg.Type = "synthetic-video-audio", 43
	p, err := f.manager.newMediaTranscriptionPolicy(ctx, n.msg, syntheticWAV())
	if err != nil {
		t.Fatal(err)
	}
	j, cap := transcriptionJob(p)
	if err = p.Check(ctx, j, cap); err != nil {
		t.Fatal(err)
	}
	f.manager.config.Behavior.ReadVideos = false
	if err = p.Check(ctx, j, cap); err == nil {
		t.Fatal("video ASR accepted after video permission revoked")
	}
	f.manager.config.Behavior.ReadVideos = true
	if err = p.Check(ctx, j, cap); err != nil {
		t.Fatal(err)
	}
}

func TestMediaTranscriptionProviderFailureKeepsVisualEvidence(t *testing.T) {
	for _, err := range []error{errors.New("media_transcription_provider_failed"), &mediaspec.Failure{Code: "ASR_RESPONSE_HAVE_NO_WORDS"}} {
		if mediaTranscriptionHardFailure(err) {
			t.Fatal("ordinary ASR result discarded visual evidence", err)
		}
	}
	for _, err := range []error{errors.New("media_plugin_configuration_changed"), errors.New("share_source_changed")} {
		if !mediaTranscriptionHardFailure(err) {
			t.Fatal("source or authority failure retained stale visual evidence", err)
		}
	}
}

func TestLiveMediaTranscriptionSourceAdapterRecovery(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in real isolation, synthetic ASR provider only")
	}
	python, _ = filepath.Abs(python)
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	f, n, p, ctx := transcriptionFixture(t, true)
	calls := 0
	p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"output":{"text":"synthetic transcript"}}`), nil
	}
	start := func() *pluginruntime.Controller {
		c, err := pluginruntime.NewController(p.store, pluginruntime.InstalledExecutor(p.store, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
		if err != nil {
			t.Fatal(err)
		}
		f.manager.packageRuntime, f.manager.packageRuntimeReady = c, true
		return c
	}
	c := start()
	text, err := p.transcribe(ctx)
	c.Close()
	if err != nil || text != "synthetic transcript" || calls != 1 {
		t.Fatal("ASR execution", text, err, calls)
	}
	c = start()
	defer c.Close()
	fresh, err := f.manager.newMediaTranscriptionPolicy(ctx, n.msg, syntheticWAV())
	if err != nil {
		t.Fatal(err)
	}
	fresh.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		t.Fatal("ASR recovery resubmitted provider")
		return nil, nil
	}
	text, err = fresh.transcribe(ctx)
	if err != nil || text != "synthetic transcript" || f.manager.billing.Totals().Requests != 1 {
		t.Fatal("ASR recovery", text, err)
	}
}

func TestLiveMediaSourceAdapterRecovery(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in real isolation, synthetic provider only")
	}
	python, _ = filepath.Abs(python)
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	f, n, p, ctx := mediaPolicyFixture(t, true)
	calls := 0
	p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		return json.RawMessage(mediaReply), nil
	}
	start := func() *pluginruntime.Controller {
		c, err := pluginruntime.NewController(p.store, pluginruntime.InstalledExecutor(p.store, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
		if err != nil {
			t.Fatal(err)
		}
		f.manager.packageRuntime, f.manager.packageRuntimeReady = c, true
		return c
	}
	c := start()
	text, err := p.Observe(ctx)
	c.Close()
	if err != nil || text != "Synthetic pixels" || calls != 1 {
		t.Fatal("source adapter execution", text, err, calls)
	}
	c = start()
	defer c.Close()
	fresh, err := f.manager.newMediaPluginPolicy(ctx, n.msg, p.Parameters().Prompt, p.Images(), 0)
	if err != nil {
		t.Fatal(err)
	}
	fresh.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		t.Fatal("recovery resubmitted provider")
		return nil, nil
	}
	text, err = fresh.Observe(ctx)
	if err != nil || text != "Synthetic pixels" || f.manager.billing.Totals().Requests != 1 {
		t.Fatal("recovery", text, err)
	}
}
