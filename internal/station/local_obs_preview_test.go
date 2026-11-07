package station

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"wechat-bot/internal/obspreview"
)

const (
	obsPackagePath     = "/api/local/project-tools/obs-preview"
	obsPreviewPath     = obsPackagePath + "/preview"
	obsFixturePassword = "synthetic-preview-password"
)

func initializeOBSOwner(t *testing.T, f *localHomeFixture) {
	t.Helper()
	f.login()
	if code, out := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatalf("initialize owner: %d %v", code, out)
	}
	catalogPath, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	f.handler.chat.projectTools.options.CatalogDirectory = catalogPath
}

func installOBSPreview(t *testing.T, f *localHomeFixture) {
	t.Helper()
	if code, out := f.request("PUT", obsPackagePath, map[string]any{"action": "install", "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatalf("install OBS: %d %v", code, out)
	}
}

func configureOBSPreview(t *testing.T, f *localHomeFixture) {
	t.Helper()
	in := localOBSInput{Action: "configure", Port: 4577, Password: obsFixturePassword}
	code, out := f.request("POST", obsPreviewPath, in, true)
	if code != http.StatusOK || out["configured"] != true || out["port"] != float64(4577) || out["config_revision"] != float64(1) ||
		strings.Contains(fmt.Sprint(out), obsFixturePassword) {
		t.Fatalf("configure OBS: %d %v", code, out)
	}
	if code, _ = f.request("POST", obsPreviewPath, in, true); code != http.StatusConflict {
		t.Fatalf("stale configuration was accepted: %d", code)
	}
}

func changeOBSEnable(t *testing.T, f *localHomeFixture, action string, expected int) {
	t.Helper()
	revision := localProjectToolRevision(t, localProjectToolByID(t, f, localOBSPackage))
	if code, out := f.request("PUT", obsPackagePath, map[string]any{"action": action, "revision": revision}, true); code != expected {
		t.Fatalf("%s OBS: %d %v", action, code, out)
	}
}

func TestLocalOBSPreviewUsesOwnerConfigurationAndClearsOnRestart(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows project-tool package")
	}
	f := newLocalHomeFixture(t, t.TempDir(), t.TempDir())
	if code, _ := f.request("GET", obsPreviewPath, nil, false); code != http.StatusUnauthorized {
		t.Fatalf("preview status was public: %d", code)
	}
	initializeOBSOwner(t, f)
	if code, _ := f.request("PUT", obsPackagePath, map[string]string{"action": "install"}, true); code != http.StatusBadRequest {
		t.Fatalf("install did not require network consent: %d", code)
	}
	installOBSPreview(t, f)
	changeOBSEnable(t, f, "enable", http.StatusConflict)
	if code, _ := f.request("POST", obsPreviewPath, localOBSInput{Action: "configure", Port: 4455, Password: obsFixturePassword}, false); code != http.StatusForbidden {
		t.Fatalf("configure bypassed CSRF: %d", code)
	}
	if code, _ := f.request("POST", obsPreviewPath, map[string]any{"action": "configure", "port": 4455, "host": "192.0.2.1", "password": obsFixturePassword}, true); code != http.StatusBadRequest {
		t.Fatalf("configure accepted a nonlocal host: %d", code)
	}
	configureOBSPreview(t, f)
	changeOBSEnable(t, f, "enable", http.StatusOK)
	tools := f.handler.chat.projectTools
	var calls atomic.Int32
	tools.obs.capture = func(_ context.Context, port int, password string) (obspreview.Frame, error) {
		calls.Add(1)
		if port != 4577 || password != obsFixturePassword {
			return obspreview.Frame{}, fmt.Errorf("wrong host configuration")
		}
		return obspreview.Frame{Scene: "合成节目场景", ImageData: "data:image/jpeg;base64,synthetic-frame"}, nil
	}
	if code, _ := f.request("POST", obsPreviewPath, localOBSInput{Action: "start"}, false); code != http.StatusForbidden {
		t.Fatalf("start bypassed CSRF: %d", code)
	}
	code, frame := f.request("POST", obsPreviewPath, localOBSInput{Action: "start"}, true)
	if code != http.StatusOK || frame["active"] != true || frame["scene"] != "合成节目场景" || frame["image_data"] == nil || frame["observed_at"] == nil {
		t.Fatalf("start OBS preview: %d %v", code, frame)
	}
	if code, out := f.request("GET", obsPreviewPath, nil, false); code != http.StatusOK || out["active"] != true || out["image_data"] != nil ||
		strings.Contains(fmt.Sprint(out), obsFixturePassword) {
		t.Fatalf("OBS status disclosed frame or password: %d %v", code, out)
	}
	if code, _ := f.request("POST", obsPreviewPath, localOBSInput{Action: "frame"}, true); code != http.StatusTooManyRequests {
		t.Fatalf("immediate frame was allowed: %d", code)
	}
	if code, out := f.request("POST", obsPreviewPath, localOBSInput{Action: "stop"}, true); code != http.StatusOK || out["active"] != false || out["image_data"] != nil || out["scene"] != nil {
		t.Fatalf("OBS stop did not clear preview: %d %v", code, out)
	}
	if code, _ := f.request("POST", obsPreviewPath, localOBSInput{Action: "frame"}, true); code != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("stopped preview read a frame: %d, %d calls", code, calls.Load())
	}
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newLocalHomeFixture(t, f.dir, f.root)
	reopened.login()
	code, state := reopened.request("GET", obsPreviewPath, nil, false)
	if code != http.StatusOK || state["configured"] != true || state["enabled"] != true || state["active"] != false || state["scene"] != nil || state["image_data"] != nil {
		t.Fatalf("restart persisted the wrong OBS state: %d %v", code, state)
	}
}

func TestLocalOBSStopCancelsInFlightFrameBeforeDisclosure(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows project-tool package")
	}
	for _, action := range []string{"stop", "disable", "logout", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			f := newLocalHomeFixture(t, t.TempDir(), t.TempDir())
			initializeOBSOwner(t, f)
			installOBSPreview(t, f)
			configureOBSPreview(t, f)
			changeOBSEnable(t, f, "enable", http.StatusOK)
			started, cancelled := make(chan struct{}), make(chan struct{})
			f.handler.chat.projectTools.obs.capture = func(ctx context.Context, _ int, _ string) (obspreview.Frame, error) {
				close(started)
				<-ctx.Done()
				close(cancelled)
				return obspreview.Frame{Scene: "迟到的画面", ImageData: "synthetic-late-frame"}, nil
			}
			done := make(chan int, 1)
			go func() {
				code, _ := f.request("POST", obsPreviewPath, localOBSInput{Action: "start"}, true)
				done <- code
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("capture never started")
			}
			switch action {
			case "disable":
				changeOBSEnable(t, f, action, http.StatusOK)
			case "logout":
				if code, _ := f.request("POST", "/api/logout", nil, true); code != http.StatusOK {
					t.Fatalf("logout while previewing: %d", code)
				}
			case "shutdown":
				f.handler.BeginShutdown()
			default:
				if code, out := f.request("POST", obsPreviewPath, localOBSInput{Action: action}, true); code != http.StatusOK || out["active"] != false {
					t.Fatalf("stop preview: %d %v", code, out)
				}
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("preview stop did not cancel the active capture")
			}
			select {
			case code := <-done:
				if code != http.StatusConflict {
					t.Fatalf("late frame survived stop: %d", code)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("preview response did not settle")
			}
		})
	}
}

func TestLocalOBSStopFencesQueuedStart(t *testing.T) {
	var preview localOBSPreview
	ticket := preview.ticket()
	preview.stop()
	if session, err := preview.claim(true, ticket); session != nil || !errors.Is(err, errOBSPreviewInactive) {
		t.Fatalf("a queued start survived stop: %+v %v", session, err)
	}
}

func TestLocalOBSPreviewReadsProgramThroughWebSocket(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows project-tool package")
	}
	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, image.NewGray(image.Rect(0, 0, 320, 180)), nil); err != nil {
		t.Fatal(err)
	}
	data := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(picture.Bytes())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		write := func(op int, body any) bool {
			if err := conn.WriteJSON(map[string]any{"op": op, "d": body}); err != nil {
				t.Error(err)
				return false
			}
			return true
		}
		var message struct {
			Op int `json:"op"`
			D  struct {
				RequestType        string `json:"requestType"`
				RequestID          string `json:"requestId"`
				Authentication     string `json:"authentication"`
				EventSubscriptions *int   `json:"eventSubscriptions"`
			} `json:"d"`
		}
		if !write(0, map[string]any{"rpcVersion": 1, "authentication": map[string]string{"salt": "c2FsdA==", "challenge": "Y2hhbGxlbmdl"}}) {
			return
		}
		if err := conn.ReadJSON(&message); err != nil || message.Op != 1 || message.D.Authentication == "" ||
			message.D.EventSubscriptions == nil || *message.D.EventSubscriptions != 0 {
			t.Error("expected authenticated Identify without subscriptions", err)
			return
		}
		if !write(2, map[string]int{"negotiatedRpcVersion": 1}) {
			return
		}
		for _, reply := range []struct {
			method string
			data   map[string]string
		}{
			{"GetCurrentProgramScene", map[string]string{"currentProgramSceneName": "合成 OBS 场景"}},
			{"GetSourceScreenshot", map[string]string{"imageData": data}},
		} {
			if err := conn.ReadJSON(&message); err != nil || message.Op != 6 || message.D.RequestType != reply.method || message.D.RequestID == "" {
				t.Errorf("expected OBS read %s, got %s: %v", reply.method, message.D.RequestType, err)
				return
			}
			if !write(7, map[string]any{"requestType": reply.method, "requestId": message.D.RequestID,
				"requestStatus": map[string]any{"result": true, "code": 100}, "responseData": reply.data}) {
				return
			}
		}
	}))
	defer upstream.Close()
	f := newLocalHomeFixture(t, t.TempDir(), t.TempDir())
	initializeOBSOwner(t, f)
	installOBSPreview(t, f)
	in := localOBSInput{Action: "configure", Port: upstream.Listener.Addr().(*net.TCPAddr).Port, Password: obsFixturePassword}
	if code, out := f.request("POST", obsPreviewPath, in, true); code != http.StatusOK {
		t.Fatalf("configure loopback fixture: %d %v", code, out)
	}
	changeOBSEnable(t, f, "enable", http.StatusOK)
	code, out := f.request("POST", obsPreviewPath, localOBSInput{Action: "start"}, true)
	got, _ := out["image_data"].(string)
	if code != http.StatusOK || out["scene"] != "合成 OBS 场景" || !strings.HasPrefix(got, "data:image/jpeg;base64,") {
		t.Fatalf("program preview through WebSocket: %d %v", code, out["error"])
	}
}
