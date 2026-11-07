package admin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func (m *Manager) localASRPluginResources(ctx context.Context, p pluginhost.Package, g pluginhost.Grant) ([]pluginruntime.ReadOnlyResource, error) {
	profile, ok := m.registeredLocalASRProfile(ctx, p, g)
	if !ok {
		return nil, nil
	}
	return profile.Resources(), nil
}

func (m *Manager) localASRPluginMetadata(ctx context.Context, p pluginhost.Package, g pluginhost.Grant) (map[string]json.RawMessage, error) {
	profile, ok := m.registeredLocalASRProfile(ctx, p, g)
	if !ok {
		return nil, nil
	}
	raw, err := json.Marshal(profile.Metadata())
	if err != nil {
		return nil, errors.New("local_asr_metadata_unavailable")
	}
	return map[string]json.RawMessage{"local_asr": raw}, nil
}

// runRegisteredLocalASR runs only the verified media plugin's packaged runner
// with values resolved from the public registration. It is not a generic host
// command service: callers provide an already bound profile and PCM WAV bytes,
// not a path, executable or argv fragment.
func (m *Manager) runRegisteredLocalASR(ctx context.Context, p pluginhost.Package, g pluginhost.Grant, profile localasr.Profile, wav []byte, timeoutSeconds int) (string, string, error) {
	store, err := m.packageStore()
	if err != nil {
		return "", "", err
	}
	var windows *agenthost.WindowsConfig
	if runtime.GOOS == "windows" {
		windows = m.agent.config().Host.Windows
	}
	return localasr.Run(ctx, store, filepath.Join(m.privateWorkspaceFor(g.Account), "scratch"), windows, p, g, profile, wav, timeoutSeconds)
}

func (m *Manager) registeredLocalASRProfile(ctx context.Context, p pluginhost.Package, g pluginhost.Grant) (localasr.Profile, bool) {
	if err := ctx.Err(); err != nil || p.ID != localasr.PluginID || g.Account == "" || g.Account != m.native.Account() || g.Plugin != p.ID || g.Version != p.Version {
		return localasr.Profile{}, false
	}
	// The selected local profile belongs to the media plugin configuration, not
	// the legacy speech configuration.  In particular, an old ASRLocalModel
	// value must never cause an arbitrary public-workspace profile to be
	// mounted after the media plugin has been configured for remote ASR (or has
	// not selected a local profile at all).
	store, err := m.packageStore()
	if err != nil {
		return localasr.Profile{}, false
	}
	return localasr.Resolve(ctx, store, filepath.Join(m.managedRoot(), "public_workspace"), p, g)
}

func (m *Manager) readRegisteredLocalASRProfile(p pluginhost.Package, g pluginhost.Grant, id string) (localasr.Profile, error) {
	return localasr.ReadProfile(filepath.Join(m.managedRoot(), "public_workspace"), p, g, id)
}
