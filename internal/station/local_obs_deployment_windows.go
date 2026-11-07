//go:build windows

package station

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/obsportable"
	"wechat-bot/internal/websession"
)

const localOBSDeploymentRoute = "/api/local/project-tools/obs-preview/deployment"

type localOBSDeploymentView struct {
	Revision        string `json:"revision"`
	Version         string `json:"version"`
	ArchiveSHA256   string `json:"archive_sha256"`
	State           string `json:"state"`
	Installed       bool   `json:"installed"`
	ReadOnly        bool   `json:"read_only"`
	RestartRequired bool   `json:"restart_required"`
	Reason          string `json:"reason,omitempty"`
}

func (h *localChatService) obsDeploymentRoutes(owner *websession.Store, mux *http.ServeMux) {
	for _, method := range []string{"GET", "PUT"} {
		mux.HandleFunc(method+" "+localOBSDeploymentRoute, owner.Protect(h.obsDeployment))
	}
}

func (h *localChatService) obsDeploymentView(ctx context.Context) (localOBSDeploymentView, error) {
	v := localOBSDeploymentView{Revision: "missing", Version: obsportable.Version(), ArchiveSHA256: obsportable.ArchiveSHA256(), State: "missing"}
	if _, err := os.Lstat(filepath.Join(h.home.dataDir, "managed-obs")); errors.Is(err, os.ErrNotExist) {
		return v, nil
	} else if err != nil {
		return v, errors.New("专用 OBS 安装目录无法读取，原文件保留")
	}
	v.Revision, v.State, v.ReadOnly = "present", "invalid", true
	if _, err := obsportable.Open(ctx, h.home.dataDir, h.home.root); err != nil {
		v.Reason = "已有专用 OBS 安装未通过校验，原文件保留；请先退出应用后检查安装目录。"
		return v, nil
	}
	v.Installed, v.State = true, "installed"
	v.RestartRequired = h.liveEncoder != "ready"
	if v.RestartRequired {
		v.Reason = "已安装，重启应用后启用专用节目输出；不会自动开播。"
	}
	return v, nil
}

func (h *localChatService) obsDeployment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	identity, err := h.home.identity.Read()
	if err != nil || identity.LocalOwner == nil {
		websession.Fail(w, 409, "请先准备本机主人身份")
		return
	}
	if h.stopping.Load() {
		websession.Fail(w, 409, "应用正在停止，请稍后再安装")
		return
	}
	if r.Method == http.MethodGet {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		view, err := h.obsDeploymentView(ctx)
		if err != nil {
			websession.Fail(w, 503, err.Error())
			return
		}
		websession.Respond(w, 200, view)
		return
	}
	var in struct {
		Revision    string `json:"revision"`
		ArchivePath string `json:"archive_path"`
		Approved    bool   `json:"approved"`
	}
	if !websession.Decode(w, r, &in) {
		return
	}
	if !in.Approved {
		websession.Fail(w, 400, "请确认安装专用 OBS；此操作不授予直播权限")
		return
	}
	if in.Revision != "missing" {
		websession.Fail(w, 409, "安装状态已变化，请刷新；不会覆盖已有安装")
		return
	}
	if !h.obsInstallMu.TryLock() {
		websession.Fail(w, 409, "专用 OBS 正在安装，请等待本次操作完成")
		return
	}
	defer h.obsInstallMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	// Installation only creates a missing runtime and never changes an encoder
	// used by a live session. Existing/unknown sessions must settle first.
	controller, owner, err := h.liveController()
	if err != nil {
		websession.Fail(w, 409, "直播会话状态不可读，未开始安装")
		return
	}
	if _, err = controller.Active(ctx, owner); err == nil {
		websession.Fail(w, 409, "请先停止并核对原直播会话，再安装专用 OBS")
		return
	} else if !errors.Is(err, apphost.ErrNotFound) {
		websession.Fail(w, 409, "直播会话状态不可读，未开始安装")
		return
	}
	view, err := h.obsDeploymentView(ctx)
	if err != nil {
		websession.Fail(w, 503, err.Error())
		return
	}
	if view.ReadOnly {
		websession.Fail(w, 409, "已有专用 OBS 安装，原文件保留，不会覆盖")
		return
	}
	install := h.obsInstaller
	if install == nil {
		install = obsportable.Install
	}
	if err = install(ctx, h.home.dataDir, h.home.root, in.ArchivePath); err != nil {
		code, message := localOBSInstallFailure(err)
		websession.Fail(w, code, message)
		return
	}
	view, err = h.obsDeploymentView(ctx)
	if err != nil || !view.Installed {
		websession.Fail(w, 503, "安装后校验未通过，请保留文件并检查专用 OBS 安装")
		return
	}
	// The running controller deliberately keeps its previous registration.
	// A desktop restart activates the installation with normal boot verification.
	websession.Respond(w, 200, view)
}

func localOBSInstallFailure(err error) (int, string) {
	switch {
	case errors.Is(err, obsportable.ErrArchiveMissing):
		return 400, "未找到官方 OBS ZIP，请核对本机文件路径"
	case errors.Is(err, obsportable.ErrArchiveDigest):
		return 400, "安装包摘要与固定官方版本不符，请使用 OBS 33.0.1 Windows x64 ZIP"
	case errors.Is(err, obsportable.ErrArchiveFormat):
		return 400, "安装包结构或路径无效，未更改原安装"
	case errors.Is(err, obsportable.ErrAlreadyInstalled):
		return 409, "已有专用 OBS 安装，不会覆盖"
	case errors.Is(err, obsportable.ErrBusy):
		return 409, "专用 OBS 正在安装或使用，请稍后重试"
	case errors.Is(err, obsportable.ErrInstallCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return 409, "安装已取消或超时；重新读取状态后可继续"
	default:
		return 503, "专用 OBS 安装未完成，请检查磁盘空间及本机目录权限"
	}
}
