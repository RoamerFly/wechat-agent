package stationruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

// PackagedToolOptions resolves the tool-runtime layout beside the station
// executable. It deliberately does not look on PATH: a station with no
// packaged tool runtime remains usable, but cannot start local tool helpers.
func PackagedToolOptions() pluginruntime.Options {
	exe, err := os.Executable()
	if err != nil {
		return pluginruntime.Options{}
	}
	return PackagedToolOptionsAt(filepath.Dir(exe))
}

// PackagedToolOptionsAt is the deterministic counterpart used by package
// assembly and tests. packageDir is the root containing runtimes/ and
// plugins/, not a user-editable Agent workspace.
func PackagedToolOptionsAt(packageDir string) pluginruntime.Options {
	python := filepath.Join(packageDir, "runtimes", "windows-python-cli")
	return pluginruntime.Options{
		Python:              filepath.Join(python, "python.exe"),
		Pythonw:             filepath.Join(python, "pythonw.exe"),
		WindowsHelper:       filepath.Join(packageDir, "runtimes", "windows-agent-host", "windows_plugin_host.pyw"),
		RuntimeRoots:        []string{python},
		CatalogDirectory:    filepath.Join(packageDir, "plugins"),
		DocumentRuntimeRoot: filepath.Join(packageDir, "runtimes", "document-workshop"),
	}
}

// ToolRuntimeReady reports whether the packaged Windows interpreter and LPAC
// helper are host-owned ordinary files. It intentionally does not require
// optional tool packages or document tooling to exist. workspace may be empty
// only for a trusted, non-Agent-owned test fixture.
func ToolRuntimeReady(options pluginruntime.Options, workspace string) bool {
	if !toolRuntimeOutsideWorkspace(options, workspace) {
		return false
	}
	for _, path := range []string{
		options.Python,
		options.Pythonw,
		options.WindowsHelper,
		filepath.Join(filepath.Dir(options.WindowsHelper), "windows_lpac.py"),
	} {
		file, err := openHostFile(path)
		if err != nil {
			return false
		}
		file.Close()
	}
	return true
}

var errToolRuntimeUnavailable = errors.New("Windows 工具运行环境未安装或路径不安全")

// InstalledToolExecutor checks admission on every cold launch. A missing
// optional runtime does not freeze the controller's availability until restart.
func InstalledToolExecutor(store *pluginhost.Store, options pluginruntime.Options, workspace string) *pluginruntime.Executor {
	executor := pluginruntime.InstalledExecutor(store, options)
	resolve := executor.Resolve
	executor.Resolve = func(ctx context.Context, p pluginhost.Package, g pluginhost.Grant) (pluginruntime.LaunchSpec, error) {
		if !ToolRuntimeReady(options, workspace) {
			return pluginruntime.LaunchSpec{}, errToolRuntimeUnavailable
		}
		return resolve(ctx, p, g)
	}
	return executor
}

// toolRuntimeOutsideWorkspace ensures the executable helpers and every
// runtime root are host-owned rather than writable by the Agent. A workspace
// overlap is unsafe even when the current files look ordinary: the Agent can
// replace them between later launches. Empty optional locations are ignored.
func toolRuntimeOutsideWorkspace(options pluginruntime.Options, workspace string) bool {
	for _, path := range []string{
		options.Python,
		options.Pythonw,
		options.WindowsHelper,
	} {
		if path != "" && withinWorkspace(workspace, path) {
			return false
		}
	}
	for _, dir := range []string{
		filepath.Dir(options.Python),
		filepath.Dir(options.Pythonw),
		filepath.Dir(options.WindowsHelper), // also contains windows_lpac.py
		options.DocumentRuntimeRoot,
	} {
		if dir != "" && dir != "." && overlapsWorkspace(workspace, dir) {
			return false
		}
	}
	for _, root := range options.RuntimeRoots {
		if root != "" && overlapsWorkspace(workspace, root) {
			return false
		}
	}
	return true
}
