package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type multiNative struct {
	fakeNative
	endpoint, tokenFile string
	directoryReads      atomic.Int32
	primaryStatusReads  atomic.Int32
	secondaryReads      atomic.Int32
}

func (n *multiNative) ForAccount(id string) (*linuxnative.Sender, error) {
	return linuxnative.NewAdmin(linuxnative.Config{URL: n.endpoint, TokenFile: n.tokenFile, Account: id})
}
func (n *multiNative) Accounts(context.Context) ([]linuxnative.AccountInfo, error) {
	n.directoryReads.Add(1)
	return []linuxnative.AccountInfo{{ID: "test", Alias: "test"}, {ID: "second", Alias: "second-alias"}}, nil
}
func (n *multiNative) DisplayName(context.Context) (string, error) { return "主号昵称", nil }
func (n *multiNative) Ready() bool                                 { return true }
func (n *multiNative) Status(ctx context.Context) (linuxnative.Status, error) {
	n.primaryStatusReads.Add(1)
	return n.fakeNative.Status(ctx)
}

func setupAccounts(t *testing.T) (*webFixture, *accountRegistry) {
	t.Helper()
	return setupAccountsWithNativeRoutes(t, nil)
}

func setupAccountsWithNativeRoutes(t *testing.T, handle func(http.ResponseWriter, *http.Request) bool) (*webFixture, *accountRegistry) {
	t.Helper()
	native := &multiNative{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-account-test-token" {
			http.Error(w, "denied", 403)
			return
		}
		if handle != nil && handle(w, r) {
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		id := ""
		if len(parts) > 3 {
			id = parts[3]
		}
		switch {
		case r.URL.Path == "/v1/status":
			respond(w, 200, map[string]any{"serverId": "synthetic-server"})
		case strings.HasSuffix(r.URL.Path, "/events"):
			respond(w, 200, map[string]any{"items": []any{}, "nextCursor": "0"})
		case r.URL.Path == "/v1/accounts/"+id:
			if id == "second" {
				native.secondaryReads.Add(1)
			}
			respond(w, 200, map[string]any{"id": id, "authenticated": true, "connection": "online"})
		case r.URL.Path == "/v1/accounts/second/capabilities":
			native.secondaryReads.Add(1)
			respond(w, 200, linuxnative.Capabilities{
				MediaKinds: []string{"voice", "video", "unrecognized"}, MediaSendMaxBytes: map[string]int64{"voice": 1 << 20},
				QuotedTextSend: true, GroupMutations: []string{"recall", "unknown"},
			})
		case strings.HasSuffix(r.URL.Path, "/threads/filehelper/messages"):
			respond(w, 200, map[string]any{"items": []map[string]any{{"id": "out", "threadId": "filehelper", "type": 1, "direction": "outgoing", "senderId": "wxid_" + id, "body": "synthetic"}}})
		case strings.Contains(r.URL.Path, "/contacts/wxid_"):
			respond(w, 200, map[string]any{"id": "wxid_" + id, "nickname": "小号昵称"})
		case r.Method == "POST" && id == "second" && (strings.HasSuffix(r.URL.Path, "/logout") || strings.HasSuffix(r.URL.Path, "/forget-credentials")):
			respond(w, 202, map[string]string{"id": "end-session", "state": "queued"})
		case r.URL.Path == "/v1/accounts/second/operations/end-session":
			respond(w, 200, map[string]string{"id": "end-session", "state": "succeeded"})
		case strings.HasSuffix(r.URL.Path, "/auth/login"):
			respond(w, 202, map[string]any{"id": "shared-operation", "state": "awaiting_scan", "result": map[string]any{"qrAvailable": true, "qrGeneration": 1}})
		case strings.HasSuffix(r.URL.Path, "/operations/shared-operation/qr"):
			w.Write([]byte("\x89PNG\r\n\x1a\n" + id))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("synthetic-account-test-token"), 0600)
	native.endpoint, native.tokenFile = upstream.URL, token
	m, err := NewManager(filepath.Join(dir, "admin"), filepath.Join(dir, "inbox"), native)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(m, "test-password-at-least-24-bytes", "")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	f := &webFixture{t: t, manager: m, server: srv, client: srv.Client(), dir: dir}
	t.Cleanup(func() { srv.Close(); m.Close() })
	// Match the normal fixture's cookie-backed session without exposing tokens.
	f.client.Jar, _ = cookiejar.New(nil)
	f.login()
	code, _, _ := f.request("GET", "/api/accounts", nil, true, false)
	if code != 200 {
		t.Fatal(code)
	}
	return f, m.accounts
}

func TestConcurrentAccountsScopeAndSharedAuthentication(t *testing.T) {
	f, registry := setupAccounts(t)
	second := registry.lookup("second")
	if second == nil {
		t.Fatal("native second account not imported")
	}
	if second.manager.dir == f.manager.dir || second.manager.stateDir == f.manager.stateDir {
		t.Fatal("shared private storage")
	}
	if second.manager.Config().Enabled {
		t.Fatal("imported account unexpectedly started")
	}
	// Same thread in separate accounts has independent durable memory.
	now := time.Now()
	for _, m := range []*Manager{f.manager, second.manager} {
		input := companion.Input{Channel: "wechat", Account: m.native.Account(), Thread: "same@chatroom", Sender: "wxid_friend", Text: m.native.Account(), IsGroup: true}
		if err := m.memory.Observe(input, m.Config().Companion, now); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, account := range []string{"test", "second"} {
		wg.Add(1)
		go func(account string) {
			defer wg.Done()
			req, _ := http.NewRequest("GET", f.server.URL+"/api/companion/state?account="+account, nil)
			response, err := f.client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			var body struct {
				Scopes []companion.ScopeSummary `json:"scopes"`
			}
			json.NewDecoder(response.Body).Decode(&body)
			if response.StatusCode != 200 || len(body.Scopes) != 1 || body.Scopes[0].ID != companion.ScopeID("wechat", account, "same@chatroom") {
				t.Errorf("cross-account scope leak: %s %d", account, response.StatusCode)
			}
		}(account)
	}
	wg.Wait()
	if code, _, _ := f.request("GET", "/api/status?account=missing", nil, true, false); code != 404 {
		t.Fatal("unknown account fell back", code)
	}
	req, _ := http.NewRequest("GET", f.server.URL+"/api/status?account=second", nil)
	req.Header.Set("X-Wechat-Account", "test")
	response, _ := f.client.Do(req)
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("conflicting context accepted")
	}
	f.request("POST", "/api/logout", map[string]any{}, true, true)
	if code, _, _ := f.request("GET", "/api/status?account=second", nil, true, false); code != 401 {
		t.Fatal("child session survived logout", code)
	}
}

func TestAccountCreationPersistenceAndQRScope(t *testing.T) {
	f, registry := setupAccounts(t)
	code, b, _ := f.request("POST", "/api/accounts", map[string]string{"label": "备用号"}, true, true)
	if code != 201 {
		t.Fatalf("create %d %s", code, b)
	}
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &created)
	if !validID(created.ID) || registry.lookup(created.ID) == nil {
		t.Fatal("new runtime missing")
	}
	// Login operations may have the same upstream ID; QR paths must still stay
	// bound to the explicitly selected account, including browser image requests.
	code, _, _ = f.request("POST", "/api/wechat/login?account="+created.ID, map[string]any{}, true, true)
	if code != 202 && code != 200 {
		t.Fatal("login", code)
	}
	code, b, _ = f.request("GET", "/api/wechat/login/shared-operation/qr?generation=1&account="+created.ID, nil, true, false)
	if code != 200 || !strings.HasSuffix(string(b), created.ID) {
		t.Fatalf("wrong account QR %d", code)
	}
	data, err := os.ReadFile(filepath.Join(f.manager.dir, "accounts.json"))
	if err != nil || !strings.Contains(string(data), created.ID) {
		t.Fatal("registry not durable")
	}
	// Child handlers reuse the root password/session authority, not an empty or
	// separately initialized admin password.
	if code, _, _ := f.request("GET", "/api/config?account="+created.ID, nil, true, false); code != 200 {
		t.Fatal("shared auth", code)
	}
	if code, _, _ := f.request("POST", "/api/config?account="+created.ID, map[string]any{}, true, false); code != 403 {
		t.Fatal("child CSRF bypass", code)
	}
}

func TestDrainIncludesAllAccountWorkers(t *testing.T) {
	f, registry := setupAccounts(t)
	code, b, _ := f.request("POST", "/api/runtime/drain", map[string]any{}, true, true)
	if code != 200 {
		t.Fatal(code)
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	json.Unmarshal(b, &out)
	if registry.lookup("second").manager.drainTicket != out.Ticket {
		t.Fatal("secondary not drained")
	}
	code, _, _ = f.request("POST", "/api/accounts", map[string]any{}, true, true)
	if code != 400 {
		t.Fatal("account added during executable drain")
	}
	code, _, _ = f.request("POST", "/api/runtime/undrain", map[string]any{"ticket": out.Ticket}, true, true)
	if code != 200 || registry.lookup("second").manager.draining() {
		t.Fatal("secondary did not resume")
	}
}

func TestTwoAccountWorkersRunAndStopIndependently(t *testing.T) {
	f, registry := setupAccounts(t)
	second := registry.lookup("second").manager
	for _, m := range []*Manager{f.manager, second} {
		m.mu.Lock()
		m.config.APIKey = "synthetic-model-key-not-real-0001"
		m.mu.Unlock()
		if err := m.Toggle(true); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.manager.BotStatus().State == "running" && second.BotStatus().State == "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.manager.BotStatus().State != "running" || second.BotStatus().State != "running" {
		t.Fatal("both workers did not start", f.manager.BotStatus(), second.BotStatus())
	}
	if err := second.Toggle(false); err != nil {
		t.Fatal(err)
	}
	if f.manager.BotStatus().State != "running" || second.BotStatus().State != "disabled" {
		t.Fatal("stopping one account affected another")
	}
}

func TestWaitingLoginWorkerCanDrainWithoutDiscardingLogin(t *testing.T) {
	f := setup(t)
	f.login()
	f.manager.mu.Lock()
	f.manager.config.APIKey = "synthetic-model-key-not-real-0001"
	f.manager.mu.Unlock()
	if err := f.manager.Toggle(true); err != nil {
		t.Fatal(err)
	}
	code, b, _ := f.request("POST", "/api/runtime/drain", map[string]any{}, true, true)
	if code != 200 {
		t.Fatal(code)
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	json.Unmarshal(b, &out)
	deadline := time.Now().Add(4 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		_, b, _ = f.request("GET", "/api/runtime/drain?ticket="+out.Ticket, nil, true, false)
		var status struct {
			Ready bool `json:"ready"`
		}
		json.Unmarshal(b, &status)
		if status.Ready {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready || !f.manager.Config().Enabled || f.native.loginCalls.Load() != 0 {
		t.Fatal("waiting login blocked drain, changed enablement or restarted login")
	}
}
