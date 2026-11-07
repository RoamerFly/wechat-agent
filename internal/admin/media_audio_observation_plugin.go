package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

// Audio observations are model opinions about the supplied samples. They do
// not certify a singer's identity, the complete recording, or human listening.
type mediaAudioObservation struct {
	Status        string                `json:"status"`
	Text          string                `json:"observation"`
	Model         string                `json:"requested_model"`
	ProviderModel string                `json:"provider_model,omitempty"`
	Sources       []mediaAudioReference `json:"sources"`
	VoiceEvidence string                `json:"voice_identity_evidence"`
}

type mediaAudioObservationPolicy struct {
	manager                                *Manager
	store                                  *pluginhost.Store
	invocationValue                        pluginhost.Invocation
	originCheck                            func(context.Context) error
	wav                                    []byte
	ref                                    mediaAudioReference
	model, prompt, digest, version         string
	tokens, timeoutSeconds, configRevision int
	arguments                              json.RawMessage
	post                                   pluginruntime.RouteTransport
	mu                                     sync.Mutex
	requested                              bool
	providerReply                          json.RawMessage
	completedJob                           pluginhost.Job
}

func audioObservationJSON(raw json.RawMessage, target any) error {
	if _, err := decodeShareJSON(raw); err != nil {
		return err
	}
	return strictServicePluginJSON(raw, target)
}

func (m *Manager) mediaAudioObservationConfigured(ctx context.Context) bool {
	s, err := m.packageStore()
	if err != nil {
		return false
	}
	i, err := s.Installed(ctx, "media-understanding")
	inv := pluginhost.Invocation{Account: s.Account(), Thread: "host", Requester: "administrator", Purpose: "inspect audio observation readiness", RequestKey: "host-audio-observation-readiness"}
	if err != nil || !i.Enabled || i.Removed || pluginhost.Authorize(i.Package, pluginhost.Grant{Account: s.Account(), Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, inv, []string{"artifact.read", "model.invoke"}) != nil {
		return false
	}
	ready := false
	for _, cap := range mustCapabilities(ctx, s, "media-understanding") {
		if cap.ID == "media.listen" && cap.Discoverable && cap.Implementation == "ready" && !cap.Async {
			ready = true
			break
		}
	}
	if !ready {
		return false
	}
	cfg, err := s.ConfigurationForExecution(ctx, "media-understanding")
	if err != nil {
		return false
	}
	var endpoint, model, key string
	return json.Unmarshal(cfg.Values["audio_review_endpoint"], &endpoint) == nil && endpoint != "" && json.Unmarshal(cfg.Values["audio_review_model"], &model) == nil && strings.TrimSpace(model) != "" && utf8.RuneCountInString(model) <= 128 && !strings.ContainsAny(model, "\r\n\x00") && json.Unmarshal(cfg.Values["audio_review_key"], &key) == nil && key != ""
}

// The origin is provided by a trusted host adapter, never by plugin arguments.
// It may bind an incoming recording, selected asset, or generated-song job;
// each origin must recheck its own requester/source/cancellation authority.
func (m *Manager) newBoundMediaAudioObservationPolicy(ctx context.Context, wav []byte, inv pluginhost.Invocation, revision int, check func(context.Context) error, prompt string, maxTokens, timeoutSeconds int) (*mediaAudioObservationPolicy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || m.native == nil || check == nil || revision < 1 || inv.Account == "" || inv.Account != m.native.Account() || inv.Thread == "" || inv.Requester == "" || inv.Purpose == "" || inv.RequestKey == "" || inv.Authorization == "" || len(inv.SourceVersions) == 0 {
		return nil, errors.New("media_audio_observation_origin_required")
	}
	for id, version := range inv.SourceVersions {
		if id == "" || version < 1 {
			return nil, errors.New("media_audio_observation_origin_required")
		}
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	if len(wav) > 3<<20 {
		return nil, errors.New("media_audio_observation_wav_exceeds_3mib")
	}
	if len(wav) < 44 || !bytes.HasPrefix(wav, []byte("RIFF")) || string(wav[8:12]) != "WAVE" {
		return nil, errors.New("media_audio_observation_wav_invalid")
	}
	if strings.TrimSpace(prompt) == "" || utf8.RuneCountInString(prompt) > 8192 || !utf8.ValidString(prompt) || strings.ContainsRune(prompt, 0) || maxTokens < 0 || timeoutSeconds < 0 || int64(timeoutSeconds) > int64((1<<63-1)/time.Second) {
		return nil, errors.New("media_audio_observation_arguments_invalid")
	}
	if !m.mediaAudioObservationConfigured(ctx) {
		return nil, errors.New("media_audio_observation_configuration_invalid")
	}
	store, err := m.packageStore()
	if err != nil {
		return nil, err
	}
	cfg, err := store.Configuration(ctx, "media-understanding")
	if err != nil {
		return nil, err
	}
	i, err := store.Installed(ctx, "media-understanding")
	if err != nil {
		return nil, err
	}
	var model string
	if json.Unmarshal(cfg.Values["audio_review_model"], &model) != nil || strings.TrimSpace(model) == "" {
		return nil, errors.New("media_audio_observation_configuration_invalid")
	}
	if maxTokens == 0 {
		if raw := cfg.Values["max_tokens"]; raw != nil && json.Unmarshal(raw, &maxTokens) != nil {
			return nil, errors.New("media_audio_observation_configuration_invalid")
		}
		if maxTokens == 0 {
			maxTokens = 768
		}
		if maxTokens < 1 {
			return nil, errors.New("media_audio_observation_configuration_invalid")
		}
	}
	if timeoutSeconds == 0 {
		timeoutSeconds = 90
	}
	// Freeze caller-owned source maps and WAV bytes before creating an identity.
	bound, _ := json.Marshal(inv)
	var frozen pluginhost.Invocation
	_ = json.Unmarshal(bound, &frozen)
	inv = frozen
	copyWAV := append([]byte(nil), wav...)
	sum := sha256.Sum256(copyWAV)
	ref := mediaAudioReference{Ref: "audio_0", SHA: hex.EncodeToString(sum[:]), Version: revision, MIME: "audio/wav"}
	args, _ := json.Marshal(map[string]any{"audio": ref, "prompt": prompt, "max_tokens": maxTokens, "timeout_seconds": timeoutSeconds})
	inv.RequestKey = "bound-media-audio-observation-" + agentID(inv.RequestKey, string(args), i.Digest, strconv.Itoa(cfg.Revision))
	p := &mediaAudioObservationPolicy{manager: m, store: store, invocationValue: inv, originCheck: check, wav: copyWAV, ref: ref, model: model, prompt: prompt, digest: i.Digest, version: i.Package.Version, tokens: maxTokens, timeoutSeconds: timeoutSeconds, configRevision: cfg.Revision, arguments: args, post: mcptools.PostPluginModelStream}
	if err = p.Check(ctx, pluginhost.Job{Plugin: "media-understanding", Tool: "media.listen", Context: inv}, pluginhost.Capability{}); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *mediaAudioObservationPolicy) invocation() pluginhost.Invocation {
	copy := p.invocationValue
	copy.SourceVersions = make(map[string]int, len(p.invocationValue.SourceVersions))
	for id, revision := range p.invocationValue.SourceVersions {
		copy.SourceVersions[id] = revision
	}
	return copy
}

func (p *mediaAudioObservationPolicy) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.manager == nil || p.manager.native == nil || p.store == nil || p.originCheck == nil || p.manager.native.Account() != p.invocationValue.Account || j.Plugin != "media-understanding" || j.Tool != "media.listen" || cap.Async || cap.ID != "" && cap.ID != j.Tool {
		return errors.New("media_audio_observation_authorization_changed")
	}
	want, _ := json.Marshal(p.invocationValue)
	got, _ := json.Marshal(j.Context)
	if !bytes.Equal(want, got) {
		return errors.New("media_audio_observation_invocation_changed")
	}
	if err := p.originCheck(ctx); err != nil {
		return err
	}
	cfg, err := p.store.Configuration(ctx, j.Plugin)
	if err != nil || cfg.Revision != p.configRevision {
		return errors.New("media_audio_observation_configuration_changed")
	}
	i, err := p.store.Installed(ctx, j.Plugin)
	if err != nil || !i.Enabled || i.Removed || i.Digest != p.digest || i.Package.Version != p.version || j.PackageDigest != "" && (j.PackageDigest != p.digest || j.Version != p.version || j.ConfigRevision != strconv.Itoa(p.configRevision)) {
		return errors.New("media_audio_observation_package_changed")
	}
	if pluginhost.Authorize(i.Package, pluginhost.Grant{Account: p.invocationValue.Account, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, j.Context, []string{"artifact.read", "model.invoke"}) != nil {
		return errors.New("media_audio_observation_permissions_changed")
	}
	if j.ID != "" {
		current, e := p.store.Job(ctx, j.ID)
		if e != nil || current.CancelRequested || current.Revision != j.Revision {
			return errors.New("media_audio_observation_task_changed")
		}
	}
	return nil
}

func (p *mediaAudioObservationPolicy) requestBody(materialized bool) map[string]any {
	var data any = map[string]any{"$media_ref": p.ref}
	if materialized {
		data = "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(p.wav)
	}
	return map[string]any{"model": p.model, "stream": true, "enable_thinking": false, "max_tokens": p.tokens, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": p.prompt}, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": data, "format": "wav"}}}}}}
}

func (p *mediaAudioObservationPolicy) materialize(_ context.Context, route string, raw json.RawMessage) (json.RawMessage, error) {
	if route != "audio-review" || !sameShareBody(raw, p.requestBody(false)) {
		return nil, errors.New("media_audio_observation_request_changed")
	}
	return json.Marshal(p.requestBody(true))
}

func (p *mediaAudioObservationPolicy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if method != "wechat/model_request" || p.requested {
		return nil, errors.New("media_audio_observation_service_unavailable")
	}
	if err := p.Check(ctx, j, cap); err != nil {
		return nil, err
	}
	var request struct {
		Route    string          `json:"route"`
		Body     json.RawMessage `json:"body"`
		MaxBytes int64           `json:"max_bytes"`
		Timeout  int             `json:"timeout_seconds"`
	}
	if audioObservationJSON(raw, &request) != nil || request.Route != "audio-review" || request.MaxBytes < 1 || request.MaxBytes > 2<<20 || request.Timeout != p.timeoutSeconds {
		return nil, errors.New("media_audio_observation_request_changed")
	}
	return pluginruntime.ProxyModelRoute(ctx, p.store, j, cap, raw, p.materialize, func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
		if err := p.Check(ctx, j, cap); err != nil {
			return nil, err
		}
		p.requested = true
		reply, err := p.post(ctx, endpoint, headers, body, limit, timeout)
		usage := deepseek.ReportedUsage(reply, time.Now().UTC())
		usage.Model, usage.Purpose = p.model, "plugin_media_audio_observation"
		config := Config{Provider: "openai-compatible", BaseURL: strings.TrimSuffix(endpoint, "/chat/completions"), Model: p.model, APIKey: strings.TrimPrefix(headers["Authorization"], "Bearer ")}
		if p.manager.billing == nil || p.manager.billing.Record(modelForBilling(config), usage) != nil {
			return nil, errors.New("media_audio_observation_usage_record_failed")
		}
		if err != nil {
			return nil, err
		}
		if err = p.Check(ctx, j, cap); err != nil {
			return nil, err
		}
		p.providerReply = append(json.RawMessage(nil), reply...)
		return reply, nil
	})
}

func (p *mediaAudioObservationPolicy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Check(ctx, j, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var result struct {
		OK        bool                  `json:"ok"`
		Text      string                `json:"observation"`
		Status    string                `json:"status"`
		Sources   []mediaAudioReference `json:"sources"`
		Coverage  string                `json:"coverage"`
		Untrusted bool                  `json:"untrusted_external_content"`
	}
	if audioObservationJSON(raw, &result) != nil || !result.OK || !result.Untrusted || result.Coverage != "provided_audio_only" || len(result.Sources) != 1 || result.Sources[0] != p.ref || !p.requested || len(p.providerReply) == 0 {
		return pluginruntime.VerifiedResult{}, errors.New("media_audio_observation_unverified")
	}
	var provider struct {
		Model   *string         `json:"model"`
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Message struct {
				Content  string          `json:"content"`
				Tools    json.RawMessage `json:"tool_calls"`
				Function json.RawMessage `json:"function_call"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	if _, err := decodeShareJSON(p.providerReply); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if json.Unmarshal(p.providerReply, &provider) != nil || len(provider.Choices) != 1 || len(provider.Error) > 0 && string(provider.Error) != "null" {
		return pluginruntime.VerifiedResult{}, errors.New("media_audio_observation_provider_invalid")
	}
	choice := provider.Choices[0]
	status := "observed"
	if choice.Finish == "length" {
		status = "partial"
	} else if choice.Finish != "stop" {
		return pluginruntime.VerifiedResult{}, errors.New("media_audio_observation_provider_incomplete")
	}
	if len(choice.Message.Tools) > 0 || len(choice.Message.Function) > 0 {
		return pluginruntime.VerifiedResult{}, errors.New("media_audio_observation_tool_call_rejected")
	}
	if result.Status != status || strings.TrimSpace(choice.Message.Content) == "" || result.Text != strings.TrimSpace(choice.Message.Content) || len(result.Text) > 2<<20 {
		return pluginruntime.VerifiedResult{}, errors.New("media_audio_observation_result_changed")
	}
	providerModel := ""
	if provider.Model != nil {
		providerModel = *provider.Model
		if utf8.RuneCountInString(providerModel) > 128 || strings.ContainsAny(providerModel, "\r\n\x00") {
			return pluginruntime.VerifiedResult{}, errors.New("media_audio_observation_provider_invalid")
		}
	}
	private, _ := json.Marshal(mediaAudioObservation{Status: status, Text: result.Text, Model: p.model, ProviderModel: providerModel, Sources: []mediaAudioReference{p.ref}, VoiceEvidence: "model_opinion_only"})
	public, _ := json.Marshal(map[string]any{"ok": true, "status": status, "coverage": "provided_audio_only", "sources": []mediaAudioReference{p.ref}, "requested_model": p.model, "provider_model": providerModel, "voice_identity_evidence": "model_opinion_only", "usage_source": "host_transport"})
	return pluginruntime.VerifiedResult{Result: public, Private: private}, nil
}

func (p *mediaAudioObservationPolicy) observe(ctx context.Context) (mediaAudioObservation, error) {
	job := pluginhost.Job{Plugin: "media-understanding", Tool: "media.listen", Context: p.invocationValue}
	if err := p.Check(ctx, job, pluginhost.Capability{}); err != nil {
		return mediaAudioObservation{}, err
	}
	controller, ready, err := p.manager.installedPluginRuntime()
	if err != nil || !ready {
		return mediaAudioObservation{}, errors.New("media_audio_observation_runtime_unavailable")
	}
	installed, err := p.store.Installed(ctx, job.Plugin)
	if err != nil {
		return mediaAudioObservation{}, err
	}
	if err = controller.EnsureStarted(ctx, job.Plugin, installed.Revision); err != nil {
		return mediaAudioObservation{}, err
	}
	call, err := controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: job.Plugin, Capability: job.Tool, Context: job.Context, Arguments: p.arguments}, p)
	if err != nil || call.Job.State != "completed" {
		return mediaAudioObservation{}, errors.New("media_audio_observation_needs_review")
	}
	p.completedJob = call.Job
	private, err := p.store.PrivateJobResult(ctx, call.Job, func(ctx context.Context, current pluginhost.Job) error {
		return p.Check(ctx, current, pluginhost.Capability{})
	})
	if err != nil {
		return mediaAudioObservation{}, errors.New("media_audio_observation_result_unavailable")
	}
	var result mediaAudioObservation
	if audioObservationJSON(private, &result) != nil || (result.Status != "observed" && result.Status != "partial") || result.Text == "" || result.Model != p.model || len(result.Sources) != 1 || result.Sources[0] != p.ref || result.VoiceEvidence != "model_opinion_only" {
		return mediaAudioObservation{}, errors.New("media_audio_observation_result_invalid")
	}
	if err = p.checkCompleted(ctx); err != nil {
		return mediaAudioObservation{}, err
	}
	return result, nil
}

func (p *mediaAudioObservationPolicy) checkCompleted(ctx context.Context) error {
	if p.completedJob.ID == "" || p.completedJob.State != "completed" {
		return errors.New("media_audio_observation_result_unavailable")
	}
	return p.Check(ctx, p.completedJob, pluginhost.Capability{})
}

var _ pluginruntime.ServicePolicy = (*mediaAudioObservationPolicy)(nil)
