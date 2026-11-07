package station

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
)

// A fresh production station deliberately has no verified provider/output
// pair. Its live API must still be owner-session protected, explain the empty
// target list, and refuse a start without recording a phantom broadcast.
func TestLocalLiveEmptyProductionBoundary(t *testing.T) {
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "host"), filepath.Join(base, "workspace"))
	if code, _ := f.request(http.MethodGet, "/api/local/live", nil, false); code != http.StatusUnauthorized {
		t.Fatalf("live targets exposed without a local owner session: %d", code)
	}
	f.login()
	if code, out := f.request(http.MethodGet, "/api/local/live", nil, false); code != http.StatusOK || out["available"] != false || out["reason"] != localLiveUnavailable {
		t.Fatalf("unprepared station live state: %d %#v", code, out)
	}
	if code, _ := f.request(http.MethodPost, "/api/local/live", map[string]string{"action": "status", "id": "live-1"}, false); code != http.StatusForbidden {
		t.Fatalf("live status bypassed CSRF: %d", code)
	}
	if code, out := f.request(http.MethodPost, "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK || out["initialized"] != true {
		t.Fatalf("initialize local owner: %d %#v", code, out)
	}
	if code, out := f.request(http.MethodGet, "/api/local/live", nil, false); code != http.StatusOK || out["available"] != false || out["reason"] != localLiveUnavailable {
		t.Fatalf("empty production live targets: %d %#v", code, out)
	}
	start := map[string]any{"action": "start", "app_id": "synthetic", "connection_id": "owner", "request_key": "live-request-1", "duration_seconds": 60}
	if code, out := f.request(http.MethodPost, "/api/local/live", start, true); code != http.StatusServiceUnavailable || out["error"] != localLiveUnavailable {
		t.Fatalf("production route admitted unverified live start: %d %#v", code, out)
	}
}

func TestLocalLiveBootRecoveryStartsForPreparedOwner(t *testing.T) {
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "host"), filepath.Join(base, "workspace"))
	f.login()
	if code, _ := f.request(http.MethodPost, "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatalf("initialize local owner: %d", code)
	}
	restarted := localRestartChat(t, f)
	restarted.handler.chat.mu.Lock()
	live, fish := restarted.handler.chat.live, restarted.handler.chat.liveFish
	restarted.handler.chat.mu.Unlock()
	if live == nil || fish == "" {
		t.Fatal("prepared local owner did not start live recovery controller on boot")
	}
}

// The route-level test owns only synthetic provider/output implementations.
// It exercises the same authenticated HTTP boundary as the desktop; no
// browser, encoder, platform account, or real broadcast is involved.
func TestLocalLiveOwnerRouteSyntheticLifecycle(t *testing.T) {
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "host"), filepath.Join(base, "workspace"))
	f.login()
	if code, _ := f.request(http.MethodPost, "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatalf("initialize local owner: %d", code)
	}
	provider := &localLiveRouteProvider{connection: apphost.Connection{ID: "broadcast", AccountID: "synthetic-owner", Label: "合成直播目标"}}
	registry, err := apphost.New(apphost.Registration{Application: apphost.Application{ID: "synthetic-live", Name: "合成直播", InstallationID: "synthetic-install", Registration: "synthetic-registration"}, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := f.handler.home.identity.Read()
	if err != nil || identity.LocalOwner == nil {
		t.Fatalf("local identity: %#v %v", identity, err)
	}
	host, err := f.handler.chat.projectTools.forFish(identity.FishID)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.chat.mu.Lock()
	f.handler.chat.applications = registry
	f.handler.chat.live = apphost.NewLiveController(registry, host.store, localLiveRouteOutput{})
	f.handler.chat.liveFish = identity.FishID
	f.handler.chat.mu.Unlock()

	if code, out := f.request(http.MethodGet, "/api/local/live", nil, false); code != http.StatusOK || out["available"] != true {
		t.Fatalf("synthetic target discovery: %d %#v", code, out)
	}
	start := map[string]any{"action": "start", "app_id": "synthetic-live", "connection_id": "broadcast", "request_key": "route-live-1", "duration_seconds": 60}
	code, started := f.request(http.MethodPost, "/api/local/live", start, true)
	if code != http.StatusOK || started["state"] != "live" {
		t.Fatalf("route start: %d %#v", code, started)
	}
	id, _ := started["id"].(string)
	if id == "" {
		t.Fatalf("missing durable live id: %#v", started)
	}
	if code, status := f.request(http.MethodPost, "/api/local/live", map[string]string{"action": "status", "id": id}, true); code != http.StatusOK || status["id"] != id || status["state"] != "live" {
		t.Fatalf("route status: %d %#v", code, status)
	}
	code, stopped := f.request(http.MethodPost, "/api/local/live", map[string]string{"action": "stop", "id": id}, true)
	if code != http.StatusOK || stopped["local_stopped"] != true || (stopped["state"] != "ended" && stopped["state"] != "stopping") {
		t.Fatalf("route stop: %d %#v", code, stopped)
	}
	// A concurrent supervisor may own the provider lock while an emergency stop
	// has already stopped local output. Reconcile the exact durable session
	// rather than assuming that the remote receipt is synchronous.
	for deadline := time.Now().Add(time.Second); stopped["state"] != "ended" && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		code, stopped = f.request(http.MethodPost, "/api/local/live", map[string]string{"action": "reconcile", "id": id}, true)
		if code != http.StatusOK {
			t.Fatalf("route reconcile after stop: %d %#v", code, stopped)
		}
	}
	if stopped["state"] != "ended" || stopped["local_stopped"] != true || stopped["remote_ended"] != true {
		t.Fatalf("route stop did not settle: %#v", stopped)
	}
}

type localLiveRouteProvider struct{ connection apphost.Connection }

func (p *localLiveRouteProvider) Connections(context.Context) ([]apphost.Connection, error) {
	return []apphost.Connection{p.connection}, nil
}
func (p *localLiveRouteProvider) Inspect(context.Context, string) (apphost.ConnectionStatus, error) {
	return apphost.ConnectionStatus{Connection: p.connection, Login: "online"}, nil
}
func (p *localLiveRouteProvider) LiveGrant(context.Context, apphost.ConnectionRef) (apphost.LiveGrant, error) {
	now := time.Now()
	return apphost.LiveGrant{Allowed: true, Version: "fixture-v1", OpensAt: now.Add(-time.Minute), ClosesAt: now.Add(time.Hour), MaxDuration: 30 * time.Minute}, nil
}
func (p *localLiveRouteProvider) StartLive(_ context.Context, ref apphost.LiveReference) (apphost.LiveStartResult, error) {
	return apphost.LiveStartResult{Receipt: apphost.LiveReceipt{Reference: ref, State: "live", RoomID: "room-1"}}, nil
}
func (p *localLiveRouteProvider) QueryLive(_ context.Context, ref apphost.LiveReference) (apphost.LiveReceipt, error) {
	return apphost.LiveReceipt{Reference: ref, State: "live", RoomID: "room-1"}, nil
}
func (p *localLiveRouteProvider) StopLive(_ context.Context, ref apphost.LiveReference) (apphost.LiveReceipt, error) {
	return apphost.LiveReceipt{Reference: ref, State: "ended", RoomID: "room-1"}, nil
}

type localLiveRouteOutput struct{}

func (localLiveRouteOutput) CheckLiveOutput(context.Context, apphost.LiveReference) error { return nil }
func (localLiveRouteOutput) StopLiveOutput(context.Context, apphost.LiveReference) error  { return nil }
