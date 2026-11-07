package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/billing"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

// speechPluginPolicy is the only route from the default cloud voice to an
// installed speech plugin.  The plugin may select neither destination nor
// voice: both are materialized from the installed package configuration.
type speechPluginPolicy struct {
	p              *permittedTools
	manager        *Manager
	admin          *speechAdminTestAuthority
	store          *pluginhost.Store
	text           string
	instruction    string
	model, voice   string
	configRevision int
	packageDigest  string
	packageVersion string
	timeout        int64
	invocation     pluginhost.Invocation
	arguments      json.RawMessage
	post           pluginruntime.RouteTransport
	mu             sync.Mutex
	requested      bool
	providerReply  json.RawMessage
	completedJob   pluginhost.Job
}

type speechPluginResult struct {
	URL                 string `json:"provider_audio_url"`
	Model               string `json:"provider_model"`
	RequestedVoice      string `json:"requested_voice"`
	ProviderVoice       string `json:"provider_voice,omitempty"`
	ProviderVoiceStatus string `json:"provider_voice_status"`
}

func (m *Manager) speechPluginConfigured(ctx context.Context) bool {
	s, err := m.packageStore()
	if err != nil {
		return false
	}
	installed, err := s.Installed(ctx, "speech")
	if err != nil || !installed.Enabled || installed.Removed || s.CheckConfiguration(ctx, "speech") != nil {
		return false
	}
	inv := pluginhost.Invocation{Account: m.native.Account(), Thread: "host:capability-check", Requester: "host", Purpose: "check installed speech capability", RequestKey: "speech-availability"}
	if pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: m.native.Account(), Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}, inv, []string{"model.invoke"}) != nil {
		return false
	}
	c, err := s.Configuration(ctx, "speech")
	if err != nil {
		return false
	}
	var endpoint, model, voice string
	if json.Unmarshal(c.Values["tts_endpoint"], &endpoint) != nil || json.Unmarshal(c.Values["tts_model"], &model) != nil || json.Unmarshal(c.Values["default_voice"], &voice) != nil || endpoint == "" || model == "" || voice == "" {
		return false
	}
	for _, key := range c.ConfiguredSecrets {
		if key == "tts_key" {
			caps, err := s.Capabilities(ctx, "speech")
			if err != nil {
				return false
			}
			for _, cap := range caps {
				if cap.ID == "speech.synthesize" && cap.Implementation == "ready" && cap.Discoverable && !cap.Async {
					return true
				}
			}
		}
	}
	return false
}

func newSpeechPluginRequest(ctx context.Context, m *Manager, raw json.RawMessage) (*speechPluginPolicy, error) {
	var in struct {
		Text        string `json:"text"`
		Instruction string `json:"instruction"`
		Timeout     int64  `json:"timeout_seconds"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	// The public schema limits Unicode characters, as does the Python runtime;
	// it must not turn a 2,000-character Chinese request into a byte-limit
	// failure.  The route layer supplies the only real timeout ceiling.
	if d.Decode(&in) != nil || in.Text == "" || utf8.RuneCountInString(in.Text) > 2000 || strings.TrimSpace(in.Text) == "" || strings.ContainsRune(in.Text, 0) || utf8.RuneCountInString(in.Instruction) > 1500 || strings.ContainsRune(in.Instruction, 0) || in.Timeout < 0 || in.Timeout > int64((1<<63-1)/int64(time.Second)) {
		return nil, toolInputError("invalid_tts_arguments")
	}
	store, err := m.packageStore()
	if err != nil {
		return nil, err
	}
	installed, installErr := store.Installed(ctx, "speech")
	if installErr != nil {
		return nil, errors.New("speech_plugin_migration_required")
	}
	if !installed.Enabled || installed.Removed {
		return nil, errors.New("speech_plugin_unavailable")
	}
	cfg, err := store.Configuration(ctx, "speech")
	if err != nil {
		return nil, err
	}
	var model, voice, defaultInstruction string
	if json.Unmarshal(cfg.Values["tts_model"], &model) != nil || json.Unmarshal(cfg.Values["default_voice"], &voice) != nil || (cfg.Values["default_instruction"] != nil && json.Unmarshal(cfg.Values["default_instruction"], &defaultInstruction) != nil) || model == "" || voice == "" || utf8.RuneCountInString(model) > 128 || utf8.RuneCountInString(voice) > 128 || utf8.RuneCountInString(defaultInstruction) > 1500 {
		return nil, errors.New("speech_plugin_configuration_invalid")
	}
	if in.Timeout == 0 {
		in.Timeout = 180
	}
	args, _ := json.Marshal(map[string]any{"text": in.Text, "instruction": in.Instruction, "timeout_seconds": in.Timeout})
	return &speechPluginPolicy{manager: m, packageDigest: installed.Digest, packageVersion: installed.Package.Version, timeout: in.Timeout, store: store, text: in.Text, instruction: in.Instruction, model: model, voice: voice, configRevision: cfg.Revision, arguments: args, post: mcptools.PostPluginModel}, nil
}

func (p *permittedTools) newSpeechPluginPolicy(ctx context.Context, raw json.RawMessage) (*speechPluginPolicy, error) {
	if p == nil || p.manager == nil || !p.manager.agentTaskActive(p.trace) || p.trace.Preview || p.trace.ReportOnly || p.trace.Account == "" || p.trace.Thread == "" || p.trace.Sender == "" || p.trace.Account != p.manager.native.Account() || !p.manager.Config().Companion.Permissions.SendVoice || !permittedRecipient(p.manager.Config().Config, p.trace.Thread) {
		return nil, errors.New("speech_plugin_request_not_authorized")
	}
	policy, err := newSpeechPluginRequest(ctx, p.manager, raw)
	if err != nil {
		return nil, err
	}
	epoch, err := p.manager.memory.RecallEpoch(ctx)
	if err != nil {
		return nil, err
	}
	sources := map[string]int{}
	if p.trace.Source != nil && p.trace.Source.EventID != "" {
		sources[p.trace.Source.EventID] = p.trace.Source.Revision
	}
	if p.trace.ReplyContext != nil {
		for id, version := range p.trace.ReplyContext.SourceVersions {
			sources[id] = version
		}
	}
	auth := p.speechPluginAuthorization()
	if auth == "" {
		return nil, errors.New("speech_plugin_request_not_authorized")
	}
	key := "speech-" + agentID(p.trace.Account, p.trace.Thread, p.serviceRequester(), p.trace.TaskID, p.trace.TraceID, policy.text, policy.instruction, policy.model, policy.voice, strconv.Itoa(policy.configRevision))
	policy.p = p
	policy.invocation = pluginhost.Invocation{Account: p.trace.Account, Thread: p.trace.Thread, Requester: p.serviceRequester(), Purpose: "synthesize conversation speech", RequestKey: key, EvidenceEpoch: epoch, Authorization: auth, SourceVersions: sources}
	return policy, nil
}

func (p *permittedTools) speechPluginAuthorization() string {
	if p == nil || p.manager == nil || !p.manager.agentTaskActive(p.trace) || p.trace.Preview || p.trace.ReportOnly || p.trace.Account != p.manager.native.Account() || !p.manager.Config().Companion.Permissions.SendVoice || !permittedRecipient(p.manager.Config().Config, p.trace.Thread) {
		return ""
	}
	b, _ := json.Marshal([]any{p.trace.Account, p.trace.Channel, p.trace.Thread, p.serviceRequester(), p.trace.TaskID, p.trace.Surf, p.MemoryResultAuthorization(), p.manager.agent.config().Enabled, p.manager.Config().Config.Enabled, p.manager.Config().Companion.Permissions.SendVoice})
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (p *speechPluginPolicy) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	manager := p.hostManager()
	wantContext, _ := json.Marshal(p.invocation)
	gotContext, _ := json.Marshal(j.Context)
	if manager == nil || j.Plugin != "speech" || j.Tool != "speech.synthesize" || cap.Async || !bytes.Equal(wantContext, gotContext) {
		return errors.New("speech_plugin_authorization_changed")
	}
	if p.admin != nil {
		if err := p.admin.valid(ctx, manager); err != nil {
			return err
		}
	} else {
		access := p.p
		if access == nil || access.manager == nil || !access.manager.agentTaskActive(access.trace) || j.Plugin != "speech" || j.Tool != "speech.synthesize" || cap.Async || p.invocation.Authorization == "" || p.invocation.Authorization != access.speechPluginAuthorization() {
			return errors.New("speech_plugin_authorization_changed")
		}
		if !access.manager.Config().Config.Enabled || !access.manager.Config().Companion.Permissions.SendVoice || !permittedRecipient(access.manager.Config().Config, access.trace.Thread) {
			return errors.New("speech_plugin_authorization_changed")
		}
		current, err := access.manager.memory.RecallEpoch(ctx)
		if err != nil || current != p.invocation.EvidenceEpoch {
			return errors.New("speech_plugin_evidence_changed")
		}
		for id, revision := range p.invocation.SourceVersions {
			ev, e := access.manager.memory.ReadEvent(ctx, []string{companion.ScopeID("wechat", access.trace.Account, access.trace.Thread)}, id)
			if e != nil || ev.Revision != revision {
				return errors.New("speech_plugin_source_changed")
			}
		}
	}
	cfg, err := p.store.Configuration(ctx, "speech")
	if err != nil || cfg.Revision != p.configRevision {
		return errors.New("speech_plugin_configuration_changed")
	}
	i, err := p.store.Installed(ctx, "speech")
	if err != nil || !i.Enabled || i.Removed || i.Digest != p.packageDigest || i.Package.Version != p.packageVersion || (j.PackageDigest != "" && (i.Digest != j.PackageDigest || i.Package.Version != j.Version || j.ConfigRevision != strconv.Itoa(p.configRevision))) {
		return errors.New("speech_plugin_version_changed")
	}
	if pluginhost.Authorize(i.Package, pluginhost.Grant{Account: p.invocation.Account, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, j.Context, []string{"model.invoke"}) != nil {
		return errors.New("speech_plugin_permissions_changed")
	}
	if j.ID != "" {
		current, e := p.store.Job(ctx, j.ID)
		if e != nil || current.CancelRequested || current.Revision != j.Revision {
			return errors.New("speech_plugin_task_changed")
		}
	}
	return nil
}

func (p *speechPluginPolicy) materialize(_ context.Context, route string, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		Model string `json:"model"`
		Input struct {
			Text        string `json:"text"`
			Voice       string `json:"voice"`
			Format      string `json:"format"`
			Rate        int    `json:"sample_rate"`
			Instruction string `json:"instruction"`
		} `json:"input"`
	}
	expectedInstruction, expectedErr := p.expectedInstruction()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || expectedErr != nil || route != "synthesize" || body.Model != p.model || body.Input.Text != p.text || body.Input.Voice != p.voice || body.Input.Format != "wav" || body.Input.Rate != 24000 || body.Input.Instruction != expectedInstruction {
		return nil, errors.New("speech_model_request_changed")
	}
	return json.Marshal(map[string]any{"model": p.model, "input": map[string]any{"text": p.text, "voice": p.voice, "format": "wav", "sample_rate": 24000, "instruction": expectedInstruction}})
}

func (p *speechPluginPolicy) expectedInstruction() (string, error) {
	cfg, err := p.store.Configuration(context.Background(), "speech")
	if err != nil || cfg.Revision != p.configRevision {
		return "", errors.New("speech_plugin_configuration_changed")
	}
	var configured string
	if cfg.Values["default_instruction"] != nil && json.Unmarshal(cfg.Values["default_instruction"], &configured) != nil || utf8.RuneCountInString(configured) > 1500 || strings.ContainsRune(configured, 0) {
		return "", errors.New("speech_plugin_configuration_invalid")
	}
	if p.instruction == "" || p.instruction == configured {
		return configured, nil
	}
	if strings.TrimSpace(p.instruction) == "" || utf8.RuneCountInString(p.instruction) > 1500 || strings.ContainsRune(p.instruction, 0) {
		return "", errors.New("invalid_tts_instruction")
	}
	if configured == "" {
		return p.instruction, nil
	}
	joined := configured + "\n本次表达要求（保持上述角色声线，只调整语气与节奏）：" + p.instruction
	if utf8.RuneCountInString(joined) > 3100 {
		return "", errors.New("tts_instruction_combined_too_long")
	}
	return joined, nil
}

func (p *speechPluginPolicy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if method != "wechat/model_request" || p.requested {
		return nil, errors.New("speech_host_service_unavailable")
	}
	if err := p.Check(ctx, j, cap); err != nil {
		return nil, err
	}
	var request struct {
		Route    string          `json:"route"`
		Body     json.RawMessage `json:"body"`
		MaxBytes int64           `json:"max_bytes"`
		Timeout  int64           `json:"timeout_seconds"`
	}
	if strictServicePluginJSON(raw, &request) != nil || request.Route != "synthesize" || request.Timeout != p.timeout || request.MaxBytes < 1 || request.MaxBytes > 1<<20 {
		return nil, errors.New("speech_model_request_changed")
	}
	return pluginruntime.ProxyModelRoute(ctx, p.store, j, cap, raw, p.materialize, func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
		if err := p.Check(ctx, j, cap); err != nil {
			return nil, err
		}
		p.requested = true
		reply, err := p.post(ctx, endpoint, headers, body, limit, timeout)
		usage := deepseek.ReportedUsage(reply, time.Now().UTC())
		usage.Model = p.model
		usage.Purpose = "plugin_speech_synthesis"
		if p.admin != nil {
			usage.Purpose = "plugin_speech_admin_test"
		}
		if p.hostManager().billing == nil || p.hostManager().billing.Record(billing.ModelConfig{Provider: "bailian", BaseURL: strings.TrimSuffix(endpoint, "/api/v1/services/aigc/multimodal-generation/generation"), Model: p.model, APIKey: strings.TrimPrefix(headers["Authorization"], "Bearer ")}, usage) != nil {
			return nil, errors.New("speech_usage_record_failed")
		}
		if err != nil {
			return nil, err
		}
		if err := p.Check(ctx, j, cap); err != nil {
			return nil, err
		}
		p.providerReply = append(json.RawMessage(nil), reply...)
		return reply, nil
	})
}

func (p *speechPluginPolicy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Check(ctx, j, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var out struct {
		OK        bool    `json:"ok"`
		Status    string  `json:"status"`
		URL       string  `json:"provider_audio_url"`
		Model     *string `json:"provider_model"`
		Voice     string  `json:"voice"`
		Coverage  string  `json:"coverage"`
		Untrusted bool    `json:"untrusted_external_content"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || !out.OK || out.Status != "provider_accepted" || out.URL == "" || out.Voice != p.voice || out.Coverage != "provider_tts_response" || !out.Untrusted || !p.requested || len(p.providerReply) == 0 {
		return pluginruntime.VerifiedResult{}, errors.New("speech_synthesis_unverified")
	}
	var provider struct {
		Model  string `json:"model"`
		Voice  string `json:"voice"`
		Output struct {
			Voice string `json:"voice"`
			Audio struct {
				URL   string `json:"url"`
				Voice string `json:"voice"`
			} `json:"audio"`
		} `json:"output"`
	}
	if json.Unmarshal(p.providerReply, &provider) != nil || provider.Output.Audio.URL == "" || provider.Output.Audio.URL != out.URL || out.Model != nil && *out.Model != provider.Model {
		return pluginruntime.VerifiedResult{}, errors.New("speech_provider_response_invalid")
	}
	providerVoice := ""
	for _, claimed := range []string{provider.Voice, provider.Output.Voice, provider.Output.Audio.Voice} {
		if claimed == "" {
			continue
		}
		if providerVoice != "" && providerVoice != claimed || claimed != p.voice {
			return pluginruntime.VerifiedResult{}, errors.New("speech_provider_voice_mismatch")
		}
		providerVoice = claimed
	}
	status := "not_reported"
	if providerVoice != "" {
		status = "verified"
	}
	private, _ := json.Marshal(speechPluginResult{URL: out.URL, Model: provider.Model, RequestedVoice: p.voice, ProviderVoice: providerVoice, ProviderVoiceStatus: status})
	// `out.voice` is the plugin echo of the configured request. It proves the
	// plugin did not alter the requested configuration, not the identity of the
	// generated waveform. Only a matching provider response earns `verified`.
	public, _ := json.Marshal(map[string]any{"ok": true, "status": "provider_accepted", "requested_voice": p.voice, "provider_voice_status": status, "coverage": out.Coverage, "usage_source": "host_transport"})
	return pluginruntime.VerifiedResult{Result: public, Private: private}, nil
}

func (p *speechPluginPolicy) synthesize(ctx context.Context) (speechPluginResult, error) {
	j := pluginhost.Job{Plugin: "speech", Tool: "speech.synthesize", Context: p.invocation}
	if err := p.Check(ctx, j, pluginhost.Capability{}); err != nil {
		return speechPluginResult{}, err
	}
	c, ready, err := p.hostManager().installedPluginRuntime()
	if err != nil || !ready {
		return speechPluginResult{}, errors.New("speech_plugin_runtime_unavailable")
	}
	i, err := p.store.Installed(ctx, "speech")
	if err != nil {
		return speechPluginResult{}, err
	}
	if err = c.EnsureStarted(ctx, "speech", i.Revision); err != nil {
		return speechPluginResult{}, err
	}
	call, err := c.Invoke(ctx, pluginruntime.CallRequest{Plugin: j.Plugin, Capability: j.Tool, Context: j.Context, Arguments: p.arguments}, p)
	if err != nil {
		return speechPluginResult{}, errors.New("speech_plugin_synthesis_needs_review")
	}
	if call.Job.State != "completed" {
		return speechPluginResult{}, errors.New("speech_plugin_synthesis_needs_review")
	}
	raw, err := p.store.PrivateJobResult(ctx, call.Job, func(ctx context.Context, j pluginhost.Job) error { return p.Check(ctx, j, pluginhost.Capability{}) })
	if err != nil {
		return speechPluginResult{}, errors.New("speech_plugin_result_unavailable")
	}
	var r speechPluginResult
	if json.Unmarshal(raw, &r) != nil || r.URL == "" || r.RequestedVoice != p.voice || (r.ProviderVoiceStatus != "verified" && r.ProviderVoiceStatus != "not_reported") || r.ProviderVoiceStatus == "verified" && r.ProviderVoice != p.voice {
		return speechPluginResult{}, errors.New("speech_plugin_result_invalid")
	}
	p.completedJob = call.Job
	return r, nil
}

// Completion does not grant a lasting right to read or upload the artifact.
// Recheck the exact completed job after every potentially slow boundary.
func (p *speechPluginPolicy) checkCompleted(ctx context.Context) error {
	if p.completedJob.ID == "" || p.completedJob.State != "completed" {
		return errors.New("speech_plugin_result_unavailable")
	}
	return p.Check(ctx, p.completedJob, pluginhost.Capability{})
}

func (p *speechPluginPolicy) readAudio(ctx context.Context, url string, limit int64, fetch func(context.Context, string, int64) ([]byte, string, error)) ([]byte, string, error) {
	if err := p.checkCompleted(ctx); err != nil {
		return nil, "", err
	}
	data, mime, err := fetch(ctx, url, limit)
	if err != nil {
		return nil, "", err
	}
	if err = p.checkCompleted(ctx); err != nil {
		return nil, "", err
	}
	return data, mime, nil
}

func (p *permittedTools) synthesizePluginSpeech(ctx context.Context, raw json.RawMessage) (string, error) {
	policy, err := p.newSpeechPluginPolicy(ctx, raw)
	if err != nil {
		return "", err
	}
	result, err := policy.synthesize(ctx)
	if err != nil {
		return "", err
	}
	data, mime, err := policy.readAudio(ctx, result.URL, 25<<20, mcptools.GetPublicMedia)
	if err != nil {
		return "", toolInputError("speech_plugin_audio_download_failed")
	}
	out, err := preparePublicAudioPreview(ctx, p.manager.dir, data, 0)
	if err != nil {
		return "", err
	}
	if err = policy.checkCompleted(ctx); err != nil {
		return "", err
	}
	staged, err := p.stageOutputWithAsset(withOutputAuthority(ctx, policy.checkCompleted), out, &assetOriginal{VoiceEngine: "plugin:speech", SynthesisText: policy.text, SynthesisTextKnown: true, VoiceComplete: true, Data: data, MIME: mime, Filename: "默认声线-语音.wav"})
	if err != nil {
		return "", err
	}
	var value map[string]any
	if json.Unmarshal([]byte(staged), &value) != nil {
		return "", errors.New("staged_speech_response_invalid")
	}
	value["requested_voice"] = result.RequestedVoice
	value["provider_voice_status"] = result.ProviderVoiceStatus
	if result.ProviderVoiceStatus == "verified" {
		value["provider_voice"] = result.ProviderVoice
	}
	value["voice_identity_evidence"] = "requested_configuration"
	if result.ProviderVoiceStatus == "verified" {
		value["voice_identity_evidence"] = "provider_response"
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

func (p *speechPluginPolicy) hostManager() *Manager {
	if p.manager != nil {
		return p.manager
	}
	if p.p != nil {
		return p.p.manager
	}
	return nil
}
