//go:build windows

package stationbrowser

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// This launches only the installed Edge binary with a test-owned profile and
// navigates to a data URL.  No listener, normal profile, or network URL is
// involved; Close must kill the job before the temporary profile is removed.
func TestPrivateEdgePipe(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed in Program Files")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	profile := t.TempDir()
	s, err := openCDP(ctx, profile)
	if err != nil {
		t.Fatalf("open private CDP: %v", err)
	}
	defer s.Close()
	var version struct {
		Product string `json:"product"`
	}
	if err := s.Call(ctx, "", "Browser.getVersion", nil, &version); err != nil {
		t.Fatalf("Browser.getVersion: %v", err)
	}
	if version.Product == "" {
		t.Fatal("missing browser product")
	}
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := s.Call(ctx, "", "Target.createTarget", map[string]any{"url": "data:text/html,private-cdp"}, &target); err != nil {
		t.Fatalf("Target.createTarget: %v", err)
	}
	if target.TargetID == "" {
		t.Fatal("missing target id")
	}
	if _, err := os.Stat(filepath.Join(profile, "DevToolsActivePort")); !os.IsNotExist(err) {
		t.Fatalf("private pipe unexpectedly exposed a debug port file: %v", err)
	}
	var processes struct {
		ProcessInfo []struct {
			Type string `json:"type"`
			ID   uint32 `json:"id"`
		} `json:"processInfo"`
	}
	if err := s.Call(ctx, "", "SystemInfo.getProcessInfo", nil, &processes); err != nil {
		t.Fatal(err)
	}
	for _, process := range processes.ProcessInfo {
		if process.Type != "browser" || process.ID == 0 {
			continue
		}
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, process.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(handle)
		cancel()
		result, err := windows.WaitForSingleObject(handle, 3000)
		if err != nil || result != windows.WAIT_OBJECT_0 {
			t.Fatalf("cancel did not stop the owned Edge process: %v %v", result, err)
		}
		return
	}
	t.Fatal("browser process missing from private CDP session")
}

func TestCDPConcurrentCloseAndCancel(t *testing.T) {
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inRead.Close()
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outWrite.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s := newCDPSession(inWrite, outRead, func() error { return nil })
	s.start(ctx)
	go func() {
		for i := 0; i < 32; i++ {
			_, _ = outWrite.Write([]byte(`{"method":"Page.frameNavigated","params":{}}` + "\x00"))
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Close() }()
	}
	cancel()
	wg.Wait()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not stop")
	}
	select {
	case _, ok := <-s.Events():
		if ok {
			for range s.Events() {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events not closed")
	}
}
