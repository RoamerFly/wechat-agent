//go:build windows

package station

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/nativeclient"
	"wechat-bot/internal/websession"
)

const douyinLiveClientInspectRoute = "/api/local/apps/douyin/live-client/inspect"

// Inspection is a private owner action, separate from the website login and
// from LiveProvider. File metadata does not establish publisher authenticity,
// a logged-in account, capture readiness or permission to begin a broadcast.
func (h *localHomeService) douyinLiveClientRoutes(owner *websession.Store, mux *http.ServeMux, service context.Context) {
	mux.HandleFunc("POST "+douyinLiveClientInspectRoute, owner.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		identity, err := h.identity.Read()
		if err != nil || identity.LocalOwner == nil || !fishidentity.ValidFishID(identity.FishID) {
			websession.Fail(w, http.StatusConflict, "请先准备本机主人身份")
			return
		}
		var in struct {
			Executable string `json:"executable"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if !websession.Decode(w, r, &in) {
			return
		}
		if len(in.Executable) > 4096 || strings.TrimSpace(in.Executable) == "" {
			websession.Fail(w, http.StatusBadRequest, "请选择已安装的直播伴侣主程序 EXE")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		stop := context.AfterFunc(service, cancel)
		defer stop()
		info, err := nativeclient.Inspect(ctx, in.Executable)
		switch {
		case err == nil:
			websession.Respond(w, http.StatusOK, map[string]any{"state": "inspected", "file": info,
				"control_available": false, "reason": "已读取程序版本及指纹；发布者、客户端登录和直播控制仍待核对"})
		case errors.Is(err, nativeclient.ErrVersion):
			websession.Respond(w, http.StatusOK, map[string]any{"state": "metadata_missing", "file": info,
				"control_available": false, "reason": "这是可读取的 Windows 程序，但版本资料不完整，请确认选中了直播伴侣主程序"})
		case errors.Is(err, nativeclient.ErrInput):
			websession.Fail(w, http.StatusBadRequest, "程序路径无效，请选择本机已安装的 EXE 文件")
		case errors.Is(err, nativeclient.ErrInvalid):
			websession.Fail(w, http.StatusUnprocessableEntity, "该文件不是可核对的 Windows x64 主程序，请勿选择安装包、快捷方式或 DLL")
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			websession.Fail(w, http.StatusRequestTimeout, "程序检查未完成，可在文件可用后重新检查")
		default:
			websession.Fail(w, http.StatusServiceUnavailable, "无法读取程序，请检查文件是否存在、是否被占用及本机访问权限")
		}
	}))
}
