package station

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/billing"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/stationruntime"
	"wechat-bot/internal/workspacefs"
)

const stationChannelsCapability = "share.resolve_channels"

type stationShareHost struct {
	store      *pluginhost.Store
	controller *pluginruntime.Controller
	send       pluginruntime.RouteTransport
	postModel  pluginruntime.RouteTransport
	localASR   stationLocalASR
	usageMu    sync.Mutex
	usage      *billing.Store
}

func (s *stationWechat) shareHost(w *stationWechatWorker, account string) (*stationShareHost, error) {
	w.shareMu.Lock()
	defer w.shareMu.Unlock()
	if s.ctx.Err() != nil {
		return nil, s.ctx.Err()
	}
	if w.share != nil {
		if w.share.store.Account() != account {
			return nil, errors.New("share_account_changed")
		}
		return w.share, nil
	}
	dir := filepath.Join(w.dir, "project-tools")
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return nil, err
	}
	root.Close()
	store, err := pluginhost.OpenStore(dir, account)
	if err != nil {
		return nil, err
	}
	executor := stationruntime.InstalledToolExecutor(store, s.shareOptions, s.workspaceRoot)
	controller, err := pluginruntime.NewController(store, executor)
	if err != nil {
		store.Close()
		return nil, err
	}
	controller.SetIdleTimeout(5 * time.Minute)
	w.share = &stationShareHost{store: store, controller: controller, send: mcptools.PostPluginRoute, localASR: s.localASR}
	return w.share, nil
}

type stationSharePolicy struct {
	host        *stationShareHost
	binding     *wechatapp.BoundChannelsRoute
	invocation  pluginhost.Invocation
	installed   pluginhost.InstalledPackage
	config      int
	sourceCheck func(context.Context) error
}

func (p *stationSharePolicy) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	if j.Plugin != wechatapp.ShareReaderToolID || j.Tool != stationChannelsCapability || cap.Async || !reflect.DeepEqual(j.Context, p.invocation) || j.PackageDigest != p.installed.Digest || j.Version != p.installed.Package.Version || j.ConfigRevision != strconv.Itoa(p.config) {
		return errors.New("share_invocation_changed")
	}
	installed, err := p.host.store.Installed(ctx, j.Plugin)
	if err != nil || !installed.Enabled || installed.Removed || installed.Revision != p.installed.Revision || installed.Digest != p.installed.Digest {
		return errors.New("share_plugin_snapshot_changed")
	}
	config, err := p.host.store.Configuration(ctx, j.Plugin)
	if err != nil || config.Revision != p.config {
		return errors.New("share_plugin_snapshot_changed")
	}
	if err := pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: p.invocation.Account, Plugin: j.Plugin, Version: j.Version, Permissions: installed.Permissions}, j.Context, []string{"network"}); err != nil {
		return err
	}
	if j.ID != "" {
		current, err := p.host.store.Job(ctx, j.ID)
		if err != nil || current.Revision != j.Revision || current.CancelRequested {
			return errors.New("share_invocation_changed")
		}
	}
	return p.sourceCheck(ctx)
}

func (p *stationSharePolicy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	check := func(ctx context.Context) error { return p.Check(ctx, j, cap) }
	switch method {
	case "wechat/http_route":
		return p.binding.Call(ctx, p.host.store, j, cap, raw, check, p.host.send)
	case "wechat/channels_bridge":
		return wechatapp.CallBoundChannelsBridge(ctx, p.host.store, p.binding, j, raw, check)
	default:
		return nil, errors.New("share_host_service_unavailable")
	}
}

func (p *stationSharePolicy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	result, _, err := p.binding.ValidateResult(ctx, j, raw, func(ctx context.Context) error { return p.Check(ctx, j, cap) })
	return result, err
}

func (h *stationShareHost) resolve(ctx context.Context, a stationAttachmentAccess, original stationAttachmentSource, message linuxnative.Message, source linuxnative.ChannelsSource) (linuxnative.ChannelsSource, error) {
	empty := linuxnative.ChannelsSource{}
	installed, err := h.store.Installed(ctx, wechatapp.ShareReaderToolID)
	if err != nil || !installed.Enabled || installed.Removed {
		return empty, videoReadError("share_reader_not_enabled", "启用当前连接的分享解析工具")
	}
	configuration, err := h.store.Configuration(ctx, wechatapp.ShareReaderToolID)
	if err != nil {
		return empty, videoReadError("share_plugin_configuration_unavailable", "读取解析配置")
	}
	epoch, err := a.store.RecallEpoch(ctx)
	if err != nil {
		return empty, err
	}
	check := func(ctx context.Context) error {
		if !a.Allowed(ctx, original.Thread) {
			return wechatapp.ErrAttachmentDenied
		}
		current, err := a.source(ctx, original.Thread, original.Message)
		if err != nil || current != original {
			return wechatapp.ErrAttachmentDenied
		}
		currentEpoch, err := a.store.RecallEpoch(ctx)
		if err != nil || currentEpoch != epoch {
			return wechatapp.ErrAttachmentDenied
		}
		return ctx.Err()
	}
	if err = check(ctx); err != nil {
		return empty, err
	}
	_, bridge, err := wechatapp.ConfiguredChannelsBridge(ctx, h.store)
	if err != nil {
		return empty, err
	}
	args, link, err := wechatapp.ChannelsResolutionArguments(ctx, source, bridge, 0, func(ctx context.Context) (string, error) {
		if err := check(ctx); err != nil {
			return "", err
		}
		return a.native.ChannelsShareURL(ctx, message.ID, message.ThreadID, source.ObjectID)
	})
	if err != nil {
		return empty, videoReadError("share_source_resolution_unavailable", "按原卡片取得解析入口")
	}
	ref := companion.SourceRef{Account: original.Account, Thread: original.Thread, MessageID: original.Message, EventID: "native:" + original.Binding, Revision: 1}
	binding, err := wechatapp.NewBoundChannelsRoute(message, source, wechatapp.ChannelsEvent{Account: ref.Account, Thread: ref.Thread, Message: ref.MessageID, EventID: ref.EventID, Revision: ref.Revision}, link)
	if err != nil {
		return empty, videoReadError("share_source_binding_invalid", "校验原卡片解析入口")
	}
	key := localCheckpointKey(a.requester, a.requestTrace, original.id(), epoch, installed.Digest, installed.Revision, configuration.Revision, string(args))
	inv := pluginhost.Invocation{Account: original.Account, Thread: original.Thread, Requester: a.requester, Purpose: "resolve authorized received Channels video", RequestKey: "station-channels-" + key, SourceVersions: map[string]int{ref.EventID: 1}, EvidenceEpoch: epoch, Authorization: original.id(), OriginSourceSender: original.Sender, RequestThread: original.Thread}
	policy := &stationSharePolicy{host: h, binding: binding, invocation: inv, installed: installed, config: configuration.Revision, sourceCheck: check}
	if err = h.controller.EnsureStarted(ctx, wechatapp.ShareReaderToolID, installed.Revision); err != nil {
		return empty, videoReadError("share_reader_runtime_unavailable", "启动隔离解析工具")
	}
	call, err := h.controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: wechatapp.ShareReaderToolID, Capability: stationChannelsCapability, Context: inv, Arguments: args}, policy)
	if ctx.Err() != nil {
		return empty, ctx.Err()
	}
	if err != nil || call.Job.State != "completed" {
		return empty, videoReadError("share_plugin_resolution_incomplete", "解析原卡片")
	}
	private, err := h.store.PrivateJobResult(ctx, call.Job, func(ctx context.Context, j pluginhost.Job) error {
		return policy.Check(ctx, j, pluginhost.Capability{})
	})
	var resolved linuxnative.ChannelsSource
	if err != nil || json.Unmarshal(private, &resolved) != nil || !wechatapp.MatchesResolvedChannels(message, original.Account, source, resolved) {
		return empty, videoReadError("share_plugin_handoff_invalid", "核验解析结果")
	}
	return resolved, nil
}
