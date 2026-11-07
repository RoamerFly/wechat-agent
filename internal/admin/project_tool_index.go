package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// packagedTool is deliberately a small, fixed index.  It is not a directory
// browser and it never discovers the desktop host, broker, account data or
// configuration files.  The returned paths are public runtime files that are
// already mounted read-only for a normal workspace request.
type packagedTool struct {
	ID          string   `json:"id"`
	Class       string   `json:"class"`
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Available   bool     `json:"available"`
	Version     string   `json:"version,omitempty"`
	Invocation  string   `json:"invocation"`
	VersionCall string   `json:"version_command"`
	VersionArgv []string `json:"version_argv"`
	Repeat      string   `json:"repeat"`
}

type packagedRuntimeManifest struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}

type capabilityProxy struct {
	Plugin        string   `json:"plugin"`
	Class         string   `json:"class"`
	Installed     bool     `json:"installed"`
	Enabled       bool     `json:"enabled"`
	Configured    bool     `json:"configured"`
	Discoverable  bool     `json:"discoverable"`
	Permissions   []string `json:"permissions"`
	DiscoveryNext string   `json:"discovery_next"`
}

type packagedToolSpec struct {
	ID, Class, Name, Directory, File, VersionCommand string
}

var packagedToolSpecs = []packagedToolSpec{
	{ID: "media.ffmpeg", Class: "media", Name: "FFmpeg", Directory: "windows-media", File: "ffmpeg.exe", VersionCommand: "-version"},
	{ID: "media.ffprobe", Class: "media", Name: "FFprobe", Directory: "windows-media", File: "ffprobe.exe", VersionCommand: "-version"},
	{ID: "python.cli", Class: "python", Name: "Python CLI", Directory: "windows-python-cli", File: "python.exe", VersionCommand: "--version"},
	{ID: "python.cli-v2", Class: "python", Name: "Python CLI v2", Directory: "windows-python-cli-v2", File: "python.exe", VersionCommand: "--version"},
}

func containedPublicRuntime(root, candidate string) (string, bool) {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	info, err := os.Stat(resolved)
	return resolved, err == nil && info.Mode().IsRegular()
}

// toolIndex reports only concrete, packaged public runtimes.  The agent can
// select a class or exact id, then use the existing platform process tool with
// the returned path.  We do not execute version probes here: discovery remains
// read-only and process identity/retry stays in server_windows_process.
func (p *projectPluginPolicy) toolIndex(ctx context.Context) projectPluginObservation {
	if err := ctx.Err(); err != nil {
		return projectPluginObservation{Error: err.Error()}
	}
	var request struct {
		Classes []string `json:"classes"`
		ID      string   `json:"id"`
	}
	if musicStrictJSON(p.request.Arguments, &request) != nil {
		return projectPluginObservation{Error: "tool_index_arguments_invalid"}
	}
	for _, class := range request.Classes {
		if !slices.Contains([]string{"media", "python"}, class) {
			return projectPluginObservation{Error: "tool_index_class_invalid"}
		}
	}
	if request.ID != "" && !slices.ContainsFunc(packagedToolSpecs, func(spec packagedToolSpec) bool { return spec.ID == request.ID }) {
		return projectPluginObservation{Error: "tool_index_id_unknown"}
	}

	x := p.p.executionContext()
	runtimeRoots := []string{filepath.Join(x.Public, "runtimes")}
	// Keep the deployment's single packaged CLI/media directory usable without
	// copying it into every account. hostConfig admits only its two safe child
	// directories, so use their parent only after that policy has accepted it.
	if cfg, err := p.p.hostConfig(); err == nil && cfg.Isolation != nil {
		for _, mounted := range cfg.Isolation.ReadOnly {
			base := filepath.Base(mounted)
			if base == "windows-media" || base == "windows-python-cli" {
				root := filepath.Dir(mounted)
				if !slices.Contains(runtimeRoots, root) {
					runtimeRoots = append(runtimeRoots, root)
				}
			}
		}
	}
	rows := make([]packagedTool, 0, len(packagedToolSpecs))
	for _, spec := range packagedToolSpecs {
		if request.ID != "" && request.ID != spec.ID || len(request.Classes) > 0 && !slices.Contains(request.Classes, spec.Class) {
			continue
		}
		binary := filepath.Join(runtimeRoots[0], spec.Directory, spec.File)
		row := packagedTool{ID: spec.ID, Class: spec.Class, Name: spec.Name, Path: binary, Invocation: "server_windows_process", Repeat: "使用新的request_key再次调用server_windows_process；相同request_key只查询原进程，绝不重放未知执行"}
		for _, runtimeRoot := range runtimeRoots {
			candidate := filepath.Join(runtimeRoot, spec.Directory, spec.File)
			if resolved, ok := containedPublicRuntime(runtimeRoot, candidate); ok {
				row.Path, row.Available = resolved, true
				manifestPath := filepath.Join(runtimeRoot, spec.Directory, "runtime-manifest.json")
				data, err := os.ReadFile(manifestPath)
				var manifest packagedRuntimeManifest
				if err == nil && json.Unmarshal(data, &manifest) == nil && manifest.Files != nil && manifest.Files[spec.File] != "" {
					row.Version = strings.TrimSpace(manifest.Version)
				}
				break
			}
		}
		row.VersionCall = `"` + row.Path + `" ` + spec.VersionCommand
		row.VersionArgv = []string{row.Path, spec.VersionCommand}
		rows = append(rows, row)
	}
	proxies, proxyIndexError := []capabilityProxy{}, ""
	if store, err := p.p.manager.packageStore(); err == nil {
		if installed, listErr := store.List(ctx); listErr == nil {
			for _, item := range installed {
				// Keep archived device work visible through its history endpoints,
				// but never advertise mobile-control through the generic project
				// discovery index on Windows production.
				if item.Package.ID == mobileDevicePluginID && p.p.manager.mobileRetired {
					continue
				}
				class := map[string]string{
					"music":               "gpu-media",
					"speech":              "speech",
					"media-understanding": "media-analysis",
					"mobile-control":      "device",
					"share-reader":        "share-reading",
					"web-search":          "network",
					"visual-generation":   "visual-generation",
					"external-tools":      "external-connection",
				}[item.Package.ID]
				if class == "" {
					class = "other"
				}
				caps, capErr := store.Capabilities(ctx, item.Package.ID)
				discoverable := false
				if capErr == nil {
					for _, cap := range caps {
						discoverable = discoverable || cap.Discoverable && cap.Implementation == "ready"
					}
				}
				proxies = append(proxies, capabilityProxy{Plugin: item.Package.ID, Class: class, Installed: !item.Removed, Enabled: item.Enabled && !item.Removed, Configured: store.CheckConfiguration(ctx, item.Package.ID) == nil, Discoverable: discoverable, Permissions: append([]string(nil), item.Permissions...), DiscoveryNext: "通过正常Agent工具列表按当前账号、会话和授权重新发现；原始二进制、服务控制文件和凭据路径不公开"})
			}
		} else {
			proxyIndexError = "plugin_proxy_index_unavailable"
		}
	} else {
		proxyIndexError = "plugin_proxy_store_unavailable"
	}
	value, err := json.Marshal(map[string]any{
		"tools":                        rows,
		"capability_proxies":           proxies,
		"capability_proxy_index_error": proxyIndexError,
		"usage":                        "选择已安装的工具后，Windows以server_windows_process(action=start,path=当前会话目录,argv=version_argv或自定义参数)直接启动返回的EXE；不要把EXE路径误作工作目录。Linux运行节点不会把Windows EXE伪装为可执行。项目读写继续用project_list/project_read、server_read/write/edit；桌面、微信、账号、凭据和隔离代理不属于此索引，仍只能使用各自受控能力。",
	})
	if err != nil {
		return projectPluginObservation{Error: errors.New("tool_index_encode_failed").Error()}
	}
	return projectPluginObservation{Value: value}
}
