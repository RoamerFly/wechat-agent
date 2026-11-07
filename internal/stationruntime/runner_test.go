package stationruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/agenthost"
)

func writeRunnerFixture(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

func runnerFixture(t *testing.T, packageDir string) agenthost.WindowsConfig {
	t.Helper()
	cfg := agenthost.WindowsConfig{
		Pythonw: filepath.Join(packageDir, "runtimes", "windows-python-cli", "pythonw.exe"),
		Helper:  filepath.Join(packageDir, "runtimes", "windows-agent-host", "windows_agent_host.pyw"),
	}
	for _, path := range []string{cfg.Pythonw, cfg.Helper} {
		writeRunnerFixture(t, path, []byte("synthetic runner; never executed"))
	}
	return cfg
}

func TestWindowsRunnerUsesPackagedFilesAndPrivateSpool(t *testing.T) {
	base := t.TempDir()
	private, workspace := filepath.Join(base, "private"), filepath.Join(base, "workspace")
	pkg := filepath.Join(base, "package")
	if cfg, err := LoadWindowsRunner(pkg, private, workspace, ""); err != nil || cfg != nil {
		t.Fatalf("uninstalled runner: %#v %v", cfg, err)
	}
	fixture := runnerFixture(t, pkg)
	cfg, err := LoadWindowsRunner(pkg, private, workspace, "")
	if err != nil || cfg == nil || cfg.Pythonw != fixture.Pythonw || cfg.Helper != fixture.Helper ||
		cfg.Spool != filepath.Join(private, "local-execution-spool") {
		t.Fatalf("packaged runner: %#v %v", cfg, err)
	}
	fixture.Spool = filepath.Join(workspace, "editable-spool")
	file := filepath.Join(base, "runtime.json")
	content, _ := json.Marshal(fixture)
	writeRunnerFixture(t, file, content)
	cfg, err = LoadWindowsRunner(pkg, private, workspace, filepath.ToSlash(file))
	if err != nil || cfg == nil || cfg.Spool != filepath.Join(private, "local-execution-spool") {
		t.Fatalf("configured runner moved the control plane: %#v %v", cfg, err)
	}
}

func TestWindowsRunnerRejectsUnsafeFilesAndSpool(t *testing.T) {
	base := t.TempDir()
	private, workspace := filepath.Join(base, "private"), filepath.Join(base, "workspace")
	valid := runnerFixture(t, filepath.Join(base, "package"))
	edited := runnerFixture(t, workspace)
	optionalShell := filepath.Join(workspace, "shell.exe")
	writeRunnerFixture(t, optionalShell, []byte("synthetic; never executed"))

	for _, candidate := range []agenthost.WindowsConfig{
		{Pythonw: valid.Pythonw, Helper: filepath.Dir(valid.Helper)},
		{Pythonw: filepath.Join(base, "missing", "pythonw.exe"), Helper: valid.Helper},
		{Pythonw: valid.Pythonw, Helper: edited.Helper},
		{Pythonw: edited.Pythonw, Helper: valid.Helper},
		{Pythonw: valid.Pythonw, Helper: valid.Helper, Shell: optionalShell},
		{Pythonw: valid.Pythonw, Helper: valid.Helper, Shell: "cmd.exe"},
	} {
		if got, err := BindWindowsRunner(&candidate, private, workspace); err == nil || got != nil {
			t.Fatalf("unsafe runner admitted: %#v %v", got, err)
		}
	}
	if got, err := BindWindowsRunner(&valid, filepath.Join(workspace, "private"), workspace); err == nil || got != nil {
		t.Fatalf("editable control directory admitted: %#v %v", got, err)
	}
	library := filepath.Join(filepath.Dir(valid.Pythonw), "editable-library")
	if got, err := BindWindowsRunner(&valid, private, library); err == nil || got != nil {
		t.Fatalf("editable subdirectory inside interpreter dependencies admitted: %#v %v", got, err)
	}
	writeRunnerFixture(t, filepath.Join(private, "local-execution-spool"), []byte("synthetic file in place of directory"))
	if got, err := BindWindowsRunner(&valid, private, workspace); err == nil || got != nil {
		t.Fatalf("non-directory control path admitted: %#v %v", got, err)
	}
}

func TestWindowsRunnerConfigIsBoundedAndOutsideWorkspace(t *testing.T) {
	base := t.TempDir()
	private, workspace := filepath.Join(base, "private"), filepath.Join(base, "workspace")
	pkg := filepath.Join(base, "package")
	fixture := runnerFixture(t, pkg)
	encoded, _ := json.Marshal(fixture)
	file := filepath.Join(base, "runtime.json")
	check := func(path string, value []byte, valid bool) {
		t.Helper()
		writeRunnerFixture(t, path, value)
		got, err := LoadWindowsRunner(pkg, private, workspace, path)
		if (err == nil && got != nil) != valid {
			t.Fatalf("credential-independent runner admission matched expected=%t: %v", valid, err)
		}
	}
	exact := append(encoded, strings.Repeat(" ", runnerConfigLimit-len(encoded))...)
	check(file, exact, true)
	check(file, append(exact, ' '), false)
	check(file, append(encoded, []byte(" {}")...), false)
	check(filepath.Join(workspace, "runtime.json"), encoded, false)
}
