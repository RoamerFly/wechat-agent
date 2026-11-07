package station

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/mcptools"
)

const localLiveToolName = "app_live"

func localLiveTool() mcptools.Tool {
	return mcptools.NewTool(localLiveToolName, "管理已授权应用的限时直播会话。先用targets核对available、获准目标和active原会话；响应中断或重启后从active恢复原id。start提供app_id、connection_id和duration_seconds；播放已登记MP4时同时提供program_artifact_id和program_sha256，program_loop控制循环，省略素材沿用测试色板。宿主绑定本轮来源、素材版本并持久去重。status只读原会话；reconcile核对原平台状态，stop停止原会话与本机输出，均只提供原id。unknown需核对原id，不可重复开始。平台与本机输出分别返回。",
		`{"type":"object","properties":{"action":{"type":"string","enum":["targets","start","status","reconcile","stop"]},"app_id":{"type":"string"},"connection_id":{"type":"string"},"duration_seconds":{"type":"integer","minimum":1},"program_artifact_id":{"type":"string"},"program_sha256":{"type":"string","minLength":64,"maxLength":64},"program_loop":{"type":"boolean"},"id":{"type":"string","format":"uuid"}},"required":["action"],"additionalProperties":false}`)
}

type localLiveToolInput struct {
	Action            string `json:"action"`
	AppID             string `json:"app_id"`
	ConnectionID      string `json:"connection_id"`
	DurationSeconds   int64  `json:"duration_seconds"`
	ID                string `json:"id"`
	ProgramArtifactID string `json:"program_artifact_id"`
	ProgramSHA256     string `json:"program_sha256"`
	ProgramLoop       bool   `json:"program_loop"`
}

// Never forward a provider error body: it may contain private ingest routing.
type localLiveToolError struct {
	status  int
	message string
}

func (e *localLiveToolError) Error() string { return e.message }
func (e *localLiveToolError) SafeToolFailure() (string, string) {
	return "app_live_" + strconv.Itoa(e.status), e.message
}

func (t *localToolSet) callLive(ctx context.Context, raw json.RawMessage) (value string, failure error) {
	stage := "请求校验"
	defer func() {
		if failure != nil {
			status, message := localLiveFailure(failure)
			if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
				status, message = 409, "本次操作已取消或超时；已开始的会话需按原编号核对或停止"
			}
			failure = &localLiveToolError{status: status, message: stage + "：" + message}
		}
	}()
	if t.publications == nil || t.run == nil || t.access == nil || t.store == nil {
		return "", apphost.ErrLiveDenied
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := t.run.CheckSource(ctx); err != nil {
		return "", apphost.ErrLiveDenied
	}
	var in localLiveToolInput
	if strictLocalMemoryDecode(raw, &in) != nil {
		return "", apphost.ErrLiveInput
	}
	// Validate field presence as well as values: an old session operation must
	// never carry even an empty redirection field.
	allowed := map[string]bool{"action": true}
	switch in.Action {
	case "targets":
		stage = "目标查询"
	case "start":
		stage = "开始直播"
		allowed["app_id"], allowed["connection_id"], allowed["duration_seconds"] = true, true, true
		allowed["program_artifact_id"], allowed["program_sha256"], allowed["program_loop"] = true, true, true
	case "status", "reconcile", "stop":
		stage = map[string]string{"status": "读取原会话", "reconcile": "核对原会话", "stop": "停止原会话"}[in.Action]
		allowed["id"] = true
	default:
		return "", apphost.ErrLiveInput
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return "", apphost.ErrLiveInput
	}
	for field := range fields {
		if !allowed[field] {
			return "", apphost.ErrLiveInput
		}
	}
	// Derive the actor exclusively from the current station identity and require
	// an exact caller match, including for queries and emergency stop.
	h := t.publications
	identity, err := h.home.identity.Read()
	if err != nil || identity.LocalOwner == nil || !fishidentity.ValidFishID(identity.FishID) ||
		!fishidentity.ValidLocalOwner(*identity.LocalOwner) || t.access.home != h.home ||
		t.access.fishID != identity.FishID || t.access.ownerID != identity.LocalOwner.ID {
		return "", apphost.ErrLiveDenied
	}
	source, ok := t.artifactSource(localWorkspaceGrant{FishID: identity.FishID, OwnerID: identity.LocalOwner.ID})
	if !ok || source == nil || source.EventID == "" || source.Revision < 1 {
		return "", apphost.ErrLiveDenied
	}
	live, owner, err := h.liveController()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	var result any
	switch in.Action {
	case "targets":
		if in.ID != "" || in.AppID != "" || in.ConnectionID != "" || in.DurationSeconds != 0 {
			return "", apphost.ErrLiveInput
		}
		targets := live.Targets(ctx)
		var active any
		if view, activeErr := live.Active(ctx, owner); activeErr == nil {
			active = view
		} else if !errors.Is(activeErr, apphost.ErrNotFound) {
			return "", activeErr
		}
		result = map[string]any{"targets": targets, "active": active, "available": len(targets) > 0, "reason": liveAvailabilityReason(targets, h.liveEncoder)}
	case "start":
		if t.inspectOnly {
			return "", apphost.ErrLiveConflict
		}
		if in.ID != "" || in.AppID == "" || in.ConnectionID == "" || in.DurationSeconds < 1 {
			return "", apphost.ErrLiveInput
		}
		if err := t.run.CheckSource(ctx); err != nil {
			return "", apphost.ErrLiveDenied
		}
		start := apphost.LiveStart{AppID: in.AppID, ConnectionID: in.ConnectionID, DurationSeconds: in.DurationSeconds,
			ProgramArtifactID: in.ProgramArtifactID, ProgramSHA256: in.ProgramSHA256, ProgramLoop: in.ProgramLoop}
		if err := h.checkLiveProgramStart(ctx, start); err != nil {
			return "", err
		}
		// Preserve the previous no-program key so interrupted old requests keep
		// their original session rather than becoming new broadcasts on upgrade.
		start.RequestKey = localCheckpointKey(localLiveToolName, source.EventID, source.Revision, identity.FishID, owner, in.AppID, in.ConnectionID, in.DurationSeconds)
		if in.ProgramArtifactID != "" || in.ProgramSHA256 != "" || in.ProgramLoop {
			start.RequestKey = localCheckpointKey(start.RequestKey, in.ProgramArtifactID, in.ProgramSHA256, in.ProgramLoop)
		}
		result, err = live.Start(ctx, owner, start)
	case "status", "reconcile", "stop":
		id, parseErr := uuid.Parse(in.ID)
		if parseErr != nil || id == uuid.Nil || id.String() != in.ID || in.AppID != "" || in.ConnectionID != "" || in.DurationSeconds != 0 {
			return "", apphost.ErrLiveInput
		}
		switch in.Action {
		case "status":
			result, err = live.Status(ctx, owner, in.ID)
		case "reconcile":
			result, err = live.Reconcile(ctx, owner, in.ID)
		case "stop":
			result, err = live.Stop(ctx, owner, in.ID)
		}
	default:
		return "", apphost.ErrLiveInput
	}
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := t.run.CheckSource(ctx); err != nil {
		return "", apphost.ErrLiveDenied
	}
	return localProcessJSON(result)
}
