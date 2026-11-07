package stationruntime

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func TestPackagedToolOptionsIsolatedFromEditableWorkspace(t *testing.T) {
	packageRoot := t.TempDir()
	workspace := filepath.Join(packageRoot, "fish-home")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	options := PackagedToolOptionsAt(packageRoot)
	writeToolRuntimeFixture(t, options)

	if !ToolRuntimeReady(options, workspace) {
		t.Fatal("isolated packaged tool runtime should be ready")
	}
}

func TestToolRuntimeRejectsEditableWorkspaceOverlap(t *testing.T) {
	packageRoot := t.TempDir()
	workspace := filepath.Join(packageRoot, "fish-home")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	base := PackagedToolOptionsAt(packageRoot)
	writeToolRuntimeFixture(t, base)

	for _, update := range []struct {
		name  string
		apply func(*toolFixtureOptions)
	}{
		{"python", func(f *toolFixtureOptions) { f.options.Python = f.requiredFile("python.exe") }},
		{"pythonw", func(f *toolFixtureOptions) { f.options.Pythonw = f.requiredFile("pythonw.exe") }},
		{"lpac_helper", func(f *toolFixtureOptions) { f.options.WindowsHelper = f.requiredHelper() }},
		{"document_runtime", func(f *toolFixtureOptions) { f.options.DocumentRuntimeRoot = f.inWorkspace("document-workshop") }},
		{"runtime_root", func(f *toolFixtureOptions) { f.options.RuntimeRoots = []string{f.inWorkspace("runtime-root")} }},
	} {
		t.Run(update.name, func(t *testing.T) {
			fixture := toolFixtureOptions{options: base, workspace: workspace}
			update.apply(&fixture)
			if !ToolRuntimeReady(fixture.options, "") {
				t.Fatal("overlap policy fixture unexpectedly made required files unavailable")
			}
			if ToolRuntimeReady(fixture.options, workspace) {
				t.Fatal("tool runtime inside editable workspace was accepted")
			}
		})
	}
}

func TestToolRuntimeRejectsWorkspaceNestedInRuntimeRoot(t *testing.T) {
	packageRoot := t.TempDir()
	options := PackagedToolOptionsAt(packageRoot)
	writeToolRuntimeFixture(t, options)
	workspace := filepath.Join(options.RuntimeRoots[0], "fish-home")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if ToolRuntimeReady(options, workspace) {
		t.Fatal("runtime root containing editable workspace was accepted")
	}
}

func TestToolRuntimeRejectsWorkspaceNestedInLPACDirectory(t *testing.T) {
	packageRoot := t.TempDir()
	options := PackagedToolOptionsAt(packageRoot)
	writeToolRuntimeFixture(t, options)
	workspace := filepath.Join(filepath.Dir(options.WindowsHelper), "fish-home")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if ToolRuntimeReady(options, workspace) {
		t.Fatal("LPAC helper directory containing editable workspace was accepted")
	}
}

func TestInstalledToolExecutorChecksRuntimeOnEachLaunch(t *testing.T) {
	base := t.TempDir()
	options := PackagedToolOptionsAt(filepath.Join(base, "package"))
	workspace := filepath.Join(base, "fish-home")
	store, err := pluginhost.OpenStore(filepath.Join(base, "private"), "synthetic-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executor := InstalledToolExecutor(store, options, workspace)
	check := func(executor *pluginruntime.Executor, expected error) {
		t.Helper()
		_, err := executor.Resolve(context.Background(), pluginhost.Package{ID: "uninstalled"}, pluginhost.Grant{})
		if !errors.Is(err, expected) {
			t.Fatalf("admission returned %v, want %v", err, expected)
		}
	}
	check(executor, errToolRuntimeUnavailable)
	writeToolRuntimeFixture(t, options)
	// The same controller can now resolve packages without a service restart.
	check(executor, sql.ErrNoRows)
	check(InstalledToolExecutor(store, options, filepath.Dir(options.WindowsHelper)), errToolRuntimeUnavailable)
	if err := os.Remove(options.WindowsHelper); err != nil {
		t.Fatal(err)
	}
	check(executor, errToolRuntimeUnavailable)
}

type toolFixtureOptions struct {
	options   pluginruntime.Options
	workspace string
}

func (f *toolFixtureOptions) inWorkspace(name string) string {
	return filepath.Join(f.workspace, name)
}

func (f *toolFixtureOptions) requiredFile(name string) string {
	path := f.inWorkspace(name)
	if err := os.WriteFile(path, []byte("synthetic"), 0o644); err != nil {
		panic(err)
	}
	return path
}

func (f *toolFixtureOptions) requiredHelper() string {
	helper := f.requiredFile("windows_plugin_host.pyw")
	if err := os.WriteFile(filepath.Join(filepath.Dir(helper), "windows_lpac.py"), []byte("synthetic"), 0o644); err != nil {
		panic(err)
	}
	return helper
}

func writeToolRuntimeFixture(t *testing.T, options pluginruntime.Options) {
	t.Helper()
	for _, path := range []string{
		options.Python,
		options.Pythonw,
		options.WindowsHelper,
		filepath.Join(filepath.Dir(options.WindowsHelper), "windows_lpac.py"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
