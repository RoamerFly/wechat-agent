package station

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
)

const (
	localComfyPackage    = "comfyui"
	localComfyCapability = "comfyui.run"
	localComfyListTool   = "project_comfy_workflows"
	localComfyStartTool  = "project_comfy_run"
	localComfyJobTool    = "project_comfy_job"
	localComfyImageLimit = 16
)

var localComfyGrants = []string{"artifact.write", "gpu"}
var errLocalComfyWorkflowSetup = errors.New("请先登记并校验至少一个获准的 ComfyUI 工作流")

func (s *localProjectTools) comfySetup(ctx context.Context, fish string) (localComfyDeployment, []localComfyWorkflow, error) {
	if s.comfy == nil {
		return localComfyDeployment{}, nil, errors.New("本机 ComfyUI 后端未登记")
	}
	deployment, err := s.comfy.Info(ctx, fish)
	if err != nil || deployment.Binding == "" {
		return localComfyDeployment{}, nil, errors.New(localComputeSetupReason(localComfyPackage, err))
	}
	workflows, err := loadLocalComfyWorkflows(deployment.WorkflowRoot)
	if err != nil || len(workflows) == 0 {
		return deployment, nil, errLocalComfyWorkflowSetup
	}
	return deployment, workflows, nil
}

func (t *localToolSet) localComfyTools(ctx context.Context) []mcptools.Tool {
	s := t.projectTools
	if s == nil || s.comfy == nil || t.run == nil || !t.run.Tracked {
		return nil
	}
	grant, err := t.access.resolve()
	if err != nil {
		return nil
	}
	host, err := s.forFish(grant.FishID)
	if err != nil {
		return nil
	}
	if _, err := installedLocalProject(ctx, host, localProjectOperation{pkg: localComfyPackage, capability: localComfyCapability}); err != nil {
		return nil
	}
	jobTool := mcptools.NewTool(localComfyJobTool, "查询原 ComfyUI 任务或取消。使用先前返回的 job_id。查询取得成品后，在回复中保留作品编号，界面可供主人下载。", `{"type":"object","properties":{"job_id":{"type":"string"},"action":{"type":"string","enum":["status","cancel"]}},"required":["job_id","action"],"additionalProperties":false}`)
	if _, _, err := s.comfySetup(ctx, grant.FishID); err != nil {
		return []mcptools.Tool{jobTool}
	}
	return []mcptools.Tool{
		mcptools.NewTool(localComfyListTool, "查看本机主人已批准的 ComfyUI 工作流及可调参数。先查列表，再选工作流编号。", `{"type":"object","additionalProperties":false}`),
		mcptools.NewTool(localComfyStartTool, "启动已批准的本机 ComfyUI 工作流并取得任务编号。inputs 只填工作流列表给出的参数。PNG 成品从任务查询取得。", `{"type":"object","properties":{"workflow_id":{"type":"string"},"inputs":{"type":"object","additionalProperties":{"type":["string","integer","number","boolean"]}},"title":{"type":"string","maxLength":100}},"required":["workflow_id"],"additionalProperties":false}`),
		jobTool,
	}
}

type localComfyInvocation struct {
	tool           *localToolSet
	host           *localProjectToolHost
	grant          localWorkspaceGrant
	installed      pluginhost.InstalledPackage
	deployment     localComfyDeployment
	workflows      []localComfyWorkflow
	configRevision string
}

func (t *localToolSet) callLocalComfy(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	s := t.projectTools
	if s == nil || s.comfy == nil || t.run == nil || !t.run.Tracked {
		return "", errors.New("当前对话没有本机 ComfyUI 执行授权")
	}
	if err := t.run.CheckSource(ctx); err != nil {
		return "", err
	}
	s.update.Lock()
	defer s.update.Unlock()
	grant, err := t.access.resolve()
	if err != nil {
		return "", err
	}
	host, err := s.forFish(grant.FishID)
	if err != nil {
		return "", err
	}
	installed, err := installedLocalProject(ctx, host, localProjectOperation{pkg: localComfyPackage, capability: localComfyCapability})
	if err != nil {
		return "", err
	}
	cfg, err := host.store.Configuration(ctx, localComfyPackage)
	if err != nil {
		return "", err
	}
	e := localComfyInvocation{tool: t, host: host, grant: grant, installed: installed, configRevision: strconv.Itoa(cfg.Revision)}
	if name == localComfyJobTool {
		return e.jobAction(ctx, raw)
	}
	e.deployment, e.workflows, err = s.comfySetup(ctx, grant.FishID)
	if err != nil {
		return "", err
	}
	if name == localComfyListTool {
		if strictLocalMemoryDecode(raw, &struct{}{}) != nil {
			return "", errors.New("工作流列表不需要参数")
		}
		return localProcessJSON(map[string]any{"workflows": localComfyWorkflowViews(e.workflows)})
	}
	return e.start(ctx, raw)
}

type localComfyJobInput struct {
	Origin      *companion.SourceRef `json:"origin"`
	Trace       string               `json:"trace"`
	Binding     string               `json:"binding"`
	Workflow    string               `json:"workflow_id"`
	Approval    string               `json:"workflow_approval"`
	GraphSHA256 string               `json:"effective_sha256"`
	SaveNode    string               `json:"save_node"`
	Title       string               `json:"title"`
}

func (e localComfyInvocation) start(ctx context.Context, raw json.RawMessage) (string, error) {
	var in struct {
		Workflow string                     `json:"workflow_id"`
		Inputs   map[string]json.RawMessage `json:"inputs"`
		Title    string                     `json:"title"`
	}
	if strictLocalMemoryDecode(raw, &in) != nil || in.Workflow == "" {
		return "", errors.New("请选用已批准的 ComfyUI 工作流编号和参数")
	}
	var workflow *localComfyWorkflow
	for i := range e.workflows {
		if e.workflows[i].ID == in.Workflow {
			workflow = &e.workflows[i]
			break
		}
	}
	if workflow == nil {
		return "", errors.New("指定的 ComfyUI 工作流未获批准")
	}
	graph, digest, err := workflow.prepare(in.Inputs)
	if err != nil {
		return "", err
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		in.Title = workflow.Label
	}
	if !safeRVCDisplay(in.Title, 100) {
		return "", errors.New("ComfyUI 标题须为不超过 100 字的单行文字")
	}
	t, g := e.tool, e.grant
	source, ok := t.store.SourceForInput(companion.Input{Channel: "local", Account: g.FishID, Thread: g.OwnerID, Sender: g.OwnerID, TraceID: t.traceID})
	if !ok {
		return "", errors.New("本轮来源已变化，未提交 ComfyUI")
	}
	args := localComfyJobInput{Origin: source, Trace: t.traceID, Binding: e.deployment.Binding, Workflow: in.Workflow,
		Approval: workflow.Fingerprint, GraphSHA256: digest, SaveNode: workflow.SaveNode, Title: in.Title}
	op := companion.LocalToolCheckpoint{Tool: localComfyStartTool, Target: in.Workflow, Grant: localCheckpointKey(g),
		Key: localCheckpointKey(localComfyStartTool, source.EventID, source.Revision, args)}
	call := pluginhost.Invocation{Account: g.FishID, Thread: g.OwnerID, Requester: g.OwnerID, Purpose: "local ComfyUI creation",
		RequestKey: op.Key, Authorization: op.Grant, SourceVersions: map[string]int{source.EventID: source.Revision}}
	if err := e.authorize(call); err != nil {
		return "", err
	}
	arguments, _ := json.Marshal(args)
	return t.dispatchCheckpointed(ctx, op, g, func() (string, string, error) {
		if err := e.checkSource(ctx, args); err != nil {
			return "", "", err
		}
		job, created, err := e.host.store.CreateJob(ctx, pluginhost.Job{Plugin: localComfyPackage, Version: e.installed.Package.Version,
			PackageDigest: e.installed.Digest, ConfigRevision: e.configRevision, Context: call, Tool: localComfyCapability, Arguments: arguments})
		if err != nil {
			return "", "", err
		}
		if created {
			job = e.submit(ctx, job, args, graph)
		}
		value, err := localProcessJSON(comfyJobView(job))
		return value, "registered", err
	})
}

func (e localComfyInvocation) authorize(call pluginhost.Invocation) error {
	g := pluginhost.Grant{Account: e.grant.FishID, Plugin: localComfyPackage, Version: e.installed.Package.Version, Permissions: e.installed.Permissions}
	return pluginhost.Authorize(e.installed.Package, g, call, localComfyGrants)
}

func (e localComfyInvocation) checkSource(ctx context.Context, args localComfyJobInput) error {
	t, g := e.tool, e.grant
	if args.Origin == nil || t.run.CheckSource(ctx) != nil || !t.access.current(g) ||
		!t.access.home.artifacts.sourceValid(localArtifact{Fish: g.FishID, Owner: g.OwnerID, Request: args.Trace, Source: args.Origin}) {
		return errors.New("ComfyUI 原请求或本机授权已变化")
	}
	return nil
}

func (e localComfyInvocation) checkWorkflow(args localComfyJobInput) error {
	workflows, err := loadLocalComfyWorkflows(e.deployment.WorkflowRoot)
	if err == nil && e.deployment.Binding == args.Binding {
		for _, workflow := range workflows {
			if workflow.ID == args.Workflow && workflow.Fingerprint == args.Approval {
				return nil
			}
		}
	}
	return errors.New("ComfyUI 部署或工作流批准已变化")
}

func (e localComfyInvocation) submit(ctx context.Context, job pluginhost.Job, args localComfyJobInput, graph []byte) pluginhost.Job {
	ref, err := comfyui.NewJobRef(job.ID, args.SaveNode)
	if err != nil {
		return job
	}
	prepared, err := comfyui.Prepare(graph, args.GraphSHA256, ref)
	if err != nil {
		return job
	}
	err = e.tool.projectTools.comfy.Use(ctx, e.grant.FishID, args.Binding, true, func(client localComfyClient) error {
		if err := e.checkSource(ctx, args); err != nil {
			return err
		}
		if err := e.checkWorkflow(args); err != nil {
			return err
		}
		claimed, err := e.host.store.ClaimJob(ctx, job.ID, job.Revision)
		if err != nil {
			return err
		}
		job = claimed
		err = client.Submit(ctx, prepared)
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		switch {
		case err == nil:
			if next, saveErr := e.host.store.AcknowledgeJob(save, job.ID, job.Revision, ref.ID); saveErr == nil {
				job = next
			}
		case errors.Is(err, comfyui.ErrSubmissionUnknown):
			if next, saveErr := e.host.store.ReviewJobFailure(save, job.ID, job.Revision, pluginhost.JobFailureDispatch); saveErr == nil {
				job = next
			}
		default:
			job = e.failComfyJob(save, job, "workflow_not_queued")
			if job.State == "failed" {
				_ = client.MarkSettled(save, ref)
			}
		}
		return err
	})
	// A startup error before claim leaves a durable, visibly unsubmitted job.
	// It can be cancelled. A new request is required to try the workflow again.
	if err != nil && job.State == "pending" {
		view := job
		view.Diagnostic = &pluginhost.JobDiagnostic{Code: "backend_not_started"}
		return view
	}
	return job
}

func comfyJobView(job pluginhost.Job) map[string]any {
	view := map[string]any{"job_id": job.ID, "state": job.State}
	if job.State == "submitting" {
		view["state"] = "needs_review"
	}
	if job.CancelRequested && job.Recovery() != "terminal" {
		view["state"] = "cancelling"
	}
	if job.Diagnostic != nil {
		view["reason"] = job.Diagnostic.Code
	}
	if len(job.Result) != 0 {
		view["verification"] = job.Result
	}
	return view
}

func (t *localToolSet) comfyCheckpointView(op companion.LocalToolCheckpoint, g localWorkspaceGrant) map[string]any {
	view := map[string]any{"tool": op.Tool, "state": "needs_review", "recovered": true}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if t.projectTools == nil || t.run == nil || t.run.CheckSource(ctx) != nil {
		return view
	}
	host, err := t.projectTools.forFish(g.FishID)
	if err != nil {
		return view
	}
	if _, err := installedLocalProject(ctx, host, localProjectOperation{pkg: localComfyPackage, capability: localComfyCapability}); err != nil {
		return view
	}
	job, err := host.store.JobByRequest(ctx, localComfyPackage, g.OwnerID, op.Key)
	if err != nil || job.Context.Authorization != op.Grant || job.Context.Account != g.FishID || job.Context.Requester != g.OwnerID {
		return view
	}
	view = comfyJobView(job)
	view["tool"], view["recovered"] = op.Tool, true
	return view
}
