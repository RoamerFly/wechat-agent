package station

// The local ASR bridge uses the stable fish's tool grant and an owner-registered
// resource root. Each application supplies its own check on the audio source.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
)

const localStationASRRegistrationFile = "station-asr.json"

// localStationASRRegistration is private station control-plane data. Workspace
// must contain the existing local-ASR public_workspace/manifests contract
// written by install_local_asr.py. ProfileID is the only owner-selected model
// handle; the Agent never receives a filesystem path, interpreter, command or
// credential.
type localStationASRRegistration struct {
	SchemaVersion int    `json:"schema_version"`
	Enabled       bool   `json:"enabled"`
	Workspace     string `json:"workspace"`
	ProfileID     string `json:"profile_id"`
}

type localStationASRBinding struct {
	installed pluginhost.InstalledPackage
	profile   localasr.Profile
	store     *pluginhost.Store
	account   string
	version   string
}

type stationLocalASR struct {
	version    func(context.Context) (string, error)
	transcribe func(context.Context, []byte, func() error) (string, error)
}

func (a stationLocalASR) ready(ctx context.Context) (string, error) {
	if a.version == nil || a.transcribe == nil {
		return "", errors.New("station_asr_host_unavailable")
	}
	return a.version(ctx)
}

func (h *localHomeService) localStationASRVersion(ctx context.Context) (string, error) {
	binding, err := h.localStationASRBinding(ctx)
	if err != nil {
		return "", err
	}
	return binding.version, nil
}

func (h *localHomeService) transcribeLocalStationAudio(ctx context.Context, wav []byte, originCheck func() error) (string, error) {
	if originCheck == nil {
		return "", errors.New("station_asr_origin_check_missing")
	}
	if err := originCheck(); err != nil {
		return "", err
	}
	binding, err := h.localStationASRBinding(ctx)
	if err != nil {
		return "", err
	}
	if err := originCheck(); err != nil {
		return "", err
	}

	scratch := filepath.Join(h.dataDir, "station-asr-scratch")
	windows, err := h.localStationASRWindows()
	if err != nil {
		return "", err
	}
	state, text, err := localasr.Run(ctx, binding.store, scratch, windows, binding.installed.Package, pluginhost.Grant{
		Account: binding.account, Plugin: binding.installed.Package.ID, Version: binding.installed.Package.Version, Permissions: binding.installed.Permissions,
	}, binding.profile, wav, binding.profile.TimeoutSeconds())
	if err != nil {
		return "", err
	}
	if err := originCheck(); err != nil {
		return "", err
	}
	after, err := h.localStationASRBinding(ctx)
	if err != nil || after.version != binding.version {
		return "", errors.New("station_asr_configuration_changed")
	}
	if state == "no_recognizable_speech" {
		return "", &mediaspec.Failure{Code: "ASR_RESPONSE_HAVE_NO_WORDS"}
	}
	if state != "transcribed" || strings.TrimSpace(text) == "" {
		return "", errors.New("station_asr_result_invalid")
	}
	return strings.TrimSpace(text), nil
}

func (h *localHomeService) localStationASRBinding(ctx context.Context) (localStationASRBinding, error) {
	if err := ctx.Err(); err != nil {
		return localStationASRBinding{}, err
	}
	if h == nil || h.projectTools == nil {
		return localStationASRBinding{}, errors.New("station_asr_host_unavailable")
	}
	if _, err := h.localStationASRWindows(); err != nil {
		return localStationASRBinding{}, err
	}
	record, err := h.identity.Read()
	if err != nil || record.LocalOwner == nil {
		return localStationASRBinding{}, errors.New("station_asr_owner_unavailable")
	}
	registration, raw, err := h.loadLocalStationASRRegistration()
	if err != nil {
		return localStationASRBinding{}, err
	}
	host, err := h.projectTools.forFish(record.FishID)
	if err != nil || host == nil || host.store.Account() != record.FishID {
		return localStationASRBinding{}, errors.New("station_asr_tool_store_unavailable")
	}
	installed, err := host.store.Installed(ctx, localasr.PluginID)
	if err != nil || installed.Removed || !installed.Enabled || installed.Package.ID != localasr.PluginID || installed.Digest == "" {
		return localStationASRBinding{}, errors.New("station_asr_tool_not_enabled")
	}
	configuration, err := host.store.ConfigurationForExecution(ctx, localasr.PluginID)
	if err != nil {
		return localStationASRBinding{}, errors.New("station_asr_configuration_unavailable")
	}
	var backend, selected string
	if json.Unmarshal(configuration.Values["asr_backend"], &backend) != nil || backend != "local" ||
		json.Unmarshal(configuration.Values["local_asr_profile_id"], &selected) != nil || selected != registration.ProfileID {
		return localStationASRBinding{}, errors.New("station_asr_profile_not_selected")
	}
	invocation := pluginhost.Invocation{Account: record.FishID, Thread: "station:local-asr", Requester: record.LocalOwner.ID, Purpose: "use the owner-registered local ASR profile", RequestKey: "station-local-asr", Authorization: localStationASRRegistrationFile}
	grant := pluginhost.Grant{Account: record.FishID, Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}
	if pluginhost.Authorize(installed.Package, grant, invocation, []string{"artifact.read", "model.invoke"}) != nil || !localStationASRCapabilityReady(ctx, host.store) {
		return localStationASRBinding{}, errors.New("station_asr_permission_unavailable")
	}
	profile, ok := localasr.Resolve(ctx, host.store, filepath.Join(registration.Workspace, "public_workspace"), installed.Package, grant)
	if !ok || profile.Metadata().ProfileID != registration.ProfileID || profile.Metadata().ResourceVersion == "" {
		return localStationASRBinding{}, errors.New("station_asr_profile_unavailable")
	}
	sum := sha256.Sum256(append(append([]byte(nil), raw...), []byte("\x00"+record.FishID+"\x00"+record.LocalOwner.ID+"\x00"+installed.Digest+"\x00"+strconv.Itoa(installed.Revision)+"\x00"+strconv.Itoa(configuration.Revision)+"\x00"+profile.Metadata().ResourceVersion)...))
	return localStationASRBinding{installed: installed, profile: profile, store: host.store, account: record.FishID, version: "station-local-asr-" + hex.EncodeToString(sum[:])}, nil
}

func localStationASRCapabilityReady(ctx context.Context, store *pluginhost.Store) bool {
	capabilities, err := store.Capabilities(ctx, localasr.PluginID)
	if err != nil {
		return false
	}
	for _, capability := range capabilities {
		if capability.ID == "media.transcribe" && capability.Discoverable && capability.Implementation == "ready" && !capability.Async {
			return true
		}
	}
	return false
}

func (h *localHomeService) localStationASRWindows() (*agenthost.WindowsConfig, error) {
	if runtime.GOOS != "windows" {
		return nil, nil
	}
	if h.windows == nil {
		return nil, errors.New("station_asr_windows_runtime_unavailable")
	}
	config := *h.windows
	config.Spool = filepath.Join(h.dataDir, "station-asr-spool")
	if err := config.Validate(); err != nil {
		return nil, errors.New("station_asr_windows_runtime_unavailable")
	}
	return &config, nil
}

func (h *localHomeService) loadLocalStationASRRegistration() (localStationASRRegistration, []byte, error) {
	var registration localStationASRRegistration
	path := filepath.Join(h.dataDir, localStationASRRegistrationFile)
	raw, err := localRVCReadRegular(path, 64<<10)
	if err != nil || json.Unmarshal(raw, &registration) != nil {
		return registration, nil, errors.New("station_asr_not_installed_or_authorized")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 4 || fields["schema_version"] == nil || fields["enabled"] == nil || fields["workspace"] == nil || fields["profile_id"] == nil || registration.SchemaVersion != 1 || !registration.Enabled || !localasr.ValidID(registration.ProfileID) {
		return registration, nil, errors.New("station_asr_registration_invalid")
	}
	// JSON examples may use forward slashes to avoid escaping Windows paths.
	// Normalize that spelling before every filesystem and profile-manifest
	// check; the stored path itself is still owner-provisioned and must be an
	// absolute link-free directory after normalization.
	registration.Workspace = filepath.Clean(registration.Workspace)
	if !localRVCSafeDirectory(registration.Workspace) || !localRVCSafeDirectory(filepath.Join(registration.Workspace, "public_workspace")) || localRVCPathsOverlap(registration.Workspace, h.dataDir) || localRVCPathsOverlap(registration.Workspace, h.root) {
		return registration, nil, errors.New("station_asr_workspace_invalid")
	}
	return registration, raw, nil
}

// configureStationASR makes the owner-provisioned profile the only execution
// choice for the station-local media tool. It clears remote ASR credentials
// and endpoints from the fish's separate tool store.
func (s *localProjectTools) configureStationASR(ctx context.Context, host *localProjectToolHost) error {
	if s == nil || s.home == nil || host == nil || host.store == nil {
		return errors.New("station_asr_tool_store_unavailable")
	}
	record, err := s.home.identity.Read()
	if err != nil || record.LocalOwner == nil || host.store.Account() != record.FishID {
		return errors.New("station_asr_owner_unavailable")
	}
	registration, _, err := s.home.loadLocalStationASRRegistration()
	if err != nil {
		return err
	}
	if _, err = s.home.localStationASRWindows(); err != nil {
		return err
	}
	installed, err := host.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		return err
	}
	grant := pluginhost.Grant{Account: record.FishID, Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}
	if _, err = localasr.ReadProfile(filepath.Join(registration.Workspace, "public_workspace"), installed.Package, grant, registration.ProfileID); err != nil {
		return err
	}
	configuration, err := host.store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		return err
	}
	values := make(map[string]json.RawMessage, len(configuration.Values)+2)
	for key, value := range configuration.Values {
		values[key] = append(json.RawMessage(nil), value...)
	}
	delete(values, "asr_endpoint")
	delete(values, "asr_model")
	values["asr_backend"] = json.RawMessage(`"local"`)
	encoded, _ := json.Marshal(registration.ProfileID)
	values["local_asr_profile_id"] = encoded
	_, err = host.store.SaveConfiguration(ctx, localasr.PluginID, pluginhost.ConfigUpdate{Revision: configuration.Revision, Values: values, ClearSecrets: []string{"asr_key"}})
	return err
}
