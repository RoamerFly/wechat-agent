package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
)

type mobileRecordingPluginFixture struct {
	f        *webFixture
	device   *mobilePlanDeviceStub
	supplied mobile.Task
	artifact mobile.Artifact
	data     []byte
	path     string
	policy   *mediaPluginPolicy
	origin   *mobileRecordingVisualOrigin
}

func newMobileRecordingPluginFixture(t *testing.T) *mobileRecordingPluginFixture {
	t.Helper()
	f, _, visual, _ := mediaPolicyFixture(t, false)
	installMobilePackageForManager(t, f.manager)
	agent := f.manager.agent.config()
	agent.OwnerIDs, agent.PrivilegedThreads = []string{"synthetic-owner"}, []string{"filehelper"}
	if err := f.manager.agent.save(agent); err != nil {
		t.Fatal(err)
	}
	f.manager.config.Companion.Permissions.Remember = true
	now := time.Now().UTC()
	task := mobile.Task{ID: "synthetic-recording-task", RequestHash: "synthetic-device-request", DeviceID: "lab", Goal: "inspect authorized screen", State: "running", Steps: 1, ObservationID: "obs-" + strings.Repeat("a", 32), Source: mobile.Source{Account: "test", Thread: "filehelper", Sender: "synthetic-owner", MessageID: "synthetic-device-origin", Admin: true}, Created: now, Updated: now, Events: []mobile.Event{{At: now, State: "running", Action: "capture", Summary: "动作提交前检查点"}}}
	settings := mobile.Defaults()
	settings.Revision, settings.Enabled, settings.Devices, settings.Actions = 7, true, []string{"lab"}, []string{"capture"}
	device := &mobilePlanDeviceStub{settings: settings, task: task}
	f.manager.mobile = device
	// Only the source gate/host policy is under test. These synthetic container
	// bytes are never passed to FFmpeg or represented as decoded video.
	data := append([]byte{0, 0, 0, 32, 'f', 't', 'y', 'p'}, bytes.Repeat([]byte("s"), 40)...)
	sum := sha256.Sum256(data)
	artifact := mobile.Artifact{ID: "clip-" + strings.Repeat("b", 32), DeviceID: task.DeviceID, RequestKey: task.ID + "-step-00", Kind: "screen_recording", MIME: "video/mp4", Bytes: len(data), SHA256: hex.EncodeToString(sum[:]), RequestedSeconds: 4}
	path := filepath.Join(f.dir, "mobile", "recordings", artifact.ID+".mp4")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	supplied := task
	supplied.Steps, supplied.ObservationID, supplied.Events = 0, "obs-"+strings.Repeat("c", 32), nil
	origin, err := f.manager.newMobileRecordingVisualOrigin(t.Context(), supplied, artifact, data)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := f.manager.newBoundMediaVisualObservationPolicy(t.Context(), visual.Images(), origin.invocation, origin.revision, origin.check, "Observe verified silent recording frames only", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &mobileRecordingPluginFixture{f: f, device: device, supplied: supplied, artifact: artifact, data: data, path: path, policy: policy, origin: origin}
}

func TestMobileRecordingVisualOriginUsesActualTaskCaptureVersion(t *testing.T) {
	fixture := newMobileRecordingPluginFixture(t)
	if fixture.origin.task.Steps != 1 || fixture.supplied.Steps != 0 || fixture.origin.task.ObservationID == fixture.supplied.ObservationID || fixture.policy.receivedSharePolicy != nil || fixture.policy.Invocation().SourceVersions["mobile-task:"+fixture.device.task.ID] != 1 || fixture.policy.Parameters().Refs[0].Version != 1 {
		t.Fatal("recording analysis trusted caller snapshot or fabricated incoming message")
	}
	completed := completeBoundVisualForTest(t, fixture.policy)
	if err := fixture.policy.Check(t.Context(), completed, pluginhost.Capability{}); err != nil {
		t.Fatal("verified recording observation not consumable", err)
	}
	fixture.device.task.State = "cancelled"
	if err := fixture.policy.Check(t.Context(), completed, pluginhost.Capability{}); err == nil {
		t.Fatal("completed recording observation survived task cancellation")
	}
	assets, err := fixture.f.manager.loadAssets()
	if err != nil || len(assets) != 0 {
		t.Fatal("policy checkpoint registered an asset before authorized consumer", assets, err)
	}
}

func TestMobileRecordingVisualRejectsWrongTaskAndIncompleteSource(t *testing.T) {
	for _, action := range []string{"wrong_task", "replaced_source", "wrong_request_hash", "wrong_device", "wrong_goal", "incomplete_state", "no_capture_checkpoint", "wrong_recording_hash", "wrong_recording_bytes", "has_audio", "original_media", "missing_host_file", "changed_host_file"} {
		t.Run(action, func(t *testing.T) {
			fixture := newMobileRecordingPluginFixture(t)
			task, artifact := fixture.supplied, fixture.artifact
			switch action {
			case "wrong_task":
				task.ID = "other-task"
			case "replaced_source":
				task.Source.Sender = "different-owner"
			case "wrong_request_hash":
				task.RequestHash = "different-request"
			case "wrong_device":
				task.DeviceID = "different-device"
			case "wrong_goal":
				task.Goal = "different-goal"
			case "incomplete_state":
				fixture.device.task.State = "needs_review"
			case "no_capture_checkpoint":
				fixture.device.task.Events = nil
			case "wrong_recording_hash":
				artifact.SHA256 = strings.Repeat("0", 64)
			case "wrong_recording_bytes":
				artifact.Bytes--
			case "has_audio":
				artifact.Audio = true
			case "original_media":
				artifact.Original = true
			case "missing_host_file":
				if err := os.Remove(fixture.path); err != nil {
					t.Fatal(err)
				}
			case "changed_host_file":
				if err := os.WriteFile(fixture.path, bytes.Repeat([]byte("x"), len(fixture.data)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.f.manager.newMobileRecordingVisualOrigin(t.Context(), task, artifact, fixture.data); err == nil {
				t.Fatal("unverified recording source accepted", action)
			}
		})
	}
}

func TestMobileRecordingVisualRevocationStopsProviderAndCompletedUse(t *testing.T) {
	for _, action := range []string{"task_cancelled", "task_version_changed", "source_replaced", "device_settings_revoked", "device_disabled", "device_package_disabled", "media_package_disabled", "owner_revoked", "remember_revoked", "recording_replaced"} {
		t.Run(action, func(t *testing.T) {
			fixture := newMobileRecordingPluginFixture(t)
			completed := completeBoundVisualForTest(t, fixture.policy)
			switch action {
			case "task_cancelled":
				fixture.device.task.State = "cancelled"
			case "task_version_changed":
				fixture.device.task.Steps++
			case "source_replaced":
				fixture.device.task.Source.MessageID = "new-source"
			case "device_settings_revoked":
				fixture.device.settings.Actions[0] = "tap"
			case "device_disabled":
				fixture.device.settings.Enabled = false
			case "device_package_disabled", "media_package_disabled":
				id := mobileDevicePluginID
				if action == "media_package_disabled" {
					id = "media-understanding"
				}
				installed, err := fixture.policy.store.Installed(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.policy.store.SetEnabled(t.Context(), id, installed.Revision, false); err != nil {
					t.Fatal(err)
				}
			case "owner_revoked":
				settings := fixture.f.manager.agent.config()
				settings.OwnerIDs = nil
				if err := fixture.f.manager.agent.save(settings); err != nil {
					t.Fatal(err)
				}
			case "remember_revoked":
				fixture.f.manager.config.Companion.Permissions.Remember = false
			case "recording_replaced":
				if err := os.WriteFile(fixture.path, bytes.Repeat([]byte("x"), len(fixture.data)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.policy.Check(t.Context(), completed, pluginhost.Capability{}); err == nil {
				t.Fatal("revoked recording result remained consumable", action)
			}
			if _, err := fixture.policy.store.PrivateJobResult(t.Context(), completed, func(ctx context.Context, job pluginhost.Job) error {
				return fixture.policy.Check(ctx, job, pluginhost.Capability{})
			}); err == nil {
				t.Fatal("revoked private recording result remained readable", action)
			}
		})
	}
}

func TestMobileRecordingVisualDiscardsSourceReplacedDuringModelRequest(t *testing.T) {
	fixture := newMobileRecordingPluginFixture(t)
	job, capability := mediaJob(fixture.policy)
	calls := 0
	fixture.policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		if err := os.WriteFile(fixture.path, bytes.Repeat([]byte("x"), len(fixture.data)), 0600); err != nil {
			t.Fatal(err)
		}
		return json.RawMessage(mediaReply), nil
	}
	if _, err := fixture.policy.Service(t.Context(), job, capability, "wechat/model_request", mediaRequest(fixture.policy)); err == nil {
		t.Fatal("replaced recording produced trusted model response", err)
	}
	if _, err := fixture.policy.Validate(t.Context(), job, capability, verifiedVisualPluginReply(t, fixture.policy, "Synthetic pixels")); err == nil {
		t.Fatal("fake recording observation was accepted")
	}
	if _, err := fixture.policy.Service(t.Context(), job, capability, "wechat/model_request", mediaRequest(fixture.policy)); err == nil || calls != 1 {
		t.Fatal("uncertain recording model request replayed", calls, err)
	}
	assets, err := fixture.f.manager.loadAssets()
	if err != nil || len(assets) != 0 {
		t.Fatal("failed recording observation registered an asset", assets, err)
	}
}

func TestMobileRecordingAnalysisMissingMediaPluginFailsBeforeSamplingOrCurrentModel(t *testing.T) {
	f := setup(t)
	f.manager.config.APIKey = "synthetic-current-chat-model-key"
	summary, asset, err := f.manager.analyzeDeviceArtifact(t.Context(), mobile.Task{ID: "synthetic"}, mobile.Artifact{}, []byte("not a recording"))
	if err == nil || !strings.Contains(err.Error(), "media_plugin_migration_required") || summary != "" || asset != "" {
		t.Fatal("recording analysis fell back to current model", summary, asset, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "provider-key")); !os.IsNotExist(err) {
		t.Fatal("missing plugin constructed the current chat model", err)
	}
}
