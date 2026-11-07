package station

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
)

const localDouyinWebRoute = "/api/local/apps/douyin/web-login"

type syntheticDouyinLoginWindow struct {
	active   atomic.Bool
	closeErr error
}

func (w *syntheticDouyinLoginWindow) Active() bool { return w.active.Load() }
func (w *syntheticDouyinLoginWindow) Close(context.Context) error {
	w.active.Store(false)
	return w.closeErr
}

func newLocalDouyinWebFixture(t *testing.T) (*localHomeFixture, *localDouyinWebLogin) {
	t.Helper()
	base := t.TempDir()
	data, home := filepath.Join(base, "host"), filepath.Join(base, "workspace")
	registry, err := apphost.New(douyinapp.New(filepath.Join(data, "apps", "douyin")).Registration())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLocalHomeHandlerWithApps(data, home, "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	f := newLocalAppObservationFixtureWithHandler(t, handler, data, home)
	m := handler.home.douyinWeb
	m.available = func() bool { return true }
	m.open = func(ctx context.Context, _ string) (localDouyinLoginWindow, error) {
		w := new(syntheticDouyinLoginWindow)
		w.active.Store(true)
		context.AfterFunc(ctx, func() { w.active.Store(false) })
		return w, nil
	}
	m.openCreator = func(ctx context.Context, _ string) (localDouyinLoginWindow, error) {
		w := new(syntheticDouyinLoginWindow)
		w.active.Store(true)
		context.AfterFunc(ctx, func() { w.active.Store(false) })
		return w, nil
	}
	return f, m
}

func TestLocalDouyinWebCreatorLifecycleKeepsPublishGrant(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	_, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true)
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_publish", "revision": view["revision"], "allow_publish": true}, true); code != 200 {
		t.Fatal(code)
	}
	_, view = f.request("GET", localDouyinWebRoute, nil, false)
	if view["publish_enabled"] != true {
		t.Fatal("publish grant was not saved")
	}
	code, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin_creator", "revision": view["revision"]}, true)
	if code != 200 || view["state"] != "login_open" || view["attempt_id"] == nil {
		t.Fatalf("creator begin: %d %v", code, view)
	}
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true); code != 200 {
		t.Fatal(code)
	}
	_, final := f.request("GET", localDouyinWebRoute, nil, false)
	if final["state"] != "saved" || final["publish_enabled"] != false || m.window != nil {
		t.Fatalf("creator confirmation did not revoke old grant: %v", final)
	}
	if code, final := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_publish", "revision": final["revision"], "allow_publish": true}, true); code != 200 || final["publish_enabled"] != true {
		t.Fatalf("re-enable publish: %d %v", code, final)
	}
}

func TestLocalDouyinWebCommentsPermissionNeedsVideoScopeAndRevokes(t *testing.T) {
	f, _ := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	_, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true)
	if view["comments_enabled"] != false {
		t.Fatalf("comments must default to disabled: %v", view)
	}
	code, rejected := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "allow_comments": true}, true)
	if code != 400 || rejected["error"] == nil {
		t.Fatalf("comments without video scope: %d %v", code, rejected)
	}
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home_and_videos", "allow_comments": true}, true)
	if code != 200 || view["comments_enabled"] != true {
		t.Fatalf("enable comments: %d %v", code, view)
	}
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home"}, true)
	if code != 200 || view["comments_enabled"] != false || view["read_scope"] != "home" {
		t.Fatalf("scope narrowing did not revoke comments: %d %v", code, view)
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home_and_videos", "allow_comments": true}, true)
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "clear", "revision": view["revision"], "approve_profile": true}, true)
	if code != 200 || view["state"] != "unconfigured" || view["comments_enabled"] != false {
		t.Fatalf("removal did not revoke comments: %d %v", code, view)
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": view["revision"], "approve_profile": true}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home_and_videos", "allow_comments": true}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": view["revision"], "approve_profile": true}, true)
	if view["comments_enabled"] != false || view["read_enabled"] != false {
		t.Fatalf("relogin did not revoke comments: %v", view)
	}
}

func initializeDouyinWeb(t *testing.T, f *localHomeFixture) {
	t.Helper()
	f.login()
	if code, _ := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != 200 {
		t.Fatal(code)
	}
}

func TestLocalDouyinWebOwnerLifecycle(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	if code, _ := f.request("GET", localDouyinWebRoute, nil, false); code != 401 {
		t.Fatal(code)
	}
	f.login()
	if code, _ := f.request("GET", localDouyinWebRoute, nil, false); code != 409 {
		t.Fatal(code)
	}
	initializeDouyinWeb(t, f)
	code, view := f.request("GET", localDouyinWebRoute, nil, false)
	if code != 200 || view["state"] != "unconfigured" || view["revision"] != float64(0) {
		t.Fatalf("initial: %d %v", code, view)
	}
	in := map[string]any{"action": "begin", "revision": 0, "approve_profile": true}
	if code, _ := f.request("POST", localDouyinWebRoute, in, false); code != 403 {
		t.Fatal(code)
	}
	in["approve_profile"] = false
	if code, _ := f.request("POST", localDouyinWebRoute, in, true); code != 400 {
		t.Fatal(code)
	}
	in["approve_profile"] = true
	code, view = f.request("POST", localDouyinWebRoute, in, true)
	if code != 200 || view["state"] != "login_open" || view["active"] != true {
		t.Fatalf("begin: %d %v", code, view)
	}
	if _, ok := view["profile"]; ok {
		t.Fatal("private profile escaped owner status")
	}
	if _, ok := view["verified"]; ok {
		t.Fatal("owner statement became platform verification")
	}
	attempt := view["attempt_id"].(string)
	// Closing the app window manually must not lose the pending owner action.
	m.window.(*syntheticDouyinLoginWindow).active.Store(false)
	_, closed := f.request("GET", localDouyinWebRoute, nil, false)
	if closed["attempt_id"] != attempt || closed["active"] != false {
		t.Fatal("manual close lost the confirmation attempt")
	}
	m.window.(*syntheticDouyinLoginWindow).active.Store(true)
	in = map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": "stale"}
	if code, _ := f.request("POST", localDouyinWebRoute, in, true); code != 409 || !m.window.Active() {
		t.Fatal("stale attempt stopped window")
	}
	in["attempt_id"] = attempt
	code, view = f.request("POST", localDouyinWebRoute, in, true)
	if code != 200 || view["state"] != "saved" || view["active"] != false || view["confirmed_at"] == nil {
		t.Fatalf("confirm: %d %v", code, view)
	}
	if code, _ := f.request("POST", localDouyinWebRoute, in, true); code != 409 {
		t.Fatal("duplicate confirmation accepted")
	}
	revision := view["revision"]
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": revision, "approve_profile": true}, true)
	if code != 200 || view["confirmed_at"] != nil {
		t.Fatalf("relogin carried confirmation: %d %v", code, view)
	}
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "cancel", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true)
	if code != 200 || view["state"] != "unconfirmed" {
		t.Fatalf("cancel: %d %v", code, view)
	}
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "clear", "revision": revision, "approve_profile": true}, true); code != 409 {
		t.Fatal("stale clear accepted")
	}
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "clear", "revision": view["revision"], "approve_profile": true}, true)
	if code != 200 || view["state"] != "unconfigured" {
		t.Fatalf("clear: %d %v", code, view)
	}
}

func TestLocalDouyinWebCloseFailureNeverConfirms(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	m.open = func(context.Context, string) (localDouyinLoginWindow, error) {
		w := &syntheticDouyinLoginWindow{closeErr: errors.New("synthetic exit not confirmed")}
		w.active.Store(true)
		return w, nil
	}
	_, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true); code != 503 {
		t.Fatal(code)
	}
	code, view := f.request("GET", localDouyinWebRoute, nil, false)
	if code != 200 || view["state"] != "unconfirmed" || view["confirmed_at"] != nil {
		t.Fatalf("failed close saved: %d %v", code, view)
	}
}

func TestLocalDouyinWebRestartUsesSameProfileWithoutAutoOpening(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	var paths []string
	opener := m.open
	m.open = func(ctx context.Context, p string) (localDouyinLoginWindow, error) {
		paths = append(paths, p)
		return opener(ctx, p)
	}
	_, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	oldAttempt := view["attempt_id"]
	// Simulate process restart: runtime state is deliberately not persisted.
	m.window.Close(context.Background())
	m.cancel()
	m.window, m.cancel, m.attempt = nil, nil, ""
	code, view := f.request("GET", localDouyinWebRoute, nil, false)
	if code != 200 || view["active"] != false || len(paths) != 1 {
		t.Fatal("restart reopened browser")
	}
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": oldAttempt}, true); code != 409 {
		t.Fatal("restart accepted old attempt")
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": view["revision"], "approve_profile": true}, true)
	if len(paths) != 2 || paths[0] != paths[1] || strings.HasPrefix(paths[0], f.root) {
		t.Fatal("profile reuse/private scope failed")
	}
	if err := os.WriteFile(filepath.Join(paths[1], "synthetic-cookie"), []byte("not a real cookie"), 0600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(paths[1]), "unrelated-data")
	os.MkdirAll(sibling, 0700)
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "clear", "revision": view["revision"], "approve_profile": true}, true)
	if code != 200 || view["state"] != "unconfigured" {
		t.Fatalf("clear: %d %v", code, view)
	}
	if _, err := os.Stat(paths[1]); !os.IsNotExist(err) {
		t.Fatal("owned profile survived clear")
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatal("clear touched sibling")
	}
}

func TestLocalDouyinWebClearRespectsOtherBrowserAndRecordIntegrity(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	var profile string
	opener := m.open
	m.open = func(ctx context.Context, p string) (localDouyinLoginWindow, error) {
		profile = p
		return opener(ctx, p)
	}
	_, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "cancel", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true)
	release, err := lockLocalDouyinWebProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": view["revision"], "approve_profile": true}, true); code != 409 {
		t.Fatal("begin changed an occupied profile")
	}
	_, current := f.request("GET", localDouyinWebRoute, nil, false)
	if current["revision"] != view["revision"] {
		t.Fatal("occupied begin advanced revision")
	}
	code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "clear", "revision": view["revision"], "approve_profile": true}, true)
	release()
	if code != 409 {
		t.Fatalf("deleted an occupied profile: %d", code)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(profile), "login.json"), []byte(`{"schema_version":1,"fish_id":"another-fish","profile":"../../escape"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request("GET", localDouyinWebRoute, nil, false); code != 503 {
		t.Fatal("corrupt identity/path was accepted")
	}
}

func TestLocalDouyinWebServiceStopAndUnavailable(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	m.available = func() bool { return false }
	if code, _ := f.request(http.MethodPost, localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true); code != 503 {
		t.Fatal(code)
	}
	m.available = func() bool { return true }
	ctx, cancel := context.WithCancel(m.ctx)
	m.ctx = ctx
	cancel()
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true); code != 503 {
		t.Fatal(code)
	}
}

func TestLocalDouyinWebClearDoesNotStopAnotherFish(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	identity, err := m.home.identity.Read()
	if err != nil {
		t.Fatal(err)
	}
	_, code, err := m.apply(context.Background(), identity.FishID, &localDouyinWebInput{Action: "begin", ApproveProfile: true})
	if err != nil || code != 200 {
		t.Fatal(code, err)
	}
	attempt, window := m.attempt, m.window
	// This second valid, isolated dataset must not cancel the first one.
	other := "fish-" + strings.Repeat("a", 48)
	_, code, err = m.apply(context.Background(), other, &localDouyinWebInput{Action: "clear", ApproveProfile: true})
	if err != nil || code != 200 {
		t.Fatal(code, err)
	}
	if m.attempt != attempt || m.window != window || !window.Active() {
		t.Fatal("another fish cleared the active login")
	}
}

func TestLocalDouyinWebMissingProfileIsNotSaved(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	var profile string
	opener := m.open
	m.open = func(ctx context.Context, p string) (localDouyinLoginWindow, error) {
		profile = p
		return opener(ctx, p)
	}
	_, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true)
	if view["state"] != "saved" {
		t.Fatal(view)
	}
	// No recursive deletion: emulate a missing profile by renaming exactly
	// this fixture-owned directory within its existing private parent.
	if err := os.Rename(profile, profile+"-moved"); err != nil {
		t.Fatal(err)
	}
	code, view := f.request("GET", localDouyinWebRoute, nil, false)
	if code != 200 || view["state"] != "unconfirmed" || view["confirmed_at"] != nil || view["reason"] == nil {
		t.Fatalf("missing profile still saved: %d %v", code, view)
	}
}

func TestLocalDouyinWebFailedReopenCannotReuseOldAttempt(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	_, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	old := view["attempt_id"]
	m.window.(*syntheticDouyinLoginWindow).active.Store(false)
	m.open = func(context.Context, string) (localDouyinLoginWindow, error) {
		return nil, errors.New("synthetic launch failure")
	}
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": view["revision"], "approve_profile": true}, true); code != 503 {
		t.Fatal(code)
	}
	_, view = f.request("GET", localDouyinWebRoute, nil, false)
	if view["attempt_id"] != nil {
		t.Fatal("failed reopen retained old attempt")
	}
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": old}, true); code != 409 {
		t.Fatal("old attempt confirmed after failed reopen")
	}
}
