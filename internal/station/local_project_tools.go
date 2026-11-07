package station

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"wechat-bot/internal/billing"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/stationruntime"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
	catalog "wechat-bot/plugins"
)

const localProjectToolIdle = 5 * time.Minute

type localProjectTools struct {
	home            *localHomeService
	options         pluginruntime.Options
	mu              sync.Mutex
	update          sync.Mutex
	host            *localProjectToolHost
	closed          bool
	active          map[string]int
	rvc             localRVCBackend
	comfy           localComfyBackend
	yue             localYuEBackend
	tts             localTTSBackend
	obs             localOBSPreview
	ttsCancel       context.CancelFunc           // protected by update
	ttsJobID        string                       // protected by update
	visionPost      pluginruntime.RouteTransport // host-only transport; tests use an offline substitute
	visionUsage     *billing.Store
	visionUsageFish string
}

type localProjectToolHost struct {
	store      *pluginhost.Store
	controller *pluginruntime.Controller
}

type localProjectToolView struct {
	ID               string                   `json:"id"`
	Name             string                   `json:"name"`
	CatalogAvailable bool                     `json:"catalog_available"`
	RuntimeReady     bool                     `json:"runtime_ready"`
	PDFReady         bool                     `json:"pdf_ready"`
	UpdateAvailable  bool                     `json:"update_available"`
	Installed        bool                     `json:"installed"`
	Enabled          bool                     `json:"enabled"`
	Revision         int                      `json:"revision"`
	State            string                   `json:"state"`
	Reason           string                   `json:"reason,omitempty"`
	VoiceProfiles    []localRVCVoice          `json:"voice_profiles,omitempty"`
	SpeechVoices     []localTTSVoice          `json:"speech_voices,omitempty"`
	Workflows        []localComfyWorkflowView `json:"workflows,omitempty"`
}

func (s *localProjectTools) runtimeReady() bool {
	return runtime.GOOS == "windows" && stationruntime.ToolRuntimeReady(s.options, s.home.root)
}

func (s *localProjectTools) pdfRuntimeReady() bool {
	if !s.runtimeReady() || s.options.DocumentRuntimeRoot == "" {
		return false
	}
	root, err := workspacefs.OpenPath(s.options.DocumentRuntimeRoot)
	if err != nil {
		return false
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, "runtime-manifest.json")
	if err != nil {
		return false
	}
	defer file.Close()
	var manifest struct {
		Version    int    `json:"schema_version"`
		Platform   string `json:"platform"`
		Policy     string `json:"policy"`
		Components struct {
			ReportLab string `json:"reportlab"`
			PDF       string `json:"pypdf"`
			Font      bool   `json:"embeddable_ttf"`
		} `json:"components"`
	}
	const limit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	return err == nil && len(raw) <= limit && json.Unmarshal(raw, &manifest) == nil && manifest.Version == 2 &&
		manifest.Platform == "windows-x64" && manifest.Policy == "reportlab-offline-pypdf-reviewed-sources-v1" &&
		manifest.Components.ReportLab == "4.4.9-offline-patch-v1" && manifest.Components.PDF == "6.21.4" && manifest.Components.Font
}

func (s *localProjectTools) forFish(fish string) (*localProjectToolHost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !fishidentity.ValidFishID(fish) {
		return nil, errors.New("本机项目工具已停止或身份不可用")
	}
	if s.host != nil {
		if s.host.store.Account() != fish {
			return nil, errors.New("本机身份已变化，请重启后使用项目工具")
		}
		return s.host, nil
	}
	dir := filepath.Join(s.home.dataDir, "project-tools", fish)
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return nil, err
	}
	if err = root.Close(); err != nil {
		return nil, err
	}
	store, err := pluginhost.OpenStore(dir, fish)
	if err != nil {
		return nil, err
	}
	options := s.options
	options.DocumentRuntimeRoot = ""
	executor := stationruntime.InstalledToolExecutor(store, options, s.home.root)
	controller, err := pluginruntime.NewController(store, executor)
	if err != nil {
		store.Close()
		return nil, err
	}
	controller.SetIdleTimeout(localProjectToolIdle)
	s.host = &localProjectToolHost{store: store, controller: controller}
	return s.host, nil
}

func (s *localProjectTools) close() error {
	s.update.Lock()
	defer s.update.Unlock()
	s.obs.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var err error
	if s.rvc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		err = s.rvc.Close(ctx)
		cancel()
	}
	if s.comfy != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		err = errors.Join(err, s.comfy.Close(ctx))
		cancel()
	}
	if s.yue != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		err = errors.Join(err, s.yue.Close(ctx))
		cancel()
	}
	if s.host == nil {
		return err
	}
	err = errors.Join(err, s.host.controller.Close(), s.host.store.Close())
	s.host = nil
	return err
}

var localProjectPackages = []struct{ ID, Name string }{
	{pluginruntime.DocumentWorkshopID, "文档工坊"},
	{localAudioPackage, "音频工坊"},
	{localasr.PluginID, "本地媒体识别"},
	{localRVCPackage, "RVC 换声"},
	{localComfyPackage, "ComfyUI 创作"},
	{localYuEPackage, "YuE 作曲"},
	{localTTSPackage, "本机语音"},
	{localOBSPackage, "OBS 本地预览"},
}

func (s *localProjectTools) markActive(id string) func() {
	s.mu.Lock()
	if s.active == nil {
		s.active = make(map[string]int)
	}
	s.active[id]++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.active[id]--
		s.mu.Unlock()
	}
}

func (s *localProjectTools) isActive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[id] > 0
}

func (s *localProjectTools) projectView(ctx context.Context, host *localProjectToolHost, id, name string) (localProjectToolView, error) {
	view := localProjectToolView{ID: id, Name: name, RuntimeReady: s.runtimeReady(), State: "not_installed"}
	var backendBusy bool
	if s.options.CatalogDirectory != "" {
		info, err := os.Lstat(filepath.Join(s.options.CatalogDirectory, view.ID, "plugin.json"))
		view.CatalogAvailable = err == nil && info.Mode().IsRegular()
	}
	if id == localRVCPackage {
		view.RuntimeReady = false
		if host != nil && s.rvc != nil {
			info, err := s.rvc.Info(ctx, host.store.Account())
			view.RuntimeReady, view.VoiceProfiles = err == nil && len(info.Profiles) != 0, info.Profiles
			backendBusy = err == nil && info.Busy
			if err != nil {
				view.Reason = localRVCSetupReason(err)
			} else if !view.RuntimeReady {
				view.Reason = "尚未登记获准使用的 RVC 声线。"
			}
		}
	}
	if id == localComfyPackage {
		view.RuntimeReady = false
		view.Reason = "comfyui_backend_not_configured"
		if host != nil && s.comfy != nil {
			info, workflows, err := s.comfySetup(ctx, host.store.Account())
			view.RuntimeReady = err == nil
			backendBusy = err == nil && info.Busy
			view.Workflows = localComfyWorkflowViews(workflows)
			if errors.Is(err, errLocalComfyWorkflowSetup) {
				view.Reason = "comfyui_workflows_missing"
			} else if err != nil {
				view.Reason = err.Error() // comfySetup returns only public setup messages.
			}
		}
	}
	if id == localYuEPackage {
		view.RuntimeReady = false
		view.Reason = "yue_backend_not_configured"
		if host != nil && s.yue != nil {
			info, err := s.yue.Info(ctx, host.store.Account())
			if err == nil {
				view.RuntimeReady, backendBusy, view.Reason = true, info.Busy, ""
			} else {
				view.Reason = localComputeSetupReason(localYuEPackage, err)
			}
		}
	}
	if id == localTTSPackage {
		view.RuntimeReady = false
		view.Reason = "station_speech_backend_not_configured"
		if host != nil && s.tts != nil {
			info, err := s.tts.Info(ctx, host.store.Account())
			if err == nil && len(info.Voices) > 0 {
				view.RuntimeReady, view.Reason, view.SpeechVoices = true, "", info.Voices
			} else {
				view.Reason = localTTSSetupReason(err)
			}
		}
	}
	if id == localOBSPackage {
		view.RuntimeReady = false
		view.Reason = "obs_preview_not_configured"
		if host != nil {
			if config, err := localOBSConfiguration(ctx, host); err == nil && config.Configured {
				view.RuntimeReady = true
				view.Reason = ""
				backendBusy = s.obs.status(config).Active
			}
		}
	}
	if host == nil {
		return view, nil
	}
	item, err := host.store.Installed(ctx, view.ID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && item.Removed {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	view.Installed, view.Enabled, view.Revision = true, item.Enabled, item.Revision
	if id == localasr.PluginID && item.Enabled {
		if _, err := s.home.localStationASRVersion(ctx); err != nil {
			if _, visionErr := s.home.localStationVisionVersion(ctx); visionErr != nil {
				view.RuntimeReady = false
				view.Reason = "本机媒体资源未就绪，请核对语音识别资源或配置视觉服务。"
			}
		}
	}
	contract, err := catalog.ContractFor(id)
	if err != nil {
		return view, err
	}
	view.UpdateAvailable = view.CatalogAvailable && item.Digest != contract.ManifestSHA256
	view.PDFReady = id == pluginruntime.DocumentWorkshopID && (item.Digest == contract.ManifestSHA256 || compatibleDocumentDigest(projectOperation(localDocumentExportTool), item.Digest)) && s.pdfRuntimeReady()
	switch {
	case !item.Enabled:
		view.State = "disabled"
	case !view.RuntimeReady:
		view.State = "runtime_missing"
	case backendBusy || s.isActive(id):
		view.State = "running"
	default:
		view.State = host.controller.Status(view.ID).State
		if view.State == "stopped" {
			view.State = "idle"
		}
	}
	return view, nil
}

func (s *localProjectTools) routes(owner *websession.Store, mux *http.ServeMux) {
	s.visionConfigurationRoutes(owner, mux)
	s.comfyDeploymentRoutes(owner, mux)
	s.yueDeploymentRoutes(owner, mux)
	mux.HandleFunc("GET /api/local/project-tools", owner.Protect(func(w http.ResponseWriter, r *http.Request) {
		record, err := s.home.identity.Read()
		if err != nil {
			websession.Fail(w, 409, "本机身份记录不可读")
			return
		}
		var host *localProjectToolHost
		if record.LocalOwner != nil {
			host, err = s.forFish(record.FishID)
			if err != nil {
				websession.Fail(w, 503, "项目工具安装状态不可读")
				return
			}
		}
		views := make([]localProjectToolView, 0, len(localProjectPackages))
		for _, pkg := range localProjectPackages {
			view, err := s.projectView(r.Context(), host, pkg.ID, pkg.Name)
			if err != nil {
				websession.Fail(w, 503, "项目工具安装状态不可读")
				return
			}
			views = append(views, view)
		}
		w.Header().Set("Cache-Control", "no-store")
		websession.Respond(w, 200, map[string]any{"tools": views})
	}))
	for _, pkg := range localProjectPackages {
		mux.HandleFunc("PUT /api/local/project-tools/"+pkg.ID, owner.Protect(func(w http.ResponseWriter, r *http.Request) {
			s.changeProjectTool(w, r, pkg.ID, pkg.Name)
		}))
	}
	preview := owner.Protect(func(w http.ResponseWriter, r *http.Request) {
		s.obsPreview(owner, w, r)
	})
	mux.HandleFunc("GET /api/local/project-tools/"+localOBSPackage+"/preview", preview)
	mux.HandleFunc("POST /api/local/project-tools/"+localOBSPackage+"/preview", preview)
}

func (s *localProjectTools) changeProjectTool(w http.ResponseWriter, r *http.Request, id, name string) {
	var in struct {
		Action           string `json:"action"`
		Revision         int    `json:"revision"`
		ApproveArtifacts bool   `json:"approve_artifacts"`
	}
	if !websession.Decode(w, r, &in) {
		return
	}
	if in.Action != "install" && in.Action != "upgrade" && in.Action != "enable" && in.Action != "disable" {
		websession.Fail(w, 400, "未知的项目工具操作")
		return
	}
	if (in.Action == "install" || in.Action == "upgrade") && !in.ApproveArtifacts {
		if id == localOBSPackage {
			websession.Fail(w, 400, "安装 OBS 预览需要主人确认访问本机 WebSocket 的权限")
			return
		}
		if id == localComfyPackage {
			websession.Fail(w, 400, "安装 ComfyUI 需要主人确认本机成品写入与计算权限")
			return
		}
		if id == localRVCPackage || id == localYuEPackage || id == localTTSPackage {
			websession.Fail(w, 400, "安装需要主人确认任务素材读写与本机计算权限")
			return
		}
		websession.Fail(w, 400, "安装需要主人明确授予本机成品读写权限")
		return
	}
	if in.Action == "enable" && id != localRVCPackage && id != localComfyPackage && id != localYuEPackage && id != localTTSPackage && id != localOBSPackage && !s.runtimeReady() {
		websession.Fail(w, 409, "Windows 项目工具运行器缺失")
		return
	}

	s.update.Lock()
	defer s.update.Unlock()
	ctx := r.Context()
	record, err := s.home.identity.Read()
	if err != nil || record.LocalOwner == nil {
		websession.Fail(w, 409, "请先准备本机主人身份")
		return
	}
	host, err := s.forFish(record.FishID)
	if err != nil {
		websession.Fail(w, 503, "项目工具安装状态不可读")
		return
	}
	current, err := host.store.Installed(ctx, id)
	if in.Action == "install" || in.Action == "upgrade" {
		missing := errors.Is(err, sql.ErrNoRows) || err == nil && current.Removed
		if in.Action == "upgrade" && (err != nil || current.Removed || current.Enabled || current.Revision != in.Revision) {
			websession.Fail(w, 409, "请先停用该工具并刷新版本，再确认更新")
			return
		}
		if err != nil && !missing {
			websession.Fail(w, 503, "项目工具安装状态不可读")
			return
		}
		contract, contractErr := catalog.ContractFor(id)
		if contractErr != nil {
			websession.Fail(w, 503, "官方工具声明不可读")
			return
		}
		if in.Action == "install" && !missing && current.Digest != contract.ManifestSHA256 {
			websession.Fail(w, 409, "已安装版本需要单独升级")
			return
		}
		if missing || current.Digest != contract.ManifestSHA256 {
			if s.options.CatalogDirectory == "" {
				websession.Fail(w, 409, "当前发行包未提供该项目工具")
				return
			}
			prepared, prepareErr := host.store.Prepare(ctx, filepath.Join(s.options.CatalogDirectory, id))
			if prepareErr != nil || prepared.Package.ID != id || prepared.Digest != contract.ManifestSHA256 {
				websession.Fail(w, 409, "项目工具发行源缺失或摘要不匹配")
				return
			}
			grants := []string{"artifact.read", "artifact.write"}
			if id == localasr.PluginID {
				grants = []string{"artifact.read", "artifact.write", "model.invoke"}
			}
			if id == localRVCPackage {
				grants = localRVCGrants
			}
			if id == localComfyPackage {
				grants = localComfyGrants
			}
			if id == localYuEPackage {
				grants = localYuEGrants
			}
			if id == localTTSPackage {
				grants = localTTSGrants
			}
			if id == localOBSPackage {
				grants = []string{"network"}
			}
			_, err = host.store.Install(ctx, prepared.Token, prepared.Digest, grants)
		}
	} else {
		if err != nil || current.Removed || current.Revision != in.Revision {
			websession.Fail(w, 409, "项目工具版本已变化，请刷新后重试")
			return
		}
		if id == localOBSPackage {
			if in.Action == "enable" {
				config, checkErr := localOBSConfiguration(ctx, host)
				if checkErr != nil || !config.Configured {
					websession.Fail(w, 409, "请先保存 OBS 本机端口和 WebSocket 密码")
					return
				}
			}
			if in.Action == "disable" {
				s.obs.stop()
			}
			_, err = host.store.SetEnabled(ctx, id, current.Revision, in.Action == "enable")
		} else if id == localTTSPackage {
			if in.Action == "enable" {
				if s.tts == nil {
					websession.Fail(w, 409, "本机语音后端未登记")
					return
				}
				if info, checkErr := s.tts.Info(ctx, record.FishID); checkErr != nil || len(info.Voices) == 0 {
					websession.Fail(w, 409, localTTSSetupReason(checkErr))
					return
				}
			}
			_, err = host.store.SetEnabled(ctx, id, current.Revision, in.Action == "enable")
			if err == nil && in.Action == "disable" && s.ttsCancel != nil {
				s.ttsCancel()
			}
		} else if id == localYuEPackage {
			if in.Action == "enable" {
				if s.yue == nil {
					websession.Fail(w, 409, "本机 YuE 后端未登记")
					return
				}
				if _, err := s.yue.Info(ctx, record.FishID); err != nil {
					websession.Fail(w, 409, localComputeSetupReason(localYuEPackage, err))
					return
				}
			}
			_, err = host.store.SetEnabled(ctx, id, current.Revision, in.Action == "enable")
			if err == nil && in.Action == "disable" && s.yue != nil {
				stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
				err = s.yue.Stop(stopCtx)
				cancel()
				if err != nil {
					websession.Fail(w, 503, "YuE 已停用，后端退出尚待核对")
					return
				}
			}
		} else if id == localComfyPackage {
			if in.Action == "enable" {
				if _, _, checkErr := s.comfySetup(ctx, record.FishID); checkErr != nil {
					websession.Fail(w, 409, checkErr.Error())
					return
				}
			}
			_, err = host.store.SetEnabled(ctx, id, current.Revision, in.Action == "enable")
			if err == nil && in.Action == "disable" && s.comfy != nil {
				stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
				err = s.comfy.Stop(stopCtx)
				cancel()
				if err != nil {
					websession.Fail(w, 503, "ComfyUI 已停用，新任务已拦截，正在核对后端退出。")
					return
				}
			}
		} else if id == localRVCPackage {
			if in.Action == "enable" {
				if s.rvc == nil {
					websession.Fail(w, 409, "当前发行包未提供 RVC 后端")
					return
				}
				info, checkErr := s.rvc.Info(ctx, record.FishID)
				if checkErr != nil || len(info.Profiles) == 0 {
					websession.Fail(w, 409, localRVCSetupReason(checkErr))
					return
				}
			}
			_, err = host.store.SetEnabled(ctx, id, current.Revision, in.Action == "enable")
			if err == nil && in.Action == "disable" && s.rvc != nil {
				stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
				err = s.rvc.Stop(stopCtx)
				cancel()
				if err != nil {
					websession.Fail(w, 503, "RVC 已停用，新任务已拦截，正在核对后端退出。")
					return
				}
			}
		} else if id == localasr.PluginID {
			if in.Action == "enable" {
				if !current.Enabled {
					if err = s.configureStationASR(ctx, host); err != nil {
						config, configErr := host.store.Configuration(ctx, localasr.PluginID)
						if configErr != nil || !stationVisionConfigured(config) {
							websession.Fail(w, 409, "媒体理解尚未配置视觉服务或可用的本机识别资源")
							return
						}
					}
				}
				err = host.controller.EnableOnDemand(ctx, id, current.Revision)
			} else {
				err = host.controller.Stop(ctx, id, current.Revision)
			}
		} else if in.Action == "enable" {
			err = host.controller.EnableOnDemand(ctx, id, current.Revision)
		} else {
			err = host.controller.Stop(ctx, id, current.Revision)
		}
	}
	if err != nil {
		websession.Fail(w, 409, "项目工具设置未保存，请核对版本、依赖和运行中的任务")
		return
	}
	view, err := s.projectView(ctx, host, id, name)
	if err != nil {
		websession.Fail(w, 503, "项目工具状态暂不可读")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	websession.Respond(w, 200, map[string]any{"tool": view})
}
