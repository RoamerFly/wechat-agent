package admin

// This file is the host-side authority boundary for the ready mobile-control
// planner.  The plugin gets task metadata but never the PNG, a device bridge,
// credentials, or direct model access.  It can ask the host to describe the
// already-bound screenshot and to make one bounded planning call.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"

	"wechat-bot/internal/deepseek"
	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

const (
	mobilePlannerPluginID   = "mobile-control"
	mobilePlannerCapability = "device.plan"
)

type mobilePlanModel interface {
	Vision(context.Context, string, []deepseek.Image) (string, error)
	ChatFunctions(context.Context, string, []map[string]string, []mcptools.Tool) (string, error)
}

type mobilePlanPluginPolicy struct {
	manager *Manager
	store   *pluginhost.Store
	task    mobile.Task
	obs     mobile.Observation

	allowedActions    []string
	settingsRevision  int64
	configFingerprint string
	sourceVersion     string
	configRevision    int
	invocation        pluginhost.Invocation
	arguments         json.RawMessage
	model             mobilePlanModel

	mu               sync.Mutex
	visionRequested  bool
	visionSucceeded  bool
	terminalFailure  bool
	planAttempts     int
	repairAllowed    bool
	planComplete     bool
	visionText       string
	lastProviderText string
	lastCanonical    mobile.Step
	lastCanonicalOK  bool
	// Tests use this to isolate protocol validation from persistent plugin
	// setup. Production always uses Check's live authority revalidation.
	checkOverride func(context.Context, pluginhost.Job, pluginhost.Capability) error
}

// mobilePlannerReady is deliberately non-starting: discovery and task queueing
// cannot install, enable, launch, or authorize a plugin as a side effect.
func (m *Manager) mobilePlannerReady(ctx context.Context) error {
	// The retired Windows path keeps task history only.  A persisted package
	// must never cause a planner model call or runtime launch after retirement.
	if m == nil || m.mobileRetired {
		return errDeviceServiceRetired
	}
	store, err := m.packageStore()
	if err != nil {
		return errors.New("device_planner_plugin_required")
	}
	installed, err := store.Installed(ctx, mobilePlannerPluginID)
	if err != nil || !installed.Enabled || installed.Removed {
		return errors.New("device_planner_plugin_required")
	}
	caps, err := store.Capabilities(ctx, mobilePlannerPluginID)
	if err != nil {
		return errors.New("device_planner_plugin_required")
	}
	ready := false
	for _, cap := range caps {
		if cap.ID == mobilePlannerCapability && cap.Implementation == "ready" && cap.Discoverable && reflect.DeepEqual(cap.Permissions, []string{"model.invoke"}) {
			ready = true
		}
	}
	if !ready || store.CheckConfiguration(ctx, mobilePlannerPluginID) != nil {
		return errors.New("device_planner_plugin_required")
	}
	controller, runtimeReady, err := m.installedPluginRuntime()
	if err != nil || !runtimeReady {
		return errors.New("device_planner_plugin_required")
	}
	if state := controller.Status(mobilePlannerPluginID).State; state != "stopped" && state != "running" {
		return errors.New("device_planner_plugin_required")
	}
	return nil
}

// planMobileWithPlugin is the replacement planner entry point.  It neither
// falls back to legacy planning nor treats a missing plugin as permission to
// contact a model.  Its caller owns the outer diagnostics observer.
func (m *Manager) planMobileWithPlugin(ctx context.Context, task mobile.Task, observation mobile.Observation) (mobile.Step, error) {
	if err := m.mobilePlannerReady(ctx); err != nil {
		return mobile.Step{}, err
	}
	policy, err := m.newMobilePlanPluginPolicy(ctx, task, observation)
	if err != nil {
		return mobile.Step{}, err
	}
	controller, ready, err := m.installedPluginRuntime()
	if err != nil || !ready {
		return mobile.Step{}, errors.New("device_planner_plugin_required")
	}
	installed, err := policy.store.Installed(ctx, mobilePlannerPluginID)
	if err != nil || !installed.Enabled || installed.Removed {
		return mobile.Step{}, errors.New("device_planner_plugin_required")
	}
	if err := controller.EnsureStarted(ctx, mobilePlannerPluginID, installed.Revision); err != nil {
		return mobile.Step{}, err
	}
	call, err := controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: mobilePlannerPluginID, Capability: mobilePlannerCapability, Context: policy.invocation, Arguments: policy.arguments}, policy)
	if err != nil {
		return mobile.Step{}, errors.New("device_planner_plugin_needs_review")
	}
	if call.Job.State != "completed" {
		return mobile.Step{}, errors.New("device_planner_plugin_needs_review")
	}
	raw, err := policy.store.PrivateJobResult(ctx, call.Job, func(ctx context.Context, job pluginhost.Job) error {
		return policy.Check(ctx, job, pluginhost.Capability{ID: mobilePlannerCapability})
	})
	if err != nil {
		return mobile.Step{}, errors.New("device_planner_plugin_result_unavailable")
	}
	var result struct {
		Step mobile.Step `json:"step"`
	}
	if mobilePlanStrictJSON(raw, &result) != nil || !mobilePlanActionAllowed(result.Step, policy.allowedActions) || !mobile.ValidStep(result.Step, policy.obs) {
		return mobile.Step{}, errors.New("device_planner_plugin_result_invalid")
	}
	return result.Step, nil
}

func (m *Manager) newMobilePlanPluginPolicy(ctx context.Context, task mobile.Task, observation mobile.Observation) (*mobilePlanPluginPolicy, error) {
	return m.newMobilePlanPluginPolicyWithModel(ctx, task, observation, nil)
}

// newMobilePlanPluginPolicyWithModel exists so the isolated stdio contract can
// inject a local fake model.  Production always passes nil and receives the
// normal configured/billed model after source authorization succeeds.
func (m *Manager) newMobilePlanPluginPolicyWithModel(ctx context.Context, task mobile.Task, observation mobile.Observation, injected mobilePlanModel) (*mobilePlanPluginPolicy, error) {
	store, err := m.packageStore()
	if err != nil {
		return nil, err
	}
	current, settings, err := m.currentMobilePlanningSource(task, observation)
	if err != nil {
		return nil, err
	}
	task, observation = current, normalizeMobileObservation(observation)
	settings.Actions = nonNilMobileSlice(settings.Actions)
	if task.ID == "" || task.RequestHash == "" || observation.ID == "" || len(observation.PNG) == 0 {
		return nil, errors.New("device_planner_source_invalid")
	}
	m.mu.Lock()
	config := cloneConfig(m.config)
	m.mu.Unlock()
	pluginConfig, err := store.Configuration(ctx, mobilePlannerPluginID)
	if err != nil {
		return nil, err
	}
	model := injected
	if model == nil {
		model, err = m.makeModel(config)
		if err != nil {
			return nil, err
		}
	}
	modelFingerprint := mobileModelFingerprint(config)
	sourceVersion := mobilePlanSourceVersion(task, observation, settings.Revision, modelFingerprint)
	thread, requester := task.Source.Thread, task.Source.Sender
	if thread == "" {
		thread = "host:mobile"
	}
	if requester == "" {
		requester = "administrator"
	}
	steps := nonNilMobileSlice(task.Events)
	if len(steps) > 4 {
		steps = steps[len(steps)-4:]
	}
	recordings := nonNilMobileSlice(task.Artifacts)
	args, err := json.Marshal(map[string]any{
		"task_id":        task.ID,
		"observation_id": observation.ID,
		"source_version": sourceVersion,
		"task": map[string]any{
			"goal":                  task.Goal,
			"device_id":             task.DeviceID,
			"recordings":            recordings,
			"recent_executed_steps": steps,
		},
		"observation": map[string]any{
			"current_app":      observation.Package,
			"allowed_packages": nonNilMobileSlice(observation.AllowedPackages),
			"browsers":         nonNilMobileSlice(observation.Browsers),
			"web_hosts":        nonNilMobileSlice(observation.WebHosts),
			"width":            observation.Width,
			"height":           observation.Height,
			"ui_nodes":         nonNilMobileSlice(observation.Nodes),
		},
		"allowed_actions": nonNilMobileSlice(settings.Actions),
	})
	if err != nil {
		return nil, errors.New("device_planner_source_invalid")
	}
	invocation := pluginhost.Invocation{
		Account:        m.native.Account(),
		Thread:         thread,
		Requester:      requester,
		Purpose:        task.Goal,
		RequestKey:     "mobile-plan-" + agentID(task.ID, observation.ID, sourceVersion),
		SourceVersions: map[string]int{task.ID: task.Steps + 1},
		Authorization:  sourceVersion,
	}
	return &mobilePlanPluginPolicy{manager: m, store: store, task: task, obs: copyMobileObservation(observation), allowedActions: nonNilMobileSlice(settings.Actions), settingsRevision: settings.Revision, configFingerprint: modelFingerprint, sourceVersion: sourceVersion, configRevision: pluginConfig.Revision, invocation: invocation, arguments: args, model: model}, nil
}

// currentMobilePlanningSource gates model construction.  A revoked/changed
// task never writes a provider key or reaches the configured model factory.
func (m *Manager) currentMobilePlanningSource(task mobile.Task, observation mobile.Observation) (mobile.Task, mobile.Settings, error) {
	if m == nil || m.mobileRetired {
		return mobile.Task{}, mobile.Settings{}, errDeviceServiceRetired
	}
	if m.mobile == nil || task.ID == "" || observation.ID == "" || len(observation.PNG) == 0 {
		return mobile.Task{}, mobile.Settings{}, errors.New("device_planner_source_invalid")
	}
	current, ok := m.mobile.Get(task.ID)
	if !ok || current.State != "running" || current.ID != task.ID || current.RequestHash != task.RequestHash || current.DeviceID != task.DeviceID || current.Goal != task.Goal || !reflect.DeepEqual(current.Source, task.Source) || current.ObservationID != observation.ID || !m.mobileSourceAllowed(current.Source) {
		return mobile.Task{}, mobile.Settings{}, errors.New("device_planner_authorization_changed")
	}
	return current, m.mobile.Settings(), nil
}

func nonNilMobileSlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	// A nil destination would turn an existing empty list back into nil. Keep
	// the JSON distinction: plugin protocol uses [] for an empty list, never
	// null for optional collections.
	return append([]T{}, values...)
}

func normalizeMobileObservation(in mobile.Observation) mobile.Observation {
	out := in
	out.AllowedPackages = nonNilMobileSlice(in.AllowedPackages)
	out.Browsers = nonNilMobileSlice(in.Browsers)
	out.WebHosts = nonNilMobileSlice(in.WebHosts)
	out.Nodes = nonNilMobileSlice(in.Nodes)
	return out
}

func copyMobileObservation(in mobile.Observation) mobile.Observation {
	out := normalizeMobileObservation(in)
	out.PNG = append([]byte(nil), in.PNG...)
	return out
}

func mobileModelFingerprint(config Config) string {
	// A hash binds key rotation without putting any credential in an invocation,
	// plugin argument, job result, or log entry.
	b, _ := json.Marshal([]any{config.Provider, config.BaseURL, config.Model, config.ThinkingEnabled, config.ReasoningEffort, config.MaxTokens, config.APIKey})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mobilePlanSourceVersion(task mobile.Task, obs mobile.Observation, settingsRevision int64, modelFingerprint string) string {
	obs = normalizeMobileObservation(obs)
	png := sha256.Sum256(obs.PNG)
	b, _ := json.Marshal([]any{png[:], task.ID, task.Source, task.RequestHash, task.DeviceID, task.Goal, task.State, task.Steps, nonNilMobileSlice(task.Artifacts), nonNilMobileSlice(task.Events), obs.ID, obs.DeviceID, obs.Package, obs.Width, obs.Height, obs.AllowedPackages, obs.Browsers, obs.WebHosts, obs.Nodes, settingsRevision, modelFingerprint})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p *mobilePlanPluginPolicy) Check(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) error {
	// Policies may outlive the request that created them.  Do not let an
	// already-created policy reach the model after the host switched to the
	// Windows history-only service.
	if p != nil && p.manager != nil && p.manager.mobileRetired {
		return errDeviceServiceRetired
	}
	if p.checkOverride != nil {
		return p.checkOverride(ctx, job, cap)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.manager == nil || p.store == nil || job.Plugin != mobilePlannerPluginID || job.Tool != mobilePlannerCapability || cap.Async || cap.ID != mobilePlannerCapability {
		return errors.New("device_planner_authorization_changed")
	}
	wantContext, _ := json.Marshal(p.invocation)
	gotContext, _ := json.Marshal(job.Context)
	if !bytes.Equal(wantContext, gotContext) || !sameJSON(job.Arguments, p.arguments) {
		return errors.New("device_planner_authorization_changed")
	}
	current, ok := p.manager.mobile.Get(p.task.ID)
	if !ok || current.State != "running" || current.ID != p.task.ID || current.RequestHash != p.task.RequestHash || !reflect.DeepEqual(current.Source, p.task.Source) || current.ObservationID != p.obs.ID || !p.manager.mobileSourceAllowed(current.Source) {
		return errors.New("device_planner_authorization_changed")
	}
	settings := p.manager.mobile.Settings()
	if settings.Revision != p.settingsRevision || !reflect.DeepEqual(nonNilMobileSlice(settings.Actions), p.allowedActions) {
		return errors.New("device_planner_settings_changed")
	}
	p.manager.mu.Lock()
	config := cloneConfig(p.manager.config)
	p.manager.mu.Unlock()
	if mobileModelFingerprint(config) != p.configFingerprint {
		return errors.New("device_planner_model_configuration_changed")
	}
	if mobilePlanSourceVersion(current, p.obs, settings.Revision, p.configFingerprint) != p.sourceVersion || job.Context.SourceVersions[p.task.ID] != p.task.Steps+1 || job.Context.Authorization != p.sourceVersion {
		return errors.New("device_planner_source_changed")
	}
	pluginConfig, err := p.store.Configuration(ctx, mobilePlannerPluginID)
	if err != nil || pluginConfig.Revision != p.configRevision {
		return errors.New("device_planner_plugin_configuration_changed")
	}
	installed, err := p.store.Installed(ctx, mobilePlannerPluginID)
	if err != nil || !installed.Enabled || installed.Removed || (job.PackageDigest != "" && (job.PackageDigest != installed.Digest || job.Version != installed.Package.Version || job.ConfigRevision != jsonNumber(p.configRevision))) {
		return errors.New("device_planner_plugin_version_changed")
	}
	if pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: p.manager.native.Account(), Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}, job.Context, []string{"model.invoke"}) != nil {
		return errors.New("device_planner_plugin_permissions_changed")
	}
	if job.ID != "" {
		live, err := p.store.Job(ctx, job.ID)
		if err != nil || live.CancelRequested || live.Revision != job.Revision {
			return errors.New("device_planner_job_changed")
		}
	}
	return nil
}

func jsonNumber(value int) string { return strings.TrimSpace(string(mustJSON(value))) }
func mustJSON(value any) []byte   { b, _ := json.Marshal(value); return b }

func (p *mobilePlanPluginPolicy) Service(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if method != "wechat/model_request" || p.model == nil || p.terminalFailure {
		return nil, errors.New("device_planner_host_service_unavailable")
	}
	if err := p.Check(ctx, job, cap); err != nil {
		p.terminalFailure = true
		return nil, errors.New("device_planner_host_service_unavailable")
	}
	var envelope struct {
		Operation     string `json:"operation"`
		ObservationID string `json:"observation_id"`
		SourceVersion string `json:"source_version"`
		Prompt        string `json:"prompt"`
		System        string `json:"system"`
		Turns         []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"turns"`
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Schema      string `json:"schema"`
		} `json:"tools"`
	}
	if mobilePlanStrictJSON(raw, &envelope) != nil || envelope.ObservationID != p.obs.ID || envelope.SourceVersion != p.sourceVersion {
		return nil, errors.New("device_planner_request_changed")
	}
	switch envelope.Operation {
	case "describe_screen":
		if p.visionRequested || !boundedPlannerText(envelope.Prompt, 8192) || envelope.System != "" || len(envelope.Turns) != 0 || len(envelope.Tools) != 0 {
			return nil, errors.New("device_planner_request_changed")
		}
		p.visionRequested = true // Unknown outcome is never resubmitted.
		text, err := p.model.Vision(ctx, envelope.Prompt, []deepseek.Image{{Data: p.obs.PNG, MIME: "image/png"}})
		if err != nil {
			p.terminalFailure = true
			return nil, err
		}
		if err := p.Check(ctx, job, cap); err != nil {
			p.terminalFailure = true
			return nil, err
		}
		p.visionText = text
		p.visionSucceeded = true
		return json.Marshal(map[string]string{"text": text})
	case "plan_step":
		if !p.visionSucceeded || p.planComplete || envelope.Prompt != "" || !boundedPlannerText(envelope.System, 16384) || !p.validPlanRequest(envelope) {
			return nil, errors.New("device_planner_request_changed")
		}
		p.planAttempts++ // Reserve before model call: unknown outcomes never retry.
		turns := make([]map[string]string, len(envelope.Turns))
		for i, turn := range envelope.Turns {
			turns[i] = map[string]string{"role": turn.Role, "content": turn.Content}
		}
		answer, err := p.model.ChatFunctions(ctx, envelope.System, turns, []mcptools.Tool{agentTool(envelope.Tools[0].Name, envelope.Tools[0].Description, envelope.Tools[0].Schema)})
		if err != nil {
			p.terminalFailure = true
			return nil, err
		}
		if err := p.Check(ctx, job, cap); err != nil {
			p.terminalFailure = true
			return nil, err
		}
		p.lastProviderText = answer
		p.lastCanonical, p.lastCanonicalOK = canonicalMobilePlanStep(answer)
		if p.lastCanonicalOK && mobile.ValidStep(p.lastCanonical, p.obs) && mobilePlanActionAllowed(p.lastCanonical, p.allowedActions) {
			p.planComplete = true
			p.repairAllowed = false
		} else if p.planAttempts == 1 {
			p.repairAllowed = true
		} else {
			p.terminalFailure = true
		}
		return json.Marshal(map[string]string{"text": answer})
	default:
		return nil, errors.New("device_planner_request_changed")
	}
}

func (p *mobilePlanPluginPolicy) validPlanRequest(in struct {
	Operation     string `json:"operation"`
	ObservationID string `json:"observation_id"`
	SourceVersion string `json:"source_version"`
	Prompt        string `json:"prompt"`
	System        string `json:"system"`
	Turns         []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"turns"`
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Schema      string `json:"schema"`
	} `json:"tools"`
}) bool {
	if len(in.Tools) != 1 || in.Tools[0].Name != "device_step" || !boundedPlannerText(in.Tools[0].Description, 8192) || !plannerSchemaObject(in.Tools[0].Schema) {
		return false
	}
	input, err := p.planInput()
	if err != nil || len(in.Turns) != 1 && len(in.Turns) != 2 || in.Turns[0].Role != "user" || !sameJSONObject(in.Turns[0].Content, input) {
		return false
	}
	if p.planAttempts == 0 {
		return len(in.Turns) == 1
	}
	return len(in.Turns) == 2 && p.planAttempts == 1 && p.repairAllowed && in.Turns[1].Role == "user" && boundedPlannerText(in.Turns[1].Content, 8192)
}

func boundedPlannerText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit && !strings.ContainsRune(value, 0)
}

func plannerSchemaObject(raw string) bool {
	if len(raw) == 0 || len(raw) > 16384 {
		return false
	}
	// The package owns the complete schema. Decode it as a generic unique-key
	// object rather than mirroring its properties in core, then require only
	// the capability-independent object root needed by ChatFunctions.
	var schema map[string]json.RawMessage
	if mobilePlanStrictJSON([]byte(raw), &schema) != nil {
		return false
	}
	var kind string
	return json.Unmarshal(schema["type"], &kind) == nil && kind == "object"
}

func (p *mobilePlanPluginPolicy) planInput() (string, error) {
	var source struct {
		TaskID        string `json:"task_id"`
		ObservationID string `json:"observation_id"`
		SourceVersion string `json:"source_version"`
		Task          struct {
			Goal       string            `json:"goal"`
			DeviceID   string            `json:"device_id"`
			Recordings []mobile.Artifact `json:"recordings"`
			Recent     []mobile.Event    `json:"recent_executed_steps"`
		} `json:"task"`
		Observation struct {
			CurrentApp string            `json:"current_app"`
			Packages   []string          `json:"allowed_packages"`
			Browsers   []string          `json:"browsers"`
			Hosts      []string          `json:"web_hosts"`
			Width      int               `json:"width"`
			Height     int               `json:"height"`
			Nodes      []json.RawMessage `json:"ui_nodes"`
		} `json:"observation"`
		Actions []string `json:"allowed_actions"`
	}
	if mobilePlanStrictJSON(p.arguments, &source) != nil {
		return "", errors.New("device_planner_source_invalid")
	}
	b, err := json.Marshal(map[string]any{
		"goal":                  source.Task.Goal,
		"device_id":             source.Task.DeviceID,
		"allowed_actions":       nonNilMobileSlice(source.Actions),
		"recordings":            nonNilMobileSlice(source.Task.Recordings),
		"current_app":           source.Observation.CurrentApp,
		"allowed_packages":      nonNilMobileSlice(source.Observation.Packages),
		"browsers":              nonNilMobileSlice(source.Observation.Browsers),
		"web_hosts":             nonNilMobileSlice(source.Observation.Hosts),
		"width":                 source.Observation.Width,
		"height":                source.Observation.Height,
		"ui_nodes":              nonNilMobileSlice(source.Observation.Nodes),
		"visual_observation":    p.visionText,
		"recent_executed_steps": nonNilMobileSlice(source.Task.Recent),
	})
	return string(b), err
}

func sameJSONObject(a, b string) bool {
	left, err := decodeShareJSON([]byte(a))
	if err != nil {
		return false
	}
	right, err := decodeShareJSON([]byte(b))
	return err == nil && reflect.DeepEqual(left, right)
}

func (p *mobilePlanPluginPolicy) Validate(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Check(ctx, job, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var result struct {
		TaskID        string      `json:"task_id"`
		ObservationID string      `json:"observation_id"`
		SourceVersion string      `json:"source_version"`
		Step          mobile.Step `json:"step"`
	}
	if mobilePlanStrictJSON(raw, &result) != nil || result.TaskID != p.task.ID || result.ObservationID != p.obs.ID || result.SourceVersion != p.sourceVersion || !p.planComplete || p.lastProviderText == "" || !p.lastCanonicalOK {
		return pluginruntime.VerifiedResult{}, errors.New("device_planner_result_unverified")
	}
	if !reflect.DeepEqual(p.lastCanonical, result.Step) || !mobile.ValidStep(p.lastCanonical, p.obs) || !mobilePlanActionAllowed(p.lastCanonical, p.allowedActions) {
		return pluginruntime.VerifiedResult{}, errors.New("device_planner_result_unverified")
	}
	private, _ := json.Marshal(struct {
		Step mobile.Step `json:"step"`
	}{Step: p.lastCanonical})
	public, _ := json.Marshal(map[string]any{"task_id": p.task.ID, "observation_id": p.obs.ID, "source_version": p.sourceVersion, "action": p.lastCanonical.Action})
	return pluginruntime.VerifiedResult{Result: public, Private: private}, nil
}

func canonicalMobilePlanStep(raw string) (mobile.Step, bool) {
	var outer struct {
		Action    string          `json:"action"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var step mobile.Step
	if mobilePlanStrictJSON([]byte(raw), &outer) != nil || outer.Action != "tool" || outer.Tool != "device_step" || mobilePlanStrictJSON(outer.Arguments, &step) != nil {
		return mobile.Step{}, false
	}
	return step, true
}

func mobilePlanActionAllowed(step mobile.Step, actions []string) bool {
	if step.Action == "finish" || step.Action == "blocked" || step.Action == "wait" {
		return true
	}
	for _, action := range actions {
		if action == step.Action {
			return true
		}
	}
	return false
}

// mobilePlanStrictJSON first uses the shared bounded duplicate-key parser, so
// every host policy gives nested JSON the same interpretation. It then applies
// this planner's concrete struct/unknown-field contract.
func mobilePlanStrictJSON(raw []byte, out any) error {
	if _, err := decodeShareJSON(raw); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
