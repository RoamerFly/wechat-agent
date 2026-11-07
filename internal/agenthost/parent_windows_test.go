//go:build windows

package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type syntheticBrokerParentSpec struct {
	Root    string `json:"root"`
	Runtime string `json:"runtime"`
	Helper  string `json:"helper"`
}

// TestSyntheticSpeechBrokerParent is run in a disposable Go process.  It
// intentionally exits without waiting for its hidden Python broker; the outer
// test then proves both the broker and LPAC child were terminated.
func TestSyntheticSpeechBrokerParent(t *testing.T) {
	if os.Getenv("WECHAT_AGENTHOST_PARENT_FIXTURE") != "1" {
		return
	}
	var spec syntheticBrokerParentSpec
	if json.Unmarshal([]byte(os.Getenv("WECHAT_AGENTHOST_PARENT_SPEC")), &spec) != nil {
		os.Exit(2)
	}
	work, spool := filepath.Join(spec.Root, "work"), filepath.Join(spec.Root, "spool")
	if err := os.MkdirAll(work, 0700); err != nil {
		os.Exit(3)
	}
	marker := filepath.Join(work, "child.pid")
	cfg := Defaults()
	cfg.Mode = "full"
	cfg.Roots = []string{work}
	cfg.Isolation = &Isolation{ReadOnly: []string{spec.Runtime}}
	cfg.Windows = &WindowsConfig{Pythonw: filepath.Join(spec.Runtime, "pythonw.exe"), Helper: spec.Helper, Spool: spool}
	argv := []string{filepath.Join(spec.Runtime, "python.exe"), "-X", "utf8", "-c", "import os,pathlib,sys,time; pathlib.Path(sys.argv[1]).write_text(str(os.getpid()), encoding='ascii'); time.sleep(60)", marker}
	go func() { _, _, _ = RunSpeechIsolated(context.Background(), cfg, work, argv, nil) }()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(spool)
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(spool, entry.Name(), "running.json"))
			child, markerErr := os.ReadFile(marker)
			if err != nil || markerErr != nil {
				continue
			}
			var running struct {
				PID       int `json:"pid"`
				HelperPID int `json:"helper_pid"`
			}
			if json.Unmarshal(raw, &running) == nil && running.PID > 0 && running.HelperPID > 0 && strings.TrimSpace(string(child)) != "" {
				result, _ := json.Marshal(map[string]int{"broker": running.HelperPID, "lpac_child": running.PID})
				fmt.Println(string(result))
				// Simulated host crash: no cancellation/receipt/cleanup path runs.
				os.Exit(0)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	os.Exit(4)
}

func TestLiveSpeechBrokerAndLPACDieWithParent(t *testing.T) {
	runtimeRoot := os.Getenv("WECHAT_AGENTHOST_PARENT_RUNTIME")
	if runtimeRoot == "" {
		runtimeRoot = os.Getenv("WECHAT_WINDOWS_SPEECH_TEST_RUNTIME")
	}
	if runtimeRoot == "" {
		runtimeRoot = "../../temp/local-station-preview-5E7-final/runtimes/windows-python-cli"
	}
	var err error
	runtimeRoot, err = filepath.Abs(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("WECHAT_AGENTHOST_PARENT_TEST") != "1" || !fileExists(filepath.Join(runtimeRoot, "python.exe")) || !fileExists(filepath.Join(runtimeRoot, "pythonw.exe")) {
		t.Skip("opt-in packaged Windows Python parent-death test")
	}
	helper, err := filepath.Abs("../../scripts/windows_agent_host.pyw")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	raw, _ := json.Marshal(syntheticBrokerParentSpec{Root: root, Runtime: runtimeRoot, Helper: helper})
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSyntheticSpeechBrokerParent$")
	cmd.Env = append(os.Environ(), "WECHAT_AGENTHOST_PARENT_FIXTURE=1", "WECHAT_AGENTHOST_PARENT_SPEC="+string(raw))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("synthetic parent failed: %v: %s", err, out)
	}
	var pids map[string]int
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			_ = json.Unmarshal([]byte(line), &pids)
		}
	}
	if len(pids) != 2 || pids["broker"] <= 0 || pids["lpac_child"] <= 0 {
		t.Fatalf("missing child process evidence: %q", out)
	}
	for name, pid := range pids {
		h, openErr := syscall.OpenProcess(0x00100000, false, uint32(pid)) // SYNCHRONIZE
		if openErr == syscall.Errno(87) {                                 // PID already reaped
			continue
		}
		if openErr != nil {
			t.Fatalf("cannot inspect %s (%d): %v", name, pid, openErr)
		}
		state, waitErr := syscall.WaitForSingleObject(h, 5000)
		syscall.CloseHandle(h)
		if waitErr != nil || state != 0 {
			t.Fatalf("orphaned %s after parent crash: pid=%d state=%d err=%v", name, pid, state, waitErr)
		}
	}
	// The offline speech request carried the synthetic argv only in memory.
	// Parent death must not forge a normal terminal receipt after it was scrubbed.
	entries, err := os.ReadDir(filepath.Join(root, "spool"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing crash spool evidence: %v", err)
	}
	dir := filepath.Join(root, "spool", entries[0].Name())
	request, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil || strings.Contains(string(request), "argv") || !strings.Contains(string(request), "offline_speech_request_arguments") {
		t.Fatalf("speech request was not scrubbed before crash: %v %q", err, request)
	}
	if _, err := os.Stat(filepath.Join(dir, "result.json")); !os.IsNotExist(err) {
		t.Fatalf("parent-death broker forged a terminal receipt: %v", err)
	}
}

func fileExists(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && stat.Mode().IsRegular()
}
