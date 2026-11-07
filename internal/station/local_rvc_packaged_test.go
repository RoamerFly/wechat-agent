package station

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalRVCPackagedOwnedBridge(t *testing.T) {
	preview := os.Getenv("AHS_STATION_RVC_PREVIEW")
	if runtime.GOOS != "windows" || preview == "" {
		t.Skip("requires isolated Windows preview with -WithRVCBackend")
	}
	root := t.TempDir()
	data, home := filepath.Join(root, "host"), filepath.Join(root, "home")
	resources, media := filepath.Join(root, "resources"), filepath.Join(root, "media")
	for _, path := range []string{data, home, media, filepath.Join(resources, "public_workspace"), filepath.Join(resources, "pri_workspace")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		if err := os.WriteFile(filepath.Join(media, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	registration, err := json.Marshal(localRVCRegistration{
		SchemaVersion: 1, Enabled: true, Workspace: resources, Media: media, Port: port,
		Profiles: []localRVCVoice{{ID: "private-fixture", Label: "合成声线"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, localRVCRegistrationFile), registration, 0600); err != nil {
		t.Fatal(err)
	}
	fish := "fish-" + strings.Repeat("a", 48)
	service := newLocalRVCServiceAt(data, home, preview)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := service.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	before, err := service.Info(ctx, fish)
	if err != nil || before.Running {
		t.Fatalf("preflight should leave broker stopped: %v, %+v", err, before)
	}
	response, code, err := service.Call(ctx, fish, before.Binding, http.MethodGet, "/health", "", nil)
	var health struct {
		Ready bool   `json:"ready"`
		Mode  string `json:"mode"`
	}
	if err != nil || code != http.StatusOK || json.Unmarshal(response, &health) != nil || !health.Ready || health.Mode != "station_rvc" {
		log, _ := os.ReadFile(filepath.Join(data, "station-rvc-logs", localRVCFishHash(fish)+".log"))
		t.Fatalf("packaged broker did not become ready: HTTP %d, %v, synthetic log %s", code, err, log)
	}
	after, err := service.Info(ctx, fish)
	if err != nil || !after.Running || after.Binding != before.Binding {
		t.Fatalf("owned broker state invalid: %v, %+v", err, after)
	}
	if err := service.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	after, err = service.Info(ctx, fish)
	if err != nil || after.Running {
		t.Fatalf("owned broker did not stop: %v, %+v", err, after)
	}
}
