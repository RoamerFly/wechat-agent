package station

// Durable YuE job recovery deliberately lives beside, rather than inside, the
// submitter.  A completed ComfyUI prompt is not a completed music task: the
// FLAC still has to be fetched, decoded, checked against the recipe which was
// actually submitted, and atomically registered before it is downloadable.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/mediadecode"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/stationyue"
	"wechat-bot/internal/workspacefs"
)

const (
	localYuEMaxInputBytes  int64  = 256 << 20
	localYuEMaxSampleRate  uint32 = 192000
	localYuEMaxChannels    uint8  = 8
	localYuEBytesPerSample int64  = 2 // VerifyFLAC asks ffmpeg for pcm_s16le.
)

// jobAction is recovery-only.  In particular, status never starts the YuE
// service: ComfyUI may have discarded its volatile queue after a stop, so a
// query cannot honestly reconstruct the original generation.
func (e localYuEInvocation) jobAction(ctx context.Context, raw json.RawMessage) (string, error) {
	var in struct {
		ID     string `json:"job_id"`
		Action string `json:"action"`
	}
	if strictLocalMemoryDecode(raw, &in) != nil || !validLocalRVCHex(in.ID, 64) || (in.Action != "status" && in.Action != "cancel") {
		return "", errors.New("请提供原 YuE 任务编号及 status 或 cancel 操作")
	}
	job, err := e.host.store.Job(ctx, in.ID)
	g := e.grant
	if err != nil || job.Plugin != localYuEPackage || job.Tool != localYuECapability ||
		job.Context.Account != g.FishID || job.Context.Requester != g.OwnerID || job.Context.Thread != g.OwnerID ||
		job.Context.Authorization != localCheckpointKey(g) || job.PackageDigest != e.installed.Digest ||
		job.ConfigRevision != e.configRevision || e.authorize(job.Context) != nil {
		return "", errors.New("YuE 任务不存在或原授权、版本已变化")
	}
	var args localYuEJobInput
	if json.Unmarshal(job.Arguments, &args) != nil {
		return "", errors.New("YuE 任务记录无效")
	}
	// A status response can reveal the effective recipe or an artifact.  Apply
	// the original source/forget boundary even after completion; terminal jobs
	// do not outlive a later revocation merely because their bytes are cached.
	if in.Action == "status" {
		if err := e.checkSource(ctx, args); err != nil {
			return "", err
		}
	}
	if job.Recovery() == "terminal" {
		if in.Action == "cancel" {
			return localProcessJSON(yueCancelView(yueJobView(job)))
		}
		return localProcessJSON(e.finishedView(job))
	}
	if in.Action == "cancel" && !job.CancelRequested {
		job, err = e.host.store.RequestJobCancel(ctx, job.ID, job.Revision)
		if err != nil {
			return "", err
		}
	}
	if job.Recovery() == "terminal" || job.State == "pending" {
		view := yueJobView(job)
		if job.State == "pending" && !job.CancelRequested {
			view["reason"] = "backend_not_started"
		}
		if in.Action == "cancel" {
			view = yueCancelView(view)
		}
		return localProcessJSON(view)
	}

	// The submission receipt is the host-side commitment to the exact effective
	// bounds.  Do not read an unfinished prompt with a missing or malformed
	// receipt, even if its job ID happens to exist in ComfyUI history.
	var submission localYuESubmission
	var deploy localYuEDeployment
	if in.Action != "cancel" {
		submission, err = localYuESubmissionFor(job, args)
		if err != nil {
			view := yueJobView(job)
			view["state"], view["reason"] = "needs_review", "submission_receipt_invalid"
			return localProcessJSON(view)
		}
		// Inspect deployment before considering the FFmpeg path in the result
		// branch.  A cancel, by contrast, is a narrow control operation and can
		// still use the original binding even when no result can be read.
		deploy, err = e.tool.projectTools.yue.Info(ctx, g.FishID)
		if err != nil || deploy.Binding != args.Binding {
			view := yueJobView(job)
			view["state"], view["reason"] = "needs_review", "deployment_changed_or_unavailable"
			return localProcessJSON(view)
		}
	}
	ref, err := comfyui.NewJobRef(job.ID, localYuESaveNode)
	if err != nil || job.RemoteID != "" && job.RemoteID != ref.ID {
		return "", errors.New("YuE 原任务编号无法核对")
	}
	var view map[string]any
	err = e.tool.projectTools.yue.Use(ctx, g.FishID, args.Binding, false, func(client localComfyAudioClient) error {
		remote, err := client.AudioStatus(ctx, ref)
		if err != nil {
			return err
		}
		view, err = e.reconcile(ctx, client, job, args, submission, deploy, ref, remote)
		if err == nil && yueTerminal(view) {
			// Settling is intentionally after the durable host transition; if it
			// fails, the service retains a harmless idle hold rather than losing a
			// result before the job has a receipt.
			_ = client.MarkSettled(ctx, ref)
		}
		return err
	})
	if errors.Is(err, errLocalYuENotRunning) || errors.Is(err, errLocalComfyNotRunning) {
		view = yueJobView(job)
		if !job.CancelRequested {
			view["state"] = "needs_review"
		}
		view["reason"] = "original_backend_not_running"
	} else if err != nil {
		return "", err
	}
	if in.Action == "cancel" {
		view = yueCancelView(view)
	}
	return localProcessJSON(view)
}

func (e localYuEInvocation) reconcile(ctx context.Context, client localComfyAudioClient, job pluginhost.Job, args localYuEJobInput, submission localYuESubmission, deploy localYuEDeployment, ref comfyui.JobRef, remote comfyui.AudioStatus) (map[string]any, error) {
	if remote.State != comfyui.Unknown && job.RemoteID == "" {
		next, err := e.host.store.AcknowledgeJob(ctx, job.ID, job.Revision, ref.ID)
		if err != nil {
			return nil, err
		}
		job = next
	}
	if job.CancelRequested {
		return e.cancelYuEJob(ctx, client, job, ref, remote.State)
	}
	if err := e.checkSource(ctx, args); err != nil {
		return nil, err
	}
	switch remote.State {
	case comfyui.Completed:
		if remote.AudioCount != 1 {
			return yueJobView(e.failYuEJob(ctx, job, "approved_flac_output_count_invalid")), nil
		}
		return e.publishYuEFLAC(ctx, client, job, args, submission, deploy, ref)
	case comfyui.Failed:
		return yueJobView(e.failYuEJob(ctx, job, "workflow_execution_failed")), nil
	case comfyui.Cancelled:
		next, err := e.host.store.FinishJob(ctx, job.ID, job.Revision, "cancelled", nil)
		return yueJobView(next), err
	case comfyui.Unknown:
		view := yueJobView(job)
		view["state"], view["reason"] = "needs_review", "original_prompt_not_found"
		return view, nil
	default:
		view := yueJobView(job)
		view["backend_state"] = remote.State
		return view, nil
	}
}

// cancelYuEJob makes cancellation durable before issuing HTTP.  A completed
// upstream prompt is deliberately withheld: a cancelled request may be kept
// as an internal provider result but never becomes a visible/downloadable song.
func (e localYuEInvocation) cancelYuEJob(ctx context.Context, client localComfyAudioClient, job pluginhost.Job, ref comfyui.JobRef, state comfyui.State) (map[string]any, error) {
	if state == comfyui.Pending || state == comfyui.Running {
		dispatched, err := client.Cancel(ctx, ref)
		if err != nil {
			return nil, err
		}
		next, err := client.AudioStatus(ctx, ref)
		if err != nil {
			return nil, err
		}
		if dispatched && next.State == comfyui.Unknown {
			state = comfyui.Cancelled
		} else {
			state = next.State
		}
	}
	switch state {
	case comfyui.Cancelled, comfyui.Completed:
		next, err := e.host.store.FinishJob(ctx, job.ID, job.Revision, "cancelled", nil)
		return yueJobView(next), err
	case comfyui.Failed:
		return yueJobView(e.failYuEJob(ctx, job, "workflow_execution_failed")), nil
	default:
		view := yueJobView(job)
		view["backend_state"] = state
		return view, nil
	}
}

func (e localYuEInvocation) failYuEJob(ctx context.Context, job pluginhost.Job, reason string) pluginhost.Job {
	receipt, _ := json.Marshal(map[string]string{"reason": reason})
	if next, err := e.host.store.FailJob(ctx, job.ID, job.Revision, receipt); err == nil {
		return next
	}
	return job
}

func (e localYuEInvocation) finishedView(job pluginhost.Job) map[string]any {
	view := yueJobView(job)
	if job.State != "completed" || len(job.Artifacts) == 0 {
		return view
	}
	artifacts := make([]map[string]any, 0, len(job.Artifacts))
	for _, id := range job.Artifacts {
		a, err := e.tool.access.home.artifacts.read(id)
		if err != nil || a.Request != job.ID || a.Fish != e.grant.FishID || a.Owner != e.grant.OwnerID || a.ProjectTool != localYuEPackage {
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

func (e localYuEInvocation) publishYuEFLAC(ctx context.Context, client localComfyAudioClient, job pluginhost.Job, args localYuEJobInput, submission localYuESubmission, deploy localYuEDeployment, ref comfyui.JobRef) (map[string]any, error) {
	if err := e.checkSource(ctx, args); err != nil {
		return nil, err
	}
	// Info was read before this method is entered; require the same binding and
	// an explicitly registered decoder rather than falling back to PATH.
	if deploy.Binding != args.Binding || deploy.FFmpegPath == "" {
		return nil, errors.New("YuE 部署或音频校验器已变化")
	}
	flac, err := client.FetchFLAC(ctx, ref)
	if err != nil {
		return nil, err
	}
	if flac.SHA256 == "" || len(flac.Data) < 4 || int64(len(flac.Data)) > localYuEMaxInputBytes {
		return nil, errors.New("YuE 成品 FLAC 无效")
	}
	dir := filepath.Join(e.tool.projectTools.home.dataDir, "yue-output-stage", job.ID)
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return nil, err
	}
	const name = "yue-result.flac"
	defer func() {
		_ = root.Remove(name)
		_ = root.Close()
		_ = os.Remove(dir)
	}()
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	n, writeErr := file.Write(flac.Data)
	if writeErr == nil && n != len(flac.Data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if writeErr = errors.Join(writeErr, file.Close()); writeErr != nil {
		return nil, writeErr
	}
	path := filepath.Join(dir, name)
	limit := localYuEDecodedLimit(submission.Effective.DurationSeconds)
	evidence, err := mediadecode.VerifyFLAC(ctx, path, mediadecode.FLACVerificationOptions{
		FFmpegPath: deploy.FFmpegPath, MaxInputBytes: localYuEMaxInputBytes, MaxDecodedPCMBytes: limit,
		MaxSampleRate: localYuEMaxSampleRate, MaxChannels: localYuEMaxChannels,
	})
	if err != nil || evidence.SHA256 != flac.SHA256 {
		return nil, errors.New("YuE 成品 FLAC 未通过完整性校验")
	}
	precision := 1 / float64(evidence.SampleRate)
	duration, err := stationyue.ValidateDuration(evidence.Duration.Seconds(), submission.Effective.DurationSeconds, submission.Effective.MinDurationSeconds, &precision)
	if err != nil || !duration.Accepted {
		return nil, errors.New("YuE 成品时长与已提交配方不一致")
	}
	if err := e.checkSource(ctx, args); err != nil {
		return nil, err
	}
	input := localArtifactInput{Key: job.ID, Expected: flac.SHA256, parent: args.Reference, project: localYuEPackage}
	artifact, err := e.tool.access.home.artifacts.publishFile(ctx, input, args.Origin, e.tool.access, e.grant, path, nil)
	if err != nil {
		return nil, err
	}
	if err := e.checkSource(ctx, args); err != nil {
		return nil, err
	}
	receipt, _ := json.Marshal(map[string]any{
		"effective": submission.Effective, "source_sha256": submission.SourceSHA256, "graph_sha256": submission.GraphSHA256,
		"sha256": evidence.SHA256, "bytes": evidence.Bytes, "sample_rate": evidence.SampleRate, "channels": evidence.Channels,
		"bits_per_sample": evidence.BitsPerSample, "decoded_frames": evidence.DecodedFrames, "duration_seconds": evidence.Duration.Seconds(),
		"duration_check": duration, "timbre_listening_verified": false,
	})
	next, err := e.host.store.CompleteJob(ctx, job.ID, job.Revision, []string{artifact.ID}, receipt)
	if err != nil {
		return nil, err
	}
	return e.finishedView(next), nil
}

func localYuESubmissionFor(job pluginhost.Job, args localYuEJobInput) (localYuESubmission, error) {
	var receipt localYuESubmission
	if len(job.Submission) == 0 || json.Unmarshal(job.Submission, &receipt) != nil ||
		!validLocalRVCHex(receipt.SourceSHA256, 64) || !validLocalRVCHex(receipt.GraphSHA256, 64) ||
		receipt.Effective.Mode != args.Request.Mode || receipt.Effective.DurationSeconds <= 0 ||
		receipt.Effective.MinDurationSeconds < 0 || receipt.Effective.MinDurationSeconds > receipt.Effective.DurationSeconds ||
		receipt.Effective.Seed != args.Request.Seed {
		return receipt, errors.New("YuE 提交配方记录无效")
	}
	if args.Reference == nil {
		if receipt.Effective.ReferenceBasename != "" || receipt.Effective.ReferenceSHA256 != "" {
			return receipt, errors.New("YuE 原创任务含有不匹配的参考音频")
		}
	} else {
		ref, err := comfyui.NewJobRef(job.ID, localYuESaveNode)
		if err != nil {
			return receipt, errors.New("YuE 任务编号无效")
		}
		want, err := comfyui.ReferenceWAVName(ref)
		if err != nil || receipt.Effective.ReferenceSHA256 != args.Reference.SHA256 || receipt.Effective.ReferenceBasename != want {
			return receipt, errors.New("YuE 参考音频配方已变化")
		}
	}
	return receipt, nil
}

func yueJobView(job pluginhost.Job) map[string]any {
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
	var args localYuEJobInput
	if json.Unmarshal(job.Arguments, &args) == nil {
		if submission, err := localYuESubmissionFor(job, args); err == nil {
			// The helper above also validates request-bound properties; the public
			// view only needs the safe effective recipe and no lyrics or workflow.
			view["effective_recipe"] = submission.Effective
		}
	}
	if len(job.Result) != 0 {
		view["verification"] = job.Result
	}
	return view
}

func localYuEDecodedLimit(seconds float64) int64 {
	// Decode at the maximum contract format, plus the same container/frame
	// tolerance accepted by ValidateDuration.  Saturate rather than overflow.
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 1
	}
	frames := math.Ceil((seconds + 1) * float64(localYuEMaxSampleRate))
	bytes := frames * float64(localYuEMaxChannels) * float64(localYuEBytesPerSample)
	if bytes >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(bytes)
}

func yueTerminal(view map[string]any) bool {
	state, _ := view["state"].(string)
	return state == "completed" || state == "failed" || state == "cancelled" || state == "artifact_unavailable"
}

// Cancellation is a control operation, not a read grant.  It remains usable
// after the source is forgotten so that a delayed generation can be stopped,
// but its response never leaks the recipe, provider receipt or output ID.
func yueCancelView(view map[string]any) map[string]any {
	return map[string]any{"job_id": view["job_id"], "state": view["state"]}
}
