package station

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/websession"
)

// Owner-only setup. Paths never enter capability discovery or model context.
type localComfyDeploymentView struct {
	Revision      string                  `json:"revision"`
	Configured    bool                    `json:"configured"`
	ReadOnly      bool                    `json:"read_only"`
	Configuration *localComfyRegistration `json:"configuration"`
	Reason        string                  `json:"reason,omitempty"`
}

func (s *localProjectTools) comfyDeploymentRoutes(owner *websession.Store, mux *http.ServeMux) {
	for _, method := range []string{"GET", "PUT"} {
		mux.HandleFunc(method+" /api/local/project-tools/comfyui/deployment", owner.Protect(s.comfyDeployment))
	}
}

func (s *localProjectTools) readComfyDeployment() (localComfyDeploymentView, error) {
	v := localComfyDeploymentView{Revision: "unconfigured"}
	path := filepath.Join(s.home.dataDir, localComfyRegistrationFile)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	raw, err := localComfyServiceReadRegular(path, localComfyMaxManifest)
	if err != nil {
		return v, errors.New("ComfyUI 部署登记无法读取，原文件保留")
	}
	sum := sha256.Sum256(raw)
	v.Revision = hex.EncodeToString(sum[:])
	var registration localComfyRegistration
	if localComfyStrictDecode(raw, &registration) != nil {
		v.Reason = localComputeSetupReason(localComfyPackage, errors.New("station_comfyui_registration_invalid"))
		return v, nil
	}
	v.Configuration = &registration
	if registration.SchemaVersion != 1 || !registration.Enabled {
		v.Reason = "请检查 ComfyUI 部署登记版本及启用授权。"
		return v, nil
	}
	if err = s.validateComfyDeployment(registration); err != nil {
		v.Reason = err.Error()
		return v, nil
	}
	v.Configured = true
	return v, nil
}

func (s *localProjectTools) validateComfyDeployment(r localComfyRegistration) error {
	if err := r.validate(s.home.dataDir, s.home.root); err != nil {
		return errors.New(localComputeSetupReason(localComfyPackage, err))
	}
	if _, err := localComfyLoadManifest(r); err != nil {
		return errors.New(localComputeSetupReason(localComfyPackage, err))
	}
	if workflows, err := loadLocalComfyWorkflows(r.Workflows); err != nil || len(workflows) == 0 {
		return errLocalComfyWorkflowSetup
	}
	return nil
}

func (s *localProjectTools) comfyDeploymentLocked(ctx context.Context) (bool, string, error) {
	record, err := s.home.identity.Read()
	if err != nil || record.LocalOwner == nil {
		return false, "", errors.New("请先准备本机主人身份")
	}
	host, err := s.forFish(record.FishID)
	if err != nil {
		return false, "", errors.New("项目工具安装状态不可读")
	}
	item, err := host.store.Installed(ctx, localComfyPackage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", errors.New("项目工具安装状态不可读")
	}
	if err == nil && !item.Removed && item.Enabled {
		return true, "请先停用 ComfyUI，再修改运行环境。", nil
	}
	pending, err := host.store.HasUnfinishedJobs(ctx, localComfyPackage)
	if err != nil {
		return false, "", errors.New("ComfyUI 任务状态不可读，原登记保留")
	}
	if pending || s.isActive(localComfyPackage) {
		return true, "请先核对或结束 ComfyUI 未完成任务，再修改运行环境。", nil
	}
	// Even an invalid registration may still have a process from the old
	// generation. Inspect the owned process registry without rereading its paths.
	if backend, ok := s.comfy.(*localComfyService); ok && backend.services.Status(localComfyServiceID(record.FishID)).Running {
		return true, "ComfyUI 后端尚未退出，请先停止原服务。", nil
	}
	return false, "", nil
}

func (s *localProjectTools) comfyDeployment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		Revision         string                 `json:"revision"`
		ApproveExecution bool                   `json:"approve_execution"`
		Configuration    localComfyRegistration `json:"configuration"`
	}
	if r.Method == http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if !websession.Decode(w, r, &input) {
			return
		}
		if !input.ApproveExecution {
			websession.Fail(w, 400, "请确认使用已审核的本机运行文件及工作流")
			return
		}
	}
	s.update.Lock()
	defer s.update.Unlock()
	locked, reason, err := s.comfyDeploymentLocked(r.Context())
	if err != nil {
		websession.Fail(w, 409, err.Error())
		return
	}
	path := filepath.Join(s.home.dataDir, localComfyRegistrationFile)
	release, err := hostfile.Lock(path + ".lock")
	if err != nil {
		websession.Fail(w, 503, "ComfyUI 部署登记暂不可写")
		return
	}
	defer release()
	current, err := s.readComfyDeployment()
	if err != nil {
		websession.Fail(w, 503, err.Error())
		return
	}
	current.ReadOnly = locked
	if locked {
		current.Reason = reason
	}
	if r.Method == http.MethodGet {
		websession.Respond(w, 200, current)
		return
	}
	if locked || input.Revision != current.Revision {
		if !locked {
			reason = "ComfyUI 部署登记已更新，请刷新后核对；当前草稿保留。"
		}
		websession.Fail(w, 409, reason)
		return
	}
	next := input.Configuration
	next.SchemaVersion, next.Enabled = 1, true
	if err = s.validateComfyDeployment(next); err != nil {
		websession.Fail(w, 400, err.Error())
		return
	}
	raw, err := json.Marshal(next)
	if err != nil {
		websession.Fail(w, 400, "ComfyUI 部署登记参数无效")
		return
	}
	if err = hostfile.WritePrivate(path, raw); err != nil {
		websession.Fail(w, 503, "ComfyUI 部署登记保存失败，原登记保留")
		return
	}
	sum := sha256.Sum256(raw)
	websession.Respond(w, 200, localComfyDeploymentView{Revision: hex.EncodeToString(sum[:]), Configured: true, Configuration: &next})
}
