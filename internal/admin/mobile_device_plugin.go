package admin

// This file is the host-only bridge for mobile-control's device capabilities.
// The isolated package can request a single already-bound operation, but it
// never sees raw screenshots, recording chunks, an ADB path, or the legacy
// bridge configuration.

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
	"reflect"
	"strings"
	"sync"

	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/workspacefs"
)

const mobileDevicePluginID = "mobile-control"

var mobileDeviceCapabilities = []string{
	"device.list", "device.observe", "device.act", "device.record", "device.read_artifact",
}

// mobileDeviceReady is deliberately non-starting. Discovery and task queueing
// must not start a native process, enable a package, or widen a device grant.
func (m *Manager) mobileDeviceReady(ctx context.Context) error {
	// Windows production keeps legacy device records as read-only history.  Do
	// this before consulting package/runtime state so an archived package cannot
	// be used to infer that a bridge remains runnable.
	if m == nil || m.mobileRetired {
		return errDeviceServiceRetired
	}
	if m.deviceIO == nil {
		return errors.New("device_plugin_broker_unavailable")
	}
	store, err := m.packageStore()
	if err != nil {
		return errors.New("device_plugin_required")
	}
	installed, err := store.Installed(ctx, mobileDevicePluginID)
	if err != nil || !installed.Enabled || installed.Removed || store.CheckConfiguration(ctx, mobileDevicePluginID) != nil {
		return errors.New("device_plugin_required")
	}
	capabilities, err := store.Capabilities(ctx, mobileDevicePluginID)
	if err != nil {
		return errors.New("device_plugin_required")
	}
	ready := map[string]bool{}
	for _, cap := range capabilities {
		if cap.Implementation == "ready" && cap.Discoverable && !cap.Async && reflect.DeepEqual(cap.Permissions, []string{"device"}) {
			ready[cap.ID] = true
		}
	}
	for _, capability := range mobileDeviceCapabilities {
		if !ready[capability] {
			return errors.New("device_plugin_required")
		}
	}
	account := m.native.Account()
	invocation := pluginhost.Invocation{Account: account, Thread: "host:mobile", Requester: "administrator", Purpose: "open mobile device broker", RequestKey: "mobile-device-ready"}
	if pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: account, Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}, invocation, []string{"device"}) != nil {
		return errors.New("device_plugin_required")
	}
	controller, runtimeReady, err := m.installedPluginRuntime()
	if err != nil || !runtimeReady {
		return errors.New("device_plugin_required")
	}
	if state := controller.Status(mobileDevicePluginID).State; state != "stopped" && state != "running" {
		return errors.New("device_plugin_required")
	}
	deployment, err := runtimeDeploymentOptions()
	if err != nil || !mobileDeviceDeploymentPath(deployment.Python, false) || !mobileDeviceDeploymentPath(deployment.CatalogDirectory, true) {
		return errors.New("device_plugin_deployment_unavailable")
	}
	registeredTools := false
	for _, root := range deployment.DeviceToolsRoots {
		registeredTools = registeredTools || mobileDeviceDeploymentPath(root, true)
	}
	if !registeredTools {
		return errors.New("device_plugin_tools_unregistered")
	}
	options, err := m.mobileDeviceIOOptions(store, installed, deployment, m.mobile.Settings())
	if err != nil {
		return err
	}
	if err = m.deviceIO.Inspect(ctx, options); err != nil {
		if errors.Is(err, mobile.ErrNeedsMigration) {
			return errors.New("device_plugin_broker_migration_required")
		}
		return errors.New("device_plugin_broker_unavailable")
	}
	return nil
}

func mobileDeviceDeploymentPath(path string, directory bool) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir() == directory
}

// mobileDevicePluginBridge keeps raw native bridge replies only in the host.
// One bridge is opened for a durable task or narrow management listing and is
// closed by mobile.Control; the isolated plugin process remains controller-
// owned and is never closed per task.
type mobileDevicePluginBridge struct {
	manager      *Manager
	store        *pluginhost.Store
	controller   *pluginruntime.Controller
	installed    pluginhost.InstalledPackage
	task         mobile.Task
	settings     mobile.Settings
	packageDir   string
	options      pluginruntime.Options
	stateDir     string
	brokerOpener func(context.Context, mobile.IOOptions) (mobile.IOBroker, error)
	mu           sync.Mutex
	broker       mobile.IOBroker
	closed       bool
	sequence     uint64
	observations map[string]mobile.Observation
	artifacts    map[string]mobile.Artifact
}

// newMobilePluginBridge is passed to Control.SetBrokerFactory by the manager
// wiring. It has no legacy fallback: an unavailable v0.3 device package means
// the task cannot be sent to any native bridge.
func (m *Manager) newMobilePluginBridge(ctx context.Context, task mobile.Task, settings mobile.Settings) (mobile.Bridge, error) {
	if m == nil || m.mobileRetired {
		return nil, errDeviceServiceRetired
	}
	if err := m.mobileDeviceReady(ctx); err != nil {
		return nil, err
	}
	store, err := m.packageStore()
	if err != nil {
		return nil, errors.New("device_plugin_required")
	}
	installed, err := store.Installed(ctx, mobileDevicePluginID)
	if err != nil || !installed.Enabled || installed.Removed {
		return nil, errors.New("device_plugin_required")
	}
	if err = m.mobileDeviceCurrent(task, settings); err != nil {
		return nil, err
	}
	controller, ready, err := m.installedPluginRuntime()
	if err != nil || !ready {
		return nil, errors.New("device_plugin_required")
	}
	deployment, err := runtimeDeploymentOptions()
	if err != nil || deployment.Python == "" || deployment.CatalogDirectory == "" {
		return nil, errors.New("device_plugin_deployment_unavailable")
	}
	packageDir, err := store.PackageDirectory(installed.Digest)
	if err != nil {
		return nil, errors.New("device_plugin_required")
	}
	// The existing mobile directory is the only compatible private state root:
	// legacy --storage remains usable when it is already beneath this directory.
	// An external ledger is not copied or silently widened; devicebroker reports
	// it as a migration requirement instead.
	stateDir := filepath.Join(m.dir, "mobile")
	if err = os.MkdirAll(stateDir, 0700); err != nil {
		return nil, errors.New("device_plugin_state_unavailable")
	}
	return &mobileDevicePluginBridge{
		manager: m, store: store, controller: controller, installed: installed,
		task: task, settings: settings, packageDir: packageDir, options: deployment,
		stateDir: stateDir, brokerOpener: m.deviceIO.Open,
		observations: map[string]mobile.Observation{}, artifacts: map[string]mobile.Artifact{},
	}, nil
}

func (m *Manager) mobileDeviceCurrent(task mobile.Task, settings mobile.Settings) error {
	if m == nil || m.mobile == nil || !settings.Enabled {
		return errors.New("device_plugin_authorization_changed")
	}
	current := m.mobile.Settings()
	if current.Revision != settings.Revision || !current.Enabled || !reflect.DeepEqual(nonNilMobileSlice(current.Devices), nonNilMobileSlice(settings.Devices)) || !reflect.DeepEqual(nonNilMobileSlice(current.Actions), nonNilMobileSlice(settings.Actions)) {
		return errors.New("device_plugin_settings_changed")
	}
	if task.ID == "" {
		if !task.Source.Admin || task.Source.Account != "" || task.Source.Thread != "" || task.Source.Sender != "" {
			return errors.New("device_plugin_authorization_changed")
		}
		return nil
	}
	currentTask, ok := m.mobile.Get(task.ID)
	if !ok || (currentTask.State != "queued" && currentTask.State != "running") || currentTask.ID != task.ID || currentTask.RequestHash != task.RequestHash || currentTask.DeviceID != task.DeviceID || currentTask.FishID != task.FishID || currentTask.DeviceBindingID != task.DeviceBindingID || currentTask.Goal != task.Goal || currentTask.Steps != task.Steps || currentTask.ObservationID != task.ObservationID || !reflect.DeepEqual(currentTask.Source, task.Source) || !m.mobileSourceAllowed(currentTask.Source) {
		return errors.New("device_plugin_authorization_changed")
	}
	if err := m.mobileTaskBindingCurrent(currentTask); err != nil {
		return err
	}
	return nil
}

// mobileAccountDeviceBinding returns an explicit fish/device binding only when
// this account has already joined the durable fish identity.  Accounts that
// predate that identity deliberately keep their owner-operated device path;
// they are not silently granted autonomous phone access.
func (m *Manager) mobileAccountDeviceBinding(deviceID string) (fishID, bindingID string, fishBound bool, err error) {
	if m == nil || m.fish == nil {
		return "", "", false, nil
	}
	fishID, accounts, err := m.fish.Bindings()
	if err != nil {
		return "", "", false, errors.New("device_fish_identity_unavailable")
	}
	account := m.native.Account()
	if !accounts[account] {
		return "", "", false, nil
	}
	boundID, bindings, err := m.fish.DeviceBindings(account)
	if err != nil || boundID != fishID {
		return "", "", true, errors.New("device_fish_identity_unavailable")
	}
	for _, binding := range bindings {
		if binding.DeviceID == deviceID {
			return fishID, binding.BindingID, true, nil
		}
	}
	return fishID, "", true, nil
}

// mobileTaskBindingCurrent is the revocation fence shared by every durable
// task path.  A rebind mints a new token, so a result from an old task cannot
// become valid again merely because the same device alias is bound later.
func (m *Manager) mobileTaskBindingCurrent(task mobile.Task) error {
	fishID, bindingID, fishBound, err := m.mobileAccountDeviceBinding(task.DeviceID)
	if err != nil {
		return err
	}
	if !fishBound {
		if task.FishID != "" || task.DeviceBindingID != "" {
			return errors.New("device_fish_binding_changed")
		}
		return nil
	}
	if task.FishID == "" || task.DeviceBindingID == "" || task.FishID != fishID || task.DeviceBindingID != bindingID {
		return errors.New("device_fish_binding_changed")
	}
	return nil
}

func (b *mobileDevicePluginBridge) Close() error {
	b.mu.Lock()
	// Closing permanently rejects new calls, but a failed native Close must
	// retain the broker reference so an explicit later Close can retry and so a
	// physical-device lease is not released merely by garbage collection.
	b.closed = true
	broker := b.broker
	b.mu.Unlock()
	if broker == nil {
		return nil
	}
	if err := broker.Close(); err != nil {
		return err
	}
	b.mu.Lock()
	if b.broker == broker {
		b.broker = nil
	}
	b.mu.Unlock()
	return nil
}

func (b *mobileDevicePluginBridge) Call(ctx context.Context, name string, request json.RawMessage) (string, error) {
	policy, err := b.newPolicy(ctx, name, request)
	if err != nil {
		return "", err
	}
	defer policy.closeIO()
	if err = b.mobilePluginCurrent(ctx, policy.task, policy.settings, policy.boundBinding, policy.invocation, policy.capability); err != nil {
		return "", err
	}
	if err = b.controller.EnsureStarted(ctx, mobileDevicePluginID, b.installed.Revision); err != nil {
		return "", err
	}
	call, err := b.controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: mobileDevicePluginID, Capability: policy.capability, Context: policy.invocation, Arguments: policy.arguments}, policy)
	if err != nil || call.Job.State != "completed" {
		if failure := policy.ioFailureValue(); failure != nil {
			return "", failure
		}
		return "", errors.New("device_plugin_needs_review")
	}
	private, err := b.store.PrivateJobResult(ctx, call.Job, func(ctx context.Context, job pluginhost.Job) error {
		return policy.Check(ctx, job, pluginhost.Capability{ID: policy.capability, Permissions: []string{"device"}})
	})
	if err != nil {
		return "", errors.New("device_plugin_result_unavailable")
	}
	var checkpoint mobileDeviceCheckpoint
	if mobilePlanStrictJSON(private, &checkpoint) != nil {
		return "", errors.New("device_plugin_result_unavailable")
	}
	raw, err := policy.readCheckpoint(ctx, call.Job, checkpoint)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (b *mobileDevicePluginBridge) newPolicy(ctx context.Context, name string, raw json.RawMessage) (*mobileDevicePluginPolicy, error) {
	if !json.Valid(raw) {
		return nil, errors.New("device_request_invalid")
	}
	var request map[string]any
	if mobilePlanStrictJSON(raw, &request) != nil || request == nil {
		return nil, errors.New("device_request_invalid")
	}
	capability, err := mobileDeviceCapability(name, request)
	if err != nil {
		return nil, err
	}
	// Control advances the durable task between observations and actions. Read
	// that current task for this *new* operation before taking its immutable
	// snapshot; already-created policies keep their own prior snapshot.
	if b.task.ID != "" {
		current, ok := b.manager.mobile.Get(b.task.ID)
		if !ok || (current.State != "queued" && current.State != "running") {
			return nil, errors.New("device_plugin_authorization_changed")
		}
		b.mu.Lock()
		b.task = current
		b.mu.Unlock()
	}
	if err = b.manager.mobileDeviceCurrent(b.task, b.settings); err != nil {
		return nil, err
	}
	// Freeze the exact task and settings after the live authorization check. A
	// later policy must not make this policy follow a newer observation/step.
	task := cloneMobileTask(b.task)
	settings := cloneMobileSettings(b.settings)
	if err = b.validateNativeRequest(task, settings, capability, request); err != nil {
		return nil, err
	}
	broker, err := b.ensureBroker(ctx, task, settings)
	if err != nil {
		return nil, err
	}
	binding, err := broker.Binding()
	if err != nil || binding == "" {
		return nil, errors.New("device_plugin_binding_changed")
	}
	if err = b.manager.mobileDeviceCurrent(task, settings); err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("device_plugin_closed")
	}
	sequence := b.sequence
	if capability != "device.act" && capability != "device.record" {
		b.sequence++
	} else {
		// The caller-supplied task request_key is the durable idempotency
		// identity for an action. A later observation or artifact read must not
		// turn the same action into a new native submission.
		sequence = 0
	}
	b.mu.Unlock()
	version := b.sourceVersion(task, settings, binding, capability, raw, sequence)
	thread, sender := task.Source.Thread, task.Source.Sender
	if thread == "" {
		thread = "host:mobile"
	}
	if sender == "" {
		sender = "administrator"
	}
	purpose := task.Goal
	if task.ID == "" {
		purpose = "list registered mobile devices"
	}
	invocation := pluginhost.Invocation{Account: b.manager.native.Account(), Thread: thread, Requester: sender, Purpose: purpose, RequestKey: "mobile-device-" + agentID(task.ID, capability, task.RequestHash, version), SourceVersions: map[string]int{task.ID: task.Steps + 1}, Authorization: version}
	if task.ID == "" {
		invocation.SourceVersions = map[string]int{}
	}
	args, _ := json.Marshal(map[string]any{"source_version": version, "request": request})
	policy := &mobileDevicePluginPolicy{bridge: b, task: task, settings: settings, boundBinding: binding, capability: capability, sourceVersion: version, invocation: invocation, arguments: args, nativeName: name, request: request}
	return policy, nil
}

func mobileDeviceCapability(name string, request map[string]any) (string, error) {
	switch name {
	case "devices":
		if len(request) != 0 {
			return "", errors.New("device_request_invalid")
		}
		return "device.list", nil
	case "observe":
		return "device.observe", nil
	case "act":
		action, _ := request["action"].(string)
		if action == "capture" {
			return "device.record", nil
		}
		return "device.act", nil
	case "read_artifact":
		return "device.read_artifact", nil
	default:
		return "", errors.New("device_request_invalid")
	}
}

func (b *mobileDevicePluginBridge) sourceVersion(task mobile.Task, settings mobile.Settings, binding, capability string, raw json.RawMessage, sequence uint64) string {
	payload, _ := json.Marshal([]any{task.ID, task.Source, task.RequestHash, task.Goal, task.DeviceID, task.FishID, task.DeviceBindingID, task.Steps, task.ObservationID, settings.Revision, b.installed.Digest, binding, capability, raw, sequence})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func cloneMobileTask(value mobile.Task) mobile.Task {
	clone := value
	clone.Artifacts = append([]mobile.Artifact(nil), value.Artifacts...)
	clone.Events = append([]mobile.Event(nil), value.Events...)
	return clone
}

func cloneMobileSettings(value mobile.Settings) mobile.Settings {
	clone := value
	clone.Devices = append([]string(nil), value.Devices...)
	clone.Actions = append([]string(nil), value.Actions...)
	clone.Bridge.Args = append([]string(nil), value.Bridge.Args...)
	clone.Bridge.Tools = append([]string(nil), value.Bridge.Tools...)
	clone.Bridge.Env = make(map[string]string, len(value.Bridge.Env))
	for key, item := range value.Bridge.Env {
		clone.Bridge.Env[key] = item
	}
	clone.Bridge.Headers = make(map[string]string, len(value.Bridge.Headers))
	for key, item := range value.Bridge.Headers {
		clone.Bridge.Headers[key] = item
	}
	return clone
}

func (b *mobileDevicePluginBridge) validateNativeRequest(task mobile.Task, settings mobile.Settings, capability string, request map[string]any) error {
	if capability == "device.list" {
		return nil
	}
	if task.ID == "" {
		return errors.New("device_task_required")
	}
	device, _ := request["device_id"].(string)
	if device == "" || device != task.DeviceID {
		return errors.New("device_request_invalid")
	}
	if capability == "device.observe" {
		return nil
	}
	if capability == "device.read_artifact" {
		id, _ := request["artifact_id"].(string)
		key, _ := request["request_key"].(string)
		b.mu.Lock()
		artifact, ok := b.artifacts[id]
		b.mu.Unlock()
		if !ok || artifact.DeviceID != device || artifact.RequestKey != key || !strings.HasPrefix(key, task.ID+"-step-") {
			return errors.New("device_artifact_not_authorized")
		}
		return nil
	}
	observationID, _ := request["observation_id"].(string)
	key, _ := request["request_key"].(string)
	if observationID == "" || observationID != task.ObservationID || !strings.HasPrefix(key, task.ID+"-step-") {
		return errors.New("device_request_invalid")
	}
	b.mu.Lock()
	observation, ok := b.observations[observationID]
	b.mu.Unlock()
	if !ok {
		return errors.New("device_observation_not_authorized")
	}
	step, ok := deviceRequestStep(request)
	if !ok || !mobile.ValidStep(step, observation) || (step.Action != "capture" && !contains(settings.Actions, step.Action)) {
		return errors.New("device_action_not_permitted")
	}
	return nil
}

func deviceRequestStep(request map[string]any) (mobile.Step, bool) {
	action, ok := request["action"].(string)
	if !ok {
		return mobile.Step{}, false
	}
	args := map[string]any{}
	for key, value := range request {
		switch key {
		case "device_id", "observation_id", "request_key", "action":
		default:
			args[key] = value
		}
	}
	return mobile.Step{Action: action, Arguments: args}, true
}

func (b *mobileDevicePluginBridge) mobilePluginCurrent(ctx context.Context, task mobile.Task, settings mobile.Settings, binding string, invocation pluginhost.Invocation, capability string) error {
	if err := b.manager.mobileDeviceCurrent(task, settings); err != nil {
		return err
	}
	installed, err := b.store.Installed(ctx, mobileDevicePluginID)
	if err != nil || !installed.Enabled || installed.Removed || installed.Digest != b.installed.Digest || installed.Revision != b.installed.Revision {
		return errors.New("device_plugin_version_changed")
	}
	code, err := b.store.PackageDirectory(installed.Digest)
	if err != nil {
		return errors.New("device_plugin_version_changed")
	}
	verified, err := pluginhost.Verify(code)
	if err != nil || !reflect.DeepEqual(verified, installed.Package) {
		return errors.New("device_plugin_version_changed")
	}
	manifest, err := os.ReadFile(filepath.Join(code, "plugin.json"))
	if err != nil || namedSpeechSHA(manifest) != installed.Digest {
		return errors.New("device_plugin_version_changed")
	}
	if b.store.CheckConfiguration(ctx, mobileDevicePluginID) != nil {
		return errors.New("device_plugin_configuration_changed")
	}
	if pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: b.manager.native.Account(), Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}, invocation, []string{"device"}) != nil {
		return errors.New("device_plugin_permissions_changed")
	}
	if err := b.bindingCurrent(binding); err != nil {
		return err
	}
	return nil
}

func (b *mobileDevicePluginBridge) bindingCurrent(want string) error {
	b.mu.Lock()
	broker := b.broker
	b.mu.Unlock()
	if broker == nil {
		return errors.New("device_plugin_binding_changed")
	}
	got, err := broker.Binding()
	if err != nil || got == "" || got != want {
		return errors.New("device_plugin_binding_changed")
	}
	return nil
}

func (b *mobileDevicePluginBridge) ensureBroker(ctx context.Context, task mobile.Task, settings mobile.Settings) (mobile.IOBroker, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.manager == nil || b.manager.mobileRetired {
		return nil, errDeviceServiceRetired
	}
	if b.closed {
		return nil, errors.New("device_plugin_closed")
	}
	if b.broker != nil {
		return b.broker, nil
	}
	if err := b.manager.mobileDeviceCurrent(task, settings); err != nil {
		return nil, err
	}
	options, err := b.manager.mobileDeviceIOOptions(b.store, b.installed, b.options, settings)
	if err != nil {
		return nil, err
	}
	broker, err := b.brokerOpener(ctx, options)
	if err != nil {
		return nil, errors.New("device_plugin_broker_unavailable")
	}
	b.broker = broker
	return broker, nil
}

// Paths come exclusively from deployment and the selected account. Inspection
// of these options never opens ADB or admits a device lease.
func (m *Manager) mobileDeviceIOOptions(store *pluginhost.Store, installed pluginhost.InstalledPackage, deployment pluginruntime.Options, settings mobile.Settings) (mobile.IOOptions, error) {
	code, err := store.PackageDirectory(installed.Digest)
	if err != nil {
		return mobile.IOOptions{}, errors.New("device_plugin_required")
	}
	workspace := filepath.Dir(deployment.CatalogDirectory)
	return mobile.IOOptions{
		Python: deployment.Python, PackageDirectory: code,
		CompatibilityEntry: filepath.Join(workspace, "scripts", "mobile_bridge.py"),
		WorkspaceDirectory: workspace, StateDirectory: filepath.Join(m.dir, "mobile"),
		GlobalLeaseDirectory: filepath.Join(workspace, ".device-broker-leases"),
		ADBRoots:             append([]string(nil), deployment.DeviceToolsRoots...), Account: m.native.Account(),
		Legacy: settings.Bridge, Devices: nonNilMobileSlice(settings.Devices), Actions: nonNilMobileSlice(settings.Actions),
	}, nil
}

type mobileDevicePluginPolicy struct {
	bridge        *mobileDevicePluginBridge
	task          mobile.Task
	settings      mobile.Settings
	boundBinding  string
	capability    string
	sourceVersion string
	invocation    pluginhost.Invocation
	arguments     json.RawMessage
	nativeName    string
	request       map[string]any

	mu        sync.Mutex
	used      bool
	failed    bool
	completed bool
	ioCall    mobile.IOCall
	ioFailure error
	receipt   json.RawMessage
	status    string
}

func (p *mobileDevicePluginPolicy) Check(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) (err error) {
	if p == nil || p.bridge == nil || cap.Async || cap.ID != p.capability || job.Plugin != mobileDevicePluginID || job.Tool != p.capability || !bytes.Equal(job.Arguments, p.arguments) {
		return errors.New("device_plugin_authorization_changed")
	}
	if p.bridge.manager == nil || p.bridge.manager.mobileRetired {
		return errDeviceServiceRetired
	}
	if want, got := mustJSON(p.invocation), mustJSON(job.Context); !bytes.Equal(want, got) || job.Context.Authorization != p.sourceVersion {
		return errors.New("device_plugin_authorization_changed")
	}
	if err := p.bridge.mobilePluginCurrent(ctx, p.task, p.settings, p.boundBinding, p.invocation, p.capability); err != nil {
		return err
	}
	if p.task.ID != "" && job.Context.SourceVersions[p.task.ID] != p.task.Steps+1 {
		return errors.New("device_plugin_source_changed")
	}
	if job.ID != "" {
		live, err := p.bridge.store.Job(ctx, job.ID)
		if err != nil || live.CancelRequested || live.Revision != job.Revision {
			return errors.New("device_plugin_job_changed")
		}
	}
	return nil
}

// Each policy owns one native call and its physical-device lease. The package
// performs high-level sequencing; this host accepts only bounded IO steps for
// the exact original request and checks live authority around every step.
func (p *mobileDevicePluginPolicy) Service(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed || p.completed || method != "wechat/device_io" {
		return nil, errors.New("device_plugin_host_service_unavailable")
	}
	if err := p.Check(ctx, job, cap); err != nil {
		return nil, err
	}
	var in struct {
		Operation     string          `json:"operation"`
		SourceVersion string          `json:"source_version"`
		DeviceID      string          `json:"device_id,omitempty"`
		Step          string          `json:"step"`
		Parameters    json.RawMessage `json:"parameters"`
	}
	var parameters map[string]json.RawMessage
	if mobilePlanStrictJSON(raw, &in) != nil || in.Operation != p.capability || in.SourceVersion != p.sourceVersion || mobilePlanStrictJSON(in.Parameters, &parameters) != nil || parameters == nil {
		return nil, errors.New("device_plugin_request_changed")
	}
	switch in.Step {
	case "profile", "state", "foreground", "screenshot", "ui", "dispatch", "record", "artifact_read":
	default:
		return nil, errors.New("device_plugin_request_changed")
	}
	if in.DeviceID != "" && (p.capability != "device.list" && in.DeviceID != p.task.DeviceID || p.capability == "device.list" && !contains(p.settings.Devices, in.DeviceID)) {
		return nil, errors.New("device_plugin_request_changed")
	}
	ioContext := mobile.WithIOAuthority(ctx, func(current context.Context) error { return p.Check(current, job, cap) })
	if p.ioCall == nil {
		broker, err := p.bridge.ensureBroker(ctx, p.task, p.settings)
		if err != nil {
			return nil, err
		}
		if err = p.Check(ctx, job, cap); err != nil {
			return nil, err
		}
		p.used = true
		p.ioCall, err = broker.Begin(ioContext, p.nativeName, mustJSON(p.request))
		if err != nil {
			p.failed = true
			p.ioFailure = mobileDeviceIOFailure(err)
			return nil, p.ioFailure
		}
	}
	if err := p.Check(ctx, job, cap); err != nil {
		p.failed = true
		return nil, err
	}
	reply, err := p.ioCall.Service(ioContext, in.Step, in.DeviceID, in.Parameters)
	if err != nil || !json.Valid(reply) {
		p.failed = true
		p.ioFailure = mobileDeviceIOFailure(err)
		return nil, p.ioFailure
	}
	if err = p.Check(ctx, job, cap); err != nil {
		p.failed = true
		return nil, err
	}
	return append(json.RawMessage(nil), reply...), nil
}

// Only fixed host failure codes may retain a resource diagnosis. Raw runner
// errors can contain filenames, serials or credentials and are never exposed.
func mobileDeviceIOFailure(err error) error {
	failure := &RuntimeFailure{Code: "device_plugin_broker_unavailable", Stage: "device_io", Message: "设备IO请求未完成；请核对原任务状态、授权和本机资源", Continuation: "保留原任务；设备动作结果未确认时先检查状态，不重复提交动作"}
	if err == nil {
		return failure
	}
	switch err.Error() {
	case "device_artifact_page_limit":
		failure.Code = "device_artifact_page_limit"
		failure.Stage = "artifact_read"
		failure.Message = "素材单页读取达到宿主资源边界；请减小bytes，使用原artifact_id、request_key及已验证的offset分小页继续读取"
		failure.NotStarted, failure.Retryable = true, true
		failure.Continuation = "不重新录制；从未完成的offset缩小单页bytes继续，每页后按实际取得的字节数推进offset"
		return failure
	case "device_io_response_limit":
		failure.Code = "device_io_response_limit"
		failure.Stage = "response"
		failure.Message = "单次设备IO响应达到宿主资源边界；可分页素材请减小bytes并沿已验证的offset分小页继续；设备动作先核查原任务状态，不重复执行"
		failure.Continuation = "可分页素材保留原artifact_id和request_key，以更小bytes继续，按实际取得的字节数推进offset；其他操作核查实际资源限制及原任务状态"
		return failure
	case "device_broker_needs_migration":
		failure.Stage = "resource"
	case "device_broker_tool_not_permitted":
		failure.Stage = "authorization"
	}
	if errors.Is(err, os.ErrPermission) {
		failure.Stage = "filesystem"
	} else if errors.Is(err, os.ErrNotExist) {
		failure.Stage = "resource"
	}
	return failure
}

func (p *mobileDevicePluginPolicy) ioFailureValue() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ioFailure
}

func (p *mobileDevicePluginPolicy) closeIO() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ioCall == nil {
		return nil
	}
	if err := p.ioCall.Close(); err != nil {
		return err
	}
	p.ioCall = nil
	return nil
}

const mobileDeviceReplyLimit = 96 << 20

type mobileDeviceCheckpoint struct {
	JobID   string `json:"job_id"`
	Binding string `json:"binding"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}

func (p *mobileDevicePluginPolicy) replyRoot() string {
	return filepath.Join(p.bridge.manager.privateWorkspaceFor(p.invocation.Account), "device-plugin-replies")
}

func (p *mobileDevicePluginPolicy) checkpointBinding() string {
	return namedSpeechSHA(mustJSON([]any{p.invocation, p.arguments, p.boundBinding, p.bridge.installed.Digest}))
}

func (p *mobileDevicePluginPolicy) readCheckpoint(ctx context.Context, job pluginhost.Job, checkpoint mobileDeviceCheckpoint) ([]byte, error) {
	if checkpoint.JobID != job.ID || !validID(job.ID) || checkpoint.Binding != p.checkpointBinding() || checkpoint.Bytes < 0 || checkpoint.Bytes > mobileDeviceReplyLimit || len(checkpoint.SHA256) != 64 {
		return nil, errors.New("device_plugin_result_unavailable")
	}
	if err := p.Check(ctx, job, pluginhost.Capability{ID: p.capability}); err != nil {
		return nil, err
	}
	root, err := workspacefs.EnsurePath(p.replyRoot())
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, filepath.Join(job.ID, "reply.json"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, mobileDeviceReplyLimit+1))
	if err != nil || int64(len(raw)) != checkpoint.Bytes || namedSpeechSHA(raw) != checkpoint.SHA256 || !json.Valid(raw) {
		return nil, errors.New("device_plugin_private_reply_changed")
	}
	if err = p.Check(ctx, job, pluginhost.Capability{ID: p.capability}); err != nil {
		return nil, err
	}
	return raw, nil
}

func (p *mobileDevicePluginPolicy) Validate(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Check(ctx, job, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var out struct {
		Result        json.RawMessage `json:"result"`
		SourceVersion string          `json:"source_version"`
		Operation     string          `json:"operation"`
	}
	if !p.used || p.failed || p.completed || p.ioCall == nil || mobilePlanStrictJSON(raw, &out) != nil || out.SourceVersion != p.sourceVersion || out.Operation != p.capability || !json.Valid(out.Result) {
		return pluginruntime.VerifiedResult{}, errors.New("device_plugin_result_unverified")
	}
	p.completed = true // Even a rejected completion cannot replay the native call.
	ioContext := mobile.WithIOAuthority(ctx, func(current context.Context) error { return p.Check(current, job, cap) })
	observed, err := p.ioCall.Complete(ioContext, out.Result)
	if err != nil || !json.Valid(observed) || len(observed) > mobileDeviceReplyLimit {
		return pluginruntime.VerifiedResult{}, errors.New("device_plugin_result_unverified")
	}
	if err = p.Check(ctx, job, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	receipt, err := p.bridge.safeReceipt(p.task, p.capability, p.nativeName, observed, p.request)
	if err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	p.receipt, p.status = receipt, mobileReceiptStatus(p.capability, receipt)
	if !validID(job.ID) {
		return pluginruntime.VerifiedResult{}, errors.New("device_plugin_result_unavailable")
	}
	root, err := workspacefs.EnsurePath(p.replyRoot())
	if err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	defer root.Close()
	if err = root.Mkdir(job.ID, 0700); err != nil {
		return pluginruntime.VerifiedResult{}, errors.New("device_plugin_reply_already_exists")
	}
	dir, err := workspacefs.EnsureDirectory(root, job.ID)
	if err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	defer dir.Close()
	file, err := dir.OpenFile("reply.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	_, writeErr := file.Write(observed)
	closeErr := file.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	checkpoint := mobileDeviceCheckpoint{JobID: job.ID, Binding: p.checkpointBinding(), SHA256: namedSpeechSHA(observed), Bytes: int64(len(observed))}
	if _, err = p.readCheckpoint(ctx, job, checkpoint); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	private, _ := json.Marshal(checkpoint)
	status := p.status
	if status == "" {
		status = "completed"
	}
	public, _ := json.Marshal(map[string]any{"operation": p.capability, "status": status, "preview": json.RawMessage(receipt)})
	return pluginruntime.VerifiedResult{Result: public, Private: private}, nil
}

func mobileReceiptStatus(capability string, receipt json.RawMessage) string {
	if capability != "device.act" && capability != "device.record" {
		return "completed"
	}
	var value struct {
		State string `json:"state"`
	}
	if json.Unmarshal(receipt, &value) != nil || (value.State != "submitted" && value.State != "unknown") {
		return ""
	}
	return value.State
}

func sameJSON(left, right json.RawMessage) bool {
	a, err := decodeShareJSON(left)
	if err != nil {
		return false
	}
	b, err := decodeShareJSON(right)
	return err == nil && reflect.DeepEqual(a, b)
}

func (b *mobileDevicePluginBridge) safeReceipt(task mobile.Task, capability, name string, raw json.RawMessage, request map[string]any) (json.RawMessage, error) {
	switch capability {
	case "device.list":
		var value struct {
			Devices []struct {
				ID       string   `json:"id"`
				Name     string   `json:"name"`
				State    string   `json:"state"`
				Packages []string `json:"packages"`
				Actions  []string `json:"actions"`
				Browsers []string `json:"browsers"`
				WebHosts []string `json:"web_hosts"`
			} `json:"devices"`
		}
		if mobileDeviceWireJSON(raw, &value) != nil {
			return nil, errors.New("device_plugin_receipt_invalid")
		}
		return json.Marshal(value)
	case "device.observe":
		var value struct {
			Text  string `json:"text"`
			Media []struct {
				MIME string `json:"mime_type"`
				Data string `json:"data_base64"`
			} `json:"media"`
		}
		var observation struct {
			ID              string            `json:"id"`
			DeviceID        string            `json:"device_id"`
			Package         string            `json:"package"`
			Width           int               `json:"width"`
			Height          int               `json:"height"`
			AllowedPackages []string          `json:"allowed_packages"`
			Browsers        []string          `json:"browsers"`
			WebHosts        []string          `json:"web_hosts"`
			Nodes           []json.RawMessage `json:"nodes"`
		}
		if mobileDeviceWireJSON(raw, &value) != nil || mobileDeviceWireJSON([]byte(value.Text), &observation) != nil || len(value.Media) != 1 || value.Media[0].MIME != "image/png" || observation.ID == "" || observation.DeviceID != task.DeviceID || observation.Width < 1 || observation.Height < 1 {
			return nil, errors.New("device_plugin_receipt_invalid")
		}
		verified := mobile.Observation{ID: observation.ID, DeviceID: observation.DeviceID, Package: observation.Package, Width: observation.Width, Height: observation.Height, AllowedPackages: nonNilMobileSlice(observation.AllowedPackages), Browsers: nonNilMobileSlice(observation.Browsers), WebHosts: nonNilMobileSlice(observation.WebHosts), Nodes: nonNilMobileSlice(observation.Nodes)}
		b.mu.Lock()
		b.observations[verified.ID] = verified
		b.mu.Unlock()
		return json.Marshal(map[string]any{"observation_id": verified.ID, "device_id": verified.DeviceID, "package": verified.Package, "width": verified.Width, "height": verified.Height, "ui_nodes": verified.Nodes})
	case "device.act", "device.record":
		var value struct {
			State  string `json:"state"`
			Result struct {
				Action    string `json:"action"`
				DeviceID  string `json:"device_id"`
				Reobserve bool   `json:"reobserve_required"`
				Artifact  *struct {
					ID               string  `json:"id"`
					DeviceID         string  `json:"device_id"`
					RequestKey       string  `json:"request_key"`
					Kind             string  `json:"kind"`
					MIME             string  `json:"mime"`
					Bytes            int     `json:"bytes"`
					SHA256           string  `json:"sha256"`
					RequestedSeconds int     `json:"requested_seconds"`
					Audio            bool    `json:"audio"`
					Original         bool    `json:"original_media"`
					CreatedAt        float64 `json:"created_at"`
				} `json:"artifact"`
			} `json:"result"`
		}
		if mobileDeviceWireJSON(raw, &value) != nil || (value.State != "submitted" && value.State != "unknown") || value.State == "submitted" && value.Result.DeviceID != task.DeviceID || value.State == "unknown" && value.Result.DeviceID != "" && value.Result.DeviceID != task.DeviceID {
			return nil, errors.New("device_plugin_receipt_invalid")
		}
		action, _ := request["action"].(string)
		if value.State == "submitted" && (value.Result.Action != action || (capability == "device.record" && action != "capture")) {
			return nil, errors.New("device_plugin_receipt_invalid")
		}
		var artifact *mobile.Artifact
		if value.State == "unknown" && value.Result.Artifact != nil {
			return nil, errors.New("device_plugin_receipt_invalid")
		}
		if value.Result.Artifact != nil {
			item := value.Result.Artifact
			converted := mobile.Artifact{ID: item.ID, DeviceID: item.DeviceID, RequestKey: item.RequestKey, Kind: item.Kind, MIME: item.MIME, Bytes: item.Bytes, SHA256: item.SHA256, RequestedSeconds: item.RequestedSeconds, Audio: item.Audio, Original: item.Original}
			key, _ := request["request_key"].(string)
			if converted.ID == "" || converted.DeviceID != task.DeviceID || converted.RequestKey != key {
				return nil, errors.New("device_plugin_receipt_invalid")
			}
			b.mu.Lock()
			b.artifacts[converted.ID] = converted
			b.mu.Unlock()
			artifact = &converted
		}
		return json.Marshal(map[string]any{"state": value.State, "action": value.Result.Action, "device_id": value.Result.DeviceID, "reobserve_required": value.Result.Reobserve, "artifact": artifact})
	case "device.read_artifact":
		var value struct {
			ID     string `json:"id"`
			Offset int    `json:"offset"`
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
			EOF    bool   `json:"eof"`
			Data   string `json:"data_base64"`
		}
		if mobileDeviceWireJSON(raw, &value) != nil || value.ID == "" {
			return nil, errors.New("device_plugin_receipt_invalid")
		}
		return json.Marshal(map[string]any{"artifact_id": value.ID, "offset": value.Offset, "bytes": value.Bytes, "sha256": value.SHA256, "eof": value.EOF})
	}
	return nil, errors.New("device_plugin_receipt_invalid")
}

// Native bridge payloads include host-private timestamps, screenshot hashes,
// and base64 data. Verify unique JSON keys, then project only receipt fields.
func mobileDeviceWireJSON(raw []byte, out any) error {
	if _, err := decodeShareJSON(raw); err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

var _ mobile.Bridge = (*mobileDevicePluginBridge)(nil)
var _ pluginruntime.CallPolicy = (*mobileDevicePluginPolicy)(nil)
var _ pluginruntime.ServicePolicy = (*mobileDevicePluginPolicy)(nil)
