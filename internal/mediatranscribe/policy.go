// Package mediatranscribe contains the host-side policy for an audio
// transcription capability.  It deliberately has no dependency on the admin
// UI or on a concrete connected application: callers supply the immutable
// origin proof, invocation, runtime and accounting hooks.
package mediatranscribe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"wechat-bot/internal/billing"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/strictjson"
)

const PluginID = "media-understanding"
const CapabilityID = "media.transcribe"

type AudioReference struct {
	Ref     string `json:"media_ref"`
	SHA     string `json:"sha256"`
	Version int    `json:"version"`
	MIME    string `json:"mime"`
}

type Result struct {
	Text    string           `json:"text"`
	Status  string           `json:"status"`
	Sources []AudioReference `json:"sources"`
}

type LocalProfile func(context.Context, pluginhost.Package, pluginhost.Grant) (localasr.Profile, bool)
type LocalRunner func(context.Context, pluginhost.Package, pluginhost.Grant, localasr.Profile, []byte, int) (string, string, error)
type Runtime func(context.Context) (*pluginruntime.Controller, bool, error)
type RecordUsage func(endpoint string, headers map[string]string, reply json.RawMessage, model string) error

// Dependencies are intentionally callbacks rather than an application
// manager. This makes the policy usable by WeChat and future connected apps,
// while the host remains responsible for credentials and accounting.
type Dependencies struct {
	Store        *pluginhost.Store
	Invocation   func() pluginhost.Invocation
	OriginCheck  func(context.Context, pluginhost.Job, pluginhost.Capability) error
	Runtime      Runtime
	Post         pluginruntime.RouteTransport
	RecordUsage  RecordUsage
	LocalProfile LocalProfile
	RunLocal     LocalRunner
}

type Policy struct {
	deps                         Dependencies
	mu                           sync.Mutex
	wav                          []byte
	ref                          AudioReference
	model, backend               string
	localProfileID, localVersion string
	localTimeout                 int
	localState, localText        string
	configRevision               int
	requested                    bool
	providerReply                json.RawMessage
	arguments                    json.RawMessage
	completedJob                 pluginhost.Job
}

func (p *Policy) ConfigRevision() int          { return p.configRevision }
func (p *Policy) CompletedJob() pluginhost.Job { return p.completedJob }
func (p *Policy) Backend() string              { return p.backend }
func (p *Policy) WAV() []byte                  { return append([]byte(nil), p.wav...) }
func (p *Policy) LocalProfile() (profileID, resourceVersion string, timeoutSeconds int) {
	return p.localProfileID, p.localVersion, p.localTimeout
}
func (p *Policy) Requested() bool { return p.requested }

func New(ctx context.Context, deps Dependencies, sourceRevision int, wav []byte) (*Policy, error) {
	if deps.Store == nil || deps.Invocation == nil || deps.OriginCheck == nil || sourceRevision < 1 || len(wav) == 0 || len(wav) > 10<<20 || !bytes.HasPrefix(wav, []byte("RIFF")) || len(wav) < 12 || string(wav[8:12]) != "WAVE" {
		return nil, errors.New("media_audio_format_invalid")
	}
	cfg, err := deps.Store.Configuration(ctx, PluginID)
	if err != nil {
		return nil, err
	}
	var model, backend string
	_ = json.Unmarshal(cfg.Values["asr_backend"], &backend)
	if backend == "" {
		backend = "remote"
	}
	if backend != "remote" && backend != "local" {
		return nil, errors.New("media_transcription_configuration_invalid")
	}
	if backend == "remote" && (json.Unmarshal(cfg.Values["asr_model"], &model) != nil || model == "") {
		return nil, errors.New("media_transcription_configuration_invalid")
	}
	copyWAV := append([]byte(nil), wav...)
	sum := sha256.Sum256(copyWAV)
	p := &Policy{deps: deps, wav: copyWAV, ref: AudioReference{Ref: "audio_0", SHA: hex.EncodeToString(sum[:]), Version: sourceRevision, MIME: "audio/wav"}, model: model, backend: backend, configRevision: cfg.Revision}
	if backend == "local" {
		if deps.LocalProfile == nil || deps.RunLocal == nil {
			return nil, errors.New("local_asr_profile_unavailable")
		}
		i, err := deps.Store.Installed(ctx, PluginID)
		if err != nil {
			return nil, errors.New("local_asr_plugin_unavailable")
		}
		inv := deps.Invocation()
		profile, ok := deps.LocalProfile(ctx, i.Package, pluginhost.Grant{Account: inv.Account, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions})
		if !ok {
			return nil, errors.New("local_asr_profile_unavailable")
		}
		p.localProfileID, p.localVersion, p.localTimeout = profile.Metadata().ProfileID, profile.Metadata().ResourceVersion, profile.TimeoutSeconds()
		if p.localTimeout < 1 {
			return nil, errors.New("local_asr_profile_unavailable")
		}
	}
	timeout := 180
	if backend == "local" {
		timeout = p.localTimeout
	}
	p.arguments, _ = json.Marshal(map[string]any{"audio": p.ref, "timeout_seconds": timeout})
	return p, nil
}

func (p *Policy) Arguments() json.RawMessage        { return append(json.RawMessage(nil), p.arguments...) }
func (p *Policy) Reference() AudioReference         { return p.ref }
func (p *Policy) invocation() pluginhost.Invocation { return p.deps.Invocation() }

func (p *Policy) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	inv := p.invocation()
	if p.deps.Store == nil || p.deps.OriginCheck == nil || p.deps.Runtime == nil || inv.Account != p.deps.Store.Account() || inv.Thread == "" || inv.Requester == "" || inv.Purpose == "" || inv.RequestKey == "" || inv.Authorization == "" || j.Plugin != PluginID || j.Tool != CapabilityID || cap.Async {
		return errors.New("media_transcription_authorization_changed")
	}
	want, _ := json.Marshal(inv)
	got, _ := json.Marshal(j.Context)
	if !bytes.Equal(want, got) {
		return errors.New("media_transcription_invocation_changed")
	}
	if err := p.deps.OriginCheck(ctx, j, cap); err != nil {
		return err
	}
	cfg, err := p.deps.Store.Configuration(ctx, j.Plugin)
	if err != nil || cfg.Revision != p.configRevision {
		return errors.New("media_plugin_configuration_changed")
	}
	i, err := p.deps.Store.Installed(ctx, j.Plugin)
	if err != nil || !i.Enabled || i.Removed || i.Digest == "" || (j.PackageDigest != "" && (i.Digest != j.PackageDigest || i.Package.Version != j.Version || j.ConfigRevision != strconv.Itoa(p.configRevision))) {
		return errors.New("media_plugin_version_changed")
	}
	required := cap.Permissions
	if len(required) == 0 {
		caps, capErr := p.deps.Store.Capabilities(ctx, j.Plugin)
		if capErr != nil {
			return errors.New("media_plugin_permissions_changed")
		}
		for _, available := range caps {
			if available.ID == j.Tool && available.Discoverable && available.Implementation == "ready" && !available.Async {
				required = available.Permissions
				break
			}
		}
	}
	if len(required) == 0 || pluginhost.Authorize(i.Package, pluginhost.Grant{Account: inv.Account, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, j.Context, required) != nil {
		return errors.New("media_plugin_permissions_changed")
	}
	if p.backend == "local" {
		if p.deps.LocalProfile == nil {
			return errors.New("media_local_transcription_profile_changed")
		}
		profile, ok := p.deps.LocalProfile(ctx, i.Package, pluginhost.Grant{Account: inv.Account, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions})
		if !ok || profile.Metadata().ProfileID != p.localProfileID || profile.Metadata().ResourceVersion != p.localVersion {
			return errors.New("media_local_transcription_profile_changed")
		}
	}
	if j.ID != "" {
		current, err := p.deps.Store.Job(ctx, j.ID)
		if err != nil || current.CancelRequested || current.Revision != j.Revision {
			return errors.New("media_plugin_task_changed")
		}
	}
	return nil
}

func (p *Policy) materialize(_ context.Context, route string, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		Model string `json:"model"`
		Input struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type  string `json:"type"`
					Audio *struct {
						Data struct {
							Ref AudioReference `json:"$media_ref"`
						} `json:"data"`
					} `json:"input_audio"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"input"`
		Parameters struct {
			Format string `json:"format"`
			Rate   string `json:"sample_rate"`
		} `json:"parameters"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF || route != "transcription" || body.Model != p.model || len(body.Input.Messages) != 1 || body.Input.Messages[0].Role != "user" || len(body.Input.Messages[0].Content) != 1 || body.Input.Messages[0].Content[0].Type != "input_audio" || body.Input.Messages[0].Content[0].Audio == nil || body.Input.Messages[0].Content[0].Audio.Data.Ref != p.ref || body.Parameters.Format != "wav" || body.Parameters.Rate != "16000" {
		return nil, errors.New("media_transcription_request_changed")
	}
	content := map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(p.wav)}}
	return json.Marshal(map[string]any{"model": p.model, "input": map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{content}}}}, "parameters": map[string]string{"format": "wav", "sample_rate": "16000"}})
}

func (p *Policy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if method == "wechat/local_transcription" {
		return p.localTranscription(ctx, j, cap, raw)
	}
	if method != "wechat/model_request" || p.backend != "remote" || p.deps.Post == nil {
		return nil, errors.New("media_host_service_unavailable")
	}
	if err := p.Check(ctx, j, cap); err != nil {
		return nil, err
	}
	if p.requested {
		return nil, errors.New("media_model_already_requested")
	}
	return pluginruntime.ProxyModelRoute(ctx, p.deps.Store, j, cap, raw, p.materialize, func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
		if err := p.Check(ctx, j, cap); err != nil {
			return nil, err
		}
		p.requested = true
		reply, err := p.deps.Post(ctx, endpoint, headers, body, limit, timeout)
		if p.deps.RecordUsage == nil || p.deps.RecordUsage(endpoint, headers, reply, p.model) != nil {
			return nil, errors.New("media_usage_record_failed")
		}
		if err != nil {
			return nil, err
		}
		p.providerReply = append(json.RawMessage(nil), reply...)
		return reply, nil
	})
}

func (p *Policy) localTranscription(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (json.RawMessage, error) {
	if p.backend != "local" || p.deps.RunLocal == nil || p.deps.LocalProfile == nil {
		return nil, errors.New("media_local_transcription_unavailable")
	}
	if err := p.Check(ctx, j, cap); err != nil {
		return nil, errors.New("media_local_transcription_unavailable")
	}
	var request struct {
		ProfileID       string         `json:"profile_id"`
		ResourceVersion string         `json:"resource_version"`
		Audio           AudioReference `json:"audio"`
		TimeoutSeconds  int            `json:"timeout_seconds"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&request) != nil || d.Decode(new(any)) != io.EOF || request.ProfileID != p.localProfileID || request.ResourceVersion != p.localVersion || request.Audio != p.ref || request.TimeoutSeconds < 1 || request.TimeoutSeconds > p.localTimeout {
		return nil, errors.New("media_local_transcription_request_changed")
	}
	if p.requested {
		return nil, errors.New("media_local_transcription_already_requested")
	}
	p.requested = true
	i, err := p.deps.Store.Installed(ctx, j.Plugin)
	if err != nil {
		return nil, errors.New("media_local_transcription_unavailable")
	}
	inv := p.invocation()
	profile, ok := p.deps.LocalProfile(ctx, i.Package, pluginhost.Grant{Account: inv.Account, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions})
	if !ok || profile.Metadata().ProfileID != p.localProfileID || profile.Metadata().ResourceVersion != p.localVersion {
		return nil, errors.New("media_local_transcription_profile_changed")
	}
	state, text, err := p.deps.RunLocal(ctx, i.Package, pluginhost.Grant{Account: inv.Account, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, profile, p.wav, request.TimeoutSeconds)
	if err != nil {
		return nil, err
	}
	p.localState, p.localText = state, text
	return json.Marshal(map[string]string{"state": state, "text": text})
}

func strictJSON(raw json.RawMessage) error {
	_, err := strictjson.Decode(raw)
	return err
}

func (p *Policy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Check(ctx, j, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if err := strictJSON(raw); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var failure struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	failureDecoded := d.Decode(&failure) == nil && d.Decode(new(any)) == io.EOF
	if failureDecoded && failure.OK != nil && !*failure.OK && p.backend == "remote" && failure.Error == "asr_provider_failed" && p.requested && len(p.providerReply) != 0 {
		var providerCode struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(p.providerReply, &providerCode) == nil && providerCode.Code != "" && providerCode.Code != "ASR_RESPONSE_HAVE_NO_WORDS" {
			return pluginruntime.VerifiedResult{Failed: true, Result: raw}, nil
		}
	}
	var result struct {
		OK bool `json:"ok"`
		Result
		Coverage  string `json:"coverage"`
		Untrusted bool   `json:"untrusted_external_content"`
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil || !result.OK || !result.Untrusted || result.Coverage != "provided_audio_only" || !p.requested || len(result.Sources) != 1 || result.Sources[0] != p.ref {
		return pluginruntime.VerifiedResult{}, errors.New("media_transcription_unverified")
	}
	if p.backend == "local" {
		if result.Status != p.localState || result.Text != p.localText || (result.Status != "transcribed" && result.Status != "no_recognizable_speech") || result.Status == "transcribed" && result.Text == "" || result.Status == "no_recognizable_speech" && result.Text != "" {
			return pluginruntime.VerifiedResult{}, errors.New("media_local_transcription_changed")
		}
		private, _ := json.Marshal(result.Result)
		public, _ := json.Marshal(map[string]any{"ok": true, "status": result.Status, "coverage": result.Coverage, "sources": []AudioReference{p.ref}, "usage_source": "local_runtime"})
		return pluginruntime.VerifiedResult{Result: public, Private: private}, nil
	}
	if p.backend != "remote" || len(p.providerReply) == 0 {
		return pluginruntime.VerifiedResult{}, errors.New("media_transcription_unverified")
	}
	var provider struct {
		Code   string `json:"code"`
		Output struct {
			Text   string `json:"text"`
			Nested struct {
				Text string `json:"text"`
			} `json:"output"`
		} `json:"output"`
	}
	if err := strictJSON(p.providerReply); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if json.Unmarshal(p.providerReply, &provider) != nil {
		return pluginruntime.VerifiedResult{}, errors.New("media_provider_response_invalid")
	}
	text := strings.TrimSpace(provider.Output.Text)
	if text == "" {
		text = strings.TrimSpace(provider.Output.Nested.Text)
	}
	if provider.Code == "ASR_RESPONSE_HAVE_NO_WORDS" {
		if result.Status != "no_recognizable_speech" || result.Text != "" {
			return pluginruntime.VerifiedResult{}, errors.New("media_transcription_changed")
		}
	} else if provider.Code != "" || result.Status != "transcribed" || result.Text == "" || result.Text != text {
		return pluginruntime.VerifiedResult{}, errors.New("media_transcription_changed")
	}
	private, _ := json.Marshal(result.Result)
	public, _ := json.Marshal(map[string]any{"ok": true, "status": result.Status, "coverage": result.Coverage, "sources": []AudioReference{p.ref}, "usage_source": "host_transport"})
	return pluginruntime.VerifiedResult{Result: public, Private: private}, nil
}

func (p *Policy) Transcribe(ctx context.Context) (string, error) {
	j := pluginhost.Job{Plugin: PluginID, Tool: CapabilityID, Context: p.invocation()}
	if err := p.Check(ctx, j, pluginhost.Capability{}); err != nil {
		return "", err
	}
	controller, ready, err := p.deps.Runtime(ctx)
	if err != nil || !ready || controller == nil {
		return "", errors.New("media_plugin_runtime_unavailable")
	}
	installed, err := p.deps.Store.Installed(ctx, j.Plugin)
	if err != nil {
		return "", err
	}
	if err = controller.EnsureStarted(ctx, j.Plugin, installed.Revision); err != nil {
		return "", err
	}
	call, err := controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: j.Plugin, Capability: j.Tool, Context: j.Context, Arguments: p.arguments}, p)
	if err != nil {
		return "", errors.New("media_plugin_transcription_needs_review")
	}
	if call.Job.State == "failed" {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(call.Job.Result, &failure) == nil && failure.Error == "asr_provider_failed" {
			return "", errors.New("media_transcription_provider_failed")
		}
		if p.backend == "local" {
			return "", errors.New("media_transcription_local_failed")
		}
	}
	if call.Job.State != "completed" {
		return "", errors.New("media_plugin_transcription_needs_review")
	}
	p.completedJob = call.Job
	raw, err := p.deps.Store.PrivateJobResult(ctx, call.Job, func(ctx context.Context, j pluginhost.Job) error { return p.Check(ctx, j, pluginhost.Capability{}) })
	if err != nil {
		return "", errors.New("media_plugin_result_unavailable")
	}
	var r Result
	if json.Unmarshal(raw, &r) != nil {
		return "", errors.New("media_plugin_result_invalid")
	}
	if err = p.CheckCompleted(ctx); err != nil {
		return "", err
	}
	if r.Status == "no_recognizable_speech" {
		return "", &mediaspec.Failure{Code: "ASR_RESPONSE_HAVE_NO_WORDS"}
	}
	if r.Status != "transcribed" || r.Text == "" {
		return "", errors.New("media_plugin_result_invalid")
	}
	return r.Text, nil
}

// CheckCompleted revalidates the last result before use.
func (p *Policy) CheckCompleted(ctx context.Context) error {
	if p.completedJob.ID == "" || p.completedJob.State != "completed" {
		return errors.New("media_plugin_result_unavailable")
	}
	return p.Check(ctx, p.completedJob, pluginhost.Capability{})
}

// UsageRecorder keeps the provider-specific accounting mapping at the shared
// transcription boundary. Connected applications only inject their own
// account-scoped billing store; they do not need an admin Config value.
func UsageRecorder(store *billing.Store) RecordUsage {
	return func(endpoint string, headers map[string]string, reply json.RawMessage, model string) error {
		if store == nil {
			return errors.New("media_usage_record_failed")
		}
		usage := deepseek.ReportedUsage(reply, time.Now().UTC())
		usage.Model = model
		usage.Purpose = "plugin_media_transcription"
		return store.Record(billing.ModelConfig{
			Provider: "bailian",
			BaseURL:  strings.TrimSuffix(endpoint, "/api/v1/services/aigc/multimodal-generation/generation"),
			Model:    model,
			APIKey:   strings.TrimPrefix(headers["Authorization"], "Bearer "),
		}, usage)
	}
}
