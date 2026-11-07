package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func runtimeDeploymentOptions() (pluginruntime.Options, error) {
	var options pluginruntime.Options
	raw := os.Getenv("WECHAT_PLUGIN_RUNTIME_CONFIG")
	if raw == "" {
		return options, nil
	}
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.DisallowUnknownFields()
	if d.Decode(&options) != nil {
		return options, errors.New("plugin runtime deployment configuration invalid")
	}
	return options, nil
}

func (m *Manager) installedPluginRuntime() (*pluginruntime.Controller, bool, error) {
	store, err := m.packageStore()
	if err != nil {
		return nil, false, err
	}
	m.packageMu.Lock()
	defer m.packageMu.Unlock()
	if m.packagesClosed {
		return nil, false, errors.New("plugin host closing")
	}
	if m.packageRuntime != nil {
		return m.packageRuntime, m.packageRuntimeReady, nil
	}
	options, err := runtimeDeploymentOptions()
	if err != nil {
		return nil, false, err
	}
	// Local ASR paths are resolved from the administrator-installed public
	// registration at launch time. They are never accepted from deployment JSON,
	// plugin configuration, or a capability argument.
	options.ResourceRoots = m.localASRPluginResources
	options.HostMetadata = m.localASRPluginMetadata
	ready := false
	if runtime.GOOS == "windows" {
		ready = options.Pythonw != "" && options.WindowsHelper != ""
		for _, p := range []string{options.Pythonw, options.WindowsHelper} {
			if st, e := os.Stat(p); e != nil || !st.Mode().IsRegular() {
				ready = false
			}
		}
	} else if runtime.GOOS == "linux" {
		_, e := exec.LookPath("bwrap")
		ready = e == nil
	}
	var executor pluginhost.Executor
	if ready {
		installed := pluginruntime.InstalledExecutor(store, options)
		if m.mobileRetired {
			resolve := installed.Resolve
			installed.Resolve = func(ctx context.Context, p pluginhost.Package, grant pluginhost.Grant) (pluginruntime.LaunchSpec, error) {
				if p.ID == mobileDevicePluginID {
					return pluginruntime.LaunchSpec{}, errDeviceServiceRetired
				}
				return resolve(ctx, p, grant)
			}
		}
		executor = installed
	}
	h, err := pluginruntime.NewController(store, executor)
	if err != nil {
		return nil, false, err
	}
	if m.agent != nil {
		h.SetIdleTimeout(time.Duration(m.agent.config().retention().PluginMinutes) * time.Minute)
	}
	if err = h.AttachManagedServices(m.managedServiceRegistry()); err != nil {
		_ = h.Close()
		return nil, false, err
	}
	m.packageRuntime = h
	m.packageRuntimeReady = ready
	return h, ready, nil
}

func (m *Manager) applyServiceRetention(retention serviceRetention) {
	m.packageMu.Lock()
	h := m.packageRuntime
	m.packageMu.Unlock()
	if h != nil {
		h.SetIdleTimeout(time.Duration(retention.PluginMinutes) * time.Minute)
	}
	m.refreshMusicIdleTimeout()
}

func (s *Server) pluginRuntimeRoute(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/plugin-packages/{id}/runtime", s.protected(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if s.manager.mobileRetired && id == mobileDevicePluginID {
			fail(w, http.StatusGone, errDeviceServiceRetired.Error())
			return
		}
		var input struct {
			Action         string `json:"action"`
			Revision       int    `json:"revision"`
			TimeoutSeconds *int   `json:"timeout_seconds"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.Action != "start" && input.Action != "stop" && input.Action != "enable_on_demand" {
			fail(w, 400, "请选择按需启用、启动或停止")
			return
		}
		seconds := 30
		if input.TimeoutSeconds != nil {
			seconds = *input.TimeoutSeconds
		}
		if seconds < 0 || int64(seconds) > int64((time.Duration(1<<63-1))/time.Second) {
			fail(w, 400, "启动等待时间无效")
			return
		}
		if s.manager.draining() {
			fail(w, 409, "服务正在收尾，请稍后重试")
			return
		}
		h, _, err := s.manager.installedPluginRuntime()
		if err != nil {
			fail(w, 503, "工具执行环境未就绪")
			return
		}
		ctx := r.Context()
		if seconds > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
			defer cancel()
		}
		switch input.Action {
		case "enable_on_demand":
			err = h.EnableOnDemand(ctx, id, input.Revision)
		case "start":
			err = h.Start(ctx, id, input.Revision)
		case "stop":
			err = h.Stop(ctx, id, input.Revision)
		}
		if err != nil {
			var dependency *pluginruntime.DependencyError
			if errors.As(err, &dependency) {
				message := pluginDependencyMessage(dependency)
				respond(w, 409, map[string]any{"error": message, "dependency": dependency})
				return
			}
			fail(w, 409, "操作未完成：请检查配置、授权、依赖或运行环境；有在途调用时需先等待结束。刷新后可核对当前状态。")
			return
		}
		store, err := s.manager.packageStore()
		if err != nil {
			fail(w, 503, "工具宿主正在关闭")
			return
		}
		i, err := store.Installed(r.Context(), id)
		if err != nil {
			fail(w, 409, "工具状态已变化，请刷新")
			return
		}
		respond(w, 200, map[string]any{"installed": i, "runtime": h.Status(id)})
	}))
}

// pluginDependencyMessage is shared by lifecycle actions and readiness
// preflight, so the same dependency condition tells an administrator the same
// concrete next step. It contains only package identifiers and stable codes.
func pluginDependencyMessage(dependency *pluginruntime.DependencyError) string {
	message := "工具依赖尚未就绪：" + dependency.Dependency
	switch dependency.Code {
	case "dependency_missing", "dependency_removed":
		message = "请先安装依赖工具：" + dependency.Dependency
	case "dependency_disabled":
		message = "请先启用依赖工具：" + dependency.Dependency
	case "dependency_cycle":
		message = "工具存在循环依赖，请检查工具声明：" + dependency.Dependency
	case "dependency_configuration":
		message = "请先完成工具配置：" + dependency.Dependency
	case "dependency_not_authorized":
		message = "工具尚未获得所需授权：" + dependency.Dependency
	case "dependency_busy":
		message = "关联工具正在执行操作，请稍后重试：" + dependency.Dependency
	case "dependency_start_failed":
		message = "工具启动失败：" + dependency.Dependency
		if len(dependency.Started) > 0 {
			message += "；已启动的依赖保持运行，可在工具详情查看。"
		}
	}
	return message
}
