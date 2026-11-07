package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediatranscribe"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

// Legacy aliases keep callers concerned only with a transcript independent
// from the concrete connected application. The implementation lives in
// mediatranscribe, not in the admin package.
type mediaAudioReference = mediatranscribe.AudioReference
type mediaTranscription = mediatranscribe.Result

// mediaTranscriptionPolicy is an admin-origin binding around the shared audio
// policy. It owns only native-message proof and the overridable transport used
// by legacy tests; protocol, authorization, idempotency and verification are
// all provided by mediatranscribe.Policy.
type mediaTranscriptionPolicy struct {
	*mediatranscribe.Policy
	receivedSharePolicy *receivedSharePolicy
	store               *pluginhost.Store
	invocationValue     pluginhost.Invocation
	originCheck         func(context.Context, pluginhost.Job, pluginhost.Capability) error
	post                pluginruntime.RouteTransport
	runLocal            mediatranscribe.LocalRunner
}

func (m *Manager) mediaTranscriptionConfigured(ctx context.Context) bool {
	s, err := m.packageStore()
	if err != nil {
		return false
	}
	installed, err := s.Installed(ctx, localasr.PluginID)
	invocation := pluginhost.Invocation{Account: s.Account(), Thread: "host", Requester: "administrator", Purpose: "inspect media transcription readiness", RequestKey: "host-media-transcription-readiness"}
	if err != nil || !installed.Enabled || installed.Removed || pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: s.Account(), Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}, invocation, []string{"artifact.read", "model.invoke"}) != nil || s.CheckConfiguration(ctx, localasr.PluginID) != nil {
		return false
	}
	ready := false
	for _, capability := range mustCapabilities(ctx, s, localasr.PluginID) {
		if capability.ID == mediatranscribe.CapabilityID && capability.Discoverable && capability.Implementation == "ready" && !capability.Async {
			ready = true
			break
		}
	}
	if !ready {
		return false
	}
	cfg, err := s.ConfigurationForExecution(ctx, localasr.PluginID)
	if err != nil {
		return false
	}
	var endpoint, model, backend string
	_ = json.Unmarshal(cfg.Values["asr_backend"], &backend)
	if backend == "" {
		backend = "remote"
	}
	if backend == "local" {
		i, err := s.Installed(ctx, localasr.PluginID)
		if err != nil || !i.Enabled || i.Removed {
			return false
		}
		_, ok := m.registeredLocalASRProfile(ctx, i.Package, pluginhost.Grant{Account: m.native.Account(), Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions})
		return ok
	}
	if backend != "remote" || json.Unmarshal(cfg.Values["asr_endpoint"], &endpoint) != nil || json.Unmarshal(cfg.Values["asr_model"], &model) != nil || endpoint == "" || model == "" {
		return false
	}
	var key string
	return json.Unmarshal(cfg.Values["asr_key"], &key) == nil && key != ""
}

func (m *Manager) newMediaTranscriptionPolicy(ctx context.Context, message linuxnative.Message, wav []byte) (*mediaTranscriptionPolicy, error) {
	base, err := m.newReceivedSharePolicyFor(ctx, message, mediatranscribe.CapabilityID)
	if err != nil {
		return nil, err
	}
	p, err := m.newMediaTranscriptionPolicyForOrigin(ctx, base.store, base.source.Revision, wav)
	if err != nil {
		return nil, err
	}
	p.receivedSharePolicy = base
	p.invocationValue = base.invocation()
	p.originCheck = func(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
		return base.Check(ctx, j, cap)
	}
	base.requestSuffix = agentID(string(p.Arguments()), strconv.Itoa(p.ConfigRevision()))
	return p, nil
}

func (m *Manager) newMediaTranscriptionPolicyForOrigin(ctx context.Context, store *pluginhost.Store, sourceRevision int, wav []byte) (*mediaTranscriptionPolicy, error) {

	if m == nil || store == nil {
		return nil, errors.New("media_transcription_origin_required")
	}
	p := &mediaTranscriptionPolicy{store: store, post: mcptools.PostPluginModel, runLocal: m.runRegisteredLocalASR}
	policy, err := mediatranscribe.New(ctx, mediatranscribe.Dependencies{
		Store: store,
		Invocation: func() pluginhost.Invocation {
			if p.receivedSharePolicy != nil {
				return p.receivedSharePolicy.invocation()
			}
			if p.invocationValue.Account != "" {
				return p.invocationValue
			}
			// Local profile setup only needs the account before the immutable
			// source binding is installed by the caller below.
			return pluginhost.Invocation{Account: store.Account()}
		},
		OriginCheck: func(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
			if p.originCheck == nil {
				return errors.New("media_transcription_origin_required")
			}
			return p.originCheck(ctx, j, cap)
		},
		Runtime: func(context.Context) (*pluginruntime.Controller, bool, error) { return m.installedPluginRuntime() },
		Post: func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
			return p.post(ctx, endpoint, headers, body, limit, timeout)
		},
		RecordUsage: mediatranscribe.UsageRecorder(m.billing),
		LocalProfile: func(ctx context.Context, pkg pluginhost.Package, grant pluginhost.Grant) (localasr.Profile, bool) {
			return m.registeredLocalASRProfile(ctx, pkg, grant)
		},
		RunLocal: func(ctx context.Context, pkg pluginhost.Package, grant pluginhost.Grant, profile localasr.Profile, wav []byte, timeout int) (string, string, error) {
			return p.runLocal(ctx, pkg, grant, profile, wav, timeout)
		},
	}, sourceRevision, wav)
	if err != nil {
		return nil, err
	}
	p.Policy = policy
	return p, nil
}

func (p *mediaTranscriptionPolicy) invocation() pluginhost.Invocation {
	if p.receivedSharePolicy != nil {
		return p.receivedSharePolicy.invocation()
	}
	return p.invocationValue
}

func (p *mediaTranscriptionPolicy) transcribe(ctx context.Context) (string, error) {
	return p.Transcribe(ctx)
}
func (p *mediaTranscriptionPolicy) checkCompleted(ctx context.Context) error {
	return p.CheckCompleted(ctx)
}

func (p *mediaTranscriptionPolicy) appendRequestSuffix(parts ...string) {
	if p.receivedSharePolicy == nil {
		return
	}
	p.receivedSharePolicy.requestSuffix = agentID(append([]string{p.receivedSharePolicy.requestSuffix}, parts...)...)
}
