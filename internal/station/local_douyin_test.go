package station

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
)

func TestLocalDouyinRetiresDeveloperAuthorizationAndAPIConnections(t *testing.T) {
	base := t.TempDir()
	data, home := filepath.Join(base, "host"), filepath.Join(base, "home")
	settings := filepath.Join(data, "apps", "douyin")
	if err := os.MkdirAll(settings, 0700); err != nil {
		t.Fatal(err)
	}
	// A previously saved credential must not reactivate a retired connection.
	original := []byte(`{"schema_version":1,"client_key":"synthetic-key","access_token":"synthetic-retired-token"}`)
	file := filepath.Join(settings, "account-settings.json")
	if err := os.WriteFile(file, original, 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := apphost.New(douyinapp.New(settings).Registration())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLocalHomeHandlerWithApps(data, home, "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	f := newLocalAppObservationFixtureWithHandler(t, handler, data, home)
	f.login()
	if code, _ := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != 200 {
		t.Fatal(code)
	}
	code, rows := f.request("GET", "/api/apps/douyin/connections", nil, false)
	if code != 200 {
		t.Fatal(rows)
	}
	items, ok := rows["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != douyinapp.WebReadConnectionID {
		t.Fatalf("ordinary account did not exclusively expose its website connection: %v", rows)
	}

	for _, connection := range []string{"search", "own-videos", "authorized-account"} {
		in := map[string]any{"app_id": "douyin", "connection_id": connection, "content_id": "synthetic", "field": "body"}
		if code, _ := f.request("POST", "/api/local/apps/observation", in, true); code != http.StatusNotFound {
			t.Errorf("retired connection %s reachable: %d", connection, code)
		}
	}
	for _, method := range []string{"GET", "POST"} {
		request, err := http.NewRequest(method, f.server.URL+"/api/local/apps/douyin/authorization", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := f.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("retired authorization route %s: %d", method, response.StatusCode)
		}
	}
	if code, out := f.request("GET", "/api/local/apps/douyin/web-login", nil, false); code != 200 || out["state"] != "unconfigured" {
		t.Fatalf("ordinary account login lost: %d %v", code, out)
	}
	stored, err := os.ReadFile(file)
	if err != nil || string(stored) != string(original) {
		t.Fatal("retiring routes changed private historical data", err)
	}
}
