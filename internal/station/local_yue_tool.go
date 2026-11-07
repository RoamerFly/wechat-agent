package station

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/stationyue"
)

const (
	localYuEPackage    = "yue"
	localYuECapability = "yue.generate"
	localYuEStartTool  = "project_yue_generate"
	localYuEJobTool    = "project_yue_job"
	localYuESaveNode   = "10"
)

var localYuEGrants = []string{"artifact.read", "artifact.write", "gpu"}

type localYuEJobInput struct {
	Origin    *companion.SourceRef `json:"origin"`
	Trace     string               `json:"trace"`
	Binding   string               `json:"binding"`
	Reference *localArtifactLink   `json:"reference,omitempty"`
	Request   stationyue.Request   `json:"recipe"`
}

type localYuESubmission struct {
	Effective    stationyue.Effective `json:"effective"`
	SourceSHA256 string               `json:"source_sha256"`
	GraphSHA256  string               `json:"graph_sha256"`
}

type localYuEInvocation struct {
	tool           *localToolSet
	host           *localProjectToolHost
	grant          localWorkspaceGrant
	installed      pluginhost.InstalledPackage
	configRevision string
}

func (t *localToolSet) localYuETools(ctx context.Context) []mcptools.Tool {
	s := t.projectTools
	if s == nil || s.yue == nil || t.run == nil || !t.run.Tracked {
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
	if _, err := installedLocalProject(ctx, host, localProjectOperation{pkg: localYuEPackage, capability: localYuECapability}); err != nil {
		return nil
	}
	capabilities, err := host.store.Capabilities(ctx, localYuEPackage)
	if err != nil {
		return nil
	}
	var tools []mcptools.Tool
	for _, capability := range capabilities {
		if !capability.Discoverable {
			continue
		}
		switch capability.ID {
		case "yue.job":
			tools = append(tools, mcptools.Tool{Name: localYuEJobTool, Description: capability.Description, InputSchema: capability.InputSchema})
		case localYuECapability:
			if _, err := s.yue.Info(ctx, grant.FishID); err == nil {
				tools = append(tools, mcptools.Tool{Name: localYuEStartTool, Description: capability.Description, InputSchema: capability.InputSchema})
			}
		}
	}
	return tools
}

func (t *localToolSet) callLocalYuE(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	s := t.projectTools
	if s == nil || s.yue == nil || t.run == nil || !t.run.Tracked {
		return "", errors.New("当前对话没有本机 YuE 执行授权")
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
	installed, err := installedLocalProject(ctx, host, localProjectOperation{pkg: localYuEPackage, capability: localYuECapability})
	if err != nil {
		return "", err
	}
	cfg, err := host.store.Configuration(ctx, localYuEPackage)
	if err != nil {
		return "", err
	}
	e := localYuEInvocation{tool: t, host: host, grant: grant, installed: installed, configRevision: strconv.Itoa(cfg.Revision)}
	if name == localYuEJobTool {
		return e.jobAction(ctx, raw)
	}
	return e.start(ctx, raw)
}

type localYuEGenerateInput struct {
	Mode      stationyue.Mode           `json:"mode"`
	Style     *string                   `json:"style"`
	Lyrics    *string                   `json:"lyrics"`
	Duration  float64                   `json:"duration_seconds"`
	Minimum   *float64                  `json:"min_duration_seconds"`
	Guidance  *float64                  `json:"text_guidance_scale"`
	Seed      *int64                    `json:"seed"`
	Gender    string                    `json:"vocal_gender"`
	Planning  string                    `json:"planning_mode"`
	RefMode   string                    `json:"reference_mode"`
	Reference string                    `json:"reference_artifact_id"`
	Sampling  map[string]map[string]any `json:"sampling"`
}

func (e localYuEInvocation) start(ctx context.Context, raw json.RawMessage) (string, error) {
	var in localYuEGenerateInput
	if len(raw) > 64<<10 || localComfyStrictDecode(raw, &in) != nil || in.Style == nil || in.Lyrics == nil || in.Duration <= 0 ||
		in.Mode != stationyue.ModeOriginal && in.Mode != stationyue.ModeCover ||
		(in.Mode == stationyue.ModeCover) != (in.Reference != "") {
		return "", errors.New("请提供原创或参考模式、风格、歌词和时长，参考模式需指定本机成品")
	}
	t, g := e.tool, e.grant
	source, ok := t.store.SourceForInput(companion.Input{Channel: "local", Account: g.FishID, Thread: g.OwnerID, Sender: g.OwnerID, TraceID: t.traceID})
	if !ok {
		return "", errors.New("本轮来源已变化")
	}
	op := companion.LocalToolCheckpoint{Tool: localYuEStartTool, Target: string(in.Mode), Grant: localCheckpointKey(g),
		Key: localCheckpointKey(localYuEStartTool, source.EventID, source.Revision, in)}
	for _, previous := range t.run.Operations {
		if previous.Key == op.Key {
			return localProcessJSON(t.yueCheckpointView(previous, g))
		}
	}
	deploy, err := t.projectTools.yue.Info(ctx, g.FishID)
	if err != nil {
		return "", errors.New("请检查本机 YuE 部署登记")
	}
	request := stationyue.Request{Mode: in.Mode, Style: *in.Style, Lyrics: *in.Lyrics, DurationSeconds: in.Duration,
		MinDurationSeconds: in.Minimum, TextGuidanceScale: in.Guidance, VocalGender: in.Gender,
		PlanningMode: in.Planning, ReferenceMode: in.RefMode, Sampling: in.Sampling}
	// Derive the default seed from the original invocation so replay cannot
	// change the fingerprint, even across a host restart.
	seed, _ := strconv.ParseUint(op.Key[:8], 16, 32)
	request.Seed = int64(seed & 0x7fffffff)
	if in.Seed != nil {
		request.Seed = *in.Seed
	}
	args := localYuEJobInput{Origin: source, Trace: t.traceID, Binding: deploy.Binding, Request: request}
	versions := map[string]int{source.EventID: source.Revision}
	var reference []byte
	if in.Reference != "" {
		bound, err := t.access.home.artifacts.bindProjectArtifact(ctx, in.Reference, t.access, g)
		if err != nil {
			return "", err
		}
		reference, _, err = readRVCReference(ctx, bound)
		if err != nil {
			return "", errors.New("YuE 参考成品须是有效的 PCM16 WAV，最多 25 MiB，可先用音频工坊处理")
		}
		args.Reference = &localArtifactLink{ID: bound.artifact.ID, SHA256: bound.artifact.SHA256}
		if bound.artifact.Source != nil {
			versions[bound.artifact.Source.EventID] = bound.artifact.Source.Revision
		}
	}
	call := pluginhost.Invocation{Account: g.FishID, Thread: g.OwnerID, Requester: g.OwnerID, Purpose: "local YuE music",
		RequestKey: op.Key, Authorization: op.Grant, SourceVersions: versions}
	if err := e.authorize(call); err != nil {
		return "", err
	}
	arguments, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return t.dispatchCheckpointed(ctx, op, g, func() (string, string, error) {
		if err := e.checkSource(ctx, args); err != nil {
			return "", "", err
		}
		job, created, err := e.host.store.CreateJob(ctx, pluginhost.Job{Plugin: localYuEPackage, Version: e.installed.Package.Version,
			PackageDigest: e.installed.Digest, ConfigRevision: e.configRevision, Context: call, Tool: localYuECapability, Arguments: arguments})
		if err != nil {
			return "", "", err
		}
		if created {
			job = e.submitYuE(ctx, job, args, deploy, reference)
		}
		value, err := localProcessJSON(yueJobView(job))
		return value, "registered", err
	})
}

func (e localYuEInvocation) authorize(call pluginhost.Invocation) error {
	grant := pluginhost.Grant{Account: e.grant.FishID, Plugin: localYuEPackage, Version: e.installed.Package.Version, Permissions: e.installed.Permissions}
	return pluginhost.Authorize(e.installed.Package, grant, call, localYuEGrants)
}

func (e localYuEInvocation) checkSource(ctx context.Context, args localYuEJobInput) error {
	t, g := e.tool, e.grant
	artifacts := t.access.home.artifacts
	if args.Origin == nil || t.run.CheckSource(ctx) != nil || !t.access.current(g) ||
		!artifacts.sourceValid(localArtifact{Fish: g.FishID, Owner: g.OwnerID, Request: args.Trace, Source: args.Origin}) {
		return errors.New("YuE 原请求或本机授权已变化")
	}
	if args.Reference != nil {
		ref, err := artifacts.bindProjectArtifact(ctx, args.Reference.ID, t.access, g)
		if err != nil || ref.artifact.SHA256 != args.Reference.SHA256 {
			return errors.New("YuE 原参考成品的来源或内容已变化")
		}
	}
	return nil
}

func (e localYuEInvocation) submitYuE(ctx context.Context, job pluginhost.Job, args localYuEJobInput, deploy localYuEDeployment, reference []byte) pluginhost.Job {
	ref, err := comfyui.NewJobRef(job.ID, localYuESaveNode)
	if err != nil {
		return job
	}
	preparationStage := pluginhost.JobFailureDispatch
	err = e.tool.projectTools.yue.Use(ctx, e.grant.FishID, args.Binding, true, func(client localComfyAudioClient) error {
		if err := e.checkSource(ctx, args); err != nil {
			preparationStage = pluginhost.JobFailureAuthority
			return err
		}
		request := args.Request
		if args.Reference != nil {
			request.ReferenceBasename, _ = comfyui.ReferenceWAVName(ref)
			request.ReferenceSHA256 = args.Reference.SHA256
		}
		info, err := client.YuERequiredNodeSchemas(ctx)
		if err != nil {
			return err
		}
		preparationStage = pluginhost.JobFailureValidation
		recipe, err := (stationyue.Compiler{WorkflowDir: deploy.WorkflowRoot, PinnedTemplateSHA256: deploy.Pins, ObjectInfo: info}).Compile(request)
		if err != nil {
			return err
		}
		prepared, err := comfyui.PrepareFLAC(recipe.Graph, recipe.EffectiveSHA256, ref)
		if err != nil {
			return err
		}
		receipt, err := json.Marshal(localYuESubmission{Effective: recipe.Effective, SourceSHA256: recipe.SourceSHA256, GraphSHA256: recipe.EffectiveSHA256})
		if err != nil {
			return err
		}
		preparationStage = pluginhost.JobFailureDispatch
		claimed, err := e.host.store.ClaimJobWithSubmission(ctx, job.ID, job.Revision, receipt)
		if err != nil {
			return err
		}
		job = claimed
		sendErr := e.dispatchYuE(ctx, client, args, request.ReferenceBasename, ref, prepared, reference)
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		switch {
		case sendErr == nil:
			if next, err := e.host.store.AcknowledgeJob(save, job.ID, job.Revision, ref.ID); err == nil {
				job = next
			}
		case errors.Is(sendErr, comfyui.ErrSubmissionUnknown):
			if next, err := e.host.store.ReviewJobFailure(save, job.ID, job.Revision, pluginhost.JobFailureDispatch); err == nil {
				job = next
			}
		default:
			reason, _ := json.Marshal(map[string]string{"reason": "workflow_not_queued"})
			if next, err := e.host.store.FailJob(save, job.ID, job.Revision, reason); err == nil {
				job = next
				_ = client.MarkSettled(save, ref)
			}
		}
		return sendErr
	})
	if err != nil && job.State == "pending" {
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if next, saveErr := e.host.store.FailJobPreparation(save, job.ID, job.Revision, preparationStage); saveErr == nil {
			job = next
		}
	}
	return job
}

func (e localYuEInvocation) dispatchYuE(ctx context.Context, client localComfyAudioClient, args localYuEJobInput, basename string, ref comfyui.JobRef, prepared comfyui.Prepared, reference []byte) error {
	if err := e.checkSource(ctx, args); err != nil {
		return err
	}
	if args.Reference != nil {
		name, err := client.UploadWAV(ctx, ref, reference)
		if err != nil || name != basename {
			return errors.New("YuE 参考音频接收未核实，未提交生成")
		}
		if err := e.checkSource(ctx, args); err != nil {
			return err
		}
	}
	return client.Submit(ctx, prepared)
}

func (t *localToolSet) yueCheckpointView(op companion.LocalToolCheckpoint, g localWorkspaceGrant) map[string]any {
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
	if _, err := installedLocalProject(ctx, host, localProjectOperation{pkg: localYuEPackage, capability: localYuECapability}); err != nil {
		return view
	}
	job, err := host.store.JobByRequest(ctx, localYuEPackage, g.OwnerID, op.Key)
	if err != nil || job.Context.Authorization != op.Grant || job.Context.Account != g.FishID || job.Context.Requester != g.OwnerID {
		return view
	}
	var args localYuEJobInput
	if json.Unmarshal(job.Arguments, &args) != nil || (localYuEInvocation{tool: t, grant: g}).checkSource(ctx, args) != nil {
		return view
	}
	view = yueJobView(job)
	view["tool"], view["recovered"] = op.Tool, true
	return view
}
