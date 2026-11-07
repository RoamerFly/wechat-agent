package station

// Visual processing is owned by the stable fish, independently of any
// connected application's credentials. Callers supply already verified pixels
// and a live source fence; this bridge never opens arbitrary paths or URLs.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"

	"wechat-bot/internal/billing"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/mediaobserve"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/workspacefs"
)

type localStationVisionBinding struct {
	host                 *localProjectToolHost
	installed            pluginhost.InstalledPackage
	configuration        pluginhost.Configuration
	fish, owner, version string
}

func (h *localHomeService) localStationVisionBinding(ctx context.Context) (localStationVisionBinding, error) {
	var out localStationVisionBinding
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if h == nil || h.projectTools == nil {
		return out, errors.New("local_vision_runtime_unavailable")
	}
	record, err := h.identity.Read()
	if err != nil || record.LocalOwner == nil {
		return out, errors.New("local_vision_identity_unavailable")
	}
	host, err := h.projectTools.forFish(record.FishID)
	if err != nil || host == nil || host.controller == nil || host.store.Account() != record.FishID {
		return out, errors.New("local_vision_runtime_unavailable")
	}
	installed, err := host.store.Installed(ctx, localasr.PluginID)
	if err != nil || installed.Removed || !installed.Enabled {
		return out, errors.New("local_vision_tool_not_enabled")
	}
	c, err := host.store.Configuration(ctx, localasr.PluginID)
	if err != nil || !stationVisionConfigured(c) {
		return out, errors.New("local_vision_not_configured")
	}
	inv := pluginhost.Invocation{Account: record.FishID, Thread: "station:local-vision", Requester: record.LocalOwner.ID, Purpose: "observe authorized source images", RequestKey: "local-vision-binding", Authorization: "host-verified-visual-source"}
	grant := pluginhost.Grant{Account: record.FishID, Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}
	if pluginhost.Authorize(installed.Package, grant, inv, []string{"artifact.read", "model.invoke"}) != nil {
		return out, errors.New("local_vision_permission_unavailable")
	}
	capabilities, err := host.store.Capabilities(ctx, localasr.PluginID)
	ready := false
	for _, capability := range capabilities {
		if capability.ID == "media.observe" && capability.Discoverable {
			ready = true
		}
	}
	if err != nil || !ready {
		return out, errors.New("local_vision_permission_unavailable")
	}
	version := localCheckpointKey(record.FishID, record.LocalOwner.ID, installed.Digest, installed.Revision, c.Revision)
	return localStationVisionBinding{host: host, installed: installed, configuration: c, fish: record.FishID, owner: record.LocalOwner.ID, version: "station-local-vision-" + version}, nil
}

func (h *localHomeService) localStationVisionVersion(ctx context.Context) (string, error) {
	b, err := h.localStationVisionBinding(ctx)
	return b.version, err
}

func (h *localHomeService) observeLocalStationImages(ctx context.Context, source, prompt string, images []deepseek.Image, tokens int, origin func() error) (string, error) {
	if origin == nil || strings.TrimSpace(source) == "" || len(source) > 8192 {
		return "", errors.New("local_vision_source_required")
	}
	if err := origin(); err != nil {
		return "", err
	}
	if h == nil || h.projectTools == nil {
		return "", errors.New("local_vision_runtime_unavailable")
	}
	s := h.projectTools
	s.update.Lock()
	b, err := h.localStationVisionBinding(ctx)
	if err != nil {
		s.update.Unlock()
		return "", err
	}
	done := s.markActive(localasr.PluginID)
	post := s.visionPost
	s.update.Unlock()
	defer done()
	check := func(ctx context.Context, _ pluginhost.Job, _ pluginhost.Capability) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := origin(); err != nil {
			return err
		}
		current, err := h.localStationVisionBinding(ctx)
		if err != nil || current.version != b.version {
			return errors.New("local_vision_configuration_or_identity_changed")
		}
		return nil
	}
	if err := check(ctx, pluginhost.Job{}, pluginhost.Capability{}); err != nil {
		return "", err
	}
	visual, err := mediaobserve.NewVisual(ctx, b.host.store, 1, prompt, images, tokens)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(source))
	sourceID := "visual-" + hex.EncodeToString(sum[:])
	inv := pluginhost.Invocation{Account: b.fish, Thread: "station:local-vision", Requester: b.owner, Purpose: "observe host-verified source images", Authorization: sourceID, SourceVersions: map[string]int{sourceID: 1}}
	inv.RequestKey = "station-visual-" + localCheckpointKey(sourceID, b.version, string(visual.Arguments()))
	usage, err := s.localVisionUsage(b.fish)
	if err != nil {
		return "", errors.New("local_vision_usage_unavailable")
	}
	visual.Bind(mediaobserve.VisualBinding{
		Invocation: func() pluginhost.Invocation { return inv }, CheckOrigin: check,
		Controller:   func() (*pluginruntime.Controller, error) { return b.host.controller, nil },
		CheckAccount: func(account string) bool { return account == b.fish }, Billing: usage, Transport: post,
	})
	text, err := visual.Observe(ctx)
	if e := check(ctx, pluginhost.Job{}, pluginhost.Capability{}); e != nil {
		return "", e
	}
	return text, err
}

func (s *localProjectTools) localVisionUsage(fish string) (*billing.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.visionUsage != nil {
		if s.visionUsageFish != fish {
			return nil, errors.New("local_vision_identity_changed")
		}
		return s.visionUsage, nil
	}
	directory := filepath.Join(s.home.dataDir, "local-vision-usage", fish)
	root, err := workspacefs.EnsurePath(directory)
	if err != nil {
		return nil, err
	}
	if err := root.Close(); err != nil {
		return nil, err
	}
	usage, err := billing.Open(directory)
	if err != nil {
		return nil, err
	}
	s.visionUsage, s.visionUsageFish = usage, fish
	return usage, nil
}
