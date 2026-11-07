package station

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/pluginhost"
)

var errProjectFollowupAuthority = errors.New("项目任务原请求、工具权限或素材已变化")

func (t *localToolSet) projectFollowupJob(ctx context.Context, resultTool, jobID string) (pluginhost.Job, error) {
	var empty pluginhost.Job
	if t.projectTools == nil || t.run == nil || !t.run.Tracked || !validLocalRVCHex(jobID, 64) {
		return empty, errProjectFollowupAuthority
	}
	var pkg, capability, start string
	var permissions []string
	switch resultTool {
	case localRVCJobTool:
		pkg, capability, start, permissions = localRVCPackage, localRVCCapability, localRVCStartTool, localRVCGrants
	case localComfyJobTool:
		pkg, capability, start, permissions = localComfyPackage, localComfyCapability, localComfyStartTool, localComfyGrants
	case localYuEJobTool:
		pkg, capability, start, permissions = localYuEPackage, localYuECapability, localYuEStartTool, localYuEGrants
	default:
		return empty, errProjectFollowupAuthority
	}
	grant, err := t.access.resolve()
	if err != nil || t.run.CheckSource(ctx) != nil {
		return empty, errProjectFollowupAuthority
	}
	source, ok := t.artifactSource(grant)
	if !ok || source == nil {
		return empty, errProjectFollowupAuthority
	}
	host, err := t.projectTools.forFish(grant.FishID)
	if err != nil {
		return empty, errProjectFollowupAuthority
	}
	job, err := host.store.Job(ctx, jobID)
	if err != nil || job.ID != jobID || job.Plugin != pkg || job.Tool != capability {
		return empty, errProjectFollowupAuthority
	}
	call := job.Context
	grantKey := localCheckpointKey(grant)
	if call.Account != grant.FishID || call.Thread != grant.OwnerID || call.Requester != grant.OwnerID ||
		call.Authorization != grantKey || call.SourceVersions[source.EventID] != source.Revision {
		return empty, errProjectFollowupAuthority
	}
	bound := false
	for _, op := range t.run.Operations {
		if op.Tool == start && op.Key == call.RequestKey && op.Grant == grantKey {
			bound = true
			break
		}
	}
	if !bound {
		return empty, errProjectFollowupAuthority
	}
	var args struct {
		Origin    *companion.SourceRef `json:"origin"`
		Trace     string               `json:"trace"`
		Artifact  *localArtifactLink   `json:"artifact"`
		Reference *localArtifactLink   `json:"reference"`
	}
	if json.Unmarshal(job.Arguments, &args) != nil || args.Trace != t.traceID || args.Origin == nil || *args.Origin != *source {
		return empty, errProjectFollowupAuthority
	}
	installed, err := installedLocalProject(ctx, host, localProjectOperation{pkg: pkg, capability: capability})
	if err != nil || installed.Digest != job.PackageDigest || installed.Package.Version != job.Version {
		return empty, errProjectFollowupAuthority
	}
	cfg, err := host.store.Configuration(ctx, pkg)
	toolGrant := pluginhost.Grant{Account: grant.FishID, Plugin: pkg, Version: job.Version, Permissions: installed.Permissions}
	if err != nil || strconv.Itoa(cfg.Revision) != job.ConfigRevision || pluginhost.Authorize(installed.Package, toolGrant, call, permissions) != nil {
		return empty, errProjectFollowupAuthority
	}
	artifacts := t.access.home.artifacts
	if pkg == localRVCPackage && args.Artifact == nil {
		return empty, errProjectFollowupAuthority
	}
	for _, link := range []*localArtifactLink{args.Artifact, args.Reference} {
		if link == nil {
			continue
		}
		a, err := artifacts.readRecord(link.ID)
		if err != nil || a.Fish != grant.FishID || a.Owner != grant.OwnerID || a.SHA256 != link.SHA256 ||
			(a.Source != nil && call.SourceVersions[a.Source.EventID] != a.Source.Revision) || !artifacts.artifactLineageValid(a) {
			return empty, errProjectFollowupAuthority
		}
	}
	return job, nil
}

// Query through the existing project-tool path. It alone validates remote
// receipts and commits verified artifacts to the job. Polling never submits a
// new job or asks a stopped backend to start again.
func (t *localToolSet) projectFollowupStatus(ctx context.Context, resultTool, jobID string) (map[string]any, bool, error) {
	job, err := t.projectFollowupJob(ctx, resultTool, jobID)
	if err != nil {
		return nil, false, err
	}
	cancelled := map[string]any{"job_id": jobID, "state": "cancelled"}
	if job.CancelRequested || job.State == "cancelled" {
		return cancelled, false, nil
	}
	raw, _ := json.Marshal(map[string]string{"job_id": jobID, "action": "status"})
	var status string
	switch resultTool {
	case localRVCJobTool:
		status, err = t.callLocalRVCMode(ctx, resultTool, raw, true)
	case localComfyJobTool:
		status, err = t.callLocalComfy(ctx, resultTool, raw)
	case localYuEJobTool:
		status, err = t.callLocalYuE(ctx, resultTool, raw)
	}
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	latest, proofErr := t.projectFollowupJob(ctx, resultTool, jobID)
	if proofErr != nil {
		return nil, false, proofErr
	}
	if latest.CancelRequested || latest.State == "cancelled" {
		return cancelled, false, nil
	}
	if errors.Is(err, errLocalRVCNotRunning) {
		return map[string]any{"job_id": jobID, "state": "needs_review", "reason": "original_backend_not_running"}, false, nil
	}
	if errors.Is(err, errLocalRVCFollowupDeployment) {
		return map[string]any{"job_id": jobID, "state": "needs_review", "reason": "deployment_changed_or_unavailable"}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var view map[string]any
	if json.Unmarshal([]byte(status), &view) != nil || view["job_id"] != jobID {
		return nil, false, errors.New("项目工具查询没有返回原任务")
	}
	switch view["state"] {
	case "cancelled", "cancelling":
		return cancelled, false, nil
	case "failed", "needs_review", "artifact_unavailable":
		return view, false, nil
	case "completed":
		if latest.State == "completed" && len(latest.Artifacts) != 0 {
			return view, false, nil
		}
		return map[string]any{"job_id": jobID, "state": "artifact_unavailable"}, false, nil
	}
	switch view["backend_state"] {
	case "queued", "running", string(comfyui.Pending), string(comfyui.Running):
		return nil, true, nil
	case "cancelled", "cancelling":
		return cancelled, false, nil
	}
	if latest.State == "pending" {
		return map[string]any{"job_id": jobID, "state": "needs_review", "reason": "backend_not_started"}, false, nil
	}
	return map[string]any{"job_id": jobID, "state": "needs_review", "reason": "project_status_unknown"}, false, nil
}
