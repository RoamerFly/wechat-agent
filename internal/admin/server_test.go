package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"wechat-bot/internal/devicebroker"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/sending"
)

type fakeNative struct{ loginCalls atomic.Int32 }

func (*fakeNative) Account() string                          { return "test" }
func (*fakeNative) Identity(context.Context) (string, error) { return "server-1", nil }
func (*fakeNative) Events(context.Context, string) (linuxnative.EventPage, error) {
	return linuxnative.EventPage{Items: []linuxnative.Event{}, NextCursor: "0"}, nil
}
func (*fakeNative) Message(context.Context, string) (linuxnative.Message, error) {
	return linuxnative.Message{}, nil
}
func (*fakeNative) Ready() bool { return false }
func (*fakeNative) SendText(context.Context, sending.TextRequest) (sending.Receipt, error) {
	panic("web controls must not send test chat messages")
}
func (*fakeNative) Status(context.Context) (linuxnative.Status, error) {
	return linuxnative.Status{Connection: "disconnected"}, nil
}
func (*fakeNative) Groups(context.Context, string) ([]linuxnative.Group, error) {
	return []linuxnative.Group{{ID: "room@chatroom", Name: "测试群"}}, nil
}
func (n *fakeNative) BeginLogin(context.Context, string) (linuxnative.LoginOperation, error) {
	n.loginCalls.Add(1)
	return linuxnative.LoginOperation{ID: "op-1", State: "awaiting_scan", QRAvailable: true, QRGeneration: 1}, nil
}
func (*fakeNative) LoginStatus(context.Context, string) (linuxnative.LoginOperation, error) {
	return linuxnative.LoginOperation{ID: "op-1", State: "awaiting_scan", QRAvailable: true, QRGeneration: 1}, nil
}
func (*fakeNative) LoginQR(context.Context, string, int) ([]byte, error) {
	return []byte("\x89PNG\r\n\x1a\n"), nil
}

type webFixture struct {
	t       *testing.T
	manager *Manager
	server  *httptest.Server
	client  *http.Client
	csrf    string
	native  *fakeNative
	dir     string
}

func setup(t *testing.T) *webFixture {
	t.Helper()
	native := &fakeNative{}
	dir := t.TempDir()
	m, err := NewManagerWithDeviceIO(dir, "", native, devicebroker.Host{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(m, "test-password-at-least-24-bytes", "")
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	jar, _ := cookiejar.New(nil)
	f := &webFixture{t: t, manager: m, server: s, client: &http.Client{Jar: jar}, native: native, dir: dir}
	t.Cleanup(func() { s.Close(); _ = m.Close() })
	return f
}
func (f *webFixture) request(method, path string, input any, origin, csrf bool) (int, []byte, *http.Response) {
	f.t.Helper()
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			f.t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	r, err := http.NewRequest(method, f.server.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if origin {
		r.Header.Set("Origin", f.server.URL)
	}
	if csrf {
		r.Header.Set("X-CSRF-Token", f.csrf)
	}
	response, err := f.client.Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return response.StatusCode, data, response
}
func (f *webFixture) login() {
	status, data, response := f.request("POST", "/api/login", map[string]string{"password": "test-password-at-least-24-bytes"}, true, false)
	if status != 200 {
		f.t.Fatalf("login %d %s", status, data)
	}
	var value struct {
		CSRF string `json:"csrf"`
	}
	if json.Unmarshal(data, &value) != nil || value.CSRF == "" {
		f.t.Fatal("missing CSRF")
	}
	f.csrf = value.CSRF
	cookies := response.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		f.t.Fatal("unsafe session cookie")
	}
}

func TestAuthCSRFKeyRedactionAndPersistentSettings(t *testing.T) {
	f := setup(t)
	if code, _, _ := f.request("GET", "/api/config", nil, false, false); code != 401 {
		t.Fatal("unauthorized configuration accessible")
	}
	if code, _, _ := f.request("POST", "/api/login", map[string]string{"password": "test-password-at-least-24-bytes"}, false, false); code != 403 {
		t.Fatal("cross-site login accepted")
	}
	f.login()
	c := defaults()
	c.APIKey = "private-provider-key-must-never-be-returned"
	c.SystemPrompt = "你是我的测试助手。"
	c.GroupRules = []GroupRule{{ID: "room@chatroom", Name: "测试群", Mode: "all", Prefix: "问："}}
	if code, _, _ := f.request("POST", "/api/config", ConfigInput{Config: c}, true, false); code != 403 {
		t.Fatal("CSRF token not required")
	}
	if code, _, _ := f.request("POST", "/api/config", ConfigInput{Config: c}, false, true); code != 403 {
		t.Fatal("Origin not required")
	}
	code, data, _ := f.request("POST", "/api/config", ConfigInput{Config: c}, true, true)
	if code != 200 || bytes.Contains(data, []byte(c.APIKey)) {
		t.Fatalf("unsafe configuration response %d", code)
	}
	var view ConfigView
	if json.Unmarshal(data, &view) != nil || !view.KeyConfigured || view.APIKey != "" {
		t.Fatal("key status incorrect")
	}
	view.Model = "another-model"
	code, data, _ = f.request("POST", "/api/config", ConfigInput{Config: view.Config}, true, true)
	if code != 200 || bytes.Contains(data, []byte(c.APIKey)) {
		t.Fatal("blank key update failed")
	}
	loaded, err := loadConfig(f.dir)
	if err != nil || loaded.APIKey != c.APIKey || loaded.Model != "another-model" || loaded.GroupRules[0].Mode != "all" {
		t.Fatal("settings did not persist or key was overwritten")
	}
	changedOrigin := f.manager.Config()
	changedOrigin.BaseURL = "https://another-provider.example/v1"
	if code, _, _ := f.request("POST", "/api/config", ConfigInput{Config: changedOrigin.Config}, true, true); code != 400 {
		t.Fatal("old provider key could silently move to another origin")
	}
	for _, path := range []string{"/api/config", "/api/status", "/api/logs"} {
		if code, data, _ := f.request("GET", path, nil, false, false); code != 200 || bytes.Contains(data, []byte(c.APIKey)) {
			t.Fatal("credential leaked or endpoint failed", path)
		}
	}
	if code, _, _ := f.request("POST", "/api/bot/start", map[string]any{}, true, true); code != 200 {
		t.Fatal("bot enable failed")
	}
	if !f.manager.Config().Enabled {
		t.Fatal("bot enable not persisted")
	}
	clear := f.manager.Config()
	clear.Enabled = false
	if code, _, _ := f.request("POST", "/api/config", ConfigInput{Config: clear.Config, ClearKey: true}, true, true); code != 200 {
		t.Fatal("clear key failed")
	}
	if f.manager.Config().KeyConfigured || f.manager.Config().Enabled {
		t.Fatal("key clear did not stop bot")
	}
	if code, _, _ := f.request("POST", "/api/logout", map[string]any{}, true, true); code != 200 {
		t.Fatal("logout failed")
	}
	if code, _, _ := f.request("GET", "/api/config", nil, false, false); code != 401 {
		t.Fatal("logout did not revoke session")
	}
}

func TestAssetsSecurityHeadersAndLoginCoalescing(t *testing.T) {
	f := setup(t)
	for _, path := range []string{"/", "/local-home", "/assets/app.js", "/assets/app.css", "/assets/theme.js", "/assets/whale-girl.png", "/assets/whale-girl-cutout.png", "/assets/life-journal.js", "/assets/night-sea.webp", "/assets/palace.js", "/assets/palace-graph.js", "/assets/palace-graph.css"} {
		code, data, response := f.request("GET", path, nil, false, false)
		if code != 200 || len(data) == 0 || !strings.Contains(response.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("asset/header failure %s", path)
		}
		if path == "/" && !bytes.Contains(data, []byte("/assets/app.css")) ||
			path == "/local-home" && !bytes.Contains(data, []byte("/assets/local-home.css")) {
			t.Fatalf("wrong HTML shell for %s", path)
		}
		if strings.HasSuffix(path, ".webp") && (response.Header.Get("Content-Type") != "image/webp" || len(data) < 12 || string(data[8:12]) != "WEBP") {
			t.Fatalf("journal illustration was not served as WebP")
		}
	}
	f.login()
	for i := 0; i < 2; i++ {
		if code, _, _ := f.request("POST", "/api/wechat/login", map[string]any{}, true, true); code != 200 {
			t.Fatal("login failed")
		}
	}
	if f.native.loginCalls.Load() != 1 {
		t.Fatal("duplicate QR login submitted")
	}
	if code, data, _ := f.request("GET", "/api/wechat/login/op-1/qr?generation=1", nil, false, false); code != 200 || !bytes.HasPrefix(data, []byte("\x89PNG")) {
		t.Fatal("QR proxy failed")
	}
	if code, _, _ := f.request("GET", "/api/wechat/login/op-1/qr?generation=bad", nil, false, false); code != 400 {
		t.Fatal("invalid QR generation accepted")
	}
}

func TestValidationRejectsBrokenOrUnsafeSettingsWithoutOverwriting(t *testing.T) {
	f := setup(t)
	f.login()
	for _, edit := range []func(*Config){
		func(c *Config) { c.BaseURL = "http://api.deepseek.com" },
		func(c *Config) { c.BaseURL = "https://user:key@example.com" },
		func(c *Config) { c.GroupRules = []GroupRule{{ID: "not-a-room", Mode: "all"}} },
		func(c *Config) { c.MaxAgeSeconds = 0 },
		func(c *Config) { c.Enabled = true },
	} {
		c := defaults()
		edit(&c)
		if code, _, _ := f.request("POST", "/api/config", ConfigInput{Config: c}, true, true); code != 400 {
			t.Fatal("invalid settings accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "config.json")); !os.IsNotExist(err) {
		t.Fatal("invalid input overwrote persistent state")
	}
}

func TestLoginRateLimitAndCredentialCreation(t *testing.T) {
	f := setup(t)
	for i := 0; i < 11; i++ {
		code, _, _ := f.request("POST", "/api/login", map[string]string{"password": "wrong"}, true, false)
		if i < 10 && code != 401 || i == 10 && code != 429 {
			t.Fatal("login rate limit failed")
		}
	}
	dir := t.TempDir()
	one, err := Password(dir, "")
	if err != nil || one != "admin" {
		t.Fatal("credential initialization failed")
	}
	two, err := Password(dir, "")
	if err != nil || one != two {
		t.Fatal("credential changed on restart")
	}
}
