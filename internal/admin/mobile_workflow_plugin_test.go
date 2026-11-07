package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

type workflowTestExecutor struct {
	python, module string
	mu             sync.Mutex
	plan           *mobilePlanPluginPolicy
	calls          []string
}

func (e *workflowTestExecutor) Start(context.Context, pluginhost.Package, pluginhost.Grant) (pluginhost.Session, error) {
	return e, nil
}
func (*workflowTestExecutor) Health(context.Context) error { return nil }
func (*workflowTestExecutor) Close() error                 { return nil }
func (e *workflowTestExecutor) Call(ctx context.Context, _ pluginhost.Invocation, name string, raw json.RawMessage) (json.RawMessage, error) {
	e.mu.Lock()
	e.calls = append(e.calls, name)
	p := e.plan
	e.mu.Unlock()
	if name == mobileWorkflowCapability {
		const script = `import importlib.util,json,sys
spec=importlib.util.spec_from_file_location('installed_workflow',sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
json.dump(m.transition(json.load(sys.stdin)),sys.stdout,ensure_ascii=False,separators=(',',':'))`
		command := exec.CommandContext(ctx, e.python, "-I", "-B", "-c", script, e.module)
		command.Stdin = bytes.NewReader(raw)
		output, err := command.Output()
		if err != nil {
			if failure, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("synthetic workflow: %s", failure.Stderr)
			}
			return nil, err
		}
		return output, nil
	}
	if name == mobilePlannerCapability {
		if p == nil {
			return nil, errors.New("test planner missing")
		}
		if _, err := pluginruntime.InvokeHostService(ctx, "wechat/model_request", mobilePlanServiceRequest(p, "describe_screen")); err != nil {
			return nil, err
		}
		if _, err := pluginruntime.InvokeHostService(ctx, "wechat/model_request", mobilePlanServiceRequest(p, "plan_step")); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"task_id": p.task.ID, "observation_id": p.obs.ID, "source_version": p.sourceVersion, "step": map[string]any{"action": p.lastCanonical.Action, "arguments": p.lastCanonical.Arguments, "summary": p.lastCanonical.Summary}})
	}
	var arguments struct {
		SourceVersion string         `json:"source_version"`
		Request       map[string]any `json:"request"`
	}
	if json.Unmarshal(raw, &arguments) != nil {
		return nil, errors.New("test native args")
	}
	device, _ := arguments.Request["device_id"].(string)
	request, _ := json.Marshal(map[string]any{"operation": name, "source_version": arguments.SourceVersion, "device_id": device, "step": "profile", "parameters": map[string]any{}})
	reply, err := pluginruntime.InvokeHostService(ctx, "wechat/device_io", request)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"operation": name, "source_version": arguments.SourceVersion, "result": reply})
}

type workflowFixture struct {
	f                     *webFixture
	service               *mobileWorkflowService
	store                 *pluginhost.Store
	executor              *workflowTestExecutor
	fake                  *fakeMobileDeviceBroker
	model                 *fakeMobilePlanModel
	observations, actions int
	block, entered        chan struct{}
}

func newWorkflowFixture(t *testing.T) *workflowFixture {
	t.Helper()
	f, _ := trustedAgent(t)
	f.manager.mu.Lock()
	f.manager.config.Enabled = true
	f.manager.config.Companion.Enabled = true
	f.manager.mu.Unlock()
	service := f.manager.mobile.(*mobileWorkflowService)
	service.loop = true // deterministic manual ticks, with the production CAS/Controller paths
	cfg := service.Settings()
	cfg.Enabled = true
	cfg.Devices = []string{"synthetic"}
	cfg.Actions = []string{"tap", "capture"}
	cfg.Bridge.Env = map[string]string{"PRIVATE": "synthetic-private-device-setting"}
	if err := service.Save(cfg); err != nil {
		t.Fatal(err)
	}
	store, _ := f.manager.packageStore()
	installed := installMobilePackageForManager(t, f.manager)
	directory, _ := store.PackageDirectory(installed.Digest)
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal(err)
	}
	executor := &workflowTestExecutor{python: python, module: filepath.Join(directory, "runtime/workflow.py")}
	controller, err := pluginruntime.NewController(store, executor)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	t.Cleanup(func() { _ = controller.Close() })
	x := &workflowFixture{f: f, service: service, store: store, executor: executor}
	x.model = &fakeMobilePlanModel{vision: "Synthetic authorized screen", answers: []string{`{"action":"tool","tool":"device_step","arguments":{"action":"tap","arguments":{"x":1,"y":2}}}`, `{"action":"tool","tool":"device_step","arguments":{"action":"finish","summary":"Actual synthetic task finished","arguments":{}}}`}}
	service.planner = func(ctx context.Context, task mobile.Task, observation mobile.Observation) (mobile.Step, error) {
		policy, err := f.manager.newMobilePlanPluginPolicyWithModel(ctx, task, observation, x.model)
		if err != nil {
			return mobile.Step{}, err
		}
		executor.mu.Lock()
		executor.plan = policy
		executor.mu.Unlock()
		result, err := controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: mobileDevicePluginID, Capability: mobilePlannerCapability, Context: policy.invocation, Arguments: policy.arguments}, policy)
		if err != nil {
			t.Log("synthetic installed planner failure", err)
			return mobile.Step{}, err
		}
		private, err := store.PrivateJobResult(ctx, result.Job, func(ctx context.Context, job pluginhost.Job) error {
			return policy.Check(ctx, job, pluginhost.Capability{ID: mobilePlannerCapability})
		})
		if err != nil {
			return mobile.Step{}, err
		}
		var wire struct {
			Step mobile.Step `json:"step"`
		}
		if json.Unmarshal(private, &wire) != nil {
			return wire.Step, errors.New("test plan result")
		}
		return wire.Step, nil
	}
	var pngBuffer bytes.Buffer
	if err = png.Encode(&pngBuffer, image.NewRGBA(image.Rect(0, 0, 100, 200))); err != nil {
		t.Fatal(err)
	}
	pngData := pngBuffer.Bytes()
	x.fake = &fakeMobileDeviceBroker{response: func(name string, raw json.RawMessage) (string, error) {
		switch name {
		case "observe":
			x.observations++
			observation := map[string]any{"id": fmt.Sprintf("obs-%032x", x.observations), "device_id": "synthetic", "package": "synthetic.app", "width": 100, "height": 200, "allowed_packages": []string{"synthetic.app"}, "browsers": []string{}, "web_hosts": []string{}, "nodes": []any{}}
			return string(mustJSON(map[string]any{"text": string(mustJSON(observation)), "media": []map[string]any{{"mime_type": "image/png", "data_base64": base64.StdEncoding.EncodeToString(pngData)}}})), nil
		case "act":
			x.actions++
			if x.entered != nil {
				close(x.entered)
				<-x.block
			}
			return `{"state":"submitted","result":{"action":"tap","device_id":"synthetic","reobserve_required":true}}`, nil
		default:
			return "", errors.New("unexpected test operation")
		}
	}}
	return x
}
func (x *workflowFixture) start(t *testing.T) mobile.Task {
	t.Helper()
	task, err := x.service.Start(mobile.Source{Account: "test", Thread: "wxid_owner", Sender: "wxid_owner", Admin: true, MessageID: "synthetic-request"}, "synthetic", "Inspect synthetic screen")
	if err != nil {
		t.Fatal(err)
	}
	installed, _ := x.store.Installed(t.Context(), mobileDevicePluginID)
	directory, _ := x.store.PackageDirectory(installed.Digest)
	x.service.bridges[task.ID] = &mobileDevicePluginBridge{manager: x.f.manager, store: x.store, controller: x.f.manager.packageRuntime, installed: installed, task: task, settings: x.service.Settings(), packageDir: directory, brokerOpener: func(context.Context, mobile.IOOptions) (mobile.IOBroker, error) { return x.fake, nil }, observations: map[string]mobile.Observation{}, artifacts: map[string]mobile.Artifact{}}
	return task
}
func (x *workflowFixture) tick(t *testing.T) {
	t.Helper()
	x.service.tick()
	deadline := time.Now().Add(5 * time.Second)
	for {
		x.service.mu.Lock()
		active := len(x.service.active)
		x.service.mu.Unlock()
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("workflow tick did not settle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func TestMobileWorkflowInstalledQueueUsesPythonTransitionsAndActualHostFacts(t *testing.T) {
	x := newWorkflowFixture(t)
	task := x.start(t)
	for deadline := time.Now().Add(12 * time.Second); time.Now().Before(deadline); {
		x.tick(t)
		current, _ := x.service.Get(task.ID)
		if current.State == "completed" {
			break
		}
		if current.State == "needs_review" || current.State == "failed" {
			t.Fatal("workflow stopped", current)
		}
		time.Sleep(60 * time.Millisecond)
	}
	current, _ := x.service.Get(task.ID)
	if current.State != "completed" || current.Steps != 1 || x.actions != 1 || x.model.chatCalls != 2 || x.observations != 3 {
		state, _ := x.store.ReadHostState(t.Context(), mobileDevicePluginID, mobileStateNamespace)
		t.Fatal(current, x.actions, x.model.chatCalls, x.observations, string(state.Records[mobileWorkflowKey(task.ID)]))
	}
	tasks := x.service.List(nil)
	if len(tasks) != 1 || tasks[0].Source != task.Source {
		t.Fatal(tasks)
	}
	page, err := x.store.Jobs(t.Context(), mobileDevicePluginID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, job := range page.Items {
		seen[job.Tool]++
		if strings.Contains(string(job.Arguments), "synthetic-private-device-setting") {
			t.Fatal("private settings entered isolated workflow")
		}
	}
	if seen[mobileWorkflowCapability] < 4 || seen[mobilePlannerCapability] != 2 || seen["device.act"] != 1 {
		t.Fatal(seen)
	}
}
func TestMobileWorkflowCancellationKeepsLateActionStopped(t *testing.T) {
	x := newWorkflowFixture(t)
	task := x.start(t)
	for i := 0; i < 7; i++ {
		x.tick(t)
	}
	x.entered, x.block = make(chan struct{}), make(chan struct{})
	x.service.tick()
	select {
	case <-x.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("action was not entered")
	}
	if err := x.service.Cancel(task.ID, task.Source); err != nil {
		t.Fatal(err)
	}
	close(x.block)
	deadline := time.Now().Add(3 * time.Second)
	for {
		x.service.mu.Lock()
		active := len(x.service.active)
		x.service.mu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late action did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		x.tick(t)
	}
	current, _ := x.service.Get(task.ID)
	if current.State != "cancelled" || x.actions != 1 || x.model.chatCalls != 1 {
		t.Fatal(current, x.actions, x.model.chatCalls)
	}
}
func TestMobileWorkflowMissingPackageKeepsMetadataAndEmergencyCancellation(t *testing.T) {
	x := newWorkflowFixture(t)
	task := x.start(t)
	installed, _ := x.store.Installed(t.Context(), mobileDevicePluginID)
	if _, err := x.store.SetEnabled(t.Context(), mobileDevicePluginID, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	x.tick(t)
	if len(x.service.List(nil)) != 1 || x.service.Settings().Revision != 1 || x.actions != 0 {
		t.Fatal("missing package lost stored state")
	}
	if err := x.service.Cancel(task.ID, mobile.Source{Admin: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.service.Start(task.Source, task.DeviceID, task.Goal+" new"); err == nil {
		t.Fatal("missing workflow used core fallback")
	}
	current, _ := x.service.Get(task.ID)
	if current.State != "cancelled" {
		t.Fatal(current)
	}
}
func TestMobileWorkflowRestartMarksClaimUnknownWithoutRepeatingDeviceIO(t *testing.T) {
	x := newWorkflowFixture(t)
	task := x.start(t)
	x.tick(t)
	x.service.stateMu.Lock()
	snapshot, store, err := x.service.load(false)
	if err != nil {
		t.Fatal(err)
	}
	record := snapshot.records[task.ID]
	record.Claim = "synthetic-interrupted-claim"
	raw, _ := json.Marshal(record)
	err = x.service.commit(snapshot, store, map[string]json.RawMessage{mobileWorkflowKey(task.ID): raw})
	x.service.stateMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	replacement := newMobileWorkflowService(x.f.manager)
	replacement.loop = true
	x.f.manager.mobile = replacement
	if err = replacement.recover(); err != nil {
		t.Fatal(err)
	}
	current, _ := replacement.Get(task.ID)
	if current.ID != task.ID || current.RequestHash != task.RequestHash || current.Source != task.Source || current.State != "needs_review" || x.observations != 0 || x.actions != 0 {
		t.Fatal(current)
	}
	if _, err = os.Stat(filepath.Join(x.f.dir, "mobile", "tasks.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created competing task JSON", err)
	}
}

func TestLiveMobileWorkflowInstalledLPACPureTransition(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in installed LPAC pure workflow; no device or model calls")
	}
	python, _ = filepath.Abs(python)
	x := newWorkflowFixture(t)
	_ = x.f.manager.packageRuntime.Close()
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	controller, err := pluginruntime.NewController(x.store, pluginruntime.InstalledExecutor(x.store, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	x.f.manager.packageRuntime, x.f.manager.packageRuntimeReady = controller, true
	task := x.start(t)
	if _, err = x.service.invoke(t.Context(), "advance", map[string]any{"task_id": task.ID}, nil); err != nil {
		t.Fatal(err)
	}
	current, _ := x.service.Get(task.ID)
	if current.State != "running" || x.actions != 0 || x.observations != 0 || x.model.chatCalls != 0 {
		t.Fatal("pure installed transition executed an external effect", current.State)
	}
	if err = x.service.Cancel(task.ID, task.Source); err != nil {
		t.Fatal(err)
	}
}
