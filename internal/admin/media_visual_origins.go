package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"wechat-bot/internal/deepseek"
	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
)

// Non-incoming images require a host-owned proof for their actual source.
// Nothing here constructs a native message or grants a conversation identity.
func (m *Manager) newBoundMediaVisualObservationPolicy(ctx context.Context, images []deepseek.Image, inv pluginhost.Invocation, revision int, check func(context.Context) error, prompt string, maxTokens int) (*mediaPluginPolicy, error) {
	if m == nil || check == nil || revision < 1 || inv.Account == "" || inv.Account != m.native.Account() || inv.Thread == "" || inv.Requester == "" || inv.Purpose == "" || inv.RequestKey == "" || inv.Authorization == "" || len(inv.SourceVersions) == 0 {
		return nil, errors.New("media_visual_origin_required")
	}
	for id, version := range inv.SourceVersions {
		if id == "" || version < 1 {
			return nil, errors.New("media_visual_origin_required")
		}
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	if err := m.mediaImageObservationReady(ctx); err != nil {
		return nil, err
	}
	store, err := m.packageStore()
	if err != nil {
		return nil, err
	}
	policy, err := newLegacyVisualPolicy(ctx, store, revision, prompt, images, maxTokens)
	if err != nil {
		return nil, err
	}
	// Caller-owned source maps must not be able to mutate an approved origin.
	raw, err := json.Marshal(inv)
	var frozen pluginhost.Invocation
	if err != nil || json.Unmarshal(raw, &frozen) != nil {
		return nil, errors.New("media_visual_origin_required")
	}
	frozen.RequestKey = "bound-media-visual-" + agentID(inv.RequestKey, string(policy.Arguments()), strconv.Itoa(policy.ConfigRevision()))
	account := inv.Account
	m.bindVisualPolicy(policy, func() pluginhost.Invocation { return frozen }, func(ctx context.Context, _ pluginhost.Job, _ pluginhost.Capability) error {
		if m.native.Account() != account {
			return errors.New("media_visual_origin_changed")
		}
		return check(ctx)
	})
	return policy, nil
}

type mobileRecordingVisualOrigin struct {
	task       mobile.Task
	data       []byte
	invocation pluginhost.Invocation
	revision   int
	check      func(context.Context) error
}

func readMobileRecordingSource(path string, size int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != int64(size) {
		return nil, errors.New("device_recording_source_changed")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("device_recording_source_changed")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(size)+1))
	if err != nil || len(data) != size {
		return nil, errors.New("device_recording_source_changed")
	}
	return data, nil
}

// collectArtifact invokes analysis after integrity-checking and writing the
// complete recording, but before appending it to Task.Artifacts. Its caller's
// task is a pre-capture snapshot; authority comes from the actual current task
// and host file, never mutable fields supplied in that snapshot.
func (m *Manager) newMobileRecordingVisualOrigin(ctx context.Context, supplied mobile.Task, artifact mobile.Artifact, data []byte) (*mobileRecordingVisualOrigin, error) {
	if m == nil || m.mobile == nil {
		return nil, errors.New("device_recording_origin_unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.mediaImageObservationReady(ctx); err != nil {
		return nil, err
	}
	current, ok := m.mobile.Get(supplied.ID)
	if !ok || current.ID == "" || current.State != "running" || current.Steps < 1 || current.RequestHash == "" || current.ID != supplied.ID || current.RequestHash != supplied.RequestHash || current.Source != supplied.Source || current.DeviceID != supplied.DeviceID || current.Goal != supplied.Goal || len(current.Events) == 0 || current.Events[len(current.Events)-1].Action != "capture" {
		return nil, errors.New("device_recording_task_changed")
	}
	keyPrefix := current.ID + "-step-"
	if len(artifact.ID) != 37 || !strings.HasPrefix(artifact.ID, "clip-") || artifact.ID != strings.ToLower(artifact.ID) || artifact.DeviceID != current.DeviceID || artifact.Kind != "screen_recording" || artifact.MIME != "video/mp4" || artifact.Audio || artifact.Original || artifact.Bytes < 32 || artifact.Bytes > 2<<20 || artifact.Bytes != len(data) || artifact.RequestedSeconds < 2 || artifact.RequestedSeconds > 12 || !strings.HasPrefix(artifact.RequestKey, keyPrefix) || len(artifact.RequestKey) != len(keyPrefix)+2 {
		return nil, errors.New("device_recording_integrity_invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(artifact.ID, "clip-")); err != nil {
		return nil, errors.New("device_recording_integrity_invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(artifact.RequestKey, keyPrefix)); err != nil {
		return nil, errors.New("device_recording_integrity_invalid")
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != artifact.SHA256 || string(data[4:8]) != "ftyp" {
		return nil, errors.New("device_recording_integrity_invalid")
	}
	path := filepath.Join(m.dir, "mobile", "recordings", artifact.ID+".mp4")
	stored, err := readMobileRecordingSource(path, artifact.Bytes)
	if err != nil || !bytes.Equal(stored, data) {
		return nil, errors.New("device_recording_source_changed")
	}
	settings := m.mobile.Settings()
	if !contains(settings.Devices, current.DeviceID) || !contains(settings.Actions, "capture") {
		return nil, errors.New("device_recording_authorization_changed")
	}
	// Freeze nested task/settings slices, including the actual capture version.
	taskRaw, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	var frozenTask mobile.Task
	if err := json.Unmarshal(taskRaw, &frozenTask); err != nil {
		return nil, err
	}
	current = frozenTask
	settingsRaw, err := json.Marshal(settings)
	var frozenSettings mobile.Settings
	if err != nil || json.Unmarshal(settingsRaw, &frozenSettings) != nil {
		return nil, errors.New("device_recording_authorization_changed")
	}
	settings = frozenSettings
	store, err := m.packageStore()
	if err != nil {
		return nil, err
	}
	installed, err := store.Installed(ctx, mobileDevicePluginID)
	if err != nil {
		return nil, errors.New("device_plugin_required")
	}
	account := m.native.Account()
	thread, requester := current.Source.Thread, current.Source.Sender
	if thread == "" {
		thread = "host:mobile-recording"
	}
	if requester == "" {
		requester = "administrator"
	}
	remember := m.Config().Companion.Permissions.Remember
	version := agentID(string(taskRaw), strconv.FormatInt(settings.Revision, 10), installed.Digest, strconv.Itoa(installed.Revision), artifact.ID, artifact.SHA256)
	invocation := pluginhost.Invocation{Account: account, Thread: thread, Requester: requester, Purpose: "observe verified mobile screen recording", RequestKey: "mobile-recording-" + agentID(current.ID, artifact.ID, version), Authorization: version, SourceVersions: map[string]int{"mobile-task:" + current.ID: current.Steps, "mobile-recording:" + artifact.ID: 1}}
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.native.Account() != account || m.draining() || m.Config().Companion.Permissions.Remember != remember {
			return errors.New("device_recording_authorization_changed")
		}
		if err := m.mobileDeviceCurrent(current, settings); err != nil {
			return err
		}
		now, ok := m.mobile.Get(current.ID)
		nowRaw, err := json.Marshal(now)
		if !ok || err != nil || !bytes.Equal(nowRaw, taskRaw) {
			return errors.New("device_recording_task_changed")
		}
		device, err := store.Installed(ctx, mobileDevicePluginID)
		if err != nil || !device.Enabled || device.Removed || device.Revision != installed.Revision || device.Digest != installed.Digest || device.Package.Version != installed.Package.Version || store.CheckConfiguration(ctx, mobileDevicePluginID) != nil || pluginhost.Authorize(device.Package, pluginhost.Grant{Account: account, Plugin: device.Package.ID, Version: device.Package.Version, Permissions: device.Permissions}, invocation, []string{"device"}) != nil {
			return errors.New("device_plugin_authorization_changed")
		}
		stored, err := readMobileRecordingSource(path, artifact.Bytes)
		if err != nil || sha256.Sum256(stored) != hash {
			return errors.New("device_recording_source_changed")
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	return &mobileRecordingVisualOrigin{task: current, data: stored, invocation: invocation, revision: current.Steps, check: check}, nil
}
