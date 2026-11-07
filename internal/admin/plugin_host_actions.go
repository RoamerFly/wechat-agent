package admin

import (
	"errors"
	"net/http"
	"path/filepath"
	mobile "wechat-bot/internal/devicespec"
)

type mobilePluginSettingsView struct {
	Revision                int64    `json:"revision"`
	Enabled                 bool     `json:"enabled"`
	Devices                 []string `json:"devices"`
	Actions                 []string `json:"actions"`
	MaxSteps                int      `json:"max_steps"`
	MaxSeconds              int      `json:"max_seconds"`
	ConnectionProfileStatus string   `json:"connection_profile_status"`
}

func mobilePluginSettings(s mobile.Settings) mobilePluginSettingsView {
	status := "待登记"
	config, storage := 0, 0
	valid := true
	for index, arg := range s.Bridge.Args {
		if arg != "--config" && arg != "--storage" {
			continue
		}
		if arg == "--config" {
			config++
		} else {
			storage++
		}
		if index+1 >= len(s.Bridge.Args) || !filepath.IsAbs(s.Bridge.Args[index+1]) {
			valid = false
		}
	}
	if valid && config == 1 && storage == 1 {
		status = "已登记"
	}
	return mobilePluginSettingsView{Revision: s.Revision, Enabled: s.Enabled, Devices: s.Devices, Actions: s.Actions, MaxSteps: s.MaxSteps, MaxSeconds: s.MaxSeconds, ConnectionProfileStatus: status}
}

func (s *Server) pluginHostActionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/plugin-packages/{id}/actions/{action}", s.protected(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != mobileDevicePluginID {
			fail(w, 404, "工具操作不可用")
			return
		}
		if s.manager.mobileRetired {
			fail(w, http.StatusGone, "手机控制已停用；保留任务、截图和录屏仅供查阅")
			return
		}
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 500, "工具存储不可用")
			return
		}
		installed, err := store.Installed(r.Context(), mobileDevicePluginID)
		action := r.PathValue("action")
		if err != nil || installed.Removed {
			if action == "tasks.cancel" || action == "tasks.stop" || action == "overview.bind" || action == "overview.unbind" || action == "overview.observation-apps" || action == "overview.pause" || action == "overview.takeover" || action == "overview.resume" {
				s.mobileHostAction(w, r, action)
				return
			}
			fail(w, 409, "手机工具未安装；只能查看历史或停止已有任务")
			return
		}
		if action == "settings.save" {
			if s.manager.draining() {
				fail(w, 409, "管理端正在准备更新，请稍后保存设备设置")
				return
			}
			var input mobilePluginSettingsView
			if !decode(w, r, &input) {
				return
			}
			current := s.manager.mobile.Settings()
			if input.Revision != current.Revision {
				fail(w, 409, "设备设置已被更新，请刷新后再保存")
				return
			}
			current.Enabled, current.Devices, current.Actions, current.MaxSteps, current.MaxSeconds = input.Enabled, input.Devices, input.Actions, input.MaxSteps, input.MaxSeconds
			if err := s.manager.mobile.Save(current); err != nil {
				if errors.Is(err, mobile.ErrRepositoryConflict) || err.Error() == "device_configuration_changed" {
					fail(w, 409, "设备设置已被更新，请刷新后再保存")
					return
				}
				fail(w, 400, "设备配置无效，请检查设备范围和动作范围")
				return
			}
			respond(w, 200, mobilePluginSettings(s.manager.mobile.Settings()))
			return
		}
		if (action == "tasks.create" || action == "overview.observe") && (!installed.Enabled || s.manager.mobileDeviceReady(r.Context()) != nil) {
			fail(w, 409, "手机工具尚未启用、授权或就绪")
			return
		}
		s.mobileHostAction(w, r, action)
	}))
}
