package admin

// This file is the deliberately small, host-owned read-only device observer.
// It never enters the device planner and therefore cannot turn a dashboard
// refresh into a model call or a device action.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	mobile "wechat-bot/internal/devicespec"
)

const mobileObservationControlKey = "observation-control"

type mobileObservationControl struct {
	Devices map[string]mobileObservationDeviceControl `json:"devices"`
}

type mobileObservationDeviceControl struct {
	Paused     bool      `json:"paused"`
	Takeover   bool      `json:"takeover"`
	CheckedAt  time.Time `json:"checked_at,omitempty"`
	Connection string    `json:"connection_state,omitempty"`
}

// mobileDeviceOverview deliberately describes a last verified observation;
// CurrentApp is not a live foreground-app claim.
type mobileDeviceOverview struct {
	DeviceID             string                          `json:"device_id"`
	FishID               string                          `json:"fish_id,omitempty"`
	BindingID            string                          `json:"binding_id,omitempty"`
	AuthorizedApps       []string                        `json:"authorized_apps"`
	AuthorizedActions    []string                        `json:"authorized_actions"`
	AuthorizationState   string                          `json:"authorization_state"`
	ObservationApps      []mobileObservationAppGrantView `json:"observation_apps"`
	ObservationAppsState string                          `json:"observation_apps_state"`
	ConnectionState      string                          `json:"connection_state"`
	CheckedAt            time.Time                       `json:"checked_at,omitempty"`
	CurrentApp           string                          `json:"current_app,omitempty"`
	ObservedAt           time.Time                       `json:"observed_at,omitempty"`
	TaskID               string                          `json:"task_id,omitempty"`
	ObservationID        string                          `json:"observation_id,omitempty"`
	ScreenshotURI        string                          `json:"screenshot_uri,omitempty"`
	Paused               bool                            `json:"paused"`
	Takeover             bool                            `json:"takeover"`
}

type mobileOverview struct {
	FishID    string                 `json:"fish_id,omitempty"`
	CheckedAt time.Time              `json:"checked_at,omitempty"`
	Devices   []mobileDeviceOverview `json:"devices"`
}

func decodeMobileObservationControl(raw json.RawMessage) (mobileObservationControl, error) {
	control := mobileObservationControl{Devices: map[string]mobileObservationDeviceControl{}}
	if len(raw) != 0 && json.Unmarshal(raw, &control) != nil {
		return mobileObservationControl{}, errors.New("device_repository_invalid")
	}
	if control.Devices == nil {
		control.Devices = map[string]mobileObservationDeviceControl{}
	}
	return control, nil
}

func (s *mobileWorkflowService) observationControl(snapshot mobileWorkflowSnapshot) (mobileObservationControl, error) {
	return decodeMobileObservationControl(snapshot.host.Records[mobileObservationControlKey])
}

func (s *mobileWorkflowService) devicePaused(deviceID string) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, _, err := s.load(false)
	if err != nil {
		return true
	}
	control, err := s.observationControl(snapshot)
	return err != nil || control.Devices[deviceID].Paused
}

func (s *mobileWorkflowService) Overview() mobileOverview {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, _, err := s.load(false)
	if err != nil {
		return mobileOverview{Devices: []mobileDeviceOverview{}}
	}
	control, controlErr := s.observationControl(snapshot)
	if controlErr != nil {
		return mobileOverview{Devices: []mobileDeviceOverview{}}
	}
	grants := s.manager.mobileDeviceGrantSummary(snapshot.settings)
	appGrants, appGrantErr := s.observationAppGrants(snapshot)
	overview := mobileOverview{Devices: make([]mobileDeviceOverview, 0, len(snapshot.settings.Devices))}
	if fishID, _, fishBound, fishErr := s.manager.mobileAccountDeviceBinding(""); fishErr == nil && fishBound {
		overview.FishID = fishID
	}
	for _, deviceID := range snapshot.settings.Devices {
		entry := mobileDeviceOverview{DeviceID: deviceID, ConnectionState: "unknown", AuthorizedApps: []string{}, AuthorizedActions: []string{}, AuthorizationState: "unavailable", ObservationApps: []mobileObservationAppGrantView{}, ObservationAppsState: "ready"}
		if appGrantErr != nil {
			entry.ObservationAppsState = "unavailable"
		}
		if grant, ok := grants[deviceID]; ok {
			entry.AuthorizedApps = grant.Packages
			entry.AuthorizedActions = grant.Actions
			entry.AuthorizationState = "ready"
		}
		if fishID, bindingID, fishBound, fishErr := s.manager.mobileAccountDeviceBinding(deviceID); fishErr == nil && fishBound && bindingID != "" {
			entry.FishID, entry.BindingID = fishID, bindingID
			if appGrantErr == nil {
				for _, grant := range currentObservationAppGrants(appGrants, deviceID, fishID, bindingID) {
					entry.ObservationApps = append(entry.ObservationApps, mobileObservationAppGrantView{
						GrantID: grant.GrantID, FishID: grant.FishID, DeviceID: grant.DeviceID, BindingID: grant.BindingID, Package: grant.Package,
						Available: mobileObservationAppAllowed(entry, grant.Package),
					})
				}
			}
		}
		state := control.Devices[deviceID]
		entry.Paused, entry.Takeover, entry.CheckedAt = state.Paused, state.Takeover, state.CheckedAt
		if state.Paused {
			entry.ConnectionState = "paused"
		} else if state.Connection == "connected" || state.Connection == "disconnected" {
			entry.ConnectionState = state.Connection
		}
		for _, task := range snapshot.tasks {
			if task.DeviceID != deviceID || task.ObservationID == "" {
				continue
			}
			observation, ok := mobileObservationFromRecord(snapshot.records[task.ID], task.ObservationID)
			if !ok || (!entry.ObservedAt.IsZero() && !observation.CapturedAt.After(entry.ObservedAt)) {
				continue
			}
			entry.ObservedAt, entry.CurrentApp, entry.TaskID, entry.ObservationID = observation.CapturedAt, observation.Package, task.ID, observation.ID
			entry.ScreenshotURI = "/api/mobile/tasks/" + task.ID + "/screenshot/" + observation.ID
		}
		overview.Devices = append(overview.Devices, entry)
		if entry.CheckedAt.After(overview.CheckedAt) {
			overview.CheckedAt = entry.CheckedAt
		}
	}
	sort.Slice(overview.Devices, func(i, j int) bool { return overview.Devices[i].DeviceID < overview.Devices[j].DeviceID })
	return overview
}

// mobileDeviceGrantSummary only parses the existing allowlisted device
// profile. It never opens ADB or a broker, so rendering an overview cannot
// wake the phone or acquire its lease.
func (m *Manager) mobileDeviceGrantSummary(settings mobile.Settings) map[string]mobile.IOGrant {
	result := map[string]mobile.IOGrant{}
	if m.deviceIO == nil {
		return result
	}
	store, err := m.packageStore()
	if err != nil {
		return result
	}
	installed, err := store.Installed(context.Background(), mobileDevicePluginID)
	if err != nil || installed.Removed {
		return result
	}
	deployment, err := runtimeDeploymentOptions()
	if err != nil {
		return result
	}
	options, err := m.mobileDeviceIOOptions(store, installed, deployment, settings)
	if err != nil {
		return result
	}
	grants, err := m.deviceIO.Grants(context.Background(), options)
	if err != nil {
		return result
	}
	for _, grant := range grants {
		result[grant.DeviceID] = grant
	}
	return result
}

func mobileObservationFromRecord(record mobileWorkflowRecord, wanted string) (mobile.Observation, bool) {
	var state struct {
		Observation mobile.Observation `json:"observation"`
		Fresh       mobile.Observation `json:"fresh"`
	}
	if json.Unmarshal(record.State, &state) != nil {
		return mobile.Observation{}, false
	}
	if state.Observation.ID == wanted {
		return state.Observation, !state.Observation.CapturedAt.IsZero()
	}
	if state.Fresh.ID == wanted {
		return state.Fresh, !state.Fresh.CapturedAt.IsZero()
	}
	return mobile.Observation{}, false
}

// ObserveOnce takes exactly one authorized screenshot. It bypasses planning,
// tool discovery and device.act; the persisted synthetic task solely gives the
// existing account-scoped screenshot endpoint an ownership record.
func (s *mobileWorkflowService) ObserveOnce(ctx context.Context, deviceID string) (mobileDeviceOverview, error) {
	if deviceID == "" {
		return mobileDeviceOverview{}, errors.New("device_not_authorized")
	}
	fishID, bindingID, fishBound, err := s.manager.mobileAccountDeviceBinding(deviceID)
	if err != nil {
		return mobileDeviceOverview{}, err
	}
	if fishBound && bindingID == "" {
		return mobileDeviceOverview{}, errors.New("device_fish_binding_required")
	}
	observationTaskID := "device-observe-" + randomToken()
	observeCtx, cancel := context.WithCancel(ctx)
	s.stateMu.Lock()
	snapshot, store, err := s.load(true)
	if err == nil && (!snapshot.settings.Enabled || !contains(snapshot.settings.Devices, deviceID)) {
		err = errors.New("device_not_authorized")
	}
	if err == nil {
		control, controlErr := s.observationControl(snapshot)
		if controlErr != nil {
			err = controlErr
		} else if control.Devices[deviceID].Paused {
			err = errors.New("device_observation_paused")
		}
	}
	if err != nil {
		s.stateMu.Unlock()
		cancel()
		return mobileDeviceOverview{}, err
	}
	// Persist a queued host-owned task before opening the broker. The bridge
	// validates exact task identity through mobilePluginCurrent; using an
	// ephemeral task would bypass that guard.
	queuedAt := time.Now().UTC().Truncate(time.Microsecond)
	task := mobile.Task{ID: observationTaskID, RequestHash: namedSpeechSHA(mustJSON([]string{"observe", deviceID, observationTaskID, fishID, bindingID})), DeviceID: deviceID, FishID: fishID, DeviceBindingID: bindingID, Goal: "只读查看设备当前画面", State: "running", Source: mobile.Source{Admin: true}, Created: queuedAt, Updated: queuedAt}
	taskRaw, _ := json.Marshal(task)
	recordRaw, _ := json.Marshal(mobileWorkflowRecord{State: json.RawMessage(`{"observation_only":true}`)})
	s.mu.Lock()
	if s.closed || s.devices[deviceID] != "" {
		s.mu.Unlock()
		s.stateMu.Unlock()
		cancel()
		return mobileDeviceOverview{}, errors.New("device_busy")
	}
	s.devices[deviceID], s.active[observationTaskID] = observationTaskID, cancel
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() {
		defer s.wg.Done()
		s.mu.Lock()
		delete(s.devices, deviceID)
		delete(s.active, observationTaskID)
		s.mu.Unlock()
		cancel()
	}()
	if err = s.commit(snapshot, store, map[string]json.RawMessage{mobileTaskRecordKey(task.ID): taskRaw, mobileWorkflowKey(task.ID): recordRaw}); err != nil {
		s.stateMu.Unlock()
		return mobileDeviceOverview{}, err
	}
	s.stateMu.Unlock()

	bridge, err := s.manager.newMobilePluginBridge(observeCtx, task, snapshot.settings)
	if err == nil {
		defer bridge.Close()
		var raw string
		if s.devicePaused(deviceID) {
			err = errors.New("device_observation_paused")
		} else {
			raw, err = bridge.Call(observeCtx, "observe", mustJSON(map[string]string{"device_id": deviceID}))
		}
		if err == nil {
			var captured json.RawMessage
			captured, err = s.captureObservation([]byte(raw))
			if err == nil {
				var observation mobile.Observation
				err = json.Unmarshal(captured, &observation)
				if err == nil && observation.DeviceID != deviceID {
					err = errors.New("device_observation_invalid")
				}
				if err == nil {
					s.stateMu.Lock()
					fresh, freshStore, loadErr := s.load(true)
					if loadErr != nil {
						err = loadErr
					} else if control, controlErr := s.observationControl(fresh); controlErr != nil {
						err = controlErr
					} else if observeCtx.Err() != nil {
						err = errors.New("device_observation_cancelled")
					} else if bindingErr := s.manager.mobileTaskBindingCurrent(task); bindingErr != nil {
						err = bindingErr
					} else if current, ok := fresh.tasks[observationTaskID]; !ok || current.State != "running" || current.RequestHash != task.RequestHash || current.DeviceID != task.DeviceID || current.ObservationID != "" {
						err = errors.New("device_observation_cancelled")
					} else if fresh.settings.Revision != snapshot.settings.Revision || control.Devices[deviceID].Paused {
						err = errors.New("device_observation_paused")
					} else {
						control.Devices[deviceID] = mobileObservationDeviceControl{CheckedAt: observation.CapturedAt, Connection: "connected"}
						task.State, task.ObservationID, task.Updated = "completed", observation.ID, observation.CapturedAt
						record, _ := json.Marshal(mobileWorkflowRecord{State: mustJSON(map[string]any{"observation_only": true, "observation": json.RawMessage(captured)})})
						controlRaw, _ := json.Marshal(control)
						taskRaw, _ := json.Marshal(task)
						err = s.commit(fresh, freshStore, map[string]json.RawMessage{mobileTaskRecordKey(task.ID): taskRaw, mobileWorkflowKey(task.ID): record, mobileObservationControlKey: controlRaw})
					}
					s.stateMu.Unlock()
					if err == nil {
						return mobileDeviceOverview{DeviceID: deviceID, ConnectionState: "connected", CheckedAt: observation.CapturedAt, CurrentApp: observation.Package, ObservedAt: observation.CapturedAt, TaskID: observationTaskID, ObservationID: observation.ID, ScreenshotURI: "/api/mobile/tasks/" + observationTaskID + "/screenshot/" + observation.ID}, nil
					}
				}
			}
		}
	}
	// A failure leaves the previous frame untouched. It is not automatically a
	// physical disconnect: configuration, validation and cancellation errors
	// must not masquerade as an offline phone.
	s.finishObservationTask(observationTaskID, err)
	s.recordObservationProbe(deviceID, "unknown")
	return mobileDeviceOverview{}, err
}

func (s *mobileWorkflowService) finishObservationTask(id string, cause error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, store, err := s.load(false)
	if err != nil {
		return
	}
	task, ok := snapshot.tasks[id]
	if !ok || task.State != "running" {
		return
	}
	task.State, task.Updated = "cancelled", time.Now().UTC().Truncate(time.Microsecond)
	task.Reason = "device_observation_unavailable"
	if cause != nil && cause.Error() == "device_observation_paused" {
		task.Reason = "device_observation_paused"
	}
	raw, _ := json.Marshal(task)
	_ = s.commit(snapshot, store, map[string]json.RawMessage{mobileTaskRecordKey(id): raw, mobileWorkflowKey(id): mobileWorkflowClearedRecord(snapshot.records[id])})
}

func (s *mobileWorkflowService) recordObservationProbe(deviceID, state string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, store, err := s.load(true)
	if err != nil || !contains(snapshot.settings.Devices, deviceID) {
		return
	}
	control, controlErr := s.observationControl(snapshot)
	if controlErr != nil {
		return
	}
	entry := control.Devices[deviceID]
	if !entry.Paused {
		entry.Connection, entry.CheckedAt = state, time.Now().UTC().Truncate(time.Microsecond)
		control.Devices[deviceID] = entry
		raw, _ := json.Marshal(control)
		_ = s.commit(snapshot, store, map[string]json.RawMessage{mobileObservationControlKey: raw})
	}
}

// SetObservationPaused is the owner-only emergency boundary. It persists
// before cancelling workers, blocks future start/advance/act calls, and does
// not remove the last verified screenshot.
func (s *mobileWorkflowService) SetObservationPaused(deviceID string, paused, takeover bool) (mobileOverview, error) {
	s.stateMu.Lock()
	snapshot, store, err := s.load(true)
	if err != nil || !contains(snapshot.settings.Devices, deviceID) {
		s.stateMu.Unlock()
		return mobileOverview{}, errors.New("device_not_authorized")
	}
	control, controlErr := s.observationControl(snapshot)
	if controlErr != nil {
		s.stateMu.Unlock()
		return mobileOverview{}, controlErr
	}
	entry := control.Devices[deviceID]
	if paused {
		entry.Paused = true
		// A normal pause must never silently clear an existing owner takeover.
		// Only the explicit resume path below is allowed to relinquish it.
		entry.Takeover = entry.Takeover || takeover
	} else {
		entry.Paused, entry.Takeover = false, false
	}
	control.Devices[deviceID] = entry
	controlRaw, _ := json.Marshal(control)
	puts := map[string]json.RawMessage{mobileObservationControlKey: controlRaw}
	if paused {
		for id, task := range snapshot.tasks {
			if task.DeviceID == deviceID && mobileWorkflowActive(task) {
				task.State, task.Reason, task.Updated = "cancelled", "device_taken_over_by_owner", time.Now().UTC()
				if !takeover {
					task.Reason = "device_observation_paused"
				}
				puts[mobileTaskRecordKey(id)], _ = json.Marshal(task)
				puts[mobileWorkflowKey(id)] = mobileWorkflowClearedRecord(snapshot.records[id])
			}
		}
	}
	err = s.commit(snapshot, store, puts)
	s.stateMu.Unlock()
	if err != nil {
		return mobileOverview{}, err
	}
	if paused {
		s.mu.Lock()
		if holder := s.devices[deviceID]; holder != "" && s.active[holder] != nil {
			s.active[holder]()
		}
		s.mu.Unlock()
	}
	return s.Overview(), nil
}

type mobileObservationService interface {
	Overview() mobileOverview
	ObserveOnce(context.Context, string) (mobileDeviceOverview, error)
	SetObservationPaused(string, bool, bool) (mobileOverview, error)
	SetObservationAppGrant(string, string, string, bool) (mobileOverview, error)
}

func (m *Manager) mobileObservationService() (mobileObservationService, bool) {
	s, ok := m.mobile.(mobileObservationService)
	return s, ok
}
