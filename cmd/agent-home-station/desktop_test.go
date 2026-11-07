package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type stationStartupWriter struct {
	once sync.Once
	line chan string
}

func (w *stationStartupWriter) Write(data []byte) (int, error) {
	text := string(data)
	if strings.Contains(text, "Agent Home Station 已启动：") {
		w.once.Do(func() { w.line <- text })
	}
	return len(data), nil
}

func TestDesktopStopUsesExplicitPipeAndEOF(t *testing.T) {
	for _, input := range []string{"stop\n", "ignored\nstop\n", ""} {
		select {
		case <-desktopStop(strings.NewReader(input)):
		case <-time.After(time.Second):
			t.Fatal("desktop stop pipe blocked")
		}
	}
}

func TestDesktopFlagAndOrdinaryCLIStdinPolicy(t *testing.T) {
	exe := t.TempDir() + "/agent-home-station.exe"
	cfg, err := parseConfig([]string{"--desktop"}, exe)
	if err != nil || !cfg.desktop {
		t.Fatalf("desktop flag was not retained: %#v %v", cfg, err)
	}
	cfg, err = parseConfig(nil, exe)
	if err != nil || cfg.desktop {
		t.Fatalf("ordinary CLI unexpectedly became desktop mode: %#v %v", cfg, err)
	}
	// A closed stdin is meaningful only after --desktop has selected the
	// private control pipe. This small contract check avoids accidentally
	// attaching a reader in ordinary run paths.
	var desktop <-chan struct{}
	if cfg.desktop {
		desktop = desktopStop(strings.NewReader(""))
	}
	select {
	case <-desktop:
		t.Fatal("ordinary command selected desktop EOF")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestDesktopRunStopsFromPrivatePipeAndReportsOwnRuntime(t *testing.T) {
	base := t.TempDir()
	dataDir, workspace := filepath.Join(base, "private"), filepath.Join(base, "workspace")
	t.Setenv("AHS_ADMIN_PASSWORD", "synthetic-local-password")
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	output := &stationStartupWriter{line: make(chan string, 1)}
	done := make(chan error, 1)
	go func() {
		done <- runWithDesktopInput(t.Context(), []string{"--desktop", "--data", dataDir, "--workspace", workspace, "--listen", "127.0.0.1:0"}, output, reader)
	}()
	var started string
	select {
	case started = <-output.line:
	case err := <-done:
		t.Fatalf("desktop station stopped before start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("desktop station did not announce startup")
	}
	address := strings.TrimSpace(strings.Split(strings.TrimPrefix(started, "Agent Home Station 已启动："), "\n")[0])
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 2 * time.Second}
	loginRequest, err := http.NewRequest(http.MethodPost, "http://"+address+"/api/login", bytes.NewBufferString(`{"password":"synthetic-local-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	loginRequest.Header.Set("Content-Type", "application/json")
	loginRequest.Header.Set("Origin", "http://"+address)
	login, err := client.Do(loginRequest)
	if err != nil {
		t.Fatal(err)
	}
	var session map[string]any
	err = json.NewDecoder(login.Body).Decode(&session)
	login.Body.Close()
	if err != nil || login.StatusCode != http.StatusOK {
		t.Fatalf("desktop login: %d %v", login.StatusCode, err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+address+"/api/local/runtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var runtime map[string]any
	err = json.NewDecoder(res.Body).Decode(&runtime)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || runtime["kind"] != "local-station" || runtime["data_directory"] != dataDir {
		t.Fatalf("runtime proof: status=%d value=%#v err=%v", res.StatusCode, runtime, err)
	}
	if _, err := writer.Write([]byte("stop\n")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("desktop pipe stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("desktop pipe did not stop station")
	}
}
