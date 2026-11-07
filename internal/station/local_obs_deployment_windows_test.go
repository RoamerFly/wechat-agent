//go:build windows

package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/obsportable"
)

// Synthetic bytes exercise management admission and inventory checks only;
// this is never reported as an actual OBS installation or executable test.
func writeSyntheticOBSDeployment(t *testing.T, station string) {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{"bin/64bit/obs64.exe", "bin/64bit/obs.dll", "core/obs-outputs/obs-outputs.dll", "core/rtmp-services/rtmp-services.dll", "core/obs-x264/obs-x264.dll", "core/obs-ffmpeg/obs-ffmpeg.dll", "core/image-source/image-source.dll", "core/obs-transitions/obs-transitions.dll", "core/aja/aja.dll", "core/obs-websocket/obs-websocket.dll"} {
		path := filepath.Join(station, "managed-obs", "studio", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		body := []byte("synthetic " + name)
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		files[name] = sha256Hex(body)
	}
	raw, err := json.Marshal(map[string]any{"version": obsportable.Version(), "archive_sha256": obsportable.ArchiveSHA256(), "files": files})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(station, "managed-obs", "install.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func obsDeploymentInput() map[string]any {
	return map[string]any{"revision": "missing", "archive_path": `E:\Synthetic\OBS.zip`, "approved": true}
}

func TestLocalOBSDeploymentOwnerInstallAndRestartBoundary(t *testing.T) {
	f := localChatFixture(t)
	if code, view := f.request("GET", localOBSDeploymentRoute, nil, false); code != 200 || view["state"] != "missing" || view["read_only"] != false {
		t.Fatal(code, view)
	}
	calls := 0
	f.handler.chat.obsInstaller = func(ctx context.Context, data, workspace, archive string) error {
		calls++
		if filepath.Clean(data) != filepath.Clean(f.dir) || workspace != f.handler.home.root || archive != `E:\Synthetic\OBS.zip` || ctx.Err() != nil {
			t.Error("wrong host install scope")
			return errors.New("synthetic wrong install scope")
		}
		writeSyntheticOBSDeployment(t, data)
		return nil
	}
	input := obsDeploymentInput()
	input["approved"] = false
	if code, _ := f.request("PUT", localOBSDeploymentRoute, input, true); code != 400 || calls != 0 {
		t.Fatal("unapproved import", code, calls)
	}
	input["approved"] = true
	if code, _ := f.request("PUT", localOBSDeploymentRoute, input, false); code != 403 || calls != 0 {
		t.Fatal("CSRF bypass", code, calls)
	}
	input["revision"] = "present"
	if code, _ := f.request("PUT", localOBSDeploymentRoute, input, true); code != 409 || calls != 0 {
		t.Fatal("stale import", code, calls)
	}
	input["revision"] = "missing"
	code, view := f.request("PUT", localOBSDeploymentRoute, input, true)
	if code != 200 || calls != 1 || view["installed"] != true || view["state"] != "installed" || view["restart_required"] != true || view["read_only"] != true {
		t.Fatal(code, view, calls)
	}
	if f.handler.chat.liveEncoder == "ready" {
		t.Fatal("installation hot-swapped the running encoder")
	}
	before, err := os.ReadFile(filepath.Join(f.dir, "managed-obs", "install.json"))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request("PUT", localOBSDeploymentRoute, input, true); code != 409 || calls != 1 {
		t.Fatal("existing installation overwritten", code, calls)
	}
	after, _ := os.ReadFile(filepath.Join(f.dir, "managed-obs", "install.json"))
	if string(after) != string(before) {
		t.Fatal("original inventory changed")
	}
	// A new desktop boot verifies and registers the installed inventory.
	restarted := localRestartChat(t, f)
	if code, view := restarted.request("GET", localOBSDeploymentRoute, nil, false); code != 200 || view["restart_required"] != false || view["installed"] != true {
		t.Fatal(code, view)
	}
	if code, live := restarted.request("GET", "/api/local/live", nil, false); code != 200 || live["encoder"] != "ready" || live["available"] != false {
		t.Fatal("install does not grant a platform", code, live)
	}
}

func TestLocalOBSDeploymentBusyActiveAndPrivateErrors(t *testing.T) {
	f := localChatFixture(t)
	calls := 0
	f.handler.chat.obsInstaller = func(context.Context, string, string, string) error {
		calls++
		return errors.New("synthetic-private-path-and-secret")
	}
	f.handler.chat.obsInstallMu.Lock()
	code, _ := f.request("PUT", localOBSDeploymentRoute, obsDeploymentInput(), true)
	f.handler.chat.obsInstallMu.Unlock()
	if code != 409 || calls != 0 {
		t.Fatal("busy install admitted", code, calls)
	}
	for _, tc := range []struct {
		err  error
		code int
	}{
		{obsportable.ErrArchiveMissing, 400}, {obsportable.ErrArchiveDigest, 400}, {obsportable.ErrArchiveFormat, 400},
		{obsportable.ErrAlreadyInstalled, 409}, {obsportable.ErrBusy, 409}, {obsportable.ErrInstallCanceled, 409}, {errors.New("synthetic-private-path-and-secret"), 503},
	} {
		f.handler.chat.obsInstaller = func(context.Context, string, string, string) error { calls++; return tc.err }
		code, view := f.request("PUT", localOBSDeploymentRoute, obsDeploymentInput(), true)
		raw, _ := json.Marshal(view)
		if code != tc.code || strings.Contains(string(raw), "synthetic-private") {
			t.Fatal("wrong or private error", code, view)
		}
	}
	p := installLocalLiveToolFixture(t, f)
	tools := localLiveToolRunFixture(t, f)
	if _, err := liveToolTestCall(t, tools, localLiveTestStart()); err != nil {
		t.Fatal(err)
	}
	before := calls
	if code, _ := f.request("PUT", localOBSDeploymentRoute, obsDeploymentInput(), true); code != 409 || calls != before {
		t.Fatal("active live install admitted", code, calls)
	}
	identity, _ := f.handler.home.identity.Read()
	_, _ = f.handler.chat.live.Stop(t.Context(), identity.LocalOwner.ID, p.ref.ID)
}

func TestLocalOBSDeploymentInvalidInventoryPreserved(t *testing.T) {
	f := localChatFixture(t)
	directory := filepath.Join(f.dir, "managed-obs")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "install.json"), []byte("invalid private inventory"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, view := f.request("GET", localOBSDeploymentRoute, nil, false); code != 200 || view["state"] != "invalid" || view["read_only"] != true || view["installed"] != false {
		t.Fatal(code, view)
	}
	if code, _ := f.request("PUT", localOBSDeploymentRoute, obsDeploymentInput(), true); code != 409 {
		t.Fatal(code)
	}
	if raw, _ := os.ReadFile(filepath.Join(directory, "install.json")); string(raw) != "invalid private inventory" {
		t.Fatal("invalid inventory was destroyed")
	}
}
