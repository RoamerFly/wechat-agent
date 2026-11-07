package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/workspacefs"
)

func readRVCReference(ctx context.Context, bound *localProjectArtifactInput) ([]byte, localRVCWAV, error) {
	var empty localRVCWAV
	artifact := bound.artifact
	if artifact.Bytes > localRVCInputLimit || !strings.EqualFold(filepath.Ext(artifact.Name), ".wav") {
		return nil, empty, errors.New("RVC 输入须为不超过 25 MiB 的已登记 WAV，可先用音频工坊截取")
	}
	file, err := bound.store.open(ctx, artifact, func() bool {
		return ctx.Err() == nil && bound.access.current(bound.grant)
	})
	if err != nil {
		return nil, empty, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, localRVCInputLimit+1))
	sum := sha256.Sum256(data)
	if err != nil || int64(len(data)) != artifact.Bytes || hex.EncodeToString(sum[:]) != artifact.SHA256 || bound.check(ctx) != nil {
		return nil, empty, errors.New("RVC 原件的内容或授权已变化")
	}
	evidence, err := inspectRVCWAV(data)
	return data, evidence, err
}

func (e localRVCInvocation) jobAction(ctx context.Context, raw json.RawMessage) (string, error) {
	var in struct {
		ID     string `json:"job_id"`
		Action string `json:"action"`
	}
	if strictLocalMemoryDecode(raw, &in) != nil || !validLocalRVCHex(in.ID, 64) ||
		(in.Action != "status" && in.Action != "cancel" && in.Action != "resume") {
		return "", errors.New("需要原 RVC 任务编号及 status、cancel 或 resume")
	}
	job, args, bound, err := e.readJob(ctx, in.ID, in.Action == "cancel")
	if err != nil {
		return "", err
	}
	if job.Recovery() == "terminal" {
		return localProcessJSON(e.finishedView(ctx, job))
	}
	if in.Action == "cancel" && !job.CancelRequested {
		job, err = e.host.store.RequestJobCancel(ctx, job.ID, job.Revision)
		if err != nil {
			return "", err
		}
	}
	if job.Recovery() == "terminal" || job.State == "pending" {
		return localProcessJSON(e.finishedView(ctx, job))
	}
	remote, found, err := e.lookup(ctx, job, args)
	if err != nil {
		return "", err
	}
	if !found {
		view := rvcJobView(job)
		view["state"] = "needs_review"
		return localProcessJSON(view)
	}
	if bound != nil {
		if err := e.checkSource(ctx, args, bound); err != nil {
			return "", err
		}
	} else if !job.CancelRequested {
		// A terminal job may be read above. Only a pending cancellation
		// can reconcile a live job without reopening its original audio.
		return "", errors.New("原 RVC 任务的素材授权不可用")
	}
	if job.RemoteID == "" {
		job, err = e.host.store.AcknowledgeJob(ctx, job.ID, job.Revision, remote.ID)
		if err != nil {
			return "", err
		}
	}
	if job.CancelRequested {
		return e.confirmCancellation(ctx, job, args, remote)
	}
	switch in.Action {
	case "resume":
		if remote.State == "failed" || remote.State == "needs_review" {
			return e.resume(ctx, job, args, bound)
		}
	default:
		if remote.State == "completed" {
			return e.publish(ctx, job, args, bound, remote)
		}
	}
	return localProcessJSON(rvcProgress(job, remote.State))
}

func (e localRVCInvocation) readJob(ctx context.Context, id string, cancelOnly bool) (pluginhost.Job, localRVCInput, *localProjectArtifactInput, error) {
	var args localRVCInput
	g, t := e.grant, e.tool
	job, err := e.host.store.Job(ctx, id)
	if err != nil || job.Plugin != localRVCPackage || job.Tool != localRVCCapability || job.Context.Account != g.FishID ||
		job.Context.Thread != g.OwnerID || job.Context.Requester != g.OwnerID ||
		job.Context.Authorization != localCheckpointKey(g) || job.PackageDigest != e.installed.Digest ||
		job.ConfigRevision != e.configRevision || json.Unmarshal(job.Arguments, &args) != nil ||
		args.Origin == nil || !args.Pin.valid() || job.Context.SourceVersions[args.Origin.EventID] != args.Origin.Revision {
		return job, args, nil, errors.New("原 RVC 任务不可用或授权已变化")
	}
	if args.Binding != e.deployment.Binding {
		if e.existingOnly {
			return job, args, nil, errLocalRVCFollowupDeployment
		}
		return job, args, nil, errors.New("原 RVC 任务不可用或授权已变化")
	}
	if job.State != "completed" && (cancelOnly || job.CancelRequested) {
		return job, args, nil, nil
	}
	artifact, err := t.access.home.artifacts.bindProjectArtifact(ctx, args.Artifact.ID, t.access, g)
	if err != nil {
		return job, args, nil, err
	}
	if err := e.checkSource(ctx, args, artifact); err != nil {
		return job, args, nil, err
	}
	return job, args, artifact, nil
}

func (e localRVCInvocation) lookup(ctx context.Context, job pluginhost.Job, args localRVCInput) (localRVCRemoteJob, bool, error) {
	var remote localRVCRemoteJob
	route := "/rvc-requests/" + job.Context.RequestKey
	if job.RemoteID != "" {
		if !validLocalRVCHex(job.RemoteID, 32) {
			return remote, false, errors.New("原 RVC 任务回执无效")
		}
		route = "/jobs/" + job.RemoteID
	}
	data, code, err := e.request(ctx, http.MethodGet, route, "", nil)
	if err != nil && e.existingOnly {
		return remote, false, err
	}
	if err != nil || code == http.StatusNotFound {
		return remote, false, nil
	}
	if code != http.StatusOK || json.Unmarshal(data, &remote) != nil || !remote.matches(args) ||
		(job.RemoteID != "" && remote.ID != job.RemoteID) {
		return remote, false, errors.New("RVC 回执与原声线或素材不一致，未登记成品")
	}
	return remote, true, nil
}

func (e localRVCInvocation) action(ctx context.Context, job pluginhost.Job, args localRVCInput, action string) (localRVCRemoteJob, error) {
	var remote localRVCRemoteJob
	data, code, err := e.request(ctx, http.MethodPost, "/jobs/"+job.RemoteID+"/"+action, "", struct{}{})
	if err != nil || code != http.StatusOK || json.Unmarshal(data, &remote) != nil || !remote.matches(args) || remote.ID != job.RemoteID {
		return remote, errors.New("RVC 原任务操作尚未确认")
	}
	return remote, nil
}

func (e localRVCInvocation) confirmCancellation(ctx context.Context, job pluginhost.Job, args localRVCInput, remote localRVCRemoteJob) (string, error) {
	switch remote.State {
	case "cancelled", "completed":
	case "cancelling":
		return localProcessJSON(rvcProgress(job, "cancelling"))
	default:
		result, err := e.action(ctx, job, args, "cancel")
		if err != nil {
			return localProcessJSON(rvcProgress(job, "cancelling"))
		}
		remote = result
	}
	if remote.State == "cancelled" || remote.State == "completed" {
		next, err := e.host.store.FinishJob(ctx, job.ID, job.Revision, "cancelled", nil)
		if err != nil {
			return "", err
		}
		return localProcessJSON(rvcProgress(next, remote.State))
	}
	return localProcessJSON(rvcProgress(job, "cancelling"))
}

func (e localRVCInvocation) resume(ctx context.Context, job pluginhost.Job, args localRVCInput, bound *localProjectArtifactInput) (string, error) {
	pin, err := e.health(ctx, args.VoiceID)
	if err != nil || pin != args.Pin {
		return "", errors.New("原声线版本已变化，无法续办此 RVC 任务")
	}
	if err = e.checkSource(ctx, args, bound); err != nil {
		return "", err
	}
	op := companion.LocalToolCheckpoint{Tool: localRVCJobTool, Grant: localCheckpointKey(e.grant), Target: job.ID,
		Key: localCheckpointKey(localRVCJobTool, e.tool.traceID, job.ID, "resume")}
	return e.tool.dispatchCheckpointed(ctx, op, e.grant, func() (string, string, error) {
		result, err := e.action(ctx, job, args, "resume")
		if err != nil {
			view, marshalErr := localProcessJSON(rvcProgress(job, "needs_review"))
			return view, "registered", marshalErr
		}
		if err := e.checkSource(ctx, args, bound); err != nil {
			return "", "", err
		}
		view, err := localProcessJSON(rvcProgress(job, result.State))
		return view, "registered", err
	})
}

func rvcProgress(job pluginhost.Job, backendState string) map[string]any {
	view := rvcJobView(job)
	switch backendState {
	case "queued", "running", "needs_review", "failed", "completed", "cancelled", "cancelling":
		view["backend_state"] = backendState
	}
	if !job.CancelRequested && job.Recovery() != "terminal" && (backendState == "failed" || backendState == "needs_review") {
		view["state"], view["resumable"] = backendState, true
	}
	return view
}

func (e localRVCInvocation) finishedView(ctx context.Context, job pluginhost.Job) map[string]any {
	view := rvcJobView(job)
	if job.State == "completed" && len(job.Artifacts) == 1 {
		artifactStore := e.tool.access.home.artifacts
		if artifact, err := artifactStore.read(job.Artifacts[0]); err == nil && artifact.Request == job.ID && artifact.Fish == e.grant.FishID && artifact.Owner == e.grant.OwnerID {
			file, err := artifactStore.open(ctx, artifact, func() bool {
				return ctx.Err() == nil && e.tool.access.current(e.grant)
			})
			if err == nil {
				file.Close()
				view["artifact"] = artifact.view()
				view["artifact_id"] = artifact.ID
				return view
			}
		}
		view["state"] = "artifact_unavailable"
	}
	return view
}

func (e localRVCInvocation) publish(ctx context.Context, job pluginhost.Job, args localRVCInput, bound *localProjectArtifactInput, remote localRVCRemoteJob) (string, error) {
	if remote.Label != args.Pin.Label || remote.ModelHash != args.Pin.ModelHash || remote.IndexHash != args.Pin.IndexHash ||
		remote.Device != args.Pin.Device || (remote.ActualDevice != "cpu" && remote.ActualDevice != "cuda:0") ||
		(args.Pin.Device != "auto" && remote.ActualDevice != args.Pin.Device) ||
		!validLocalRVCHex(remote.Hash, 64) || remote.Bytes < 44 || remote.Bytes > 128<<20 {
		return "", errors.New("RVC 成品的声线、设备或摘要与原任务不符")
	}
	data, code, err := e.request(ctx, http.MethodGet, "/jobs/"+job.RemoteID+"/audio", "", nil)
	if err != nil || code != http.StatusOK {
		return "", errors.New("RVC 成品尚未取得")
	}
	sum := sha256.Sum256(data)
	wav, err := inspectRVCWAV(data)
	if err != nil || int64(len(data)) != remote.Bytes || hex.EncodeToString(sum[:]) != remote.Hash ||
		wav.Rate != remote.SampleRate || wav.Frames != remote.Frames || wav.Channels != remote.Channels || remote.Bits != 16 ||
		math.Abs(wav.seconds()-remote.Duration) > 1e-6 || math.Abs(wav.seconds()-args.Seconds) > .25 {
		return "", errors.New("RVC 成品的 WAV 字节、摘要或时长与回执不符")
	}
	if err := e.checkSource(ctx, args, bound); err != nil {
		return "", err
	}
	artifact, err := e.publishBytes(ctx, job, args, remote.Hash, data)
	if err != nil {
		return "", err
	}
	if err := e.checkSource(ctx, args, bound); err != nil {
		return "", err
	}
	receipt, _ := json.Marshal(map[string]any{"voice_profile_id": args.VoiceID, "sha256": remote.Hash, "device": remote.ActualDevice,
		"duration_seconds": wav.seconds(), "timbre_listening_verified": false})
	next, err := e.host.store.CompleteJob(ctx, job.ID, job.Revision, []string{artifact.ID}, receipt)
	if err != nil {
		return "", err
	}
	view := rvcJobView(next)
	view["artifact"], view["artifact_id"], view["verification"] = artifact.view(), artifact.ID, json.RawMessage(receipt)
	return localProcessJSON(view)
}

func (e localRVCInvocation) publishBytes(ctx context.Context, job pluginhost.Job, args localRVCInput, digest string, data []byte) (localArtifact, error) {
	dir := filepath.Join(e.tool.projectTools.home.dataDir, "rvc-output-stage", job.ID)
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return localArtifact{}, err
	}
	const name = "rvc-result.wav"
	defer func() {
		_ = root.Remove(name)
		_ = root.Close()
		_ = os.Remove(dir)
	}()
	// This fixed, job-owned scratch file may remain after a host crash. It is
	// only a transfer copy; the immutable artifact below has the stable ID.
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return localArtifact{}, err
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return localArtifact{}, err
	}
	n, err := file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return localArtifact{}, err
	}
	input := localArtifactInput{Key: job.ID, Expected: digest, parent: &args.Artifact}
	return e.tool.access.home.artifacts.publishFile(ctx, input, args.Origin, e.tool.access, e.grant, filepath.Join(dir, name), nil)
}
