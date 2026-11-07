package station

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/workspacefs"
)

func (e localComfyInvocation) jobAction(ctx context.Context, raw json.RawMessage) (string, error) {
	var in struct {
		ID     string `json:"job_id"`
		Action string `json:"action"`
	}
	if strictLocalMemoryDecode(raw, &in) != nil || !validLocalRVCHex(in.ID, 64) || in.Action != "status" && in.Action != "cancel" {
		return "", errors.New("请提供原 ComfyUI 任务编号和查询或取消操作")
	}
	job, err := e.host.store.Job(ctx, in.ID)
	g := e.grant
	if err != nil || job.Plugin != localComfyPackage || job.Tool != localComfyCapability || job.Context.Account != g.FishID ||
		job.Context.Requester != g.OwnerID || job.Context.Thread != g.OwnerID || job.Context.Authorization != localCheckpointKey(g) ||
		job.PackageDigest != e.installed.Digest || job.ConfigRevision != e.configRevision || e.authorize(job.Context) != nil {
		return "", errors.New("ComfyUI 任务不存在或原授权、版本已变化")
	}
	var args localComfyJobInput
	if json.Unmarshal(job.Arguments, &args) != nil {
		return "", errors.New("ComfyUI 任务记录无效")
	}
	if in.Action == "status" {
		if err := e.checkSource(ctx, args); err != nil {
			return "", err
		}
	}
	if job.Recovery() == "terminal" {
		if in.Action == "cancel" {
			return localProcessJSON(map[string]any{"job_id": job.ID, "state": job.State})
		}
		return localProcessJSON(e.finishedView(job))
	}
	if in.Action == "cancel" && !job.CancelRequested {
		job, err = e.host.store.RequestJobCancel(ctx, job.ID, job.Revision)
		if err != nil {
			return "", err
		}
	}
	if job.State == "pending" || job.Recovery() == "terminal" {
		view := comfyJobView(job)
		if job.State == "pending" {
			view["reason"] = "backend_not_started"
		}
		return localProcessJSON(view)
	}
	if !job.CancelRequested {
		e.deployment, err = e.tool.projectTools.comfy.Info(ctx, g.FishID)
		if err != nil || e.checkWorkflow(args) != nil {
			view := comfyJobView(job)
			view["state"], view["reason"] = "needs_review", "deployment_or_workflow_changed"
			return localProcessJSON(view)
		}
	}
	ref, err := comfyui.NewJobRef(job.ID, args.SaveNode)
	if err != nil || job.RemoteID != "" && job.RemoteID != ref.ID {
		return "", errors.New("ComfyUI 原任务编号无法核对")
	}
	var view map[string]any
	err = e.tool.projectTools.comfy.Use(ctx, g.FishID, args.Binding, false, func(client localComfyClient) error {
		state, err := client.Status(ctx, ref)
		if err != nil {
			return err
		}
		view, err = e.reconcile(ctx, client, job, args, ref, state)
		if err == nil {
			switch view["state"] {
			case "completed", "failed", "cancelled", "artifact_unavailable":
				_ = client.MarkSettled(ctx, ref)
			}
		}
		return err
	})
	if errors.Is(err, errLocalComfyNotRunning) {
		view = comfyJobView(job)
		if !job.CancelRequested {
			view["state"] = "needs_review"
		}
		view["reason"] = "original_backend_not_running"
	} else if err != nil {
		return "", err
	}
	return localProcessJSON(view)
}

func (e localComfyInvocation) reconcile(ctx context.Context, client localComfyClient, job pluginhost.Job, args localComfyJobInput, ref comfyui.JobRef, remote comfyui.JobStatus) (map[string]any, error) {
	if remote.State != comfyui.Unknown && job.RemoteID == "" {
		next, err := e.host.store.AcknowledgeJob(ctx, job.ID, job.Revision, ref.ID)
		if err != nil {
			return nil, err
		}
		job = next
	}
	if job.CancelRequested {
		return e.cancelComfyJob(ctx, client, job, ref, remote.State)
	}
	if err := e.checkSource(ctx, args); err != nil {
		return nil, err
	}
	switch remote.State {
	case comfyui.Completed:
		if remote.ImageCount < 1 || remote.ImageCount > localComfyImageLimit {
			return comfyJobView(e.failComfyJob(ctx, job, "approved_png_output_count_invalid")), nil
		}
		return e.publishComfyImages(ctx, client, job, args, ref, remote.ImageCount)
	case comfyui.Failed:
		return comfyJobView(e.failComfyJob(ctx, job, "workflow_execution_failed")), nil
	case comfyui.Cancelled:
		next, err := e.host.store.FinishJob(ctx, job.ID, job.Revision, "cancelled", nil)
		return comfyJobView(next), err
	case comfyui.Unknown:
		view := comfyJobView(job)
		view["state"], view["reason"] = "needs_review", "original_prompt_not_found"
		return view, nil
	default:
		view := comfyJobView(job)
		view["backend_state"] = remote.State
		return view, nil
	}
}

func (e localComfyInvocation) cancelComfyJob(ctx context.Context, client localComfyClient, job pluginhost.Job, ref comfyui.JobRef, state comfyui.State) (map[string]any, error) {
	if state == comfyui.Pending || state == comfyui.Running {
		dispatched, err := client.Cancel(ctx, ref)
		if err != nil {
			return nil, err
		}
		next, err := client.Status(ctx, ref)
		if err != nil {
			return nil, err
		}
		// Removing a queued item from ComfyUI leaves no history entry.
		if dispatched && next.State == comfyui.Unknown {
			state = comfyui.Cancelled
		} else {
			state = next.State
		}
	}
	switch state {
	case comfyui.Cancelled:
		next, err := e.host.store.FinishJob(ctx, job.ID, job.Revision, "cancelled", nil)
		return comfyJobView(next), err
	case comfyui.Completed:
		next, err := e.host.store.CompleteJob(ctx, job.ID, job.Revision, nil, json.RawMessage(`{"output_withheld_after_cancel":true}`))
		return comfyJobView(next), err
	case comfyui.Failed:
		return comfyJobView(e.failComfyJob(ctx, job, "workflow_execution_failed")), nil
	default:
		view := comfyJobView(job)
		view["backend_state"] = state
		return view, nil
	}
}

func (e localComfyInvocation) failComfyJob(ctx context.Context, job pluginhost.Job, reason string) pluginhost.Job {
	receipt, _ := json.Marshal(map[string]string{"reason": reason})
	if next, err := e.host.store.FailJob(ctx, job.ID, job.Revision, receipt); err == nil {
		return next
	}
	return job
}

func (e localComfyInvocation) finishedView(job pluginhost.Job) map[string]any {
	view := comfyJobView(job)
	if len(job.Artifacts) == 0 {
		return view
	}
	store := e.tool.access.home.artifacts
	artifacts := make([]map[string]any, 0, len(job.Artifacts))
	for _, id := range job.Artifacts {
		a, err := store.read(id)
		if err != nil || a.Request != job.ID || a.Fish != e.grant.FishID || a.Owner != e.grant.OwnerID {
			view["state"] = "artifact_unavailable"
			return view
		}
		artifacts = append(artifacts, a.view())
	}
	view["artifacts"] = artifacts
	if len(artifacts) == 1 {
		view["artifact_id"] = job.Artifacts[0]
	}
	return view
}

func (e localComfyInvocation) publishComfyImages(ctx context.Context, client localComfyClient, job pluginhost.Job, args localComfyJobInput, ref comfyui.JobRef, count int) (map[string]any, error) {
	dir := filepath.Join(e.tool.projectTools.home.dataDir, "comfyui-output-stage", job.ID)
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = root.Close()
		_ = os.Remove(dir)
	}()
	ids := make([]string, 0, count)
	type imageEvidence struct {
		Width  int    `json:"width"`
		Height int    `json:"height"`
		SHA256 string `json:"sha256"`
	}
	evidence := make([]imageEvidence, 0, count)
	for index := range count {
		img, err := client.FetchPNG(ctx, ref, index)
		if err != nil {
			return nil, err
		}
		if err := e.checkSource(ctx, args); err != nil {
			return nil, err
		}
		name := fmt.Sprintf("comfy-%02d.png", index+1)
		if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		n, err := file.Write(img.Data)
		if err == nil && n != len(img.Data) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = file.Sync()
		}
		if err = errors.Join(err, file.Close()); err != nil {
			_ = root.Remove(name)
			return nil, err
		}
		input := localArtifactInput{Key: job.ID, Expected: img.SHA256, project: localComfyPackage}
		a, err := e.tool.access.home.artifacts.publishFile(ctx, input, args.Origin, e.tool.access, e.grant, filepath.Join(dir, name), nil)
		_ = root.Remove(name)
		if err != nil {
			return nil, err
		}
		ids = append(ids, a.ID)
		evidence = append(evidence, imageEvidence{img.Width, img.Height, img.SHA256})
	}
	if err := e.checkSource(ctx, args); err != nil {
		return nil, err
	}
	if err := e.checkWorkflow(args); err != nil {
		return nil, err
	}
	receipt, _ := json.Marshal(map[string]any{"workflow_id": args.Workflow, "images": evidence})
	next, err := e.host.store.CompleteJob(ctx, job.ID, job.Revision, ids, receipt)
	if err != nil {
		return nil, err
	}
	return e.finishedView(next), nil
}
