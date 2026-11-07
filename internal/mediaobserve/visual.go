package mediaobserve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/billing"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/strictjson"
)

type ImageReference struct {
	Ref     string `json:"media_ref"`
	SHA     string `json:"sha256"`
	Version int    `json:"version"`
	MIME    string `json:"mime"`
}
type mediaObservation struct {
	Text    string           `json:"observation"`
	Status  string           `json:"status"`
	Sources []ImageReference `json:"sources"`
}

// Visual verifies a single tool-mediated observation of host-approved images.
type Visual struct {
	binding                        VisualBinding
	store                          *pluginhost.Store
	packageDigest, packageVersion  string
	completedJob                   pluginhost.Job
	mu                             sync.Mutex
	images                         []deepseek.Image
	refs                           []ImageReference
	prompt, model, backend, effort string
	tokens, configRevision         int
	thinking, requested            bool
	providerReply                  json.RawMessage
	arguments                      json.RawMessage
}

// VisualBinding supplies application-owned authority and host-owned runtime services.
// Invocation is evaluated again before each boundary so a trusted caller can
// distinguish continuation windows without replacing their source proof.
type VisualBinding struct {
	Invocation   func() pluginhost.Invocation
	CheckOrigin  func(context.Context, pluginhost.Job, pluginhost.Capability) error
	CheckAccount func(string) bool
	Configured   func(context.Context) bool
	Controller   func() (*pluginruntime.Controller, error)
	Billing      *billing.Store
	Transport    pluginruntime.RouteTransport
}

// Bind attaches authority and runtime services before use.
func (p *Visual) Bind(binding VisualBinding) {
	p.binding = binding
}

// Arguments returns a copy of the bound tool inputs for use in request identity.
func (p *Visual) Arguments() json.RawMessage { return bytes.Clone(p.arguments) }

// ConfigRevision returns the captured version of the media tool configuration.
func (p *Visual) ConfigRevision() int { return p.configRevision }

// VisualParameters describes the frozen model options and image references.
type VisualParameters struct {
	Prompt, Model string
	Tokens        int
	Thinking      bool
	Refs          []ImageReference
}

// Parameters returns the provider request metadata, without raw images or credentials.
func (p *Visual) Parameters() VisualParameters {
	return VisualParameters{Prompt: p.prompt, Model: p.model, Tokens: p.tokens, Thinking: p.thinking, Refs: append([]ImageReference(nil), p.refs...)}
}

// Images returns an independent copy of the exact source images.
func (p *Visual) Images() []deepseek.Image {
	images := make([]deepseek.Image, len(p.images))
	for i, image := range p.images {
		images[i] = deepseek.Image{Data: bytes.Clone(image.Data), MIME: image.MIME}
	}
	return images
}

var safeVisualFailure = regexp.MustCompile("^[a-z_]{1,96}$")

// NewVisual freezes the verified image inputs and installed configuration.
func NewVisual(ctx context.Context, store *pluginhost.Store, revision int, prompt string, images []deepseek.Image, tokens int) (*Visual, error) {
	if store == nil || revision < 1 || prompt == "" || !utf8.ValidString(prompt) || utf8.RuneCountInString(prompt) > 8192 || len(images) == 0 {
		return nil, errors.New("media_observation_input_invalid")
	}
	config, err := store.Configuration(ctx, "media-understanding")
	if err != nil {
		return nil, err
	}
	installed, err := store.Installed(ctx, "media-understanding")
	if err != nil {
		return nil, err
	}
	p := &Visual{store: store, packageDigest: installed.Digest, packageVersion: installed.Package.Version, prompt: prompt, configRevision: config.Revision}
	json.Unmarshal(config.Values["vision_model"], &p.model)
	json.Unmarshal(config.Values["vision_backend"], &p.backend)
	json.Unmarshal(config.Values["thinking"], &p.thinking)
	json.Unmarshal(config.Values["reasoning_effort"], &p.effort)
	json.Unmarshal(config.Values["max_tokens"], &p.tokens)
	if p.backend == "" {
		p.backend = "openai-compatible"
	}
	if p.effort == "" {
		p.effort = "low"
	}
	if p.tokens == 0 {
		p.tokens = 768
	}
	if tokens > 0 {
		p.tokens = tokens
	}
	if tokens < 0 || p.model == "" {
		return nil, errors.New("media_plugin_configuration_invalid")
	}
	total := 0
	for i, image := range images {
		kind := http.DetectContentType(image.Data)
		if kind != "image/png" && kind != "image/jpeg" && kind != "image/webp" && kind != "image/gif" {
			return nil, errors.New("media_image_format_invalid")
		}
		total += len(image.Data)
		// Existing image transport budgets; reject rather than truncate or change count.
		if len(image.Data) > 8<<20 || total > 20<<20 {
			return nil, errors.New("media_image_transport_limit")
		}
		copy := append([]byte(nil), image.Data...)
		sum := sha256.Sum256(copy)
		p.images = append(p.images, deepseek.Image{Data: copy, MIME: kind})
		p.refs = append(p.refs, ImageReference{Ref: fmt.Sprintf("image_%d", i), SHA: hex.EncodeToString(sum[:]), Version: revision, MIME: kind})
	}
	p.arguments, _ = json.Marshal(map[string]any{"images": p.refs, "prompt": prompt, "max_tokens": p.tokens, "thinking": p.thinking, "reasoning_effort": p.effort})
	return p, nil
}

// Invocation returns the current source binding without exposing its mutable map.
func (p *Visual) Invocation() pluginhost.Invocation {
	if p.binding.Invocation == nil {
		return pluginhost.Invocation{}
	}
	invocation := p.binding.Invocation()
	invocation.SourceVersions = maps.Clone(invocation.SourceVersions)
	return invocation
}

func (p *Visual) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	invocation := p.Invocation()
	if p.store == nil || p.binding.CheckOrigin == nil || p.binding.Controller == nil || j.Plugin != "media-understanding" || j.Tool != "media.observe" || cap.Async || invocation.Account != p.store.Account() ||
		(p.binding.CheckAccount != nil && !p.binding.CheckAccount(invocation.Account)) {
		return errors.New("media_observation_authorization_changed")
	}
	want, _ := json.Marshal(invocation)
	actual, _ := json.Marshal(j.Context)
	if !bytes.Equal(want, actual) {
		return errors.New("media_observation_invocation_changed")
	}
	if err := p.binding.CheckOrigin(ctx, j, cap); err != nil {
		return err
	}
	config, err := p.store.Configuration(ctx, j.Plugin)
	if err != nil || config.Revision != p.configRevision {
		return errors.New("media_plugin_configuration_changed")
	}
	installed, err := p.store.Installed(ctx, j.Plugin)
	if err != nil || !installed.Enabled || installed.Removed || installed.Digest != p.packageDigest || installed.Package.Version != p.packageVersion ||
		(p.binding.Configured != nil && !p.binding.Configured(ctx)) {
		return errors.New("media_plugin_version_changed")
	}
	if j.PackageDigest != "" {
		i, err := p.store.Installed(ctx, j.Plugin)
		if err != nil || i.Digest != j.PackageDigest || i.Package.Version != j.Version || j.ConfigRevision != strconv.Itoa(p.configRevision) {
			return errors.New("media_plugin_version_changed")
		}
		if pluginhost.Authorize(i.Package, pluginhost.Grant{Account: invocation.Account, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, j.Context, i.Package.Permissions) != nil {
			return errors.New("media_plugin_permissions_changed")
		}
	}
	if j.ID != "" {
		current, err := p.store.Job(ctx, j.ID)
		if err != nil || current.CancelRequested || current.Revision != j.Revision {
			return errors.New("media_plugin_task_changed")
		}
	}
	return nil
}

func (p *Visual) materialize(_ context.Context, route string, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Stream    bool `json:"stream"`
		MaxTokens int  `json:"max_tokens"`
		Thinking  *struct {
			Type string `json:"type"`
		} `json:"thinking,omitempty"`
		Effort string `json:"reasoning_effort,omitempty"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || route != "vision" || body.Model != p.model || body.Stream || body.MaxTokens != p.tokens || len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
		return nil, errors.New("media_model_request_changed")
	}
	var system string
	if json.Unmarshal(body.Messages[0].Content, &system) != nil || system == "" || len(system) > 8192 {
		return nil, errors.New("media_model_system_invalid")
	}
	if p.backend == "deepseek" {
		want := "disabled"
		if p.thinking {
			want = "enabled"
		}
		if body.Thinking == nil || body.Thinking.Type != want || p.thinking && body.Effort != p.effort || !p.thinking && body.Effort != "" {
			return nil, errors.New("media_thinking_changed")
		}
	} else if p.backend != "openai-compatible" || p.thinking || body.Thinking != nil || body.Effort != "" {
		return nil, errors.New("media_thinking_protocol_invalid")
	}
	var parts []struct {
		Type  string `json:"type"`
		Text  string `json:"text,omitempty"`
		Image *struct {
			URL struct {
				Ref ImageReference `json:"$media_ref"`
			} `json:"url"`
			Detail string `json:"detail"`
		} `json:"image_url,omitempty"`
	}
	d = json.NewDecoder(bytes.NewReader(body.Messages[1].Content))
	d.DisallowUnknownFields()
	if d.Decode(&parts) != nil || len(parts) != len(p.refs)+1 || parts[0].Type != "text" || parts[0].Text != p.prompt || parts[0].Image != nil {
		return nil, errors.New("media_prompt_changed")
	}
	content := []any{map[string]any{"type": "text", "text": p.prompt}}
	for i, ref := range p.refs {
		part := parts[i+1]
		if part.Type != "image_url" || part.Text != "" || part.Image == nil || part.Image.URL.Ref != ref || part.Image.Detail != "auto" {
			return nil, errors.New("media_image_reference_changed")
		}
		image := p.images[i]
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + image.MIME + ";base64," + base64.StdEncoding.EncodeToString(image.Data), "detail": "auto"}})
	}
	body.Messages[1].Content, _ = json.Marshal(content)
	return json.Marshal(body)
}

func (p *Visual) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if method != "wechat/model_request" {
		return nil, errors.New("media_host_service_unavailable")
	}
	if err := p.Check(ctx, j, cap); err != nil {
		return nil, err
	}
	if p.requested {
		return nil, errors.New("media_model_already_requested")
	}
	return pluginruntime.ProxyModelRoute(ctx, p.store, j, cap, raw, p.materialize, func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
		if err := p.Check(ctx, j, cap); err != nil {
			return nil, err
		}
		p.requested = true
		post := p.binding.Transport
		if post == nil {
			post = mcptools.PostPluginModel
		}
		reply, err := post(ctx, endpoint, headers, body, limit, timeout)
		// Bill the actual HTTP attempt even if its result is later rejected/forgotten.
		usage := deepseek.ReportedUsage(reply, time.Now().UTC())
		usage.Model = p.model
		usage.Purpose = "plugin_media_observation"
		c := billing.ModelConfig{Provider: p.backend, BaseURL: strings.TrimSuffix(endpoint, "/chat/completions"), Model: p.model, APIKey: strings.TrimPrefix(headers["Authorization"], "Bearer ")}
		if p.binding.Billing == nil || p.binding.Billing.Record(c, usage) != nil {
			return nil, errors.New("media_usage_record_failed")
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

func (p *Visual) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Check(ctx, j, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if _, err := strictjson.Decode(raw); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var failure struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&failure) == nil && failure.OK != nil && !*failure.OK && safeVisualFailure.MatchString(failure.Error) {
		return pluginruntime.VerifiedResult{Failed: true, Result: raw}, nil
	}
	var result struct {
		OK bool `json:"ok"`
		mediaObservation
		Coverage      string          `json:"coverage"`
		Untrusted     bool            `json:"untrusted_external_content"`
		Audio         *bool           `json:"audio_observed"`
		Model         string          `json:"requested_model"`
		ProviderModel *string         `json:"provider_model"`
		Tokens        int             `json:"requested_max_tokens"`
		Thinking      bool            `json:"thinking"`
		Effort        *string         `json:"reasoning_effort"`
		Usage         json.RawMessage `json:"usage"`
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil || !result.OK || !result.Untrusted || result.Audio == nil || *result.Audio || result.Coverage != "provided_images_only" || result.Model != p.model || result.Tokens != p.tokens || result.Thinking != p.thinking || !p.requested || len(p.providerReply) == 0 {
		return pluginruntime.VerifiedResult{}, errors.New("media_observation_unverified")
	}
	if p.thinking && (result.Effort == nil || *result.Effort != p.effort) || !p.thinking && result.Effort != nil {
		return pluginruntime.VerifiedResult{}, errors.New("media_observation_unverified")
	}
	var provider struct {
		Choices []struct {
			Message struct {
				Content  string          `json:"content"`
				Tools    json.RawMessage `json:"tool_calls"`
				Function json.RawMessage `json:"function_call"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	if _, err := strictjson.Decode(p.providerReply); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if json.Unmarshal(p.providerReply, &provider) != nil || len(provider.Choices) != 1 {
		return pluginruntime.VerifiedResult{}, errors.New("media_provider_response_invalid")
	}
	choice := provider.Choices[0]
	status := "observed"
	if choice.Finish == "length" {
		status = "partial"
	} else if choice.Finish != "stop" {
		return pluginruntime.VerifiedResult{}, errors.New("media_provider_response_incomplete")
	}
	if len(choice.Message.Tools) > 0 && string(choice.Message.Tools) != "null" && string(choice.Message.Tools) != "[]" || len(choice.Message.Function) > 0 && string(choice.Message.Function) != "null" {
		return pluginruntime.VerifiedResult{}, errors.New("media_provider_tool_call_rejected")
	}
	if result.Status != status || result.Text == "" || result.Text != strings.TrimSpace(choice.Message.Content) || len(result.Sources) != len(p.refs) {
		return pluginruntime.VerifiedResult{}, errors.New("media_observation_changed")
	}
	for i, ref := range p.refs {
		if result.Sources[i] != ref {
			return pluginruntime.VerifiedResult{}, errors.New("media_observation_source_changed")
		}
	}
	private, _ := json.Marshal(result.mediaObservation)
	public, _ := json.Marshal(map[string]any{"ok": true, "status": status, "coverage": result.Coverage, "sources": p.refs, "usage_source": "host_transport"})
	return pluginruntime.VerifiedResult{Result: public, Private: private}, nil
}

func (p *Visual) Observe(ctx context.Context) (string, error) {
	j := pluginhost.Job{Plugin: "media-understanding", Tool: "media.observe", Context: p.Invocation()}
	if err := p.Check(ctx, j, pluginhost.Capability{}); err != nil {
		return "", err
	}
	controller, err := p.binding.Controller()
	if err != nil || controller == nil {
		return "", errors.New("media_plugin_runtime_unavailable")
	}
	installed, err := p.store.Installed(ctx, j.Plugin)
	if err != nil {
		return "", err
	}
	if err = controller.EnsureStarted(ctx, j.Plugin, installed.Revision); err != nil {
		return "", err
	}
	call, err := controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: j.Plugin, Capability: j.Tool, Context: j.Context, Arguments: p.arguments}, p)
	if err != nil {
		return "", errors.New("media_plugin_observation_needs_review")
	}
	if call.Job.State != "completed" {
		var failure struct {
			Error string `json:"error"`
		}
		if call.Job.State == "failed" && json.Unmarshal(call.Job.Result, &failure) == nil && safeVisualFailure.MatchString(failure.Error) {
			return "", errors.New(failure.Error)
		}
		return "", errors.New("media_plugin_observation_needs_review")
	}
	p.completedJob = call.Job
	raw, err := p.store.PrivateJobResult(ctx, call.Job, func(ctx context.Context, j pluginhost.Job) error { return p.Check(ctx, j, pluginhost.Capability{}) })
	if err != nil {
		return "", errors.New("media_plugin_result_unavailable")
	}
	var result mediaObservation
	if json.Unmarshal(raw, &result) != nil || result.Text == "" {
		return "", errors.New("media_plugin_result_invalid")
	}
	if err := p.CheckCompleted(ctx); err != nil {
		return "", err
	}
	if result.Status == "partial" {
		return "[本次画面观察被模型截断，以下内容不完整]\n" + result.Text, nil
	}
	return result.Text, nil
}

// CheckCompleted revalidates the last result before a caller publishes it.
func (p *Visual) CheckCompleted(ctx context.Context) error {
	if p.completedJob.ID == "" || p.completedJob.State != "completed" {
		return errors.New("media_plugin_result_unavailable")
	}
	return p.Check(ctx, p.completedJob, pluginhost.Capability{})
}
