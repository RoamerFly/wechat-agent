package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"wechat-bot/internal/station"
)

func TestLocalAndLegacySessionsAreIndependentInOneBrowser(t *testing.T) {
	base := t.TempDir()
	legacy := setup(t)
	const stationPassword = "synthetic-local-password"
	handler, err := station.NewLocalHomeHandler(filepath.Join(base, "host"), filepath.Join(base, "workspace"), stationPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	// Cookies are shared across ports on loopback.
	local := &webFixture{t: t, server: server, client: legacy.client}
	legacy.login()
	if code, _, _ := local.request("GET", "/api/apps", nil, false, false); code != http.StatusUnauthorized {
		t.Fatalf("legacy session opened the station registry: %d", code)
	}
	code, body, _ := local.request("POST", "/api/login", map[string]string{"password": stationPassword}, true, false)
	var auth struct {
		CSRF string `json:"csrf"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &auth) != nil || auth.CSRF == "" {
		t.Fatalf("station login failed: %d", code)
	}
	local.csrf = auth.CSRF
	if code, _, _ := local.request("GET", "/api/apps", nil, false, false); code != http.StatusOK {
		t.Fatalf("owner session could not open the station registry: %d", code)
	}
	if code, _, _ := legacy.request("GET", "/api/apps", nil, false, false); code != http.StatusOK {
		t.Fatalf("station login replaced the legacy session: %d", code)
	}
	oldPassword, newPassword := "test-password-at-least-24-bytes", "synthetic-legacy-replacement"
	if code, _, _ := legacy.request("POST", "/api/password", map[string]string{"current_password": oldPassword, "new_password": newPassword}, true, true); code != http.StatusOK {
		t.Fatalf("legacy password rotation failed: %d", code)
	}
	if code, _, _ := local.request("GET", "/api/apps", nil, false, false); code != http.StatusOK {
		t.Fatalf("legacy password rotation invalidated the station session: %d", code)
	}
	if code, _, _ := legacy.request("GET", "/api/apps", nil, false, false); code != http.StatusUnauthorized {
		t.Fatalf("owner session opened the legacy registry: %d", code)
	}
	if code, _, _ := legacy.request("POST", "/api/login", map[string]string{"password": newPassword}, true, false); code != http.StatusOK {
		t.Fatalf("legacy reauthentication failed: %d", code)
	}
	if code, _, _ := local.request("POST", "/api/logout", nil, true, true); code != http.StatusOK {
		t.Fatalf("station logout failed: %d", code)
	}
	if code, _, _ := local.request("GET", "/api/apps", nil, false, false); code != http.StatusUnauthorized {
		t.Fatalf("station session survived logout: %d", code)
	}
	if code, _, _ := legacy.request("GET", "/api/apps", nil, false, false); code != http.StatusOK {
		t.Fatalf("station logout invalidated the legacy session: %d", code)
	}
}
