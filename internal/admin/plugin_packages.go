package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"wechat-bot/internal/pluginhost"
	catalog "wechat-bot/plugins"
)

// The new package store is opened lazily. Reading the old plugin settings or
// starting the existing application does not migrate or change its configuration.
func (m *Manager) packageStore() (*pluginhost.Store, error) {
	m.packageMu.Lock()
	defer m.packageMu.Unlock()
	if m.packagesClosed {
		return nil, errors.New("工具宿主正在关闭")
	}
	if m.packages != nil {
		return m.packages, nil
	}
	store, err := pluginhost.OpenStore(filepath.Join(m.dir, "plugin-host"), m.native.Account())
	if err != nil {
		return nil, err
	}
	m.packages = store
	return store, nil
}
func (s *Server) pluginPackageRoutes(mux *http.ServeMux) {
	s.pluginMigrationRoutes(mux)
	s.pluginRuntimeRoute(mux)
	s.pluginHostActionRoutes(mux)
	mux.HandleFunc("GET /api/plugin-packages/{id}", s.protected(func(w http.ResponseWriter, r *http.Request) {
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 500, "工具存储不可用")
			return
		}
		id := r.PathValue("id")
		// The retired controller is exposed only as a read-only archive.  Do
		// this before consulting its installed declaration so a surviving old
		// package cannot restore settings, runtime, or action controls.
		if s.manager.mobileRetired && id == mobileDevicePluginID {
			if err := s.manager.mobile.InitializationError(); err != nil {
				fail(w, http.StatusServiceUnavailable, "手机历史数据暂不可用，请检查本地存储；原记录未覆盖")
				return
			}
			respond(w, 200, s.mobilePluginArchive())
			return
		}
		installed, err := store.Installed(r.Context(), id)
		if err == nil && installed.Removed && id == mobileDevicePluginID {
			state, stateErr := store.ReadHostState(r.Context(), mobileDevicePluginID, mobileStateNamespace)
			if stateErr != nil {
				fail(w, 500, "手机历史读取失败")
				return
			}
			if state.Revision > 0 {
				respond(w, 200, s.mobilePluginArchive())
				return
			}
			fail(w, 404, "工具尚未安装")
			return
		}
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				fail(w, 500, "工具登记读取失败")
				return
			}
			if id == mobileDevicePluginID {
				state, stateErr := store.ReadHostState(r.Context(), mobileDevicePluginID, mobileStateNamespace)
				if stateErr == nil && state.Revision > 0 {
					respond(w, 200, s.mobilePluginArchive())
					return
				}
			}
			fail(w, 404, "工具尚未安装")
			return
		}
		schema, err := store.ConfigSchema(r.Context(), id)
		if err != nil {
			fail(w, 409, "工具配置声明无效或已变化")
			return
		}
		config, err := store.Configuration(r.Context(), id)
		if err != nil {
			fail(w, 500, "工具配置读取失败")
			return
		}
		h, connected, runtimeErr := s.manager.installedPluginRuntime()
		var status any
		if runtimeErr == nil {
			status = h.Status(id)
		}
		uiRaw, err := store.PackageFile(r.Context(), id, "ui.schema.json")
		if err != nil {
			fail(w, 409, "工具界面声明无效或已变化")
			return
		}
		ui, err := pluginhost.ParseUISchema(uiRaw)
		if err != nil {
			fail(w, 409, "工具界面声明无效或已变化")
			return
		}
		result := map[string]any{"installed": installed, "schema": schema, "configuration": config, "runtime_connected": connected, "runtime": status, "ui_schema": ui}
		result["readiness"] = pluginReadiness(r.Context(), store, h, installed, connected)
		if id == mobileDevicePluginID {
			plannerErr := s.manager.mobileDeviceReady(r.Context())
			overview := mobileOverview{Devices: []mobileDeviceOverview{}}
			if observer, ok := s.manager.mobileObservationService(); ok {
				overview = observer.Overview()
			}
			result["host_data"] = map[string]any{"state": "active", "settings": mobilePluginSettings(s.manager.mobile.Settings()), "tasks": s.manager.mobile.List(nil), "resources": map[string]any{}, "overview": overview, "diagnostics": map[string]any{"planner_available": plannerErr == nil, "planner_reason": mobileAvailabilityMessage(plannerErr)}}
		}
		respond(w, 200, result)
	}))
	mux.HandleFunc("POST /api/plugin-packages/{id}/configuration", s.protected(func(w http.ResponseWriter, r *http.Request) {
		if s.manager.mobileRetired && r.PathValue("id") == mobileDevicePluginID {
			fail(w, http.StatusGone, "手机控制已停用；不能修改历史工具配置")
			return
		}
		var input pluginhost.ConfigUpdate
		if !decode(w, r, &input) {
			return
		}
		if s.manager.draining() {
			fail(w, 409, "服务正在收尾，请稍后重试")
			return
		}
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 500, "工具存储不可用")
			return
		}
		value, err := store.SaveConfiguration(r.Context(), r.PathValue("id"), input)
		if err != nil {
			fail(w, 409, "配置未保存：请检查字段、保存版本，或先停用工具")
			return
		}
		respond(w, 200, value)
	}))
	mux.HandleFunc("GET /api/plugin-packages/{id}/jobs", s.protected(func(w http.ResponseWriter, r *http.Request) {
		limit := 30
		after := int64(0)
		var err error
		if raw, ok := r.URL.Query()["limit"]; ok {
			limit, err = strconv.Atoi(raw[0])
			if err != nil || limit < 1 {
				fail(w, 400, "页大小必须为正整数")
				return
			}
		}
		if raw, ok := r.URL.Query()["after"]; ok {
			after, err = strconv.ParseInt(raw[0], 10, 64)
			if err != nil || after < 0 {
				fail(w, 400, "续读位置无效")
				return
			}
		}
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 500, "工具存储不可用")
			return
		}
		page, err := store.Jobs(r.Context(), r.PathValue("id"), after, limit)
		if err != nil {
			fail(w, 500, "工具任务读取失败")
			return
		}
		respond(w, 200, page)
	}))
	mux.HandleFunc("POST /api/plugin-packages/{id}/jobs/{job}/cancel", s.protected(func(w http.ResponseWriter, r *http.Request) {
		if s.manager.mobileRetired && r.PathValue("id") == mobileDevicePluginID {
			fail(w, http.StatusGone, "手机控制已停用；不能取消历史工具任务")
			return
		}
		var input struct {
			Revision int `json:"revision"`
		}
		if !decode(w, r, &input) {
			return
		}
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 500, "工具存储不可用")
			return
		}
		job, err := store.Job(r.Context(), r.PathValue("job"))
		if err != nil || job.Plugin != r.PathValue("id") {
			fail(w, 404, "工具任务不存在")
			return
		}
		job, err = store.RequestJobCancel(r.Context(), job.ID, input.Revision)
		if err != nil {
			fail(w, 409, "任务状态已变化，请刷新后重试")
			return
		}
		respond(w, 200, job)
	}))
	mux.HandleFunc("GET /api/plugin-packages", s.protected(func(w http.ResponseWriter, r *http.Request) {
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 500, "工具存储不可用")
			return
		}
		installed, err := store.List(r.Context())
		if err != nil {
			fail(w, 500, "工具登记读取失败")
			return
		}
		official, err := catalog.List()
		if err != nil {
			fail(w, 500, "工具目录读取失败")
			return
		}
		hadMobileInstalled := false
		if s.manager.mobileRetired {
			filtered := installed[:0]
			for _, item := range installed {
				if item.Package.ID == mobileDevicePluginID {
					hadMobileInstalled = true
					continue
				}
				filtered = append(filtered, item)
			}
			installed = filtered
			filteredCatalog := official[:0]
			for _, item := range official {
				if item.ID != mobileDevicePluginID {
					filteredCatalog = append(filteredCatalog, item)
				}
			}
			official = filteredCatalog
		}
		h, connected, runtimeErr := s.manager.installedPluginRuntime()
		statuses := map[string]any{}
		readiness := make(map[string]pluginPackageReadiness, len(installed))
		for _, item := range installed {
			if runtimeErr == nil {
				statuses[item.Package.ID] = h.Status(item.Package.ID)
			}
			readiness[item.Package.ID] = pluginReadiness(r.Context(), store, h, item, connected)
		}
		retained := []string{}
		if state, stateErr := store.ReadHostState(r.Context(), mobileDevicePluginID, mobileStateNamespace); stateErr == nil && (state.Revision > 0 || hadMobileInstalled) {
			retained = append(retained, mobileDevicePluginID)
		}
		permissions := s.manager.Config().Companion.Permissions
		communicationState := "可用"
		if !permissions.SendFile {
			communicationState = "文件发送未授权"
		}
		builtins := []map[string]any{{
			"id": "wechat-file-transport", "name": "微信文件收发（内置）", "state": communicationState,
			"description":   "随核心常驻：在当前账号已授权会话取得文件原件、准备并发送文件、核对投递回执。文档/语音等工具只能加工素材，不能替代或卸载此能力。",
			"permissions":   []string{"当前账号", "已授权会话", "接收文件原件", map[bool]string{true: "发送文件", false: "发送文件未授权"}[permissions.SendFile]},
			"uninstallable": false,
		}}
		var sendTypes []string
		for _, kind := range []struct {
			label   string
			allowed bool
		}{
			{"文字", permissions.ReplyText}, {"图片", permissions.SendImage}, {"语音", permissions.SendVoice}, {"视频", permissions.SendVideo},
		} {
			if kind.allowed {
				sendTypes = append(sendTypes, kind.label)
			}
		}
		sendState := "发送未授权"
		if len(sendTypes) > 0 {
			sendState = "可发送" + strings.Join(sendTypes, "、")
		}
		builtins = append(builtins, map[string]any{
			"id": "wechat-messages", "name": "微信消息收发", "state": sendState,
			"description": "接收会话消息，按账号和会话权限发送文字、图片、语音或视频。",
			"permissions": []string{"当前账号", "已授权会话"}, "uninstallable": false,
		})
		respond(w, 200, map[string]any{"installed": installed, "catalog": official, "runtime_connected": connected, "runtimes": statuses, "readiness": readiness, "retained_ids": retained, "builtins": builtins})
	}))
	mux.HandleFunc("POST /api/plugin-packages/prepare", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Directory string `json:"directory"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.Directory == "" || !filepath.IsAbs(input.Directory) {
			fail(w, 400, "请选择本机工具包的绝对目录")
			return
		}
		if s.manager.draining() {
			fail(w, 409, "服务正在收尾，请稍后重试")
			return
		}
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 500, "工具存储不可用")
			return
		}
		prepared, err := store.Prepare(r.Context(), input.Directory)
		if err != nil {
			fail(w, 400, "工具包校验失败；请检查清单、文件摘要和目录权限")
			return
		}
		if s.manager.mobileRetired && prepared.Package.ID == mobileDevicePluginID {
			fail(w, http.StatusGone, "手机控制已停用；旧任务只保留为只读历史，不能重新安装")
			return
		}
		respond(w, 200, prepared)
	}))
	mux.HandleFunc("POST /api/plugin-packages/import", s.protected(func(w http.ResponseWriter, r *http.Request) {
		if s.manager.draining() {
			fail(w, 409, "服务正在收尾，请稍后重试")
			return
		}
		if r.Header.Get("Content-Type") != "application/zip" {
			fail(w, 415, "请选择 ZIP 格式的工具包")
			return
		}
		if r.ContentLength > pluginhost.MaxArchiveBytes {
			fail(w, 413, "工具 ZIP 超过 64 MiB；运行环境和模型请作为独立资源安装")
			return
		}
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 500, "工具存储不可用")
			return
		}
		prepared, err := store.PrepareArchive(r.Context(), r.Body)
		if errors.Is(err, pluginhost.ErrArchiveLimit) {
			fail(w, 413, "工具包超过导入限制：ZIP 64 MiB、展开后 256 MiB、4096 个条目；模型和运行环境应独立安装")
			return
		}
		if err != nil {
			fail(w, 400, "工具包检查失败：需要完整的 ZIP 工具包、有效清单和文件摘要，不能包含链接或重复路径")
			return
		}
		if s.manager.mobileRetired && prepared.Package.ID == mobileDevicePluginID {
			fail(w, http.StatusGone, "手机控制已停用；旧任务只保留为只读历史，不能重新安装")
			return
		}
		respond(w, 200, prepared)
	}))
	mux.HandleFunc("POST /api/plugin-packages/install", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Token       string   `json:"token"`
			Digest      string   `json:"digest"`
			Permissions []string `json:"permissions"`
			Confirm     bool     `json:"confirm"`
		}
		if !decode(w, r, &input) {
			return
		}
		if !input.Confirm {
			fail(w, 400, "需要管理员确认安装和所选权限")
			return
		}
		if s.manager.draining() {
			fail(w, 409, "服务正在收尾，请稍后重试")
			return
		}
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 500, "工具存储不可用")
			return
		}
		if s.manager.mobileRetired {
			prepared, preparedErr := store.Prepared(r.Context(), input.Token)
			if preparedErr != nil {
				// Never pass an unreadable approval record through to installation
				// while device control is retired. A stale or malformed token must
				// not become a bypass around the retired package identity check.
				fail(w, 409, "安装未完成：包尚未就绪、确认已失效、权限不匹配或文件已变化，请重新检查")
				return
			}
			if prepared.Package.ID == mobileDevicePluginID {
				fail(w, http.StatusGone, "手机控制已停用；旧任务只保留为只读历史，不能重新安装")
				return
			}
		}
		installed, err := store.Install(r.Context(), input.Token, input.Digest, input.Permissions)
		if err != nil {
			if errors.Is(err, pluginhost.ErrConfigurationMigrationRequired) {
				respond(w, http.StatusConflict, map[string]string{"code": "configuration_migration_required", "error": "升级需要迁移工具配置；原安装、配置和密钥已保留。"})
				return
			}
			fail(w, 409, "安装未完成：包尚未就绪、确认已失效、权限不匹配或文件已变化，请重新检查")
			return
		}
		respond(w, 201, installed)
	}))
}

// An uninstalled package cannot supply a UI declaration. The host retains only
// the history/resource view, never creation, cancellation, settings or
// observation actions from the former package.
func (s *Server) mobilePluginArchive() map[string]any {
	readOnly := s.manager.mobileRetired
	overview := mobileOverview{Devices: []mobileDeviceOverview{}}
	if !readOnly {
		if observer, ok := s.manager.mobileObservationService(); ok {
			overview = observer.Overview()
		}
	}
	taskSection := pluginhost.UISection{ID: "tasks", Title: "保留的设备任务", Component: "tasks", Binding: "host_data.tasks"}
	if !readOnly {
		taskSection.Action = "tasks.cancel"
	}
	reason := "手机工具未安装；只能查看历史或停止已有任务"
	if readOnly {
		reason = "手机控制已停用；只能查看已保留的任务、截图和录屏"
	}
	return map[string]any{
		"archive": true,
		"ui_schema": pluginhost.UISchema{Version: 1, Sections: []pluginhost.UISection{
			taskSection,
			{ID: "resources", Title: "任务素材", Component: "resources", Binding: "host_data.resources"},
			{ID: "diagnostics", Title: "状态与说明", Component: "diagnostics", Binding: "host_data.diagnostics"},
		}},
		"host_data": map[string]any{
			"state": "archive", "read_only": readOnly, "tasks_retained": true, "tasks": s.manager.mobile.List(nil), "resources": map[string]any{}, "overview": overview,
			"diagnostics": map[string]any{"planner_available": false, "planner_reason": reason},
		},
	}
}
