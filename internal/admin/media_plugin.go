package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaobserve"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

type mediaPluginPolicy struct {
	*mediaobserve.Visual
	receivedSharePolicy *receivedSharePolicy
	store               *pluginhost.Store
	post                pluginruntime.RouteTransport
}

func (m *Manager) newMediaPluginPolicy(ctx context.Context, message linuxnative.Message, prompt string, images []deepseek.Image, tokens int) (*mediaPluginPolicy, error) {
	base, err := m.newReceivedSharePolicyFor(ctx, message, "media.observe")
	if err != nil {
		return nil, err
	}
	p, err := newLegacyVisualPolicy(ctx, base.store, base.source.Revision, prompt, images, tokens)
	if err != nil {
		return nil, err
	}
	p.receivedSharePolicy = base
	base.requestSuffix = agentID(string(p.Arguments()), strconv.Itoa(p.ConfigRevision()))
	m.bindVisualPolicy(p, base.invocation, base.Check)
	return p, nil
}

func newLegacyVisualPolicy(ctx context.Context, store *pluginhost.Store, revision int, prompt string, images []deepseek.Image, tokens int) (*mediaPluginPolicy, error) {
	visual, err := mediaobserve.NewVisual(ctx, store, revision, prompt, images, tokens)
	if err != nil {
		return nil, err
	}
	return &mediaPluginPolicy{Visual: visual, store: store, post: mcptools.PostPluginModel}, nil
}

func (m *Manager) bindVisualPolicy(p *mediaPluginPolicy, invocation func() pluginhost.Invocation, check func(context.Context, pluginhost.Job, pluginhost.Capability) error) {
	p.Bind(mediaobserve.VisualBinding{
		Invocation:   invocation,
		CheckOrigin:  check,
		CheckAccount: func(account string) bool { return account == m.native.Account() },
		Configured:   m.mediaObservationConfigured,
		Controller: func() (*pluginruntime.Controller, error) {
			controller, ready, err := m.installedPluginRuntime()
			if err != nil || !ready {
				return nil, errors.New("media_plugin_runtime_unavailable")
			}
			return controller, nil
		},
		Billing: m.billing,
		Transport: func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
			return p.post(ctx, endpoint, headers, body, limit, timeout)
		},
	})
}

func (a mediaUnderstanding) observeImages(ctx context.Context, message linuxnative.Message, prompt string, images []deepseek.Image) (string, error) {
	if err := a.manager.mediaImageObservationReady(ctx); err != nil {
		return "", err
	}
	policy, err := a.manager.newMediaPluginPolicy(ctx, message, prompt, images, 0)
	if err != nil {
		return "", err
	}
	return policy.Observe(ctx)
}

// mediaImageObservationReady is deliberately stricter than merely having a
// model key: incoming images may only be materialized for the installed,
// authorized media-understanding package. It also lets callers reject before a
// private native attachment download.
func (m *Manager) mediaImageObservationReady(ctx context.Context) error {
	installed, err := m.installedServiceState(ctx, "media-understanding", "media")
	if err != nil {
		return err
	}
	if !installed {
		return errors.New("media_plugin_migration_required")
	}
	if !m.mediaObservationConfigured(ctx) {
		return errors.New("media_observation_configuration_required")
	}
	return nil
}

func mediaObservationReason(err error) string {
	var startup *pluginruntime.EnsureStartedError
	if errors.As(err, &startup) {
		return startup.Code
	}
	if err != nil && publicShareErrorCode.MatchString(err.Error()) && (strings.HasPrefix(err.Error(), "media_") || strings.HasPrefix(err.Error(), "share_")) {
		return err.Error()
	}
	return "vision_provider_failed_check_model"
}
