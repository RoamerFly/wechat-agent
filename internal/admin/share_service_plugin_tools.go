package admin

// The share-reader package has private resolver contracts (short links,
// cookies, export identifiers and media locations).  This file is the only
// Agent-facing façade: an Agent can select an incoming native share by message
// ID and page safe text/candidate *descriptions*, but cannot manufacture a
// resolver URL or recover a private playback URL.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

const shareServicePluginID = "share-reader"

func shareServiceCapability(id string) pluginhost.Capability {
	description := "Read one authorized incoming public share selected by source_message_id."
	schema := `{"type":"object","properties":{"source_message_id":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1},"candidate_offset":{"type":"integer","minimum":0},"candidate_limit":{"type":"integer","minimum":1},"timeout_seconds":{"type":"integer","minimum":1}},"required":["source_message_id"],"additionalProperties":false}`
	if id == "share.resolve_channels" {
		description = "Read one authorized incoming Channels share selected by source_message_id."
		// A verified Channels object has one resolved media candidate. It does
		// not have a remote candidate/text page API, so advertising pagination
		// here would silently ignore an Agent request.
		schema = `{"type":"object","properties":{"source_message_id":{"type":"string","minLength":1},"timeout_seconds":{"type":"integer","minimum":1}},"required":["source_message_id"],"additionalProperties":false}`
	}
	return pluginhost.Capability{
		ID:             id,
		Implementation: "ready",
		Discoverable:   true,
		Permissions:    []string{"network"},
		SideEffect:     "read",
		Description:    description + " The host re-reads the native card and returns untrusted text/candidate previews only; it never exposes resolver URLs, cookies, export IDs, playback locations, files or video observations.",
		InputSchema:    json.RawMessage(schema),
		OutputSchema:   json.RawMessage(`{"type":"object"}`),
	}
}

func shareServiceHasPaging(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return true
	}
	for _, field := range []string{"offset", "limit", "candidate_offset", "candidate_limit"} {
		if _, ok := fields[field]; ok {
			return true
		}
	}
	return false
}

// shareServiceCapabilityAvailable deliberately does not inspect a resolver
// cookie, bridge setting, or media-understanding installation. Discovery tells
// the Agent that an installed share reader exists. Invocation then reports the
// actual source/bridge/provider state instead of silently substituting another
// reader or claiming that video was watched.
func (p *permittedTools) shareServiceCapabilityAvailable(ctx context.Context) bool {
	return len(p.shareServiceCapabilities(ctx)) > 0
}

func (p *permittedTools) shareServiceCapabilities(ctx context.Context) []pluginhost.Capability {
	installed, err := p.manager.installedShareState(ctx)
	if !installed || err != nil {
		return nil
	}
	store, err := p.manager.packageStore()
	if err != nil {
		return nil
	}
	available := []pluginhost.Capability{}
	for _, cap := range mustCapabilities(ctx, store, shareServicePluginID) {
		if (cap.ID == "share.resolve_public" || cap.ID == "share.resolve_channels") && cap.Discoverable && cap.Implementation == "ready" && !cap.Async {
			available = append(available, shareServiceCapability(cap.ID))
		}
	}
	return available
}

type shareServiceArguments struct {
	SourceMessageID string `json:"source_message_id"`
	Offset          int    `json:"offset"`
	Limit           int    `json:"limit"`
	CandidateOffset int    `json:"candidate_offset"`
	CandidateLimit  int    `json:"candidate_limit"`
	Timeout         *int64 `json:"timeout_seconds"`
}

func decodeShareServiceArguments(raw json.RawMessage) (shareServiceArguments, error) {
	var in shareServiceArguments
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || in.SourceMessageID == "" || len(in.SourceMessageID) > 512 || in.Offset < 0 || in.Limit < 0 || in.CandidateOffset < 0 || in.CandidateLimit < 0 || in.Timeout != nil && (*in.Timeout < 1 || *in.Timeout > math.MaxInt64/int64(time.Second)) {
		return shareServiceArguments{}, errors.New("share_service_arguments_invalid")
	}
	if in.Limit == 0 {
		in.Limit = 14000
	}
	if in.CandidateLimit == 0 {
		in.CandidateLimit = 20
	}
	return in, nil
}

func (in shareServiceArguments) timeoutFor(capability string) int64 {
	if in.Timeout != nil {
		return *in.Timeout
	}
	if capability == "share.resolve_channels" {
		return 25 // documented provider default, not a ceiling for explicit input
	}
	return 30
}

// shareServiceSource makes the message id a selector, never evidence supplied
// by the model.  Both resolver policies below re-read it again before every
// remote step, which protects source edits, cancellation, forgetting and
// authorization changes while an asynchronous plugin job is in progress.
func (p *permittedTools) shareServiceSource(ctx context.Context, id string) (linuxnative.Message, error) {
	msg, err := p.manager.native.Message(ctx, id)
	if err != nil {
		return linuxnative.Message{}, errors.New("share_service_source_unavailable")
	}
	if msg.AccountID == "" {
		msg.AccountID = p.manager.native.Account()
	}
	if msg.ID != id || msg.AccountID != p.trace.Account || msg.Direction != "incoming" || msg.Type != 49 || msg.AppMessageType == nil || !p.conversationReadAllowed(msg.ThreadID) {
		return linuxnative.Message{}, errors.New("share_service_source_not_authorized")
	}
	if _, err := wechatapp.ShareReadCapability(msg); err != nil {
		return linuxnative.Message{}, err
	}
	return msg, nil
}

func pageShareText(text string, offset, limit int) (string, int, *int) {
	runes := []rune(text)
	end := offset + limit
	if end > len(runes) {
		end = len(runes)
	}
	var next *int
	if end < len(runes) {
		n := end
		next = &n
	}
	return string(runes[offset:end]), len(runes), next
}

func pageShareCandidates(total, offset, limit int) ([]map[string]any, *int) {
	end := offset + limit
	if end > total {
		end = total
	}
	items := make([]map[string]any, 0, end-offset)
	for n := offset; n < end; n++ {
		// A position proves only that a verified resolver found a candidate.
		// It intentionally does not disclose the opaque locator.
		items = append(items, map[string]any{"position": n, "kind": "media_candidate", "video_observed": false, "media_downloaded": false})
	}
	var next *int
	if end < total {
		n := end
		next = &n
	}
	return items, next
}

func publicShareFacadeResult(message linuxnative.Message, observation publicShareObservation, in shareServiceArguments) map[string]any {
	title := observation.Title
	if title == "" && message.Link != nil {
		title = message.Link.Title
	}
	text, total, next := pageShareText(observation.Text, in.Offset, in.Limit)
	candidates, nextCandidate := pageShareCandidates(len(observation.Locators), in.CandidateOffset, in.CandidateLimit)
	coverage := "card_and_media_candidates"
	if observation.Coverage == "page_text" {
		coverage = "page_text"
	}
	return map[string]any{
		"source_message_id":          message.ID,
		"source_kind":                "public_share",
		"title":                      title,
		"text":                       text,
		"offset":                     in.Offset,
		"requested_limit":            in.Limit,
		"total_characters":           total,
		"next_offset":                next,
		"candidate_previews":         candidates,
		"candidate_offset":           in.CandidateOffset,
		"requested_candidate_limit":  in.CandidateLimit,
		"total_candidates":           len(observation.Locators),
		"next_candidate_offset":      nextCandidate,
		"coverage":                   coverage,
		"effective_timeout_seconds":  in.timeoutFor("share.resolve_public"),
		"video_observed":             false,
		"media_downloaded":           false,
		"untrusted_external_content": true,
		"complete":                   next == nil && nextCandidate == nil,
	}
}

func channelsShareFacadeResult(message linuxnative.Message, source linuxnative.ChannelsSource, in shareServiceArguments) map[string]any {
	title, author := source.Title, source.Author
	if message.Link != nil {
		if title == "" {
			title = message.Link.Title
		}
		if author == "" {
			author = message.Link.SourceName
		}
	}
	// The policy validated a private media location, but this output is only a
	// candidate preview. Never project that location, object ID or decode key.
	total := 0
	if source.MediaURL != "" {
		total = 1
	}
	candidates, next := pageShareCandidates(total, in.CandidateOffset, in.CandidateLimit)
	return map[string]any{
		"source_message_id":          message.ID,
		"source_kind":                "channels_share",
		"title":                      title,
		"author":                     author,
		"text":                       "",
		"offset":                     in.Offset,
		"requested_limit":            in.Limit,
		"total_characters":           0,
		"next_offset":                nil,
		"candidate_previews":         candidates,
		"candidate_offset":           in.CandidateOffset,
		"requested_candidate_limit":  in.CandidateLimit,
		"total_candidates":           total,
		"next_candidate_offset":      next,
		"coverage":                   "card_and_media_candidates",
		"effective_timeout_seconds":  in.timeoutFor("share.resolve_channels"),
		"candidate_pagination":       "single_verified_media_candidate",
		"video_observed":             false,
		"media_downloaded":           false,
		"untrusted_external_content": true,
		"complete":                   next == nil,
	}
}

// resolvePublicShareFacade uses the existing public-share policy and validator,
// but binds every explicit page selector into the durable request.  A second
// page therefore cannot be mistaken for a retry of page zero, and no requested
// range is silently replaced by the policy's defaults.
func resolvePublicShareFacade(ctx context.Context, policy *publicSharePolicy, in shareServiceArguments) (publicShareObservation, pluginhost.Job, error) {
	var result publicShareObservation
	policy.requestSuffix = "facade-" + agentID(strconv.Itoa(in.Offset), strconv.Itoa(in.Limit), strconv.Itoa(in.CandidateOffset), strconv.Itoa(in.CandidateLimit), strconv.FormatInt(in.timeoutFor("share.resolve_public"), 10))
	invocation := policy.invocation()
	job := pluginhost.Job{Plugin: shareServicePluginID, Tool: "share.resolve_public", Context: invocation}
	if err := policy.Check(ctx, job, pluginhost.Capability{}); err != nil {
		return result, job, err
	}
	h, ready, err := policy.manager.installedPluginRuntime()
	if err != nil || !ready {
		return result, job, errors.New("share_plugin_runtime_unavailable")
	}
	installed, err := policy.store.Installed(ctx, job.Plugin)
	if err != nil {
		return result, job, err
	}
	if err = h.EnsureStarted(ctx, job.Plugin, installed.Revision); err != nil {
		return result, job, err
	}
	args, _ := json.Marshal(map[string]any{"url": policy.message.Link.URL, "video_required": *policy.message.AppMessageType == 4, "offset": in.Offset, "limit": in.Limit, "candidate_offset": in.CandidateOffset, "candidate_limit": in.CandidateLimit, "timeout_seconds": in.timeoutFor("share.resolve_public")})
	out, err := h.Invoke(ctx, pluginruntime.CallRequest{Plugin: job.Plugin, Capability: job.Tool, Context: job.Context, Arguments: args}, policy)
	if err != nil {
		return result, out.Job, errors.New("share_plugin_resolution_incomplete")
	}
	if out.Job.State != "completed" {
		if out.Job.State == "failed" {
			var failure struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(out.Job.Result, &failure) == nil && publicShareErrorCode.MatchString(failure.Error) {
				return result, out.Job, errors.New(failure.Error)
			}
		}
		return result, out.Job, errors.New("share_plugin_resolution_needs_review")
	}
	private, err := policy.store.PrivateJobResult(ctx, out.Job, func(ctx context.Context, current pluginhost.Job) error {
		return policy.Check(ctx, current, pluginhost.Capability{})
	})
	if err != nil || json.Unmarshal(private, &result) != nil {
		return result, out.Job, errors.New("share_plugin_handoff_unavailable")
	}
	return result, out.Job, nil
}

func (p *permittedTools) callShareServicePlugin(ctx context.Context, capability, requestKey string, raw json.RawMessage) (any, error) {
	if capability != "share.resolve_public" && capability != "share.resolve_channels" || !p.shareServiceCapabilityAvailable(ctx) {
		return nil, errors.New("share_service_capability_unavailable")
	}
	in, err := decodeShareServiceArguments(raw)
	if err != nil {
		return nil, err
	}
	normalized, _ := json.Marshal(in)
	message, err := p.shareServiceSource(ctx, in.SourceMessageID)
	if err != nil {
		return nil, err
	}
	ctx = withSourceActor(ctx, p, agentID("service-share", requestKey, capability, message.ID, string(normalized)), message.ThreadID)
	resolver, err := wechatapp.ShareReadCapability(message)
	if err != nil {
		return nil, err
	}
	if capability != resolver {
		return nil, errors.New("share_service_capability_source_mismatch")
	}
	switch resolver {
	case "share.resolve_public":
		policy, err := p.manager.newPublicSharePolicy(ctx, message)
		if err != nil {
			return nil, err
		}
		observation, _, err := resolvePublicShareFacade(ctx, policy, in)
		if err != nil {
			return nil, err
		}
		if in.Offset > utf8.RuneCountInString(observation.Text) || in.CandidateOffset > len(observation.Locators) {
			return nil, errors.New("share_service_offset_outside_results")
		}
		return publicShareFacadeResult(message, observation, in), nil
	case "share.resolve_channels":
		if shareServiceHasPaging(raw) {
			return nil, errors.New("share_service_channels_pagination_unsupported")
		}
		reader, ok := p.manager.native.(channelsReader)
		if !ok {
			return nil, errors.New("channels_source_backend_missing")
		}
		source, err := wechatapp.ReadChannelsSource(ctx, message, p.trace.Account, reader)
		if err != nil {
			return nil, errors.New("channels_source_scope_mismatch")
		}
		policy, err := p.manager.newReceivedSharePolicy(ctx, message)
		if err != nil {
			return nil, err
		}
		policy.requestSuffix = "facade-" + agentID(strconv.FormatInt(in.timeoutFor("share.resolve_channels"), 10))
		policy.timeoutSeconds = in.timeoutFor("share.resolve_channels")
		resolved, err := policy.resolve(ctx, source)
		if err != nil {
			return nil, err
		}
		candidateTotal := 0
		if resolved.MediaURL != "" {
			candidateTotal = 1
		}
		if in.CandidateOffset > candidateTotal {
			return nil, errors.New("share_service_offset_outside_results")
		}
		return channelsShareFacadeResult(message, resolved, in), nil
	}
	return nil, errors.New("share_service_source_kind_unsupported")
}
