package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

const localProcessTool = "server_windows_process"
const localArtifactPublishTool = "artifact_publish"

// Hash only typed, normalized arguments. No command or file content is copied
// into the journal. The key is also usable as a Jobs lookup key after a crash.
func localCheckpointKey(values ...any) string {
	data, _ := json.Marshal(values)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (t *localToolSet) writeCheckpointed(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var in localWorkspaceFileInput
	if err := strictLocalMemoryDecode(raw, &in); err != nil {
		return "", errors.New("本机家目录文件参数无效")
	}
	grant, err := t.access.resolve()
	if err != nil {
		return "", err
	}
	in.Path, err = localWorkspacePath(grant, in.Path)
	if err != nil {
		return "", err
	}
	target, _ := filepath.Rel(grant.Directory, in.Path)
	// Ignore fields that do not affect the write/edit.
	var key string
	if name == "server_write" {
		key = localCheckpointKey(name, in.Path, in.Content)
	} else {
		key = localCheckpointKey(name, in.Path, in.Expected, in.Old, in.New)
	}
	op := companion.LocalToolCheckpoint{Key: key, Tool: name, Grant: localCheckpointKey(grant), Target: target}
	return t.dispatchCheckpointed(ctx, op, grant, func() (string, string, error) {
		result, err := t.files.call(ctx, name, in, grant)
		return result, result, err
	})
}

func (t *localToolSet) startCheckpointed(ctx context.Context, in localProcessInput) (string, error) {
	grant, err := t.access.resolve()
	if err != nil {
		return "", err
	}
	scope := localProcessScope(grant.FishID, grant.OwnerID)
	op := companion.LocalToolCheckpoint{
		Key:  localCheckpointKey(localProcessTool, scope, t.traceID, in.Path, in.Argv, in.Seconds),
		Tool: localProcessTool, Grant: scope,
	}
	in.Key = "local-" + op.Key
	// An existing reservation is already an uncertain operation. Preserve the
	// recovery path before validating current runtime configuration; otherwise
	// a restarted host with a missing runner could hide the durable process.
	for _, previous := range t.run.Operations {
		if previous.Key == op.Key {
			return t.dispatchCheckpointed(ctx, op, grant, func() (string, string, error) {
				return "", "", errors.New("已存在的进程操作不应再次启动")
			})
		}
	}
	// Validate before Reserve.  A malformed start is a repairable model
	// argument error, not an uncertain side effect, so it must not put the
	// remainder of this turn into inspect-only recovery.
	cfg, known, err := t.process.preflight(in, grant)
	if err != nil {
		return "", err
	}
	if known != "" {
		return known, nil
	}
	return t.dispatchCheckpointed(ctx, op, grant, func() (string, string, error) {
		result, err := t.process.startPrepared(ctx, in, grant, cfg)
		if err != nil {
			return "", "", err
		}
		var receipt struct{ ID string }
		if err := json.Unmarshal([]byte(result), &receipt); err != nil {
			return "", "", err
		}
		// The registry already owns the process ID, state and output. Only a
		// known not-started response is kept in the request journal.
		if receipt.ID != "" {
			return result, "registered", nil
		}
		return result, result, nil
	})
}

func (t *localToolSet) artifactSource(grant localWorkspaceGrant) (*companion.SourceRef, bool) {
	return t.store.SourceForInput(companion.Input{Channel: "local", Account: grant.FishID, Thread: grant.OwnerID, Sender: grant.OwnerID, TraceID: t.traceID})
}

func (t *localToolSet) publishCheckpointed(ctx context.Context, raw json.RawMessage) (string, error) {
	var in struct {
		Path     string `json:"path"`
		Expected string `json:"expected_sha256"`
	}
	if t.run == nil || t.store == nil || t.access.home.artifacts == nil {
		return "", errors.New("本轮没有成品登记授权")
	}
	if strictLocalMemoryDecode(raw, &in) != nil {
		return "", mcptools.ArgumentError("$", "请提供 path 和 expected_sha256")
	}
	if in.Expected != "" && !artifactID("sha-"+in.Expected, "sha-") {
		return "", mcptools.ArgumentError("$.expected_sha256", "请提供文件的 64 位小写 SHA-256")
	}
	grant, err := t.access.resolve()
	if err != nil {
		return "", err
	}
	path, err := localWorkspacePath(grant, in.Path)
	if err != nil {
		return "", mcptools.ArgumentError("$.path", "请提供家目录内的成品文件路径")
	}
	source, ok := t.artifactSource(grant)
	if !ok || t.run.CheckSource(ctx) != nil {
		return "", errors.New("本轮来源已变化，无法登记成品")
	}
	target, _ := filepath.Rel(grant.Directory, path)
	path = filepath.Join(grant.Directory, target)
	op := companion.LocalToolCheckpoint{Key: localCheckpointKey(localArtifactPublishTool, path, in.Expected),
		Tool: localArtifactPublishTool, Grant: localCheckpointKey(grant), Target: target}
	return t.dispatchCheckpointed(ctx, op, grant, func() (string, string, error) {
		artifact, err := t.access.home.artifacts.publishFile(ctx, localArtifactInput{Expected: in.Expected, Key: t.traceID}, source, t.access, grant, path, nil)
		if errors.Is(err, errLocalArtifactDigestMismatch) {
			result, encodeErr := localProcessJSON(map[string]any{"state": "not_published", "reason": errLocalArtifactDigestMismatch.Error()})
			return result, "hash_mismatch", encodeErr
		}
		if err != nil {
			return "", "", err
		}
		result, err := localProcessJSON(artifact.view())
		return result, "registered", err
	})
}

func (t *localToolSet) artifactCheckpointView(op companion.LocalToolCheckpoint, grant localWorkspaceGrant) map[string]any {
	view := map[string]any{"tool": op.Tool, "state": "needs_review", "recovered": true}
	if t.run == nil || t.store == nil || t.access.home.artifacts == nil {
		return view
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path, err := localWorkspacePath(grant, op.Target)
	if err != nil || t.run.CheckSource(ctx) != nil || !t.access.current(grant) {
		return view
	}
	if op.Result == "hash_mismatch" {
		view["state"], view["reason"] = "not_published", errLocalArtifactDigestMismatch.Error()
		return view
	}
	source, ok := t.artifactSource(grant)
	if !ok {
		return view
	}
	identity, err := localArtifactIdentity(localArtifactInput{Key: t.traceID}, source, grant, path)
	if err != nil {
		return view
	}
	store := t.access.home.artifacts
	artifact, err := store.read(identity.ID)
	if err != nil || (localCheckpointKey(op.Tool, path, artifact.SHA256) != op.Key && localCheckpointKey(op.Tool, path, "") != op.Key) {
		return view
	}
	file, err := store.open(ctx, artifact, func() bool { return t.access.current(grant) && store.artifactLineageValid(artifact) })
	if err != nil {
		return view
	}
	_ = file.Close()
	view = artifact.view()
	view["tool"], view["state"], view["recovered"] = op.Tool, "recorded", true
	return view
}

func (t *localToolSet) dispatchCheckpointed(ctx context.Context, op companion.LocalToolCheckpoint, grant localWorkspaceGrant, call func() (result, checkpoint string, err error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	for _, previous := range t.run.Operations {
		if previous.Key == op.Key {
			if err := t.run.CheckSource(ctx); err != nil {
				return "", errors.New("原请求来源已变化，不能返回工具结果")
			}
			return localProcessJSON(t.checkpointView(previous, grant))
		}
	}
	if t.inspectOnly {
		return "", errors.New("续办只能核对已有操作或取消原进程；新增写入和启动需要主人发起新请求")
	}
	if err := t.run.Reserve(ctx, op); err != nil {
		return "", errors.New("操作检查点未能保存，未执行")
	}
	t.run.Operations = append(t.run.Operations, op)
	result, checkpoint, err := call()
	if err != nil {
		t.inspectOnly = true
		return "", errors.Join(err, errors.New("已登记操作，但结果未核实。请先读取文件或查原进程，勿重做"))
	}
	// A tool may have completed while the model request was being cancelled.
	// Give its outcome a bounded chance to commit before reporting the result.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := t.run.RecordResult(saveCtx, op.Key, checkpoint); err != nil {
		t.inspectOnly = true
		return "", errors.New("操作已登记，但结果未能保存。请先核对，勿重做")
	}
	t.run.Operations[len(t.run.Operations)-1].Result = checkpoint
	return result, nil
}

func (t *localToolSet) checkpointView(op companion.LocalToolCheckpoint, grant localWorkspaceGrant) map[string]any {
	view := map[string]any{"tool": op.Tool, "state": "needs_review", "recovered": true}
	if op.Tool == localProcessTool {
		if t.process == nil {
			return view
		}
		scope, err := t.process.ownerScope()
		if err != nil || scope != op.Grant {
			return view
		}
		if p, ok := t.process.jobs.FindByKey(scope, "local-"+op.Key); ok {
			view["id"], view["state"], view["reason"] = p.ID, p.State, p.Reason
			return view
		}
		if op.Result != "" && op.Result != "registered" {
			view["state"], view["previous_result"] = "not_started", json.RawMessage(op.Result)
		}
		return view
	}
	if grant.Directory == "" || op.Grant != localCheckpointKey(grant) {
		view["state"] = "grant_changed"
		return view
	}
	if op.Tool == localArtifactPublishTool {
		return t.artifactCheckpointView(op, grant)
	}
	if op.Tool == localRVCStartTool || op.Tool == localRVCJobTool {
		return t.rvcCheckpointView(op, grant)
	}
	if op.Tool == localComfyStartTool {
		return t.comfyCheckpointView(op, grant)
	}
	if op.Tool == localYuEStartTool {
		return t.yueCheckpointView(op, grant)
	}
	if op.Tool == localTTSStartTool {
		return t.ttsCheckpointView(op, grant)
	}
	if projectOperation(op.Tool).capability != "" {
		return t.projectCheckpointView(op, grant)
	}
	view["path"] = op.Target
	if op.Result != "" {
		view["state"], view["previous_result"] = "recorded", json.RawMessage(op.Result)
	}
	return view
}
