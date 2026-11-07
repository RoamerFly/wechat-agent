package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

// BoundChannelsRoute is the host-owned, source-bound resolver chain for one
// incoming Channels card. It never accepts a route constructed from model
// arguments, and it retains no provider credentials.
type BoundChannelsRoute struct {
	mu    sync.Mutex
	route ChannelsRoute
}

func NewBoundChannelsRoute(message linuxnative.Message, source linuxnative.ChannelsSource, identity ChannelsEvent, verifiedShare string) (*BoundChannelsRoute, error) {
	route, err := NewChannelsRoute(message, source, identity, verifiedShare)
	if err != nil {
		return nil, err
	}
	return &BoundChannelsRoute{route: route}, nil
}

// Call performs one verified parser transition. Uncertain submissions poison
// the chain, so a POST can never be silently replayed against another card.
func (b *BoundChannelsRoute) Call(ctx context.Context, store *pluginhost.Store, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage, check func(context.Context) error, send pluginruntime.RouteTransport) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if check == nil || send == nil || j.Context.Account != b.route.Account || j.Context.Thread != b.route.Thread || j.Context.SourceVersions[b.route.Event] != b.route.Revision || j.Tool != "share.resolve_channels" || cap.ID != j.Tool {
		return nil, errors.New("share_source_binding_changed")
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	phase, err := b.route.ExpectedRequest(raw)
	if err != nil {
		return nil, err
	}
	b.route.Phase = "failed"
	result, err := pluginruntime.ProxyHTTPRoute(ctx, store, j, cap, raw, func(string, json.RawMessage) error { return nil }, send)
	if err != nil {
		return nil, err
	}
	if err = check(ctx); err != nil {
		return nil, err
	}
	if err = b.route.Accept(phase, result); err != nil {
		return nil, err
	}
	return result, nil
}

// ValidateResult releases a sanitized checkpoint plus a private host handoff.
// A locator only proves that a subsequent media read may begin.
func (b *BoundChannelsRoute) ValidateResult(ctx context.Context, j pluginhost.Job, raw json.RawMessage, check func(context.Context) error) (pluginruntime.VerifiedResult, linuxnative.ChannelsSource, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fail := func() (pluginruntime.VerifiedResult, linuxnative.ChannelsSource, error) {
		return pluginruntime.VerifiedResult{}, linuxnative.ChannelsSource{}, errors.New("share_result_binding_failed")
	}
	if check == nil || b.route.Phase != "complete" || b.route.Resolved == nil || j.Context.Account != b.route.Account || j.Context.Thread != b.route.Thread || j.Context.SourceVersions[b.route.Event] != b.route.Revision || j.Tool != "share.resolve_channels" {
		return fail()
	}
	if err := check(ctx); err != nil {
		return fail()
	}
	checkpoint, source, err := b.route.ValidateResult(raw)
	if err != nil {
		return fail()
	}
	if err := check(ctx); err != nil {
		return fail()
	}
	private, err := json.Marshal(source)
	if err != nil {
		return fail()
	}
	return pluginruntime.VerifiedResult{Result: checkpoint, Private: private}, source, nil
}
