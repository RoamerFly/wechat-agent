package admin

// Device workflow decisions belong to the installed package. This adapter
// stores original host records, grants one durable effect claim per tick, and
// mediates its verified device/model result. It contains no device task loop.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/workspacefs"
)

const mobileWorkflowCapability = "device.workflow"

type mobileWorkflowEffect struct {
	ID         string          `json:"id"`
	Capability string          `json:"capability"`
	Arguments  json.RawMessage `json:"arguments"`
}
type mobileWorkflowFact struct {
	EffectID string          `json:"effect_id"`
	Error    string          `json:"error,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
}
type mobileWorkflowRecord struct {
	State json.RawMessage     `json:"state"`
	Fact  *mobileWorkflowFact `json:"fact,omitempty"`
	Claim string              `json:"claim,omitempty"`
}
type mobileWorkflowSnapshot struct {
	host     pluginhost.HostState
	settings mobile.Settings
	tasks    map[string]mobile.Task
	records  map[string]mobileWorkflowRecord
}
type mobileWorkflowService struct {
	planner                   mobile.Planner
	manager                   *Manager
	stateMu                   sync.Mutex
	mu                        sync.Mutex
	closed, loaded, recovered bool
	wake                      chan struct{}
	loop                      bool
	wg                        sync.WaitGroup
	active                    map[string]context.CancelFunc
	devices                   map[string]string
	bridges                   map[string]*mobileDevicePluginBridge
}

func newMobileWorkflowService(m *Manager) *mobileWorkflowService {
	return &mobileWorkflowService{manager: m, planner: m.planMobile, wake: make(chan struct{}, 1), active: map[string]context.CancelFunc{}, devices: map[string]string{}, bridges: map[string]*mobileDevicePluginBridge{}}
}
func mobileWorkflowKey(id string) string { return "workflow:" + namedSpeechSHA([]byte(id)) }
func mobileWorkflowClone[T any](value T) T {
	var out T
	raw, _ := json.Marshal(value)
	_ = json.Unmarshal(raw, &out)
	return out
}
func (s *mobileWorkflowService) load(create bool) (mobileWorkflowSnapshot, *pluginhost.Store, error) {
	empty := mobileWorkflowSnapshot{settings: mobile.Defaults(), tasks: map[string]mobile.Task{}, records: map[string]mobileWorkflowRecord{}}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return empty, nil, errDeviceServiceClosed
	}
	store, err := s.manager.openMobileStateStore(create)
	if err != nil || store == nil {
		return empty, nil, err
	}
	host, err := importMobileState(context.Background(), store, s.manager.dir)
	if err != nil {
		return empty, nil, err
	}
	snapshot, err := decodeMobileHostState(host, store.Account())
	if err != nil {
		return empty, nil, err
	}
	out := mobileWorkflowSnapshot{host: host, settings: snapshot.Settings, tasks: snapshot.Tasks, records: map[string]mobileWorkflowRecord{}}
	for id := range out.tasks {
		if raw, ok := host.Records[mobileWorkflowKey(id)]; ok {
			var record mobileWorkflowRecord
			if mobilePlanStrictJSON(raw, &record) != nil || !json.Valid(record.State) {
				return empty, nil, errors.New("device_repository_invalid")
			}
			out.records[id] = record
		}
	}
	s.mu.Lock()
	s.loaded = host.Revision > 0
	s.mu.Unlock()
	return out, store, nil
}
func (s *mobileWorkflowService) commit(snapshot mobileWorkflowSnapshot, store *pluginhost.Store, puts map[string]json.RawMessage) error {
	if store == nil {
		return errors.New("device_repository_unavailable")
	}
	if snapshot.host.Revision == 0 {
		sum := namedSpeechSHA([]byte("mobile-empty-origin-v1"))
		records, err := mobileStateRecords(snapshot.settings, snapshot.tasks, store.Account())
		if err != nil {
			return err
		}
		records["manifest"], _ = json.Marshal(mobileImportManifest{Version: 1, SourceHash: sum})
		for key, value := range puts {
			records[key] = value
		}
		_, err = store.InitializeHostState(context.Background(), mobileDevicePluginID, mobileStateNamespace, sum, records)
		if err == nil {
			s.mu.Lock()
			s.loaded = true
			s.mu.Unlock()
		}
		return err
	}
	_, err := store.UpdateHostState(context.Background(), mobileDevicePluginID, mobileStateNamespace, snapshot.host.Revision, puts, nil)
	return err
}
func (s *mobileWorkflowService) Settings() mobile.Settings {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, _, err := s.load(false)
	if err != nil {
		return mobile.Defaults()
	}
	return mobileWorkflowClone(snapshot.settings)
}
func (s *mobileWorkflowService) List(source *mobile.Source) []mobile.Task {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, _, err := s.load(false)
	if err != nil {
		return []mobile.Task{}
	}
	return mobileTasksForSource(snapshot.tasks, source)
}
func (s *mobileWorkflowService) Get(id string) (mobile.Task, bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, _, err := s.load(false)
	if err != nil {
		return mobile.Task{}, false
	}
	task, ok := snapshot.tasks[id]
	return mobileWorkflowClone(task), ok
}
func (s *mobileWorkflowService) InitializationError() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	_, _, err := s.load(false)
	return err
}
func (s *mobileWorkflowService) Busy() bool {
	s.mu.Lock()
	active, loop := len(s.active) > 0, s.loop && !s.closed
	s.mu.Unlock()
	if active {
		return true
	}
	if !loop {
		return false
	}
	for _, task := range s.List(nil) {
		if mobileWorkflowActive(task) {
			return true
		}
	}
	return false
}
func (s *mobileWorkflowService) signal() {
	s.mu.Lock()
	if !s.loop && !s.closed {
		s.loop = true
		s.wg.Add(1)
		go s.schedule()
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *mobileWorkflowService) revoke() {
	s.mu.Lock()
	for _, cancel := range s.active {
		cancel()
	}
	s.mu.Unlock()
}

// Clearing a workflow must stop it without erasing its last verified
// observation. The screenshot is historical evidence for the task/UI, even
// when a later owner pause or cancellation prevents any next effect.
func mobileWorkflowClearedRecord(record mobileWorkflowRecord) json.RawMessage {
	if !json.Valid(record.State) {
		return json.RawMessage(`{"state":{}}`)
	}
	record.Claim, record.Fact = "", nil
	raw, err := json.Marshal(record)
	if err != nil {
		return json.RawMessage(`{"state":{}}`)
	}
	return raw
}
func mobileWorkflowCancelAllowed(caller, original mobile.Source) bool {
	return caller.Admin && caller.Account == "" && caller.Thread == "" && caller.Sender == "" || caller.Account != "" && caller.Thread != "" && caller.Sender != "" && caller.Account == original.Account && caller.Thread == original.Thread && caller.Sender == original.Sender
}
func (s *mobileWorkflowService) Save(settings mobile.Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, store, err := s.load(true)
	if err != nil {
		return err
	}
	if settings.Revision != snapshot.settings.Revision {
		return errors.New("device_configuration_changed")
	}
	settings.Revision++
	puts := map[string]json.RawMessage{}
	puts["settings"], _ = json.Marshal(settings)
	for id, task := range snapshot.tasks {
		if mobileWorkflowActive(task) {
			task.State, task.Reason, task.Updated = "cancelled", "device_permission_changed", time.Now().UTC()
			puts[mobileTaskRecordKey(id)], _ = json.Marshal(task)
			puts[mobileWorkflowKey(id)] = mobileWorkflowClearedRecord(snapshot.records[id])
		}
	}
	// Settings are protected host metadata. Revocation stops in-flight authority
	// before storage is attempted; a failed write can never leave device I/O live.
	s.revoke()
	return s.commit(snapshot, store, puts)
}
func (s *mobileWorkflowService) Cancel(id string, source mobile.Source) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, store, err := s.load(false)
	if err != nil {
		return err
	}
	task, ok := snapshot.tasks[id]
	if !ok || !mobileWorkflowCancelAllowed(source, task.Source) {
		return errors.New("device_task_not_found")
	}
	original := task.Source
	if source.Admin && source.Account == "" && source.Thread == "" {
		original = source
	}
	if !s.manager.mobileSourceAllowed(original) {
		return errors.New("device_task_not_found")
	}
	if task.State == "completed" || task.State == "cancelled" {
		return nil
	}
	task.State, task.Reason, task.Updated = "cancelled", "device_cancelled_by_owner", time.Now().UTC()
	raw, _ := json.Marshal(task)
	s.mu.Lock()
	if cancel := s.active[id]; cancel != nil {
		cancel()
	}
	s.mu.Unlock()
	return s.commit(snapshot, store, map[string]json.RawMessage{mobileTaskRecordKey(id): raw, mobileWorkflowKey(id): mobileWorkflowClearedRecord(snapshot.records[id])})
}
func (s *mobileWorkflowService) StopAll() error {
	s.mu.Lock()
	loaded := s.loaded
	s.mu.Unlock()
	if !loaded {
		return nil
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, store, err := s.load(false)
	s.revoke()
	if err != nil {
		return err
	}
	puts := map[string]json.RawMessage{}
	for id, task := range snapshot.tasks {
		if mobileWorkflowActive(task) {
			task.State, task.Reason, task.Updated = "cancelled", "device_cancelled_by_owner", time.Now().UTC()
			puts[mobileTaskRecordKey(id)], _ = json.Marshal(task)
			puts[mobileWorkflowKey(id)] = mobileWorkflowClearedRecord(snapshot.records[id])
		}
	}
	if len(puts) == 0 {
		return nil
	}
	return s.commit(snapshot, store, puts)
}
func (s *mobileWorkflowService) Close() error {
	err := s.StopAll()
	s.mu.Lock()
	s.closed = true
	for _, cancel := range s.active {
		cancel()
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.wg.Wait()
	s.mu.Lock()
	bridges := s.bridges
	s.mu.Unlock()
	for _, bridge := range bridges {
		err = errors.Join(err, bridge.Close())
	}
	return err
}
func (s *mobileWorkflowService) Start(source mobile.Source, device, goal string) (mobile.Task, error) {
	if !s.manager.mobileSourceAllowed(source) || source.MessageID == "" || len(source.MessageID) > 8192 {
		return mobile.Task{}, errors.New("device_task_not_permitted")
	}
	fishID, bindingID, fishBound, err := s.manager.mobileAccountDeviceBinding(device)
	if err != nil {
		return mobile.Task{}, err
	}
	if fishBound && bindingID == "" {
		return mobile.Task{}, errors.New("device_fish_binding_required")
	}
	if s.devicePaused(device) {
		return mobile.Task{}, errors.New("device_observation_paused")
	}
	if err := s.recover(); err != nil {
		return mobile.Task{}, err
	}
	hashParts := []any{source, device, goal}
	if fishBound {
		// Preserve legacy owner-task idempotency exactly.  A fish-bound task
		// deliberately includes the revocable binding token so a rebind can
		// never reuse an old operation's idempotency identity.
		hashParts = append(hashParts, fishID, bindingID)
	}
	requestHash := namedSpeechSHA(mustJSON(hashParts))
	id := "device-" + randomToken()
	if len(id) > 128 {
		id = id[:128]
	}
	raw, err := s.invoke(context.Background(), "start", map[string]any{"id": id, "device_id": device, "fish_id": fishID, "device_binding_id": bindingID, "goal": goal, "source": source, "request_hash": requestHash}, nil)
	if err != nil {
		return mobile.Task{}, err
	}
	var task mobile.Task
	if json.Unmarshal(raw, &task) != nil || task.ID == "" {
		return task, errors.New("device_workflow_result_invalid")
	}
	s.signal()
	return task, nil
}
func (s *mobileWorkflowService) recover() error {
	s.mu.Lock()
	done := s.recovered
	s.mu.Unlock()
	if done {
		return nil
	}
	s.stateMu.Lock()
	snapshot, store, err := s.load(false)
	if err != nil {
		s.stateMu.Unlock()
		return err
	}
	active := false
	stale := map[string]json.RawMessage{}
	for id, task := range snapshot.tasks {
		if mobileWorkflowActive(task) {
			if bindingErr := s.manager.mobileTaskBindingCurrent(task); bindingErr != nil {
				task.State, task.Reason, task.Updated = "stopped", "device_fish_binding_changed", time.Now().UTC()
				stale[mobileTaskRecordKey(id)], _ = json.Marshal(task)
				stale[mobileWorkflowKey(id)] = mobileWorkflowClearedRecord(snapshot.records[id])
				continue
			}
		}
		if mobileWorkflowActive(task) && mobileObservationOnly(snapshot.records[id]) {
			s.mu.Lock()
			live := s.active[id] != nil
			s.mu.Unlock()
			if live {
				// This is an in-process ObserveOnce, not a restart remnant.
				continue
			}
			task.State, task.Reason, task.Updated = "cancelled", "device_observation_interrupted", time.Now().UTC()
			stale[mobileTaskRecordKey(id)], _ = json.Marshal(task)
			stale[mobileWorkflowKey(id)] = mobileWorkflowClearedRecord(snapshot.records[id])
			continue
		}
		active = active || mobileWorkflowActive(task)
	}
	if len(stale) > 0 {
		err = s.commit(snapshot, store, stale)
		if err != nil {
			s.stateMu.Unlock()
			return err
		}
	}
	s.stateMu.Unlock()
	if active {
		if _, err = s.invoke(context.Background(), "recover", map[string]any{}, nil); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.recovered = true
	s.mu.Unlock()
	return nil
}
func mobileObservationOnly(record mobileWorkflowRecord) bool {
	var state struct {
		ObservationOnly bool `json:"observation_only"`
	}
	return json.Unmarshal(record.State, &state) == nil && state.ObservationOnly
}
func (s *mobileWorkflowService) Devices(ctx context.Context) (json.RawMessage, error) {
	bridge, err := s.manager.newMobilePluginBridge(ctx, mobile.Task{Source: mobile.Source{Admin: true}}, s.Settings())
	if err != nil {
		return nil, err
	}
	defer bridge.Close()
	raw, err := bridge.Call(ctx, "devices", json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}
func (s *mobileWorkflowService) Screenshot(taskID, observationID string) ([]byte, error) {
	task, _ := s.Get(taskID)
	return readMobileScreenshot(s.manager.dir, task, observationID)
}
func (s *mobileWorkflowService) Artifact(taskID, artifactID string) (mobile.Artifact, []byte, error) {
	task, _ := s.Get(taskID)
	return readMobileArtifact(s.manager.dir, task, artifactID)
}

var _ deviceService = (*mobileWorkflowService)(nil)

type mobileWorkflowPatch struct {
	Settings  json.RawMessage            `json:"settings,omitempty"`
	Tasks     map[string]mobile.Task     `json:"tasks"`
	Workflows map[string]json.RawMessage `json:"workflows"`
	Result    json.RawMessage            `json:"result"`
}
type mobileWorkflowPolicy struct {
	service        *mobileWorkflowService
	store          *pluginhost.Store
	installed      pluginhost.InstalledPackage
	configRevision int
	snapshot       mobileWorkflowSnapshot
	op             string
	input          json.RawMessage
	fact           *mobileWorkflowFact
	now            time.Time
	invocation     pluginhost.Invocation
	arguments      json.RawMessage
}
type mobileWorkflowCheckpoint struct {
	JobID  string `json:"job_id"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

func mobileWorkflowView(settings mobile.Settings) map[string]any {
	return map[string]any{"revision": settings.Revision, "enabled": settings.Enabled, "devices": nonNilMobileSlice(settings.Devices), "actions": nonNilMobileSlice(settings.Actions), "max_steps": settings.MaxSteps, "max_seconds": settings.MaxSeconds}
}
func (s *mobileWorkflowService) ready(ctx context.Context) (*pluginhost.Store, pluginhost.InstalledPackage, int, error) {
	store, err := s.manager.packageStore()
	if err != nil {
		return nil, pluginhost.InstalledPackage{}, 0, err
	}
	installed, err := store.Installed(ctx, mobileDevicePluginID)
	if err != nil || !installed.Enabled || installed.Removed || store.CheckConfiguration(ctx, mobileDevicePluginID) != nil {
		return nil, installed, 0, errors.New("device_workflow_plugin_required")
	}
	caps, err := store.Capabilities(ctx, mobileDevicePluginID)
	if err != nil {
		return nil, installed, 0, err
	}
	found := false
	for _, cap := range caps {
		if cap.ID == mobileWorkflowCapability && cap.Implementation == "ready" && cap.Discoverable && !cap.Async {
			found = true
		}
	}
	if !found {
		return nil, installed, 0, errors.New("device_workflow_plugin_required")
	}
	dir, err := store.PackageDirectory(installed.Digest)
	if err != nil {
		return nil, installed, 0, err
	}
	if _, err = pluginhost.Verify(dir); err != nil {
		return nil, installed, 0, err
	}
	config, err := store.Configuration(ctx, mobileDevicePluginID)
	return store, installed, config.Revision, err
}
func (p *mobileWorkflowPolicy) Check(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s := p.service
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || s.manager.native.Account() != p.invocation.Account || s.manager.draining() || job.Plugin != mobileDevicePluginID || job.Tool != mobileWorkflowCapability || cap.ID != "" && cap.ID != mobileWorkflowCapability || !reflect.DeepEqual(job.Context, p.invocation) || !sameJSON(job.Arguments, p.arguments) {
		return errors.New("device_workflow_authorization_changed")
	}
	_, installed, revision, err := s.ready(ctx)
	if err != nil || installed.Digest != p.installed.Digest || installed.Revision != p.installed.Revision || !reflect.DeepEqual(installed.Permissions, p.installed.Permissions) || revision != p.configRevision {
		return errors.New("device_workflow_package_changed")
	}
	if err = pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: p.invocation.Account, Plugin: mobileDevicePluginID, Version: installed.Package.Version, Permissions: installed.Permissions}, p.invocation, []string{"device", "model.invoke"}); err != nil {
		return err
	}
	if p.op == "start" {
		var proposed mobile.Task
		if json.Unmarshal(p.input, &proposed) != nil || p.service.manager.mobileTaskBindingCurrent(proposed) != nil {
			return errors.New("device_fish_binding_changed")
		}
	}
	current, err := p.store.ReadHostState(ctx, mobileDevicePluginID, mobileStateNamespace)
	if err != nil || current.Revision != p.snapshot.host.Revision || current.SourceHash != p.snapshot.host.SourceHash {
		return errors.New("device_workflow_state_changed")
	}
	if job.ID != "" {
		live, err := p.store.Job(ctx, job.ID)
		if err != nil || live.Revision != job.Revision || live.CancelRequested {
			return errors.New("device_workflow_job_changed")
		}
	}
	return nil
}
func (p *mobileWorkflowPolicy) Validate(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	if err := p.Check(ctx, job, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var patch mobileWorkflowPatch
	if mobilePlanStrictJSON(raw, &patch) != nil {
		return pluginruntime.VerifiedResult{}, errors.New("device_workflow_result_invalid")
	}
	if err := p.prove(patch); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if err := p.bindingPatchCurrent(patch); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if len(raw) > 8<<20 {
		return pluginruntime.VerifiedResult{}, errors.New("device_workflow_result_limit")
	}
	root := filepath.Join(p.service.manager.privateWorkspaceFor(p.invocation.Account), "device-workflow-replies")
	if err := ensureManaged(root); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	dir := filepath.Join(root, job.ID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if err := writeExternalPrivate(filepath.Join(dir, "result.json"), raw); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	checkpoint, _ := json.Marshal(mobileWorkflowCheckpoint{JobID: job.ID, SHA256: namedSpeechSHA(raw), Bytes: int64(len(raw))})
	return pluginruntime.VerifiedResult{Result: json.RawMessage(`{"host_verified":true,"wechat_sends":0}`), Private: checkpoint}, nil
}

// bindingPatchCurrent is checked after package execution and again immediately
// before commit.  A fish/account or device unlink while planning therefore
// cannot turn a late result into a completed task. Cleanup operations remain
// allowed to retire already-revoked tasks.
func (p *mobileWorkflowPolicy) bindingPatchCurrent(patch mobileWorkflowPatch) error {
	if p.op == "cancel" || p.op == "stop" || p.op == "recover" || p.op == "save" {
		return nil
	}
	for _, task := range patch.Tasks {
		if err := p.service.manager.mobileTaskBindingCurrent(task); err != nil {
			return errors.New("device_fish_binding_changed")
		}
	}
	return nil
}
func (p *mobileWorkflowPolicy) prove(patch mobileWorkflowPatch) error {
	denied := errors.New("device_workflow_result_changed")
	if patch.Settings != nil {
		return denied
	}
	if patch.Tasks == nil || patch.Workflows == nil || len(patch.Tasks) > 200 {
		return denied
	}
	if p.op == "advance" && len(patch.Tasks) > 1 {
		return denied
	}
	var input map[string]json.RawMessage
	_ = json.Unmarshal(p.input, &input)
	for id, next := range patch.Tasks {
		old, exists := p.snapshot.tasks[id]
		if !exists {
			var expected mobile.Task
			_ = json.Unmarshal(p.input, &expected)
			if p.op != "start" || id != expected.ID || next.ID != expected.ID || next.RequestHash != expected.RequestHash || next.Source != expected.Source || next.DeviceID != expected.DeviceID || next.FishID != expected.FishID || next.DeviceBindingID != expected.DeviceBindingID || next.Goal != expected.Goal || next.State != "queued" || next.Steps != 0 || !next.Created.Equal(p.now) || !next.Updated.Equal(p.now) || len(next.Events) != 0 || len(next.Artifacts) != 0 {
				return denied
			}
			continue
		}
		if mobileObservationOnly(p.snapshot.records[id]) {
			// These records are created by the host dashboard and are never
			// mutable workflow state, even if a stale package returns them.
			if !reflect.DeepEqual(old, next) {
				return denied
			}
			continue
		}
		if id != next.ID || old.ID != next.ID || old.RequestHash != next.RequestHash || old.Source != next.Source || old.DeviceID != next.DeviceID || old.FishID != next.FishID || old.DeviceBindingID != next.DeviceBindingID || old.Goal != next.Goal || !old.Created.Equal(next.Created) || next.Steps < old.Steps || next.Steps > old.Steps+1 {
			return denied
		}
		if !contains([]string{"queued", "running", "completed", "cancelled", "failed", "stopped", "needs_review"}, next.State) {
			return denied
		}
		if p.op == "recover" {
			if !mobileWorkflowActive(old) || next.State != "needs_review" || next.Reason != "device_worker_interrupted_reobserve_required" || next.Steps != old.Steps {
				return denied
			}
		}
		if p.op == "advance" {
			var selected string
			_ = json.Unmarshal(input["task_id"], &selected)
			if selected != "" && selected != id {
				return denied
			}
			if !mobileWorkflowActive(old) {
				if !reflect.DeepEqual(old, next) {
					return denied
				}
			}
			if next.State == "completed" {
				var fact struct {
					Step mobile.Step `json:"step"`
				}
				var metadata struct {
					Effect mobileWorkflowEffect `json:"effect"`
				}
				_ = json.Unmarshal(p.snapshot.records[id].State, &metadata)
				if p.fact == nil || p.fact.Error != "" || metadata.Effect.Capability != "device.plan" || json.Unmarshal(p.fact.Value, &fact) != nil || fact.Step.Action != "finish" || next.Result != fact.Step.Summary {
					return denied
				}
			}
			if len(next.Artifacts) < len(old.Artifacts) || len(next.Artifacts) > len(old.Artifacts)+1 || len(old.Artifacts) > 0 && !reflect.DeepEqual(next.Artifacts[:len(old.Artifacts)], old.Artifacts) {
				return denied
			}
			if len(next.Artifacts) > len(old.Artifacts) {
				var fact struct {
					Summary string `json:"summary"`
					AssetID string `json:"asset_id"`
				}
				if p.fact == nil || p.fact.Error != "" || json.Unmarshal(p.fact.Value, &fact) != nil {
					return denied
				}
				a := next.Artifacts[len(old.Artifacts)]
				if a.Summary != fact.Summary || a.AssetID != fact.AssetID {
					return denied
				}
			}
		}
	}
	for id, state := range patch.Workflows {
		task, ok := patch.Tasks[id]
		if !ok || !json.Valid(state) {
			return denied
		}
		var metadata map[string]json.RawMessage
		if json.Unmarshal(state, &metadata) != nil {
			return denied
		}
		for key := range metadata {
			if !contains([]string{"phase", "iteration", "next_at", "step", "observation", "fresh", "artifact", "offset", "started", "deadline", "serial", "effect"}, key) {
				return denied
			}
		}
		if oldRaw := p.snapshot.records[id].State; len(oldRaw) > 0 {
			var old map[string]json.RawMessage
			_ = json.Unmarshal(oldRaw, &old)
			if len(old["step"]) > 0 && len(metadata["step"]) > 0 && !sameJSON(old["step"], metadata["step"]) {
				return denied
			}
			for _, key := range []string{"started", "deadline"} {
				if len(old[key]) > 0 && (mobileWorkflowActive(task) || len(metadata[key]) > 0) && !bytes.Equal(old[key], metadata[key]) {
					return denied
				}
			}
		}
		previous := p.snapshot.records[id]
		var oldMetadata map[string]json.RawMessage
		_ = json.Unmarshal(previous.State, &oldMetadata)
		consuming := p.fact != nil && previous.Fact != nil && reflect.DeepEqual(p.fact, previous.Fact)
		if p.op == "advance" && (previous.Claim != "" || previous.Fact != nil) && !consuming {
			if !sameJSON(previous.State, state) || !reflect.DeepEqual(p.snapshot.tasks[id], task) {
				return denied
			}
		}
		var previousEffect mobileWorkflowEffect
		_ = json.Unmarshal(oldMetadata["effect"], &previousEffect)
		var factFields map[string]json.RawMessage
		if p.fact != nil {
			_ = json.Unmarshal(p.fact.Value, &factFields)
		}
		for _, field := range []string{"step", "observation", "fresh", "artifact"} {
			value := metadata[field]
			if len(value) == 0 || sameJSON(value, oldMetadata[field]) {
				continue
			}
			if !consuming || p.fact == nil || p.fact.Error != "" {
				return denied
			}
			expected := factFields[field]
			switch field {
			case "step":
				if previousEffect.Capability != "device.plan" {
					return denied
				}
			case "observation", "fresh":
				if previousEffect.Capability != "device.observe" {
					return denied
				}
				expected = p.fact.Value
			case "artifact":
				if previousEffect.Capability != "device.record" {
					return denied
				}
			}
			if !consuming || p.fact.Error != "" || !sameJSON(value, expected) {
				return denied
			}
		}
		var iteration, oldIteration int
		_ = json.Unmarshal(metadata["iteration"], &iteration)
		_ = json.Unmarshal(oldMetadata["iteration"], &oldIteration)
		if iteration < oldIteration || iteration > oldIteration+1 {
			return denied
		}
		if raw := metadata["started"]; len(raw) > 0 {
			var started string
			_ = json.Unmarshal(raw, &started)
			var old map[string]json.RawMessage
			_ = json.Unmarshal(p.snapshot.records[id].State, &old)
			if len(old["started"]) == 0 && started != p.now.Format(time.RFC3339Nano) {
				return denied
			}
			var deadline string
			_ = json.Unmarshal(metadata["deadline"], &deadline)
			a, e := time.Parse(time.RFC3339Nano, started)
			b, f := time.Parse(time.RFC3339Nano, deadline)
			if e != nil || f != nil || !b.Equal(a.Add(time.Duration(p.snapshot.settings.MaxSeconds)*time.Second)) {
				return denied
			}
		}
		var effect mobileWorkflowEffect
		if len(metadata["effect"]) > 0 {
			if json.Unmarshal(metadata["effect"], &effect) != nil || effect.ID == "" || !validID(effect.ID) || !saneWorkflowEffect(p.snapshot.settings, task, effect) {
				return denied
			}
			if !p.service.manager.mobileSourceAllowed(task.Source) {
				return denied
			}
			if err := p.proveEffect(task, metadata, effect); err != nil {
				return err
			}
		}
		if task.Steps > p.snapshot.tasks[id].Steps && effect.Capability != "device.act" && effect.Capability != "device.record" {
			return denied
		}
	}
	if p.op == "start" {
		var expected struct {
			RequestHash string `json:"request_hash"`
		}
		_ = json.Unmarshal(p.input, &expected)
		var result mobile.Task
		if json.Unmarshal(patch.Result, &result) != nil || result.RequestHash != expected.RequestHash {
			return denied
		}
		saved, ok := patch.Tasks[result.ID]
		if !ok {
			saved, ok = p.snapshot.tasks[result.ID]
		}
		if !ok || !reflect.DeepEqual(saved, result) {
			return denied
		}
	}
	return nil
}
func saneWorkflowEffect(settings mobile.Settings, task mobile.Task, effect mobileWorkflowEffect) bool {
	if !mobileWorkflowActive(task) || !contains(settings.Devices, task.DeviceID) {
		return false
	}
	var args map[string]json.RawMessage
	if mobilePlanStrictJSON(effect.Arguments, &args) != nil || args == nil {
		return false
	}
	return contains([]string{"device.observe", "device.plan", "device.act", "device.record", "device.read_artifact", "artifact.analyze"}, effect.Capability)
}
func (p *mobileWorkflowPolicy) proveEffect(task mobile.Task, metadata map[string]json.RawMessage, effect mobileWorkflowEffect) error {
	denied := errors.New("device_workflow_effect_unverified")
	var args map[string]json.RawMessage
	_ = json.Unmarshal(effect.Arguments, &args)
	if effect.Capability == "device.plan" {
		var id string
		_ = json.Unmarshal(args["observation_id"], &id)
		if len(args) != 1 || id != task.ObservationID || !regexpMobileObservation(id) {
			return denied
		}
		return nil
	}
	if effect.Capability == "artifact.analyze" {
		var id string
		_ = json.Unmarshal(args["artifact_id"], &id)
		var artifact mobile.Artifact
		if len(args) != 1 || json.Unmarshal(metadata["artifact"], &artifact) != nil || id != artifact.ID {
			return denied
		}
		return nil
	}
	var request map[string]any
	if mobilePlanStrictJSON(effect.Arguments, &request) != nil {
		return denied
	}
	bridge := &mobileDevicePluginBridge{observations: map[string]mobile.Observation{}, artifacts: map[string]mobile.Artifact{}}
	if effect.Capability == "device.act" || effect.Capability == "device.record" {
		var step mobile.Step
		var fresh mobile.Observation
		if json.Unmarshal(metadata["step"], &step) != nil || json.Unmarshal(metadata["fresh"], &fresh) != nil {
			return denied
		}
		if fresh.ID != task.ObservationID {
			return denied
		}
		bridge.observations[fresh.ID] = fresh
		expected := map[string]any{}
		for key, value := range step.Arguments {
			expected[key] = value
		}
		for _, key := range []string{"device_id", "observation_id", "request_key", "action"} {
			expected[key] = request[key]
		}
		if expected["device_id"] != task.DeviceID || expected["observation_id"] != task.ObservationID || expected["action"] != step.Action || !sameJSON(mustJSON(expected), effect.Arguments) {
			return denied
		}
		if step.Action != "capture" && !contains(p.snapshot.settings.Actions, step.Action) {
			return denied
		}
		var old map[string]json.RawMessage
		_ = json.Unmarshal(p.snapshot.records[task.ID].State, &old)
		if len(old["step"]) == 0 || !sameJSON(old["step"], metadata["step"]) {
			return denied
		}
		return bridge.validateNativeRequest(task, p.snapshot.settings, effect.Capability, request)
	}
	if effect.Capability == "device.read_artifact" {
		var artifact mobile.Artifact
		if json.Unmarshal(metadata["artifact"], &artifact) != nil {
			return denied
		}
		bridge.artifacts[artifact.ID] = artifact
		return bridge.validateNativeRequest(task, p.snapshot.settings, effect.Capability, request)
	}
	return bridge.validateNativeRequest(task, p.snapshot.settings, effect.Capability, request)
}
func (s *mobileWorkflowService) invoke(ctx context.Context, op string, input map[string]any, fact *mobileWorkflowFact) (json.RawMessage, error) {
	controller, ready, err := s.manager.installedPluginRuntime()
	if err != nil || !ready {
		return nil, errors.New("device_workflow_plugin_required")
	}
	store, installed, _, err := s.ready(ctx)
	if err != nil {
		return nil, err
	}
	if err = controller.EnsureStarted(ctx, mobileDevicePluginID, installed.Revision); err != nil {
		return nil, err
	}
	s.stateMu.Lock()
	snapshot, _, err := s.load(true)
	s.stateMu.Unlock()
	if err != nil {
		return nil, err
	}
	_, installed, revision, err := s.ready(ctx)
	if err != nil {
		return nil, err
	}
	if fact != nil {
		id, _ := input["task_id"].(string)
		saved := snapshot.records[id]
		var metadata struct {
			Effect *mobileWorkflowEffect `json:"effect"`
		}
		_ = json.Unmarshal(saved.State, &metadata)
		if saved.Fact == nil || !reflect.DeepEqual(saved.Fact, fact) || metadata.Effect == nil || metadata.Effect.ID != fact.EffectID {
			return nil, errors.New("device_workflow_fact_changed")
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	workflows := map[string]json.RawMessage{}
	workflowTasks := map[string]mobile.Task{}
	for id, record := range snapshot.records {
		if mobileObservationOnly(record) {
			// A dashboard observation is host-owned evidence, not an input to
			// the package state machine (including recover).
			continue
		}
		if task, ok := snapshot.tasks[id]; ok {
			workflowTasks[id] = task
		}
		if len(record.State) > 0 {
			workflows[id] = record.State
		}
	}
	for id, task := range snapshot.tasks {
		if _, recorded := snapshot.records[id]; !recorded {
			workflowTasks[id] = task
		}
	}
	rawInput, _ := json.Marshal(input)
	arguments, _ := json.Marshal(map[string]any{"op": op, "snapshot": map[string]any{"settings": mobileWorkflowView(snapshot.settings), "tasks": workflowTasks, "workflows": workflows}, "input": input, "now": now.Format(time.RFC3339Nano), "nonce": randomToken(), "facts": func() any {
		if fact == nil {
			return map[string]any{}
		}
		return fact
	}()})
	authorization := agentID(store.Account(), installed.Digest, fmt.Sprint(revision), fmt.Sprint(snapshot.host.Revision), namedSpeechSHA(arguments))
	inv := pluginhost.Invocation{Account: store.Account(), Thread: "host:mobile-workflow", Requester: "administrator", Purpose: "advance authorized device workflow " + op, RequestKey: "mobile-workflow-" + authorization, Authorization: authorization}
	if sourceRaw, ok := input["source"]; ok {
		encoded, _ := json.Marshal(sourceRaw)
		var source mobile.Source
		_ = json.Unmarshal(encoded, &source)
		if source.Thread != "" && source.Sender != "" {
			inv.Thread, inv.Requester, inv.Purpose = source.Thread, source.Sender, "create authorized device task"
		}
	}
	policy := &mobileWorkflowPolicy{service: s, store: store, installed: installed, configRevision: revision, snapshot: snapshot, op: op, input: rawInput, fact: fact, now: now, invocation: inv, arguments: arguments}
	call, err := controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: mobileDevicePluginID, Capability: mobileWorkflowCapability, Context: inv, Arguments: arguments}, policy)
	if err != nil || call.Job.State != "completed" {
		return nil, errors.New("device_workflow_needs_review")
	}
	private, err := store.PrivateJobResult(ctx, call.Job, func(ctx context.Context, job pluginhost.Job) error {
		return policy.Check(ctx, job, pluginhost.Capability{})
	})
	if err != nil {
		return nil, err
	}
	var checkpoint mobileWorkflowCheckpoint
	if mobilePlanStrictJSON(private, &checkpoint) != nil || checkpoint.JobID != call.Job.ID || checkpoint.Bytes < 0 || checkpoint.Bytes > 8<<20 {
		return nil, errors.New("device_workflow_result_unavailable")
	}
	root, err := workspacefs.EnsurePath(filepath.Join(s.manager.privateWorkspaceFor(store.Account()), "device-workflow-replies"))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, filepath.Join(checkpoint.JobID, "result.json"))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, checkpoint.Bytes+1))
	_ = file.Close()
	if err != nil || int64(len(data)) != checkpoint.Bytes || namedSpeechSHA(data) != checkpoint.SHA256 {
		return nil, errors.New("device_workflow_result_unavailable")
	}
	var patch mobileWorkflowPatch
	if mobilePlanStrictJSON(data, &patch) != nil {
		return nil, errors.New("device_workflow_result_invalid")
	}
	if err = policy.prove(patch); err != nil {
		return nil, err
	}
	if err = policy.bindingPatchCurrent(patch); err != nil {
		return nil, err
	}
	s.stateMu.Lock()
	if err = policy.Check(ctx, call.Job, pluginhost.Capability{}); err != nil {
		s.stateMu.Unlock()
		return nil, err
	}
	if err = policy.bindingPatchCurrent(patch); err != nil {
		s.stateMu.Unlock()
		return nil, err
	}
	puts := map[string]json.RawMessage{}
	for id, task := range patch.Tasks {
		puts[mobileTaskRecordKey(id)], _ = json.Marshal(task)
	}
	for id, state := range patch.Workflows {
		record := snapshot.records[id]
		record.State = state
		if op == "recover" || fact != nil && record.Fact != nil && reflect.DeepEqual(record.Fact, fact) {
			record.Fact = nil
			record.Claim = ""
		}
		puts[mobileWorkflowKey(id)], _ = json.Marshal(record)
	}
	if len(puts) > 0 {
		if err = s.commit(snapshot, store, puts); err != nil {
			s.stateMu.Unlock()
			return nil, err
		}
	}
	s.stateMu.Unlock()
	for id, next := range patch.Tasks {
		old, exists := snapshot.tasks[id]
		if exists && mobileWorkflowActive(old) && !mobileWorkflowActive(next) {
			s.manager.deviceFinished(next)
		}
	}
	return append(json.RawMessage(nil), patch.Result...), nil
}

// schedule dispatches persisted capability intents, not mobile workflow steps.
// Each dispatch is exactly one installed tool call; the next state transition
// is a later tick, after its immutable host fact has been committed.
func (s *mobileWorkflowService) schedule() {
	defer s.wg.Done()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		s.tick()
		select {
		case <-s.wake:
		case <-ticker.C:
		}
	}
}
func (s *mobileWorkflowService) tick() {
	s.mu.Lock()
	closed := s.closed
	workflowBusy := s.active["@workflow"] != nil
	s.mu.Unlock()
	if closed || workflowBusy {
		return
	}
	s.stateMu.Lock()
	snapshot, store, err := s.load(false)
	s.stateMu.Unlock()
	if err != nil || store == nil {
		return
	}
	// A fish/device unlink can happen while a model is planning.  Retire its
	// durable task before it can be selected for another workflow transition.
	invalid := map[string]json.RawMessage{}
	for id, task := range snapshot.tasks {
		if !mobileWorkflowActive(task) || mobileObservationOnly(snapshot.records[id]) || s.manager.mobileTaskBindingCurrent(task) == nil {
			continue
		}
		task.State, task.Reason, task.Updated = "stopped", "device_fish_binding_changed", time.Now().UTC()
		invalid[mobileTaskRecordKey(id)], _ = json.Marshal(task)
		invalid[mobileWorkflowKey(id)] = mobileWorkflowClearedRecord(snapshot.records[id])
	}
	if len(invalid) > 0 {
		s.stateMu.Lock()
		fresh, freshStore, loadErr := s.load(false)
		if loadErr == nil && fresh.host.Revision == snapshot.host.Revision {
			_ = s.commit(fresh, freshStore, invalid)
		}
		s.stateMu.Unlock()
		return
	}
	ids := []string{}
	for id, task := range snapshot.tasks {
		if mobileWorkflowActive(task) && !mobileObservationOnly(snapshot.records[id]) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		record := snapshot.records[id]
		var metadata struct {
			Effect   *mobileWorkflowEffect `json:"effect"`
			Deadline time.Time             `json:"deadline"`
		}
		if json.Unmarshal(record.State, &metadata) != nil || metadata.Effect == nil || record.Fact != nil {
			continue
		}
		task := snapshot.tasks[id]
		s.mu.Lock()
		busy := s.active[id] != nil || s.devices[task.DeviceID] != ""
		s.mu.Unlock()
		if busy {
			continue
		}
		if record.Claim != "" {
			s.finishFact(id, record.Claim, mobileWorkflowFact{EffectID: metadata.Effect.ID, Error: "device_action_outcome_unknown"}, snapshot.settings, task)
			return
		}
		s.dispatch(snapshot, task, record, *metadata.Effect, metadata.Deadline)
		return
	}
	for _, id := range ids {
		if snapshot.records[id].Fact != nil {
			s.flowTick(id, snapshot.records[id].Fact)
			return
		}
	}
	running := map[string]bool{}
	for _, id := range ids {
		if snapshot.tasks[id].State == "running" {
			running[snapshot.tasks[id].DeviceID] = true
		}
	}
	for _, id := range ids {
		record := snapshot.records[id]
		var metadata struct {
			Effect *mobileWorkflowEffect `json:"effect"`
			Next   time.Time             `json:"next_at"`
		}
		_ = json.Unmarshal(record.State, &metadata)
		task := snapshot.tasks[id]
		if metadata.Effect != nil || !metadata.Next.IsZero() && time.Now().Before(metadata.Next) || task.State == "queued" && running[task.DeviceID] {
			continue
		}
		s.flowTick("", nil)
		return
	}
}
func (s *mobileWorkflowService) flowTick(id string, fact *mobileWorkflowFact) {
	s.mu.Lock()
	if s.closed || s.active["@workflow"] != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.active["@workflow"] = cancel
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer func() { s.mu.Lock(); delete(s.active, "@workflow"); s.mu.Unlock(); s.signal() }()
		_, _ = s.invoke(ctx, "advance", map[string]any{"task_id": id}, fact)
	}()
}
func (s *mobileWorkflowService) dispatch(snapshot mobileWorkflowSnapshot, task mobile.Task, record mobileWorkflowRecord, effect mobileWorkflowEffect, deadline time.Time) {
	paused := s.devicePaused(task.DeviceID)
	if !s.manager.mobileSourceAllowed(task.Source) || paused {
		s.stateMu.Lock()
		fresh, store, err := s.load(false)
		if err == nil {
			current, ok := fresh.tasks[task.ID]
			if ok && current.RequestHash == task.RequestHash && mobileWorkflowActive(current) {
				current.State, current.Reason, current.Updated = "stopped", "device_permission_changed", time.Now().UTC()
				if paused {
					current.State, current.Reason = "cancelled", "device_observation_paused"
				}
				raw, _ := json.Marshal(current)
				_ = s.commit(fresh, store, map[string]json.RawMessage{mobileTaskRecordKey(task.ID): raw, mobileWorkflowKey(task.ID): mobileWorkflowClearedRecord(fresh.records[task.ID])})
			}
		}
		s.stateMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if !deadline.IsZero() {
		ctx, cancel = context.WithDeadline(context.Background(), deadline)
	}
	s.mu.Lock()
	if s.closed || s.active[task.ID] != nil || s.devices[task.DeviceID] != "" {
		s.mu.Unlock()
		cancel()
		return
	}
	s.active[task.ID] = cancel
	s.devices[task.DeviceID] = task.ID
	s.mu.Unlock()
	token := randomToken()
	record.Claim = token
	s.stateMu.Lock()
	current, store, err := s.load(false)
	if err == nil && current.host.Revision == snapshot.host.Revision {
		raw, _ := json.Marshal(record)
		err = s.commit(current, store, map[string]json.RawMessage{mobileWorkflowKey(task.ID): raw})
	} else if err == nil {
		err = pluginhost.ErrRevision
	}
	s.stateMu.Unlock()
	if err != nil {
		cancel()
		s.mu.Lock()
		delete(s.active, task.ID)
		delete(s.devices, task.DeviceID)
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.active, task.ID)
			delete(s.devices, task.DeviceID)
			s.mu.Unlock()
			s.signal()
		}()
		value, err := s.execute(ctx, task, snapshot.settings, effect)
		fact := mobileWorkflowFact{EffectID: effect.ID, Value: value}
		if err != nil {
			fact.Value = nil
			fact.Error = "device_operation_unavailable"
			var failure *RuntimeFailure
			if errors.As(err, &failure) && strings.HasPrefix(failure.Code, "device_") {
				fact.Error = failure.Code
			}
			if effect.Capability == "device.act" || effect.Capability == "device.record" {
				fact.Error = "device_action_outcome_unknown"
			}
		}
		s.finishFact(task.ID, token, fact, snapshot.settings, task)
	}()
}
func (s *mobileWorkflowService) finishFact(id, token string, fact mobileWorkflowFact, settings mobile.Settings, task mobile.Task) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	snapshot, store, err := s.load(false)
	if err != nil {
		return
	}
	current, ok := snapshot.tasks[id]
	record := snapshot.records[id]
	if !ok || !reflect.DeepEqual(current, task) || !reflect.DeepEqual(snapshot.settings, settings) || record.Claim != token || !mobileWorkflowActive(current) {
		return
	}
	if s.manager.mobileTaskBindingCurrent(current) != nil {
		current.State, current.Reason, current.Updated = "stopped", "device_fish_binding_changed", time.Now().UTC()
		raw, _ := json.Marshal(current)
		_ = s.commit(snapshot, store, map[string]json.RawMessage{mobileTaskRecordKey(id): raw, mobileWorkflowKey(id): mobileWorkflowClearedRecord(record)})
		return
	}
	record.Fact = &fact
	record.Claim = ""
	raw, _ := json.Marshal(record)
	_ = s.commit(snapshot, store, map[string]json.RawMessage{mobileWorkflowKey(id): raw})
}
func (s *mobileWorkflowService) bridge(ctx context.Context, task mobile.Task, settings mobile.Settings) (*mobileDevicePluginBridge, error) {
	s.mu.Lock()
	bridge := s.bridges[task.ID]
	s.mu.Unlock()
	if bridge != nil {
		return bridge, nil
	}
	created, err := s.manager.newMobilePluginBridge(ctx, task, settings)
	if err != nil {
		return nil, err
	}
	bridge, ok := created.(*mobileDevicePluginBridge)
	if !ok {
		_ = created.Close()
		return nil, errors.New("device_plugin_broker_unavailable")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = bridge.Close()
		return nil, errDeviceServiceClosed
	}
	s.bridges[task.ID] = bridge
	s.mu.Unlock()
	return bridge, nil
}
func (s *mobileWorkflowService) execute(ctx context.Context, task mobile.Task, settings mobile.Settings, effect mobileWorkflowEffect) (json.RawMessage, error) {
	// Recheck immediately before every effect. A takeover which races a queued
	// task must stop observe as well as device.act/record, not merely hide it.
	if s.devicePaused(task.DeviceID) {
		return nil, errors.New("device_observation_paused")
	}
	if err := s.manager.mobileDeviceCurrent(task, settings); err != nil {
		return nil, err
	}
	if effect.Capability == "device.plan" {
		observation, err := s.observation(task.ID, task.ObservationID)
		if err != nil {
			return nil, err
		}
		step, err := s.planner(ctx, task, observation)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"step": step})
	}
	if effect.Capability == "artifact.analyze" {
		var args struct {
			ArtifactID string `json:"artifact_id"`
		}
		if json.Unmarshal(effect.Arguments, &args) != nil {
			return nil, errors.New("device_artifact_invalid")
		}
		s.stateMu.Lock()
		snapshot, _, err := s.load(false)
		s.stateMu.Unlock()
		if err != nil {
			return nil, err
		}
		var metadata struct {
			Artifact mobile.Artifact `json:"artifact"`
		}
		if json.Unmarshal(snapshot.records[task.ID].State, &metadata) != nil || metadata.Artifact.ID != args.ArtifactID {
			return nil, errors.New("device_artifact_invalid")
		}
		data, err := readMobileBlob(s.manager.dir, "recordings", metadata.Artifact.ID+".mp4", 128<<20)
		if err != nil {
			return nil, err
		}
		summary, assetID, analysisErr := s.manager.analyzeDeviceArtifact(ctx, task, metadata.Artifact, data)
		if analysisErr != nil {
			summary = "实际屏幕录制；没有音轨，尚未分析画面；不是原视频下载"
			assetID = ""
		}
		return json.Marshal(map[string]any{"summary": summary, "asset_id": assetID, "analysis_failed": analysisErr != nil})
	}
	bridge, err := s.bridge(ctx, task, settings)
	if err != nil {
		return nil, err
	}
	name := "observe"
	switch effect.Capability {
	case "device.observe":
	case "device.act", "device.record":
		name = "act"
	case "device.read_artifact":
		name = "read_artifact"
	default:
		return nil, errors.New("device_workflow_effect_invalid")
	}
	raw, err := bridge.Call(ctx, name, effect.Arguments)
	if err != nil {
		return nil, err
	}
	switch effect.Capability {
	case "device.observe":
		return s.captureObservation([]byte(raw))
	case "device.act", "device.record":
		var result struct {
			State  string `json:"state"`
			Result struct {
				Artifact *mobile.Artifact `json:"artifact"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(raw), &result) != nil {
			return nil, errors.New("device_action_outcome_unknown")
		}
		return json.Marshal(map[string]any{"state": result.State, "artifact": result.Result.Artifact})
	case "device.read_artifact":
		return s.captureArtifact(task, bridge, effect.Arguments, []byte(raw))
	}
	return nil, errors.New("device_workflow_effect_invalid")
}
func (s *mobileWorkflowService) captureObservation(raw []byte) (json.RawMessage, error) {
	var result struct {
		Text  string `json:"text"`
		Media []struct {
			MIME string `json:"mime_type"`
			Data string `json:"data_base64"`
		} `json:"media"`
	}
	var observation mobile.Observation
	if json.Unmarshal(raw, &result) != nil || json.Unmarshal([]byte(result.Text), &observation) != nil || !regexpMobileObservation(observation.ID) || len(result.Media) != 1 || result.Media[0].MIME != "image/png" {
		return nil, errors.New("device_observation_invalid")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(result.Media[0].Data)
	if err != nil || len(data) > 128<<20 {
		return nil, errors.New("device_screenshot_invalid")
	}
	image, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || image.Width != observation.Width || image.Height != observation.Height {
		return nil, errors.New("device_screenshot_invalid")
	}
	if err = s.writeBlob("observations", observation.ID+".png", data); err != nil {
		return nil, err
	}
	value := map[string]any{}
	_ = json.Unmarshal([]byte(result.Text), &value)
	// A screenshot is a historical observation. Record the host-verified
	// observation time once, next to the validated screenshot, rather than
	// reusing task update timestamps which may change during recovery.
	observation.CapturedAt = time.Now().UTC().Truncate(time.Microsecond)
	value["captured_at"] = observation.CapturedAt
	value["png_sha256"] = namedSpeechSHA(data)
	return json.Marshal(value)
}
func (s *mobileWorkflowService) observation(taskID, id string) (mobile.Observation, error) {
	s.stateMu.Lock()
	snapshot, _, err := s.load(false)
	s.stateMu.Unlock()
	if err != nil {
		return mobile.Observation{}, err
	}
	var metadata struct {
		Observation mobile.Observation `json:"observation"`
	}
	if json.Unmarshal(snapshot.records[taskID].State, &metadata) != nil || metadata.Observation.ID != id {
		return mobile.Observation{}, errors.New("device_observation_not_found")
	}
	data, err := readMobileBlob(s.manager.dir, "observations", id+".png", 128<<20)
	if err != nil {
		return mobile.Observation{}, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(snapshot.records[taskID].State, &fields)
	var known struct {
		Hash string `json:"png_sha256"`
	}
	_ = json.Unmarshal(fields["observation"], &known)
	if namedSpeechSHA(data) != known.Hash {
		return mobile.Observation{}, errors.New("device_observation_changed")
	}
	metadata.Observation.PNG = data
	return metadata.Observation, nil
}
func (s *mobileWorkflowService) writeBlob(directory, name string, data []byte) error {
	base := filepath.Join(s.manager.dir, "mobile", directory)
	if err := ensureManaged(base); err != nil {
		return err
	}
	root, err := workspacefs.EnsurePath(base)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	return errors.Join(err, file.Close())
}
func (s *mobileWorkflowService) captureArtifact(task mobile.Task, bridge *mobileDevicePluginBridge, args, raw json.RawMessage) (json.RawMessage, error) {
	var request struct {
		ID     string `json:"artifact_id"`
		Offset int    `json:"offset"`
	}
	var chunk struct {
		ID     string `json:"id"`
		Offset int    `json:"offset"`
		Bytes  int    `json:"bytes"`
		SHA    string `json:"sha256"`
		Data   string `json:"data_base64"`
		EOF    bool   `json:"eof"`
	}
	if json.Unmarshal(args, &request) != nil || json.Unmarshal(raw, &chunk) != nil || chunk.ID != request.ID || chunk.Offset != request.Offset {
		return nil, errors.New("device_recording_chunk_invalid")
	}
	bridge.mu.Lock()
	artifact, ok := bridge.artifacts[chunk.ID]
	bridge.mu.Unlock()
	data, err := base64.StdEncoding.Strict().DecodeString(chunk.Data)
	if !ok || err != nil || len(data) == 0 || chunk.Bytes != artifact.Bytes || chunk.SHA != artifact.SHA256 || chunk.Offset+len(data) > artifact.Bytes || chunk.EOF != (chunk.Offset+len(data) == artifact.Bytes) {
		return nil, errors.New("device_recording_chunk_invalid")
	}
	base := filepath.Join(s.manager.dir, "mobile", "recordings")
	if err = ensureManaged(base); err != nil {
		return nil, err
	}
	root, err := workspacefs.EnsurePath(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := artifact.ID + ".mp4"
	flags := os.O_RDWR
	if request.Offset == 0 {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := root.OpenFile(name, flags, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != int64(request.Offset) {
		return nil, errors.New("device_recording_chunk_invalid")
	}
	if _, err = file.Seek(int64(request.Offset), io.SeekStart); err != nil {
		return nil, err
	}
	if _, err = file.Write(data); err != nil {
		return nil, err
	}
	if chunk.EOF {
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		hash := sha256.New()
		if _, err = io.Copy(hash, file); err != nil || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
			return nil, errors.New("device_recording_integrity_invalid")
		}
		if _, err = file.Seek(4, io.SeekStart); err != nil {
			return nil, err
		}
		header := make([]byte, 4)
		if _, err = io.ReadFull(file, header); err != nil || string(header) != "ftyp" {
			return nil, errors.New("device_recording_integrity_invalid")
		}
	}
	return json.Marshal(map[string]any{"id": artifact.ID, "offset": request.Offset, "next_offset": request.Offset + len(data), "sha256": artifact.SHA256, "complete": chunk.EOF})
}
