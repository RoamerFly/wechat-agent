package station

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/fishidentity"
)

type localHomeFixture struct {
	t       *testing.T
	server  *httptest.Server
	client  *http.Client
	csrf    string
	dir     string
	root    string
	handler *LocalHomeHandler
}

func newLocalHomeFixture(t *testing.T, dir, root string) *localHomeFixture {
	t.Helper()
	handler, err := NewLocalHomeHandler(dir, root, "synthetic-local-password", "")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	jar, _ := cookiejar.New(nil)
	f := &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}, dir: dir, root: root, handler: handler}
	t.Cleanup(func() { _ = handler.Close() })
	t.Cleanup(server.Close)
	return f
}

func (f *localHomeFixture) request(method, path string, value any, csrf bool) (int, map[string]any) {
	f.t.Helper()
	var body io.Reader
	if value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			f.t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, f.server.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Origin", f.server.URL)
	if value != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set("X-CSRF-Token", f.csrf)
	}
	// This header is intentionally irrelevant to the local owner. It must not
	// retarget the home or require that a WeChat account exists.
	req.Header.Set("X-Wechat-Account", "missing-wechat-account")
	res, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		f.t.Fatal(err)
	}
	return res.StatusCode, out
}

func (f *localHomeFixture) login() {
	f.t.Helper()
	code, out := f.request("POST", "/api/login", map[string]string{"password": "synthetic-local-password"}, false)
	if code != http.StatusOK {
		f.t.Fatalf("local login: %d %#v", code, out)
	}
	f.csrf, _ = out["csrf"].(string)
}

func TestLocalHomeWithoutWeChatPreservesIdentityAndAuthorization(t *testing.T) {
	base := t.TempDir()
	dir, root := filepath.Join(base, "host"), filepath.Join(base, "workspace")
	f := newLocalHomeFixture(t, dir, root)
	if code, _ := f.request("GET", "/api/local/home", nil, false); code != http.StatusUnauthorized {
		t.Fatal("private local home was public", code)
	}
	f.login()
	if code, out := f.request("GET", "/api/local/home", nil, true); code != http.StatusOK || out["initialized"] != false {
		t.Fatalf("fresh local state: %d %#v", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "fish-identity.json")); !os.IsNotExist(err) {
		t.Fatal("reading local state created an identity", err)
	}
	if code, _ := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, false); code != http.StatusForbidden {
		t.Fatal("local initialize bypassed CSRF", code)
	}
	code, first := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true)
	if code != http.StatusOK || first["home_access"] != true || first["fish_id"] == "" || first["owner_id"] == "" {
		t.Fatalf("local initialize: %d %#v", code, first)
	}
	home, _ := first["directory"].(string)
	for _, child := range []string{"Connected Apps", "Project Tools", "diary", "projects", "works", "scratch"} {
		if info, err := os.Stat(filepath.Join(home, child)); err != nil || !info.IsDir() {
			t.Fatalf("missing home child %s: %v", child, err)
		}
	}
	// Files in the companion's home are independent from the old account-private
	// tree and remain present when no application is bound at all.
	journal := filepath.Join(home, "diary", "synthetic-note.md")
	if err := os.WriteFile(journal, []byte("one persistent fish"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := fishidentity.New(dir)
	fish, bindings, err := registry.Bindings()
	if err != nil || fish != first["fish_id"] || len(bindings) != 0 {
		t.Fatalf("local setup invented an application account: %s %#v %v", fish, bindings, err)
	}
	code, paused := f.request("POST", "/api/local/home", map[string]any{"action": "pause", "revision": first["revision"]}, true)
	if code != http.StatusOK || paused["home_access"] != false {
		t.Fatalf("pause local grant: %d %#v", code, paused)
	}
	if code, _ := f.request("POST", "/api/local/home", map[string]any{"action": "authorize", "revision": first["revision"]}, true); code != http.StatusConflict {
		t.Fatal("stale grant write accepted", code)
	}
	f.server.Close()
	restarted := newLocalHomeFixture(t, dir, root)
	restarted.login()
	code, next := restarted.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true)
	if code != http.StatusOK || next["fish_id"] != first["fish_id"] || next["owner_id"] != first["owner_id"] || next["directory"] != home || next["home_access"] != false {
		t.Fatalf("restart or repeated setup replaced identity/revived grant: %d %#v", code, next)
	}
	if raw, err := os.ReadFile(journal); err != nil || string(raw) != "one persistent fish" {
		t.Fatal("home data lost", err)
	}
	if code, _ := restarted.request("POST", "/api/local/home", map[string]any{"action": "authorize", "revision": next["revision"]}, true); code != http.StatusOK {
		t.Fatal("explicit resume failed", code)
	}
}

func TestLocalHomeCredentialBoundaryAndNoWeChatRoutes(t *testing.T) {
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "host"), filepath.Join(base, "workspace"))
	f.login()
	if code, out := f.request("GET", "/healthz", nil, false); code != http.StatusOK || out["wechat_required"] != false {
		t.Fatalf("standalone health: %d %#v", code, out)
	}
	for _, path := range []string{"/api/accounts", "/api/wechat/login", "/api/config", "/api/agent"} {
		req, _ := http.NewRequest("GET", f.server.URL+path, nil)
		res, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("standalone home unexpectedly mounted application route %s: %d", path, res.StatusCode)
		}
	}
	serverURL, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range f.client.Jar.Cookies(serverURL) {
		if cookie.Name == "wechat_admin_session" {
			t.Fatal("standalone login overwrote existing management cookie")
		}
	}
	// A malicious installation root may never turn the granted home into an
	// ancestor of the host's private credential directory.
	id := "fish-" + strings.Repeat("a", 48)
	root := filepath.Join(base, "evil-root")
	dataDir := filepath.Join(root, "fish_home", id, "home", "host-secrets")
	h := localHomeService{root: root, dataDir: dataDir}
	if _, err := h.directory(fishidentity.Record{FishID: id, LocalOwner: &fishidentity.LocalOwner{}}); err == nil {
		t.Fatal("home grant could contain host credentials")
	}
}

func TestLocalHomeRuntimeIsAuthenticatedAndIdentifiesOnlyThisStation(t *testing.T) {
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "private"), filepath.Join(base, "workspace"))
	if code, _ := f.request("GET", "/api/local/runtime", nil, false); code != http.StatusUnauthorized {
		t.Fatalf("runtime was public: %d", code)
	}
	f.login()
	code, got := f.request("GET", "/api/local/runtime", nil, true)
	if code != http.StatusOK || got["kind"] != "local-station" || int(got["pid"].(float64)) != os.Getpid() || got["data_directory"] != f.dir {
		t.Fatalf("runtime identity: %d %#v", code, got)
	}
}
