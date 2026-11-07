package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"wechat-bot/internal/billing"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

// The cloud provider has no registered singing-voice identity. Its plugin can
// request one bound model call, but cannot choose credentials, delivery, or an
// audio URL different from the actual provider response.
type musicPluginPolicy struct {
	p                           *permittedTools
	store                       *pluginhost.Store
	prompt, lyrics, title, kind string
	model, digest, version      string
	configRevision              int
	timeout                     int64
	invocation                  pluginhost.Invocation
	arguments                   json.RawMessage
	post                        pluginruntime.RouteTransport
	mu                          sync.Mutex
	requested                   bool
	providerReply               json.RawMessage
	completedJob                pluginhost.Job
}

type musicPluginResult struct {
	URL            string `json:"provider_audio_url"`
	Model          string `json:"provider_model,omitempty"`
	RequestedModel string `json:"requested_model"`
}

func musicStrictJSON(raw json.RawMessage, out any) error {
	if _, err := decodeShareJSON(raw); err != nil {
		return err
	}
	return strictServicePluginJSON(raw, out)
}

func musicIdentifier(s string) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= 128 && !strings.ContainsAny(s, "\r\n\x00")
}

func musicProviderURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && len(s) <= 4096 && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Fragment == "" && !strings.ContainsAny(s, "\\\x00") && strings.IndexFunc(s, unicode.IsSpace) < 0
}

func (m *Manager) musicPluginConfigured(ctx context.Context) bool {
	s, err := m.packageStore()
	if err != nil {
		return false
	}
	cfg, err := s.Configuration(ctx, "music")
	if err != nil {
		return false
	}
	i, err := s.Installed(ctx, "music")
	inv := pluginhost.Invocation{Account: s.Account(), Thread: "host", Requester: "administrator", Purpose: "inspect music readiness", RequestKey: "host-readiness"}
	if err != nil || !i.Enabled || i.Removed || pluginhost.Authorize(i.Package, pluginhost.Grant{Account: s.Account(), Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, inv, []string{"model.invoke"}) != nil || s.CheckConfiguration(ctx, "music") != nil {
		return false
	}
	var endpoint, model string
	if json.Unmarshal(cfg.Values["music_endpoint"], &endpoint) != nil || json.Unmarshal(cfg.Values["music_model"], &model) != nil || !musicIdentifier(model) || !musicProviderURL(endpoint) || !strings.HasPrefix(endpoint, "https://") {
		return false
	}
	for _, key := range cfg.ConfiguredSecrets {
		if key == "music_key" {
			caps, e := s.Capabilities(ctx, "music")
			if e != nil {
				return false
			}
			for _, cap := range caps {
				if cap.ID == "music.generate" && cap.Implementation == "ready" && cap.Discoverable && !cap.Async {
					return true
				}
			}
		}
	}
	return false
}

func (p *permittedTools) musicPluginAuthorization(kind string) string {
	if p == nil || p.manager == nil || p.manager.native == nil || !p.manager.agentTaskActive(p.trace) || p.trace.Preview || p.trace.ReportOnly || p.trace.Surf || p.trace.Channel != "wechat" || p.trace.Account == "" || p.trace.Thread == "" || p.trace.Sender == "" || p.trace.Account != p.manager.native.Account() || !p.manager.Config().Config.Enabled || !p.manager.agent.config().Enabled || !p.musicKindPermitted(kind) || !permittedRecipient(p.manager.Config().Config, p.trace.Thread) {
		return ""
	}
	b, _ := json.Marshal([]any{p.trace.Account, p.trace.Channel, p.trace.Thread, p.trace.Sender, p.serviceRequester(), p.trace.TaskID, p.trace.TraceID, p.trace.Source, p.trace.ReplyContext, p.trace.RequestedText, p.trace.Text, p.MemoryResultAuthorization(), p.manager.Config().Companion.Permissions.SendVoice, p.manager.Config().Companion.Permissions.SendFile, kind})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p *permittedTools) cloudMusicVoiceAllowed(title, kind string) error {
	voices, err := p.accessibleVoices()
	if err != nil {
		return err
	}
	request := p.trace.RequestedText
	if request == "" {
		request = companion.UserRequestText(p.trace.Text)
	}
	if err = validateRequestedRVCVoice(request+" "+title, "", "", voices); err != nil {
		return err
	}
	voice, profile := p.SelectedVoiceIdentity()
	if voice != "" || profile != "" {
		return toolInputError("music_plugin_named_voice_unsupported")
	}
	if p.trace.TaskID != "" {
		task, ok := p.manager.agent.lookupTask(p.trace.TaskID, p.trace.Account, p.trace.Thread)
		if !ok || task.Sender != p.trace.Sender || p.manager.validateTaskSource(task) != nil {
			return toolInputError("music_plugin_task_constraints_changed")
		}
		if err = validateRequestedRVCVoice(task.RequestedText+" "+task.MusicTitle, "", "", voices); err != nil {
			return err
		}
		if task.Constraints != nil && (task.Constraints.VoiceID != "" || task.Constraints.VoiceProfile != "") || task.MusicSpec != nil && (task.MusicSpec.VoiceID != "" || task.MusicSpec.VoiceProfile != "" || task.MusicSpec.Reference != "" || task.MusicSpec.Mode == "cover") {
			return toolInputError("music_plugin_named_voice_unsupported")
		}
		if task.MusicTitle != "" && task.MusicTitle != title || task.MusicKind != "" && task.MusicKind != kind {
			return toolInputError("music_plugin_task_delivery_changed")
		}
	}
	return nil
}

func (p *permittedTools) newMusicPluginPolicy(ctx context.Context, raw json.RawMessage) (*musicPluginPolicy, error) {
	var in struct {
		Prompt  string `json:"prompt"`
		Lyrics  string `json:"lyrics"`
		Title   string `json:"title"`
		Kind    string `json:"kind"`
		Timeout *int64 `json:"timeout_seconds"`
	}
	if musicStrictJSON(raw, &in) != nil || utf8.RuneCountInString(in.Prompt) > 2000 || utf8.RuneCountInString(in.Lyrics) > 2000 || strings.TrimSpace(in.Prompt+in.Lyrics) == "" || strings.ContainsRune(in.Prompt+in.Lyrics, 0) || songTitle(in.Title) == "" || utf8.RuneCountInString(in.Title) > 24 || in.Timeout != nil && (*in.Timeout < 1 || *in.Timeout > int64((1<<63-1)/time.Second)) {
		return nil, toolInputError("invalid_music_arguments")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if fields["timeout_seconds"] != nil && in.Timeout == nil {
		return nil, toolInputError("invalid_music_arguments")
	}
	kind := p.musicDeliveryKind(in.Kind)
	if kind != "voice" && kind != "file" && kind != "both" {
		return nil, toolInputError("invalid_music_delivery_kind")
	}
	auth := p.musicPluginAuthorization(kind)
	if auth == "" {
		return nil, errors.New("music_plugin_request_not_authorized")
	}
	if err := p.cloudMusicVoiceAllowed(in.Title, kind); err != nil {
		return nil, err
	}
	installed, err := p.manager.installedServiceState(ctx, "music", "music")
	if err != nil {
		return nil, err
	}
	if !installed {
		return nil, toolInputError("music_plugin_migration_required")
	}
	if !p.manager.musicPluginConfigured(ctx) {
		return nil, errors.New("music_plugin_configuration_invalid")
	}
	store, err := p.manager.packageStore()
	if err != nil {
		return nil, err
	}
	cfg, err := store.Configuration(ctx, "music")
	if err != nil {
		return nil, err
	}
	i, err := store.Installed(ctx, "music")
	if err != nil {
		return nil, err
	}
	var model string
	if json.Unmarshal(cfg.Values["music_model"], &model) != nil || !musicIdentifier(model) {
		return nil, errors.New("music_plugin_configuration_invalid")
	}
	timeout := int64(180)
	if in.Timeout != nil {
		timeout = *in.Timeout
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
		for id, revision := range p.trace.ReplyContext.SourceVersions {
			sources[id] = revision
		}
	}
	key := "music-" + agentID(p.trace.Account, p.trace.Thread, p.serviceRequester(), p.trace.TaskID, p.trace.TraceID, in.Title, kind, in.Prompt, in.Lyrics, model, i.Digest, strconv.Itoa(cfg.Revision), strconv.FormatInt(timeout, 10))
	inv := pluginhost.Invocation{Account: p.trace.Account, Thread: p.trace.Thread, Requester: p.serviceRequester(), Purpose: "generate original conversation music", RequestKey: key, EvidenceEpoch: epoch, Authorization: auth, SourceVersions: sources}
	args, _ := json.Marshal(map[string]any{"prompt": in.Prompt, "lyrics": in.Lyrics, "timeout_seconds": timeout})
	policy := &musicPluginPolicy{p: p, store: store, prompt: in.Prompt, lyrics: in.Lyrics, title: in.Title, kind: kind, model: model, digest: i.Digest, version: i.Package.Version, configRevision: cfg.Revision, timeout: timeout, invocation: inv, arguments: args, post: mcptools.PostPluginModel}
	if err = policy.Check(ctx, pluginhost.Job{Plugin: "music", Tool: "music.generate", Context: inv}, pluginhost.Capability{}); err != nil {
		return nil, err
	}
	return policy, nil
}

func (p *musicPluginPolicy) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	want, _ := json.Marshal(p.invocation)
	got, _ := json.Marshal(j.Context)
	if j.Plugin != "music" || j.Tool != "music.generate" || cap.Async || cap.ID != "" && cap.ID != j.Tool || string(want) != string(got) || p.invocation.Authorization == "" || p.invocation.Authorization != p.p.musicPluginAuthorization(p.kind) {
		return errors.New("music_plugin_authorization_changed")
	}
	if err := p.p.cloudMusicVoiceAllowed(p.title, p.kind); err != nil {
		return err
	}
	current, err := p.p.manager.memory.RecallEpoch(ctx)
	if err != nil || current != p.invocation.EvidenceEpoch {
		return errors.New("music_plugin_evidence_changed")
	}
	for id, revision := range p.invocation.SourceVersions {
		ev, e := p.p.manager.memory.ReadEvent(ctx, []string{companion.ScopeID("wechat", p.invocation.Account, p.invocation.Thread)}, id)
		if e != nil || ev.Revision != revision {
			return errors.New("music_plugin_source_changed")
		}
		if source := p.p.trace.Source; source != nil && source.EventID == id && (ev.Role != "user" || ev.Sender != p.p.trace.Sender) {
			return errors.New("music_plugin_source_actor_changed")
		}
	}
	if source := p.p.trace.Source; source != nil && (source.Integrity == "conflicted" || source.Integrity == "quarantined" || source.Account != "" && source.Account != p.invocation.Account || source.Thread != "" && source.Thread != p.invocation.Thread || source.Sender != "" && source.Sender != p.p.trace.Sender) {
		return errors.New("music_plugin_source_changed")
	}
	cfg, err := p.store.Configuration(ctx, "music")
	if err != nil || cfg.Revision != p.configRevision {
		return errors.New("music_plugin_configuration_changed")
	}
	i, err := p.store.Installed(ctx, "music")
	if err != nil || !i.Enabled || i.Removed || i.Digest != p.digest || i.Package.Version != p.version || j.PackageDigest != "" && (j.PackageDigest != p.digest || j.Version != p.version || j.ConfigRevision != strconv.Itoa(p.configRevision)) {
		return errors.New("music_plugin_version_changed")
	}
	if pluginhost.Authorize(i.Package, pluginhost.Grant{Account: p.invocation.Account, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, j.Context, []string{"model.invoke"}) != nil {
		return errors.New("music_plugin_permissions_changed")
	}
	if j.ID != "" {
		current, e := p.store.Job(ctx, j.ID)
		if e != nil || current.CancelRequested || current.Revision != j.Revision {
			return errors.New("music_plugin_task_changed")
		}
	}
	return nil
}

func (p *musicPluginPolicy) materialize(_ context.Context, route string, raw json.RawMessage) (json.RawMessage, error) {
	input := map[string]any{"prompt": p.prompt, "gender": "female", "format": "mp3"}
	if p.lyrics != "" {
		input["lyrics"] = p.lyrics
	}
	expected, _ := json.Marshal(map[string]any{"model": p.model, "input": input})
	var body any
	if route != "generate" || musicStrictJSON(raw, &body) != nil || !sameJSON(raw, expected) {
		return nil, errors.New("music_model_request_changed")
	}
	return expected, nil
}

func (p *musicPluginPolicy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if method != "wechat/model_request" || p.requested {
		return nil, errors.New("music_host_service_unavailable")
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
	if musicStrictJSON(raw, &request) != nil || request.Route != "generate" || request.MaxBytes != 1<<20 || request.Timeout != p.timeout {
		return nil, errors.New("music_model_request_changed")
	}
	return pluginruntime.ProxyModelRoute(ctx, p.store, j, cap, raw, p.materialize, func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
		if err := p.Check(ctx, j, cap); err != nil {
			return nil, err
		}
		p.requested = true // An uncertain attempt is never replayed.
		reply, err := p.post(ctx, endpoint, headers, body, limit, timeout)
		usage := deepseek.ReportedUsage(reply, time.Now().UTC())
		usage.Model, usage.Purpose = p.model, "plugin_music_generation"
		if p.p.manager.billing == nil || p.p.manager.billing.Record(billing.ModelConfig{Provider: "bailian", BaseURL: strings.TrimSuffix(endpoint, "/api/v1/services/audio/music/generation"), Model: p.model, APIKey: strings.TrimPrefix(headers["Authorization"], "Bearer ")}, usage) != nil {
			return nil, errors.New("music_usage_record_failed")
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

func (p *musicPluginPolicy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
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
		Coverage  string  `json:"coverage"`
		Untrusted bool    `json:"untrusted_external_content"`
	}
	if musicStrictJSON(raw, &out) != nil || !out.OK || out.Status != "provider_accepted" || !musicProviderURL(out.URL) || out.Coverage != "provider_music_response" || !out.Untrusted || !p.requested || len(p.providerReply) == 0 {
		return pluginruntime.VerifiedResult{}, errors.New("music_generation_unverified")
	}
	var provider struct {
		Model  *string         `json:"model"`
		Error  json.RawMessage `json:"error"`
		Output struct {
			Audio struct {
				URL string `json:"url"`
			} `json:"audio"`
		} `json:"output"`
	}
	if _, err := decodeShareJSON(p.providerReply); err != nil || json.Unmarshal(p.providerReply, &provider) != nil || provider.Output.Audio.URL != out.URL || len(provider.Error) != 0 && string(provider.Error) != "null" || out.Model != nil && (provider.Model == nil || *out.Model != *provider.Model) {
		return pluginruntime.VerifiedResult{}, errors.New("music_provider_response_invalid")
	}
	model := ""
	if provider.Model != nil {
		model = *provider.Model
		if model != "" && !musicIdentifier(model) {
			return pluginruntime.VerifiedResult{}, errors.New("music_provider_response_invalid")
		}
	}
	private, _ := json.Marshal(musicPluginResult{URL: out.URL, Model: model, RequestedModel: p.model})
	public, _ := json.Marshal(map[string]any{"ok": true, "status": "provider_accepted", "requested_model": p.model, "provider_model": model, "coverage": out.Coverage, "usage_source": "host_transport", "artifact_verified": false, "wechat_sends": 0})
	return pluginruntime.VerifiedResult{Result: public, Private: private}, nil
}

func (p *musicPluginPolicy) generate(ctx context.Context) (musicPluginResult, error) {
	j := pluginhost.Job{Plugin: "music", Tool: "music.generate", Context: p.invocation}
	if err := p.Check(ctx, j, pluginhost.Capability{}); err != nil {
		return musicPluginResult{}, err
	}
	c, ready, err := p.p.manager.installedPluginRuntime()
	if err != nil || !ready {
		return musicPluginResult{}, errors.New("music_plugin_runtime_unavailable")
	}
	i, err := p.store.Installed(ctx, "music")
	if err != nil {
		return musicPluginResult{}, err
	}
	if err = c.EnsureStarted(ctx, "music", i.Revision); err != nil {
		return musicPluginResult{}, err
	}
	call, err := c.Invoke(ctx, pluginruntime.CallRequest{Plugin: j.Plugin, Capability: j.Tool, Context: j.Context, Arguments: p.arguments}, p)
	if err != nil || call.Job.State != "completed" {
		return musicPluginResult{}, errors.New("music_plugin_generation_needs_review")
	}
	raw, err := p.store.PrivateJobResult(ctx, call.Job, func(ctx context.Context, j pluginhost.Job) error { return p.Check(ctx, j, pluginhost.Capability{}) })
	if err != nil {
		return musicPluginResult{}, errors.New("music_plugin_result_unavailable")
	}
	var result musicPluginResult
	if musicStrictJSON(raw, &result) != nil || !musicProviderURL(result.URL) || result.RequestedModel != p.model {
		return musicPluginResult{}, errors.New("music_plugin_result_invalid")
	}
	p.completedJob = call.Job
	return result, nil
}

func (p *permittedTools) generatePluginMusic(ctx context.Context, raw json.RawMessage) (string, error) {
	policy, err := p.newMusicPluginPolicy(ctx, raw)
	if err != nil {
		return "", err
	}
	result, err := policy.generate(ctx)
	if err != nil {
		return "", err
	}
	data, _, err := mcptools.GetPublicMedia(ctx, result.URL, 25<<20)
	if err != nil {
		return "", toolInputError("music_plugin_audio_download_failed")
	}
	mime, err := verifyMusicPluginAudio(ctx, p.manager.dir, data)
	if err != nil {
		return "", err
	}
	if err = policy.Check(ctx, policy.completedJob, pluginhost.Capability{}); err != nil {
		return "", err
	}
	staged, err := p.stageSongOutput(withOutputAuthority(ctx, func(ctx context.Context) error {
		return policy.Check(ctx, policy.completedJob, pluginhost.Capability{})
	}), data, mime, policy.title, policy.kind)
	if err != nil {
		return "", err
	}
	var value map[string]any
	if json.Unmarshal([]byte(staged), &value) != nil {
		return "", errors.New("staged_music_response_invalid")
	}
	value["requested_model"], value["provider_model"] = result.RequestedModel, result.Model
	value["coverage"], value["wechat_sends"] = "provider_music_response", 0
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

// File delivery does not pass through voice conversion, so verify the actual
// downloaded container and audio stream rather than trusting its Content-Type.
func verifyMusicPluginAudio(ctx context.Context, dir string, data []byte) (string, error) {
	if len(data) == 0 || len(data) > 25<<20 {
		return "", toolInputError("music_plugin_audio_invalid")
	}
	tmp, err := os.MkdirTemp(dir, "music-plugin-audio-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	input := filepath.Join(tmp, "download.audio")
	if err = os.WriteFile(input, data, 0600); err != nil {
		return "", err
	}
	body, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file", "-format_whitelist", "mp3,wav,flac", "-select_streams", "a:0", "-show_entries", "stream=codec_type:format=format_name,duration", "-of", "json", input).Output()
	var probe struct {
		Streams []struct {
			Type string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Name     string `json:"format_name"`
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err != nil || len(body) > 65536 || json.Unmarshal(body, &probe) != nil || len(probe.Streams) != 1 || probe.Streams[0].Type != "audio" {
		return "", toolInputError("music_plugin_audio_invalid")
	}
	seconds, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return "", toolInputError("music_plugin_audio_invalid")
	}
	mime := map[string]string{"mp3": "audio/mpeg", "wav": "audio/wav", "flac": "audio/flac"}[probe.Format.Name]
	if mime == "" {
		return "", toolInputError("music_plugin_audio_invalid")
	}
	return mime, nil
}

var _ pluginruntime.ServicePolicy = (*musicPluginPolicy)(nil)
