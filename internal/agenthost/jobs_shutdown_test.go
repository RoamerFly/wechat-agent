package agenthost

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBackgroundProcessBeginShutdownClosesAdmissionWithoutStarting(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows argv admission contract")
	}
	root := t.TempDir()
	exe := filepath.Join(root, "synthetic.exe")
	if err := os.WriteFile(exe, []byte("not executable; must never start"), 0600); err != nil {
		t.Fatal(err)
	}
	jobs, err := OpenJobs(filepath.Join(root, "records"))
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	jobs.BeginShutdown()
	cfg := Config{Mode: "full", Roots: []string{root}, TimeoutSeconds: 60, OutputBytes: 16000,
		Isolation: &Isolation{}, Windows: &WindowsConfig{Pythonw: filepath.Join(root, "pythonw.exe"), Helper: filepath.Join(root, "helper.pyw"), Spool: filepath.Join(root, "spool")}}
	_, err = jobs.StartWindowsArgv(cfg, "synthetic-scope", "never-start", root, []string{exe}, 1, func() bool {
		t.Error("shutdown must reject admission before checking a new grant")
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "进程管理器已关闭") {
		t.Fatalf("closed registry admitted a start: %v", err)
	}
	if len(jobs.List("synthetic-scope")) != 0 {
		t.Fatal("shutdown created a durable job")
	}
}
