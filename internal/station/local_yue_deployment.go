package station

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/stationyue"
	"wechat-bot/internal/websession"

	"github.com/google/uuid"
)

// Owner-only setup. Paths never enter capability discovery or model context.
type localYuEDeploymentView struct {
	Revision      string                `json:"revision"`
	Configured    bool                  `json:"configured"`
	ReadOnly      bool                  `json:"read_only"`
	Configuration *localYuERegistration `json:"configuration"`
	Reason        string                `json:"reason,omitempty"`
}

func (s *localProjectTools) yueDeploymentRoutes(owner *websession.Store, mux *http.ServeMux) {
	for _, method := range []string{"GET", "PUT"} {
		mux.HandleFunc(method+" /api/local/project-tools/yue/deployment", owner.Protect(s.yueDeployment))
	}
}

func (s *localProjectTools) readYuEDeployment() (localYuEDeploymentView, error) {
	v := localYuEDeploymentView{Revision: "unconfigured"}
	path := filepath.Join(s.home.dataDir, localYuERegistrationFile)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	raw, err := localComfyServiceReadRegular(path, localYuEMaxManifest)
	if err != nil {
		return v, errors.New("YuE 部署登记无法读取，原文件保留")
	}
	sum := sha256.Sum256(raw)
	v.Revision = hex.EncodeToString(sum[:])
	var registration localYuERegistration
	if localComfyStrictDecode(raw, &registration) != nil {
		v.Reason = localComputeSetupReason(localYuEPackage, errors.New("station_yue_registration_invalid"))
		return v, nil
	}
	v.Configuration = &registration
	if registration.SchemaVersion != 1 || !registration.Enabled {
		v.Reason = "请检查 YuE 部署登记版本及启用授权。"
		return v, nil
	}
	if err = s.validateYuEDeployment(registration); err != nil {
		v.Reason = err.Error()
		return v, nil
	}
	v.Configured = true
	return v, nil
}

func (s *localProjectTools) validateYuEDeployment(r localYuERegistration) error {
	if err := r.validate(s.home.dataDir, s.home.root); err != nil {
		return errors.New(localComputeSetupReason(localYuEPackage, err))
	}
	if _, err := localYuELoadManifest(r); err != nil {
		return errors.New(localComputeSetupReason(localYuEPackage, err))
	}
	for mode, name := range localYuEDeploymentTemplates() {
		raw, err := localComfyServiceReadRegular(filepath.Join(r.Workflows, name), 1<<20)
		if err != nil || stationyue.ValidateTemplate(raw, mode) != nil {
			return errors.New("YuE 工作流无效，请核对已审核的原创及翻唱工作流。")
		}
	}
	return nil
}

func (s *localProjectTools) yueDeploymentLocked(ctx context.Context) (bool, string, error) {
	record, err := s.home.identity.Read()
	if err != nil || record.LocalOwner == nil {
		return false, "", errors.New("请先准备本机主人身份")
	}
	host, err := s.forFish(record.FishID)
	if err != nil {
		return false, "", errors.New("项目工具安装状态不可读")
	}
	item, err := host.store.Installed(ctx, localYuEPackage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", errors.New("项目工具安装状态不可读")
	}
	if err == nil && !item.Removed && item.Enabled {
		return true, "请先停用 YuE，再修改运行环境。", nil
	}
	pending, err := host.store.HasUnfinishedJobs(ctx, localYuEPackage)
	if err != nil {
		return false, "", errors.New("YuE 任务状态不可读，原登记保留")
	}
	if pending || s.isActive(localYuEPackage) {
		return true, "请先核对或结束 YuE 未完成任务，再修改运行环境。", nil
	}
	// Even an invalid registration may still have a process from the old
	// generation. Inspect the owned process registry without rereading its paths.
	if backend, ok := s.yue.(*localYuEService); ok {
		if backend.managed.services.Status(localYuEServiceID(record.FishID)).Running {
			return true, "YuE 后端尚未退出，请先停止原服务。", nil
		}
	} else {
		if s.yue == nil {
			return false, "", errors.New("YuE 后端状态不可读，原登记保留")
		}
		info, err := s.yue.Info(ctx, record.FishID)
		if err != nil {
			return false, "", errors.New("YuE 后端状态不可读，原登记保留")
		}
		if info.Running || info.Busy {
			return true, "YuE 后端尚未退出，请先停止原服务。", nil
		}
	}
	return false, "", nil
}

func (s *localProjectTools) yueDeployment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		Revision         string               `json:"revision"`
		ApproveExecution bool                 `json:"approve_execution"`
		Configuration    localYuERegistration `json:"configuration"`
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
	locked, reason, err := s.yueDeploymentLocked(r.Context())
	if err != nil {
		websession.Fail(w, 409, err.Error())
		return
	}
	path := filepath.Join(s.home.dataDir, localYuERegistrationFile)
	release, err := hostfile.Lock(path + ".lock")
	if err != nil {
		websession.Fail(w, 503, "YuE 部署登记暂不可写")
		return
	}
	defer release()
	current, err := s.readYuEDeployment()
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
			reason = "YuE 部署登记已更新，请刷新后核对；当前草稿保留。"
		}
		websession.Fail(w, 409, reason)
		return
	}
	next := input.Configuration
	next.SchemaVersion, next.Enabled = 1, true
	var candidateManifest string
	defer func() {
		if candidateManifest != "" {
			_ = os.Remove(candidateManifest)
		}
	}()
	if err = localYuEPrepareDeployment(&next); err == nil && next.Manifest == "" {
		candidateManifest, err = s.prepareYuEDeploymentManifest(&next)
	}
	if err == nil {
		err = s.validateYuEDeployment(next)
	}
	if err != nil {
		websession.Fail(w, 400, err.Error())
		return
	}
	raw, err := json.Marshal(next)
	if err != nil {
		websession.Fail(w, 400, "YuE 部署登记参数无效")
		return
	}
	if err = hostfile.WritePrivate(path, raw); err != nil {
		websession.Fail(w, 503, "YuE 部署登记保存失败，原登记保留")
		return
	}
	candidateManifest = "" // the committed generation owns its private manifest
	sum := sha256.Sum256(raw)
	websession.Respond(w, 200, localYuEDeploymentView{Revision: hex.EncodeToString(sum[:]), Configured: true, Configuration: &next})
}

// Only omitted pins are computed from owner-selected files. Nonempty promises
// remain unchanged and validate must reject any mismatch.
func localYuEPrepareDeployment(r *localYuERegistration) error {
	if r.WorkflowPins == nil {
		r.WorkflowPins = map[string]string{}
	}
	for mode, name := range localYuEDeploymentTemplates() {
		if r.WorkflowPins[string(mode)] == "" {
			sum, err := localYuEDeploymentDigest(filepath.Join(r.Workflows, name))
			if err != nil {
				return errors.New("YuE 工作流无法审核")
			}
			r.WorkflowPins[string(mode)] = sum
		}
	}
	if r.FFmpegSHA256 == "" {
		sum, err := localYuEDeploymentDigest(r.FFmpeg)
		if err != nil {
			return errors.New("YuE 音频解码器无法审核")
		}
		r.FFmpegSHA256 = sum
	}
	return nil
}

func localYuEDeploymentTemplates() map[stationyue.Mode]string {
	return map[stationyue.Mode]string{stationyue.ModeOriginal: "audio_yue2_text_to_music.json", stationyue.ModeCover: "audio_yue2_music_cover.json"}
}

func localYuEDeploymentDigest(path string) (string, error) {
	if !localComfySafeRegular(path) {
		return "", errors.New("invalid file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, io.LimitReader(f, (512<<20)+1))
	if err != nil || size > 512<<20 {
		return "", errors.New("invalid file size")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// This records the two files required by the existing service admission policy;
// it is not a complete audit of the portable runtime or its models.
func (s *localProjectTools) prepareYuEDeploymentManifest(r *localYuERegistration) (string, error) {
	failure := errors.New("YuE 运行文件无法审核，原登记保留")
	if !localComfyServiceSafeDirectory(r.Runtime) || localComfyPathsOverlap(r.Runtime, s.home.dataDir) || localComfyPathsOverlap(r.Runtime, s.home.root) || !localComfySafeRelative(r.Python) || !localComfySafeRelative(r.Entry) {
		return "", failure
	}
	files := map[string]string{}
	for _, relative := range []string{r.Python, r.Entry} {
		digest, err := localYuEDeploymentDigest(filepath.Join(r.Runtime, relative))
		if err != nil {
			return "", failure
		}
		files[relative] = digest
	}
	raw, err := json.Marshal(localYuEManifest{SchemaVersion: 1, Policy: "station-yue-v1", Files: files})
	if err != nil {
		return "", failure
	}
	// A new private generation never replaces an owner's explicit manifest.
	path := filepath.Join(s.home.dataDir, "station-yue-manifest-"+uuid.NewString()+".json")
	if err = hostfile.WritePrivate(path, raw); err != nil {
		return "", failure
	}
	r.Manifest = path
	return path, nil
}
