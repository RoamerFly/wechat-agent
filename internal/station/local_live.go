package station

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/websession"
)

// localLiveUnavailable is intentionally useful on a production station with
// no provider or encoder adapter: the page receives an explicit capability
// state rather than an empty, unexplained control panel.
const localLiveUnavailable = "当前没有已验证的直播应用和本机输出，暂不能开始直播"

type localLiveInput struct {
	Action string `json:"action"`
	ID     string `json:"id"`
	apphost.LiveStart
}

// liveController creates a controller only for the prepared local owner. The
// owner identity comes from the station record, never a browser supplied
// account field or a connected application account.
func (h *localChatService) liveController() (*apphost.LiveController, string, error) {
	if h == nil || h.stopping.Load() {
		return nil, "", apphost.ErrLiveUnavailable
	}
	identity, err := h.home.identity.Read()
	if err != nil || identity.LocalOwner == nil {
		return nil, "", apphost.ErrLiveUnavailable
	}
	if h.projectTools == nil {
		return nil, "", apphost.ErrLiveUnavailable
	}
	host, err := h.projectTools.forFish(identity.FishID)
	if err != nil {
		return nil, "", apphost.ErrLiveUnavailable
	}
	h.mu.Lock()
	if h.closed || h.stopping.Load() {
		h.mu.Unlock()
		return nil, "", apphost.ErrLiveUnavailable
	}
	if h.live != nil {
		if h.liveFish != identity.FishID {
			h.mu.Unlock()
			return nil, "", apphost.ErrLiveUnavailable
		}
		live := h.live
		h.mu.Unlock()
		return live, identity.LocalOwner.ID, nil
	}
	live := apphost.NewLiveControllerWithSource(h.applications, host.store, h.liveOutput, h.liveSource)
	done := make(chan struct{})
	h.live, h.liveFish = live, identity.FishID
	h.liveRunDone = done
	h.mu.Unlock()
	go func() {
		defer close(done)
		live.Run(h.ctx)
	}()
	return live, identity.LocalOwner.ID, nil
}

func (h *localChatService) startLiveRecovery() {
	if h == nil || h.stopping.Load() {
		return
	}
	// An uninitialized new station has no owner ledger to recover. Do not
	// create identity or project data just because the desktop opened.
	_, _, _ = h.liveController()
}

func (h *localChatService) liveRoutes(s *websession.Store, mux *http.ServeMux) {
	mux.HandleFunc("GET /api/local/live", s.Protect(h.liveView))
	mux.HandleFunc("POST /api/local/live", s.Protect(h.liveAction))
}

func (h *localChatService) liveView(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	live, owner, err := h.liveController()
	if err != nil {
		websession.Respond(w, http.StatusOK, map[string]any{"targets": []apphost.LiveTarget{}, "active": nil, "available": false, "encoder": h.liveEncoder, "reason": localLiveUnavailable})
		return
	}
	var active any
	if view, activeErr := live.Active(r.Context(), owner); activeErr == nil {
		active = view
	} else if !errors.Is(activeErr, apphost.ErrNotFound) {
		status, message := localLiveFailure(activeErr)
		websession.Fail(w, status, message)
		return
	}
	targets := live.Targets(r.Context())
	websession.Respond(w, http.StatusOK, map[string]any{"targets": targets, "active": active, "available": len(targets) > 0, "encoder": h.liveEncoder, "reason": liveAvailabilityReason(targets, h.liveEncoder)})
}

func (h *localChatService) liveAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in localLiveInput
	if !websession.Decode(w, r, &in) {
		return
	}
	in.Action = strings.TrimSpace(in.Action)
	live, owner, err := h.liveController()
	if err != nil {
		status, message := localLiveFailure(err)
		websession.Fail(w, status, message)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	var out apphost.LiveView
	switch in.Action {
	case "start":
		if in.ID != "" {
			websession.Fail(w, http.StatusBadRequest, "开始直播时请提供目标、时长和本次请求编号")
			return
		}
		if err = h.checkLiveProgramStart(ctx, in.LiveStart); err == nil {
			out, err = live.Start(ctx, owner, in.LiveStart)
		}
	case "stop":
		if in.ID == "" || in.AppID != "" || in.ConnectionID != "" || in.RequestKey != "" || in.DurationSeconds != 0 || in.ProgramArtifactID != "" || in.ProgramSHA256 != "" || in.ProgramLoop {
			websession.Fail(w, http.StatusBadRequest, "停止直播时请只提供原直播会话编号")
			return
		}
		out, err = live.Stop(ctx, owner, in.ID)
	case "status", "reconcile":
		if in.ID == "" || in.AppID != "" || in.ConnectionID != "" || in.RequestKey != "" || in.DurationSeconds != 0 || in.ProgramArtifactID != "" || in.ProgramSHA256 != "" || in.ProgramLoop {
			websession.Fail(w, http.StatusBadRequest, "查询直播时请只提供原直播会话编号")
			return
		}
		if in.Action == "reconcile" {
			out, err = live.Reconcile(ctx, owner, in.ID)
		} else {
			out, err = live.Status(ctx, owner, in.ID)
		}
	default:
		websession.Fail(w, http.StatusBadRequest, "请选择开始、停止或查询直播")
		return
	}
	if err != nil {
		status, message := localLiveFailure(err)
		websession.Fail(w, status, message)
		return
	}
	websession.Respond(w, http.StatusOK, out)
}

func liveAvailabilityReason(targets []apphost.LiveTarget, encoder string) string {
	if len(targets) != 0 {
		return "可选择已验证的直播目标"
	}
	switch encoder {
	case "ready":
		return "专用 OBS 已通过安装校验，等待获准的直播应用"
	case "invalid":
		return "专用 OBS 安装校验失败，请检查本机安装"
	default:
		return localLiveUnavailable
	}
}

func localLiveFailure(err error) (int, string) {
	switch {
	case errors.Is(err, errLiveProgramSource):
		return http.StatusForbidden, errLiveProgramSource.Error()
	case errors.Is(err, errLiveProgramFormat):
		return http.StatusBadRequest, errLiveProgramFormat.Error()
	case errors.Is(err, errLiveProgramProbe):
		return http.StatusServiceUnavailable, errLiveProgramProbe.Error()
	case errors.Is(err, apphost.ErrLiveInput):
		return http.StatusBadRequest, "直播请求编号、目标或时长无效"
	case errors.Is(err, apphost.ErrLiveDenied):
		return http.StatusForbidden, "目标应用未授予本次直播时段权限"
	case errors.Is(err, apphost.ErrNotFound):
		return http.StatusNotFound, "没有找到原直播会话"
	case errors.Is(err, apphost.ErrLiveUnavailable):
		return http.StatusServiceUnavailable, localLiveUnavailable
	case errors.Is(err, apphost.ErrLiveConflict):
		return http.StatusConflict, "直播会话状态已变化，请查询原会话后续办"
	default:
		return http.StatusConflict, "直播会话暂不可用，原记录保留，请稍后查询"
	}
}
