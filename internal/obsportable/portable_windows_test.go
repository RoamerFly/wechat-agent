//go:build windows

package obsportable

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticInstallation(t *testing.T) (string, string, string) {
	t.Helper()
	base := t.TempDir()
	station := filepath.Join(base, "station")
	studio := filepath.Join(station, "managed-obs", "studio")
	m := manifest{Version: releaseVersion, ArchiveDigest: releaseDigest, Files: map[string]string{}}
	for _, name := range []string{executable, "bin/64bit/obs.dll",
		"core/obs-outputs/obs-outputs.dll", "core/rtmp-services/rtmp-services.dll",
		"core/obs-x264/obs-x264.dll", "core/obs-ffmpeg/obs-ffmpeg.dll", "core/image-source/image-source.dll",
		"core/obs-transitions/obs-transitions.dll", "core/aja/aja.dll", "core/obs-websocket/obs-websocket.dll"} {
		data := []byte("synthetic " + name)
		path := filepath.Join(studio, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		m.Files[name] = hex.EncodeToString(sum[:])
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(station, "managed-obs", "install.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return station, filepath.Join(base, "workspace"), studio
}

func TestPortableRuntimeVerifiesInventoryAndExclusiveLease(t *testing.T) {
	station, workspace, studio := syntheticInstallation(t)
	ctx := t.Context()
	install, err := Open(ctx, station, workspace)
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := install.Encoder(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Stop(ctx)
	if _, err := install.Encoder(ctx); !errors.Is(err, ErrBusy) {
		t.Fatalf("second lease: %v", err)
	}
	if err := encoder.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(studio, "core", "obs-outputs", "obs-outputs.dll")
	if err := os.WriteFile(module, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Encoder(ctx); !errors.Is(err, ErrInvalid) {
		t.Fatalf("changed plugin accepted by cached install: %v", err)
	}
}

func TestPortableRuntimeRejectsWorkspaceAndExtraModule(t *testing.T) {
	station, workspace, studio := syntheticInstallation(t)
	if _, err := Open(t.Context(), station, station); !errors.Is(err, ErrInvalid) {
		t.Fatalf("workspace overlap: %v", err)
	}
	module := filepath.Join(studio, "core", "unapproved", "unapproved.dll")
	if err := os.MkdirAll(filepath.Dir(module), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, []byte("not allowed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), station, workspace); !errors.Is(err, ErrInvalid) {
		t.Fatalf("extra module accepted: %v", err)
	}
}

func TestPortableProfileReplacesStaleConfigAndUsesOnlyLoopback(t *testing.T) {
	_, _, studio := syntheticInstallation(t)
	root, err := os.OpenRoot(studio)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	sentinel := filepath.Join("config", "obs-studio", ".sentinel")
	if err := root.MkdirAll(sentinel, 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(filepath.Join(sentinel, "run_previous"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ticket := strings.Repeat("a", 48)
	if err := writeProfile(root, 32500, ticket); err != nil {
		t.Fatal(err)
	}
	service, err := root.ReadFile(filepath.FromSlash("config/obs-studio/basic/profiles/Station/service.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Type     string         `json:"type"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(service, &decoded); err != nil || decoded.Type != "rtmp_custom" ||
		decoded.Settings["server"] != "rtmp://127.0.0.1:32500/live" || decoded.Settings["key"] != ticket ||
		decoded.Settings["use_auth"] != false {
		t.Fatalf("unexpected local profile: %+v, %v", decoded, err)
	}
	ws, err := root.ReadFile(filepath.FromSlash("config/obs-studio/plugin_config/obs-websocket/config.json"))
	var config map[string]bool
	if err != nil || json.Unmarshal(ws, &config) != nil || config["server_enabled"] || !strings.Contains(string(ws), "server_enabled") {
		t.Fatalf("portable WebSocket is not explicitly disabled: %s, %v", ws, err)
	}
	if _, err := root.Lstat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale sentinel not cleared: %v", err)
	}
	if _, err := root.Lstat(filepath.FromSlash(executable)); err != nil {
		t.Fatalf("immutable binary touched: %v", err)
	}
}
