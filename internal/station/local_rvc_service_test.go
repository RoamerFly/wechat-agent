package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalRVCInfoReadsOnlyPinnedRegistration(t *testing.T) {
	root := t.TempDir()
	data, editable, station, media, executable := filepath.Join(root, "data"), filepath.Join(root, "agent-home"), filepath.Join(root, "station"), filepath.Join(root, "media"), filepath.Join(root, "desktop")
	for _, path := range []string{data, editable, filepath.Join(station, "public_workspace"), filepath.Join(station, "pri_workspace"), media} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, executable := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		if err := os.WriteFile(filepath.Join(media, executable), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeLocalRVCControlFixture(t, executable)
	registration := localRVCRegistration{SchemaVersion: 1, Enabled: true, Workspace: station, Media: media, Port: 6249, Profiles: []localRVCVoice{{ID: "mom-rvc", Label: "Mom RVC"}}}
	raw, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, localRVCRegistrationFile), raw, 0600); err != nil {
		t.Fatal(err)
	}

	s := newLocalRVCServiceAt(data, editable, executable)
	info, err := s.Info(t.Context(), localRVCFixtureFish)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Binding == "" || len(info.Profiles) != 1 || info.Profiles[0].ID != "mom-rvc" || info.Running || info.Busy {
		t.Fatalf("unexpected deployment: %#v", info)
	}
	_, _, err = s.CallExisting(t.Context(), localRVCFixtureFish, info.Binding, http.MethodGet, "/jobs/0123456789abcdef0123456789abcdef", "", nil)
	if !errors.Is(err, errLocalRVCNotRunning) {
		t.Fatalf("background lookup started or reached an unowned RVC service: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, localRVCTokenFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection must not mint an authentication token: %v", err)
	}
}

func TestLocalRVCStartPortOccupiedDoesNotMintToken(t *testing.T) {
	root := t.TempDir()
	data, editable, station, media, executable := filepath.Join(root, "data"), filepath.Join(root, "agent-home"), filepath.Join(root, "station"), filepath.Join(root, "media"), filepath.Join(root, "desktop")
	for _, path := range []string{data, editable, filepath.Join(station, "public_workspace"), filepath.Join(station, "pri_workspace"), media, filepath.Join(executable, "runtimes", "windows-python-cli")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		if err := os.WriteFile(filepath.Join(media, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The port guard runs before invoking this file, so it is intentionally not
	// executable in this fixture.
	if err := os.WriteFile(filepath.Join(executable, "runtimes", "windows-python-cli", "python.exe"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	writeLocalRVCControlFixture(t, executable)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	registration := localRVCRegistration{SchemaVersion: 1, Enabled: true, Workspace: station, Media: media, Port: port, Profiles: []localRVCVoice{{ID: "mom-rvc", Label: "Mom RVC"}}}
	raw, _ := json.Marshal(registration)
	if err := os.WriteFile(filepath.Join(data, localRVCRegistrationFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s := newLocalRVCServiceAt(data, editable, executable)
	loaded, manifest, err := s.loadRegistration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = s.start(t.Context(), localRVCFixtureFish, localRVCBinding(loaded, manifest), loaded, manifest)
	if err == nil || err.Error() != "station_rvc_port_occupied_no_adoption" {
		t.Fatalf("start error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, localRVCTokenFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed port admission must not mint a token: %v", err)
	}
}

func TestLocalRVCWaitReadyRejectsMalformedProof(t *testing.T) {
	s := newLocalRVCServiceAt(t.TempDir(), filepath.Join(t.TempDir(), "agent-home"), t.TempDir())
	path := filepath.Join(t.TempDir(), "bad-ready.json")
	if err := os.WriteFile(path, []byte(`{"nonce":"wrong","port":1,"scope":"wrong","pid":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := s.waitReady(ctx, localRVCFixtureFish, "binding", 6211, path, "expected")
	if err == nil || err.Error() != "station_rvc_ready_proof_invalid" {
		t.Fatalf("waitReady error = %v", err)
	}
}

const localRVCFixtureFish = "fish-0123456789abcdef0123456789abcdef0123456789abcdef"

func writeLocalRVCControlFixture(t *testing.T, executable string) {
	t.Helper()
	control := filepath.Join(executable, "runtimes", "windows-station-rvc")
	if err := os.MkdirAll(control, 0700); err != nil {
		t.Fatal(err)
	}
	files := make(map[string]string, len(localRVCControlFiles))
	for _, name := range localRVCControlFiles {
		content := []byte("fixture:" + name)
		if err := os.WriteFile(filepath.Join(control, name), content, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		files[name] = hex.EncodeToString(sum[:])
	}
	manifest, err := json.Marshal(map[string]any{"schema_version": 1, "platform": "windows-x64", "policy": "station-rvc-v1", "files": files})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "runtime-manifest.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
}
