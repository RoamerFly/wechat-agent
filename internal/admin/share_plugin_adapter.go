package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func (m *Manager) installedShareState(ctx context.Context) (bool, error) {
	return m.installedServiceState(ctx, "share-reader", "share")
}

func (m *Manager) installedServiceState(ctx context.Context, id, prefix string) (bool, error) {
	s, err := m.packageStore()
	if err != nil {
		return true, errors.New(prefix + "_plugin_store_unavailable")
	}
	i, err := s.Installed(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, errors.New(prefix + "_plugin_store_unavailable")
	}
	if !i.Enabled || i.Removed {
		return true, errors.New(prefix + "_plugin_disabled")
	}
	return true, nil
}

type receivedSharePolicy struct {
	manager            *Manager
	store              *pluginhost.Store
	message            linuxnative.Message
	source             companion.SourceRef
	fingerprint, epoch string
	remember           bool
	tool               string
	requestSuffix      string
	// resolutionSnapshotSuffix distinguishes immutable read-only share parses
	// from a stale job that happened to use the same source message.  It is
	// deliberately separate from requestSuffix: callers may already use that
	// suffix for their own request scope, and recomputing from the combined
	// value would make an identical retry look new.
	resolutionSnapshotSuffix string
	timeoutSeconds           int64
	actor                    *sourceActorProof
	binding                  *wechatapp.BoundChannelsRoute
	send                     pluginruntime.RouteTransport
	failureMu                sync.Mutex
	failure                  *shareResolutionFailure
}

// Only host-observed, reviewed diagnostics cross this boundary. Provider text,
// URLs, credentials and arbitrary executor errors must never reach the journal.
type shareResolutionFailure struct {
	code, stage string
	status      int
	provider    *int64
}

func (e *shareResolutionFailure) Error() string { return e.code }
func (e *shareResolutionFailure) SafeToolFailure() (string, string) {
	return e.code, "分享解析未完成；请根据日志中的解析阶段、HTTP状态和安全错误码检查服务状态。原任务未自动重提。"
}

func safeShareResolutionFailure(stage string, err error) *shareResolutionFailure {
	switch stage {
	case "channels_parse", "channels_feed", "channels_shortlink", "channels_bridge", "result_validation", "plugin_invoke":
	default:
		stage = "host_callback"
	}
	failure := &shareResolutionFailure{code: "share_plugin_resolution_incomplete", stage: stage}
	if err == nil {
		return failure
	}
	if errors.Is(err, context.Canceled) {
		failure.code = "share_resolution_cancelled"
		return failure
	}
	if errors.Is(err, context.DeadlineExceeded) {
		failure.code = "share_resolution_timeout"
		return failure
	}
	// CreateJob protects an immutable request key from being rebound to
	// different arguments or a changed package/configuration snapshot.  This is
	// actionable for a read-only parser, but remains deliberately opaque about
	// the conflicting values.
	if errors.Is(err, pluginhost.ErrRequestConflict) {
		failure.code = "share_request_conflict"
		return failure
	}
	var web *mcptools.WebError
	if errors.As(err, &web) {
		switch web.Code {
		case "plugin_route_network_failed", "plugin_route_http_rejected", "plugin_route_response_failed", "plugin_route_response_invalid", "response_too_large":
			failure.code = "share_" + web.Code
			if web.Status >= 100 && web.Status <= 599 {
				failure.status = web.Status
			}
		}
		return failure
	}
	switch err.Error() {
	case "share_native_source_required", "share_source_scope_mismatch", "share_persisted_source_unavailable", "source_actor_not_authorized", "source_actor_authorization_changed", "share_request_authorization_changed", "share_plugin_disabled", "share_plugin_snapshot_changed", "share_invocation_changed", "share_source_changed", "share_native_source_unavailable", "share_native_source_changed", "share_source_binding_unavailable", "share_source_binding_changed", "share_route_out_of_order", "share_route_source_mismatch", "share_route_chain_finished", "share_route_response_invalid", "share_route_response_binding_failed", "share_result_binding_failed", "share_bridge_unavailable", "share_bridge_rejected", "share_bridge_response_invalid", "share_bridge_source_mismatch", "share_bridge_timeout_invalid", "share_bridge_already_consumed", "share_bridge_configuration_unavailable", "share_bridge_configuration_invalid":
		failure.code = err.Error()
	}
	return failure
}

func (p *receivedSharePolicy) recordResolutionFailure(j pluginhost.Job, failure *shareResolutionFailure) *shareResolutionFailure {
	p.failureMu.Lock()
	defer p.failureMu.Unlock()
	// A failed callback is more specific than its later failed result validation.
	if p.failure != nil {
		return p.failure
	}
	p.failure = failure
	note := ""
	if failure.provider != nil {
		note = "provider_code=" + strconv.FormatInt(*failure.provider, 10)
	}
	p.manager.record(bot.Record{Event: "share_resolution_failed", Category: "error", ThreadID: p.message.ThreadID, MessageID: p.message.ID, CallID: j.ID, Tool: j.Tool, Status: "needs_review", Reason: failure.code, ToolCode: failure.code, ToolStage: failure.stage, ToolHTTPStatus: failure.status, DecisionNote: note, Origin: "share"})
	return failure
}

func (m *Manager) newReceivedSharePolicy(ctx context.Context, message linuxnative.Message) (*receivedSharePolicy, error) {
	return m.newReceivedSharePolicyFor(ctx, message, "share.resolve_channels")
}

func (m *Manager) newReceivedSharePolicyFor(ctx context.Context, message linuxnative.Message, tool string) (*receivedSharePolicy, error) {
	return m.newReceivedSharePolicyInner(ctx, message, tool, false)
}

// newExplicitPublicURLSourcePolicy uses an actual incoming message solely as
// the source/actor proof for a caller-supplied public URL. It deliberately
// does not change that native message's type, body or link into a pretend
// share card: the requested URL is bound separately by publicSharePolicy.
func (m *Manager) newExplicitPublicURLSourcePolicy(ctx context.Context, message linuxnative.Message) (*receivedSharePolicy, error) {
	return m.newReceivedSharePolicyInner(ctx, message, "share.resolve_public", true)
}

func (m *Manager) newReceivedSharePolicyInner(ctx context.Context, message linuxnative.Message, tool string, explicitPublicURL bool) (*receivedSharePolicy, error) {
	resolver, _ := wechatapp.ShareReadCapability(message)
	validKind := resolver != "" && resolver == tool || tool == "media.observe" && (message.Type == 3 || message.Type == 47 || message.Type == 43 || message.Type == 49 && message.AppMessageType != nil && (*message.AppMessageType == 4 || *message.AppMessageType == 5 || *message.AppMessageType == 51)) || tool == "media.transcribe" && (message.Type == 34 || message.Type == 43 || message.Type == 49) || tool == "media.listen" && message.Type == 34
	if explicitPublicURL {
		validKind = tool == "share.resolve_public"
	}
	if m.memory == nil || message.ID == "" || message.ThreadID == "" || message.SenderID == "" || message.Direction != "incoming" || !validKind {
		return nil, errors.New("share_native_source_required")
	}
	if message.AccountID == "" {
		message.AccountID = m.native.Account()
	}
	if message.AccountID != m.native.Account() {
		return nil, errors.New("share_source_scope_mismatch")
	}
	store, err := m.packageStore()
	if err != nil {
		return nil, err
	}
	epoch, err := m.memory.RecallEpoch(ctx)
	if err != nil {
		return nil, err
	}
	p := &receivedSharePolicy{manager: m, store: store, message: message, tool: tool, fingerprint: wechatapp.ReceivedSourceFingerprint(message, tool), epoch: epoch, remember: m.Config().Companion.Permissions.Remember, send: mcptools.PostPluginRoute}
	if actor, ok := sourceActorFromContext(ctx); ok {
		// The proof is useful only for the exact account and source conversation
		// it was minted for. Do not downgrade a mismatched explicit request into
		// an automatic one.
		if actor.account != message.AccountID || actor.sourceThread != message.ThreadID || !actor.active() {
			return nil, errors.New("source_actor_not_authorized")
		}
		p.actor = &actor
	}
	if p.remember {
		ref, ok := companion.MediaSourceFromContext(ctx)
		if !ok {
			// Explicit rereads can resolve an existing source, never create one
			// from model arguments or infer identity from a neighboring message.
			in := companion.Input{Channel: "wechat", Account: message.AccountID, Thread: message.ThreadID, Sender: message.SenderID, TraceID: message.ID, MessageTime: message.CreatedAt, Source: &companion.SourceRef{InstanceID: m.memory.SourceInstanceID(), MessageID: message.ID, ServerID: message.ServerID}}
			found, valid := m.memory.SourceForInput(in)
			if !valid {
				// The caller has read the actual native message. Repair only the
				// old false collision with an unknown legacy namespace; explicit
				// quarantine, forgetting and current-dataset conflicts stay blocked.
				found, valid, err = m.memory.ReconcileLegacySource(ctx, in)
				if err != nil {
					return nil, err
				}
			}
			if !valid {
				return nil, errors.New("share_persisted_source_unavailable")
			}
			ref = *found
		}
		if ref.Account != message.AccountID || ref.Thread != message.ThreadID || ref.MessageID != message.ID || ref.Sender != message.SenderID {
			return nil, errors.New("share_source_scope_mismatch")
		}
		p.source = ref
	} else {
		p.source = companion.SourceRef{Account: message.AccountID, Thread: message.ThreadID, Sender: message.SenderID, MessageID: message.ID, ServerID: message.ServerID, EventID: "native:" + p.fingerprint, Revision: 1, MessageType: message.Type, OccurredAt: message.CreatedAt}
	}
	return p, nil
}

func (p *receivedSharePolicy) invocation() pluginhost.Invocation {
	c := pluginhost.Invocation{Account: p.message.AccountID, Thread: p.message.ThreadID, Requester: p.message.SenderID, Purpose: "read received video share", RequestKey: "received-share-" + agentID(p.source.EventID, p.message.ID), SourceVersions: map[string]int{p.source.EventID: p.source.Revision}, EvidenceEpoch: p.epoch, Authorization: p.fingerprint}
	if p.actor != nil {
		c.Requester = p.actor.actor
		c.OriginSourceSender = p.message.SenderID
		c.RequestThread = p.actor.requestThread
		c.RequestKey += "-actor-" + agentID(p.actor.actor, p.actor.requestThread, p.actor.taskID, p.actor.requestKey, p.message.ID, p.tool)
	}
	if p.requestSuffix != "" {
		c.RequestKey += "-" + p.requestSuffix
	}
	if p.resolutionSnapshotSuffix != "" {
		c.RequestKey += "-" + p.resolutionSnapshotSuffix
	}
	if p.tool == "media.observe" || p.tool == "media.transcribe" || p.tool == "media.listen" {
		c.Purpose = "observe received media"
		parts := []string{p.source.EventID, p.message.ID, p.fingerprint, p.requestSuffix, p.epoch, strconv.Itoa(p.source.Revision)}
		if p.actor != nil {
			parts = append(parts, p.actor.actor, p.actor.requestThread, p.actor.taskID, p.actor.requestKey, p.tool)
		}
		c.RequestKey = "received-media-" + agentID(parts...)
	}
	return c
}

// setReadOnlyResolutionSnapshot makes the request key track every immutable
// input which the runtime stores in its job fingerprint.  It is intentionally
// limited to share.resolve_channels: other tools have distinct side-effect and
// replay contracts.  The digest is a one-way identifier, so neither a short
// link nor private configuration is exposed in the durable request key.
func (p *receivedSharePolicy) setReadOnlyResolutionSnapshot(raw json.RawMessage, installed pluginhost.InstalledPackage, configuration pluginhost.Configuration) {
	if p.tool != "share.resolve_channels" {
		return
	}
	p.resolutionSnapshotSuffix = "snapshot-" + agentID(
		"share.resolve_channels.readonly.v1",
		p.requestSuffix,
		string(raw),
		p.source.EventID,
		strconv.Itoa(p.source.Revision),
		p.epoch,
		p.fingerprint,
		installed.Package.Version,
		installed.Digest,
		strconv.Itoa(configuration.Revision),
	)
}

func (p *receivedSharePolicy) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.actor != nil && !p.actor.active() {
		return errors.New("source_actor_authorization_changed")
	}
	c := p.manager.Config().Config
	plugin, readAllowed := "share-reader", c.Behavior.ReadVideos
	if p.tool == "media.observe" || p.tool == "media.transcribe" || p.tool == "media.listen" {
		plugin = "media-understanding"
		if p.tool == "media.transcribe" || p.tool == "media.listen" {
			readAllowed = p.message.Type == 34 || c.Behavior.ReadVideos
		} else if p.message.Type == 3 || p.message.Type == 47 {
			readAllowed = c.Behavior.ReadImages
		}
	}
	if j.Plugin != plugin || j.Tool != p.tool || cap.Async || !c.Enabled || !readAllowed || !permittedRecipient(c, p.message.ThreadID) || c.Companion.Permissions.Remember != p.remember || p.manager.native.Account() != p.message.AccountID {
		return errors.New("share_request_authorization_changed")
	}
	if strings.HasSuffix(p.message.ThreadID, "@chatroom") && !c.Companion.Permissions.ObserveGroups {
		return errors.New("share_request_authorization_changed")
	}
	i, err := p.store.Installed(ctx, j.Plugin)
	if err != nil || !i.Enabled || i.Removed {
		return errors.New("share_plugin_disabled")
	}
	// A job created by the plugin runtime has an immutable package/config
	// snapshot.  The direct host callbacks do not go through the normal HTTP
	// route proxy, so they must enforce that snapshot here as well.  In
	// particular, a bridge request must not keep using an endpoint after an
	// administrator changed the installed plugin configuration.
	if j.PackageDigest != "" {
		if i.Digest != j.PackageDigest || i.Package.Version != j.Version {
			return errors.New("share_plugin_snapshot_changed")
		}
		configuration, configErr := p.store.ConfigurationForExecution(ctx, j.Plugin)
		if configErr != nil || j.ConfigRevision != strconv.Itoa(configuration.Revision) {
			return errors.New("share_plugin_snapshot_changed")
		}
		required := cap.Permissions
		if len(required) == 0 {
			capabilities, capabilitiesErr := p.store.Capabilities(ctx, j.Plugin)
			if capabilitiesErr != nil {
				return errors.New("share_plugin_snapshot_changed")
			}
			for _, available := range capabilities {
				if available.ID == j.Tool {
					required = available.Permissions
					break
				}
			}
		}
		if len(required) == 0 || pluginhost.Authorize(i.Package, pluginhost.Grant{Account: p.message.AccountID, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, j.Context, required) != nil {
			return errors.New("share_plugin_snapshot_changed")
		}
	}
	expected := p.invocation()
	a, _ := json.Marshal(j.Context)
	b, _ := json.Marshal(expected)
	if string(a) != string(b) {
		return errors.New("share_invocation_changed")
	}
	epoch, err := p.manager.memory.RecallEpoch(ctx)
	if err != nil || epoch != p.epoch {
		return errors.New("share_source_changed")
	}
	if p.remember {
		ev, err := p.manager.memory.ReadEvent(ctx, []string{companion.ScopeID("wechat", p.message.AccountID, p.message.ThreadID)}, p.source.EventID)
		if err != nil || ev.Revision != p.source.Revision || ev.Sender != p.message.SenderID || ev.Trace != p.message.ID || ev.Hash != p.source.BodyHash {
			return errors.New("share_source_changed")
		}
	}
	current, err := p.manager.native.Message(ctx, p.message.ID)
	if err != nil {
		return errors.New("share_native_source_unavailable")
	}
	if current.AccountID == "" {
		current.AccountID = p.manager.native.Account()
	}
	if wechatapp.ReceivedSourceFingerprint(current, p.tool) != p.fingerprint {
		return errors.New("share_native_source_changed")
	}
	return nil
}

func (p *receivedSharePolicy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (result json.RawMessage, err error) {
	stage := "host_callback"
	var route struct {
		Route string `json:"route"`
	}
	if method == "wechat/http_route" && json.Unmarshal(raw, &route) == nil {
		stage = strings.ReplaceAll(route.Route, "-", "_")
	} else if method == "wechat/channels_bridge" {
		stage = "channels_bridge"
	}
	defer func() {
		if err != nil {
			p.recordResolutionFailure(j, safeShareResolutionFailure(stage, err))
		}
	}()
	if method == "wechat/channels_bridge" {
		if p.binding == nil || p.message.AppMessageType == nil || *p.message.AppMessageType != 51 {
			return nil, errors.New("share_bridge_unavailable")
		}
		return wechatapp.CallBoundChannelsBridge(ctx, p.store, p.binding, j, raw, func(ctx context.Context) error { return p.Check(ctx, j, cap) })
	}
	if method != "wechat/http_route" || p.binding == nil {
		return nil, errors.New("share_host_service_unavailable")
	}
	return p.binding.Call(ctx, p.store, j, cap, raw, func(ctx context.Context) error { return p.Check(ctx, j, cap) }, func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, maximum int64, timeout time.Duration) (json.RawMessage, error) {
		reply, sendErr := p.send(ctx, endpoint, headers, body, maximum, timeout)
		if sendErr != nil {
			p.recordResolutionFailure(j, safeShareResolutionFailure(stage, sendErr))
		} else {
			var provider struct {
				Code    *int64 `json:"code"`
				ErrCode *int64 `json:"errCode"`
			}
			if json.Unmarshal(reply, &provider) == nil {
				code := provider.ErrCode
				if route.Route == "channels-parse" {
					code = provider.Code
				}
				if code != nil && *code != 0 {
					p.recordResolutionFailure(j, &shareResolutionFailure{code: "share_route_provider_rejected", stage: stage, provider: code})
				}
			}
		}
		return reply, sendErr
	})
}

func (p *receivedSharePolicy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (result pluginruntime.VerifiedResult, err error) {
	defer func() {
		if err != nil {
			err = p.recordResolutionFailure(j, safeShareResolutionFailure("result_validation", err))
		}
	}()
	if p.binding == nil {
		return pluginruntime.VerifiedResult{}, errors.New("share_source_binding_unavailable")
	}
	verified, _, err := p.binding.ValidateResult(ctx, j, raw, func(ctx context.Context) error { return p.Check(ctx, j, cap) })
	return verified, err
}

func (m *Manager) resolveInstalledChannels(ctx context.Context, message linuxnative.Message, source linuxnative.ChannelsSource) (linuxnative.ChannelsSource, error) {
	p, err := m.newReceivedSharePolicy(ctx, message)
	if err != nil {
		return source, err
	}
	return p.resolve(ctx, source)
}

func (p *receivedSharePolicy) resolve(ctx context.Context, source linuxnative.ChannelsSource) (linuxnative.ChannelsSource, error) {
	m, message := p.manager, p.message
	// Re-read the exact message before choosing any parser route, including
	// when this request is a delayed or explicitly retried read.
	reader, ok := m.native.(channelsReader)
	if !ok {
		return source, errors.New("channels_source_backend_missing")
	}
	if err := wechatapp.RecheckChannelsSource(ctx, message, m.native.Account(), reader, source); err != nil {
		return source, err
	}
	preflight := pluginhost.Job{Plugin: "share-reader", Tool: "share.resolve_channels", Context: p.invocation()}
	if err := p.Check(ctx, preflight, pluginhost.Capability{}); err != nil {
		return source, err
	}
	_, bridgeConfigured, err := wechatapp.ConfiguredChannelsBridge(ctx, p.store)
	if err != nil {
		return source, err
	}
	raw, link, err := wechatapp.ChannelsResolutionArguments(ctx, source, bridgeConfigured, p.timeoutSeconds, func(ctx context.Context) (string, error) {
		native, ok := m.native.(interface {
			ChannelsShareURL(context.Context, string, string, string) (string, error)
		})
		if !ok {
			return "", errors.New("share_native_shortlink_required")
		}
		// A legacy short link is only a fallback for this bound message.
		if legacy, found, err := legacyShareBindingURL(m.dir, source); err != nil || found {
			return legacy, err
		}
		link, err := native.ChannelsShareURL(ctx, message.ID, message.ThreadID, source.ObjectID)
		if err != nil {
			return "", errors.New("share_native_shortlink_unavailable")
		}
		return link, nil
	})
	if err != nil {
		return source, err
	}
	p.binding, err = wechatapp.NewBoundChannelsRoute(message, source, wechatapp.ChannelsEvent{Account: p.source.Account, Thread: p.source.Thread, Message: p.source.MessageID, EventID: p.source.EventID, Revision: p.source.Revision}, link)
	if err != nil {
		return source, err
	}
	h, ready, err := m.installedPluginRuntime()
	if err != nil || !ready {
		return source, errors.New("share_plugin_runtime_unavailable")
	}
	i, err := p.store.Installed(ctx, "share-reader")
	if err != nil {
		return source, err
	}
	configuration, err := p.store.Configuration(ctx, "share-reader")
	if err != nil {
		return source, errors.New("share_plugin_configuration_unavailable")
	}
	// This must happen before both Check calls.  The runtime persists the
	// resulting invocation and rechecks it around dispatch, so a retry with an
	// identical immutable parser request deduplicates while a changed read-only
	// request cannot collide with an older job for the same received card.
	p.setReadOnlyResolutionSnapshot(raw, i, configuration)
	job := pluginhost.Job{Plugin: "share-reader", Tool: "share.resolve_channels", Context: p.invocation()}
	if err = p.Check(ctx, job, pluginhost.Capability{}); err != nil {
		return source, err
	}
	if err = h.EnsureStarted(ctx, "share-reader", i.Revision); err != nil {
		return source, err
	}
	out, err := h.Invoke(ctx, pluginruntime.CallRequest{Plugin: job.Plugin, Capability: job.Tool, Context: job.Context, Arguments: raw}, p)
	if err != nil {
		return source, p.recordResolutionFailure(out.Job, safeShareResolutionFailure("plugin_invoke", err))
	}
	if out.Job.State != "completed" {
		return source, errors.New("share_plugin_resolution_needs_review")
	}
	private, err := p.store.PrivateJobResult(ctx, out.Job, func(ctx context.Context, j pluginhost.Job) error { return p.Check(ctx, j, pluginhost.Capability{}) })
	if err != nil {
		return source, errors.New("share_plugin_handoff_unavailable")
	}
	var resolved linuxnative.ChannelsSource
	if json.Unmarshal(private, &resolved) != nil || !wechatapp.MatchesResolvedChannels(message, p.message.AccountID, source, resolved) {
		return source, errors.New("share_plugin_handoff_invalid")
	}
	return resolved, nil
}
