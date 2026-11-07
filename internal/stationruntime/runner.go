// Package stationruntime resolves host-owned runners for the local station.
package stationruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/workspacefs"
)

const runnerConfigLimit = 64 << 10

// LoadWindowsRunner resolves an explicit owner configuration or the station's
// packaged runner. A package without a runner can still start normally.
func LoadWindowsRunner(packageDir, privateDir, workspace, configFile string) (*agenthost.WindowsConfig, error) {
	var cfg agenthost.WindowsConfig
	if configFile == "" {
		cfg.Pythonw = filepath.Join(packageDir, "runtimes", "windows-python-cli", "pythonw.exe")
		cfg.Helper = filepath.Join(packageDir, "runtimes", "windows-agent-host", "windows_agent_host.pyw")
		for _, path := range []string{cfg.Pythonw, cfg.Helper} {
			if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
		}
	} else {
		if !filepath.IsAbs(configFile) {
			return nil, errors.New("Windows 运行器配置需要绝对路径")
		}
		configFile = filepath.Clean(configFile)
		if withinWorkspace(workspace, configFile) {
			return nil, errors.New("Windows 运行器配置不能放在大肥鱼可修改的工作空间")
		}
		file, err := openHostFile(configFile)
		if err != nil {
			return nil, errors.New("Windows 运行器配置不可读或经过链接")
		}
		raw, err := io.ReadAll(io.LimitReader(file, runnerConfigLimit+1))
		file.Close()
		if err != nil || len(raw) > runnerConfigLimit {
			return nil, errors.New("Windows 运行器配置不可读或超过 64 KiB")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return nil, errors.New("Windows 运行器配置无效")
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, errors.New("Windows 运行器配置不能包含多份文档")
		}
	}
	return BindWindowsRunner(&cfg, privateDir, workspace)
}

// BindWindowsRunner pins the control spool to the private data root and checks
// executables again at the station boundary, including direct callers.
func BindWindowsRunner(cfg *agenthost.WindowsConfig, privateDir, workspace string) (*agenthost.WindowsConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	bound := *cfg
	bound.Spool = filepath.Join(privateDir, "local-execution-spool")
	if err := bound.Validate(); err != nil {
		return nil, errors.New("本机 Windows 运行器路径配置无效")
	}
	paths := []string{bound.Pythonw, bound.Helper}
	if bound.Shell != "" {
		paths = append(paths, bound.Shell)
	}
	for _, path := range paths {
		if withinWorkspace(workspace, path) {
			return nil, errors.New("宿主运行器不能放在大肥鱼可修改的工作空间")
		}
		file, err := openHostFile(path)
		if err != nil {
			return nil, errors.New("Windows 运行器文件缺失、不是普通文件或经过链接")
		}
		file.Close()
	}
	for _, dir := range []string{filepath.Dir(bound.Pythonw), filepath.Dir(bound.Helper)} {
		if overlapsWorkspace(workspace, dir) {
			return nil, errors.New("Windows 运行器依赖目录不能与工作空间重叠")
		}
	}
	if withinWorkspace(workspace, bound.Spool) {
		return nil, errors.New("Windows 运行器控制目录不能放在工作空间")
	}
	if dir, err := workspacefs.OpenPath(bound.Spool); err == nil {
		dir.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("Windows 运行器控制目录不可用或经过链接")
	}
	return &bound, nil
}

func openHostFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("host file must have a clean absolute path")
	}
	root, err := workspacefs.OpenPath(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, workspacefs.ErrNotRegular
	}
	file, err := workspacefs.OpenRegular(root, name)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		file.Close()
		return nil, errors.New("host file changed while opening")
	}
	return file, nil
}

func overlapsWorkspace(workspace, dir string) bool {
	return withinWorkspace(workspace, dir) || withinWorkspace(dir, workspace) || withinWorkspace(dir, resolvedWorkspace(workspace))
}

func withinWorkspace(workspace, path string) bool {
	if workspace == "" || path == "" {
		return false
	}
	for _, root := range []string{workspace, resolvedWorkspace(workspace)} {
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func resolvedWorkspace(workspace string) string {
	if path, err := filepath.EvalSymlinks(workspace); err == nil {
		return path
	}
	return workspace
}

// PackagedCommandRuntimeRoots admits only the packaged command dependencies.
// Each returned directory is a read-only mount for the caller.
func PackagedCommandRuntimeRoots(w *agenthost.WindowsConfig) []string {
	if w == nil || !strings.EqualFold(filepath.Base(w.Pythonw), "pythonw.exe") || !strings.EqualFold(filepath.Base(filepath.Dir(w.Pythonw)), "windows-python-cli") {
		return nil
	}
	runtimeRoot := filepath.Dir(filepath.Dir(w.Pythonw))
	var roots []string
	for _, name := range []string{"windows-python-cli", "windows-media"} {
		candidate := filepath.Join(runtimeRoot, name)
		root, err := workspacefs.OpenPath(candidate)
		if err == nil {
			root.Close()
			roots = append(roots, candidate)
		}
	}
	return roots
}
