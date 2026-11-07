package station

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
)

func TestLocalWechatAccountSetupUsesOwnerAndExactNativeIdentity(t *testing.T) {
	base := t.TempDir()
	host := filepath.Join(base, "host")
	native := filepath.Join(host, "apps", "wechat", "native")
	if err := os.MkdirAll(native, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "api-token"), []byte("synthetic-native-setup-token"), 0600); err != nil {
		t.Fatal(err)
	}
	const key = "11111111111141118111111111111111"
	const account = "account-" + key
	var mu sync.Mutex
	created, starts, qrReads := 0, 0, 0
	generation, savedState := 2, "awaiting_scan"
	identity, operationKind := account, "login"
	generationID := "fixture-original"
	unknownResponse := true
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer synthetic-native-setup-token" {
			t.Error("missing host credential")
			w.WriteHeader(401)
			return
		}
		out := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		operation := func() map[string]any {
			return map[string]any{"id": "operation-1", "accountId": identity, "kind": operationKind, "state": savedState,
				"result": map[string]any{"qrAvailable": true, "qrGeneration": generation, "code": "synthetic-private-result"}}
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/status":
			out(map[string]any{"pid": 5678, "dataDir": native, "serverId": "fixture-station", "ownershipGeneration": generationID})
		case "GET /v1/accounts":
			items := []map[string]any{{"id": "other", "authenticated": true, "connection": "online"}}
			if created > 0 {
				items = append(items, map[string]any{"id": account})
			}
			out(map[string]any{"items": items})
		case "GET /v1/accounts/" + account:
			if created == 0 {
				w.WriteHeader(404)
				return
			}
			out(map[string]any{"id": account})
		case "POST /v1/accounts":
			var input map[string]string
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input["id"] != account {
				t.Error("wrong native account registration", input)
			}
			created++
			w.WriteHeader(201)
			out(map[string]any{"id": account})
		case "POST /v1/accounts/" + account + "/auth/login":
			if r.Header.Get("Idempotency-Key") != "station-login-"+key {
				t.Error("login retry changed the key")
			}
			if starts == 0 {
				starts++
			}
			w.WriteHeader(202)
			if unknownResponse {
				unknownResponse = false
				return
			}
			out(operation())
		case "GET /v1/accounts/" + account + "/operations/operation-1":
			out(operation())
		case "GET /v1/accounts/" + account + "/operations/operation-1/qr":
			qrReads++
			if r.URL.Query().Get("generation") != "2" {
				t.Error("wrong generation")
			}
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nsynthetic"))
		default:
			t.Error("unexpected native path", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer daemon.Close()
	registration, err := wechatapp.StationRegistration(t.Context(), daemon.URL, 5678, host)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := apphost.New(registration)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLocalHomeHandlerWithApps(host, filepath.Join(base, "workspace"), "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	f := &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}}
	const connections = "/api/apps/wechat/connections"
	in := map[string]string{"request_id": key}
	expect := func(method, path string, value any, csrf bool, want int) map[string]any {
		t.Helper()
		code, body := f.request(method, path, value, csrf)
		if code != want {
			t.Fatalf("%s %s: got %d, want %d: %#v", method, path, code, want, body)
		}
		return body
	}
	login := connections + "/" + wechatapp.ConnectionID(account) + "/login"
	status := login + "/operation-1"
	qr := status + "/qr?generation="
	expect("POST", connections, in, false, 401)
	expect("GET", qr+"2", nil, false, 401)
	f.login()
	expect("POST", connections, in, false, 403)
	expect("POST", connections, map[string]string{"request_id": "../escape"}, true, 400)
	expect("POST", login, in, true, 404)
	expect("POST", connections+"/"+wechatapp.ConnectionID("other")+"/login", in, true, 409)
	for i := 0; i < 2; i++ {
		row := expect("POST", connections, in, true, 200)
		if row["account_id"] != account || row["id"] != wechatapp.ConnectionID(account) {
			t.Fatal(row)
		}
	}
	expect("POST", login, in, true, 503)
	op := expect("POST", login, in, true, 200)
	if op["id"] != "operation-1" || op["state"] != "awaiting_scan" || op["qr_available"] != true || op["code"] != nil {
		t.Fatal(op)
	}
	expect("GET", status, nil, false, 200)
	expect("GET", qr+"1", nil, false, 404)
	expect("GET", qr+"101", nil, false, 400)
	image, err := f.client.Get(server.URL + qr + "2")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(image.Body)
	image.Body.Close()
	if err != nil || image.StatusCode != 200 || image.Header.Get("Content-Type") != "image/png" || image.Header.Get("Cache-Control") != "no-store" || !strings.HasSuffix(string(payload), "synthetic") {
		t.Fatal(image.StatusCode, image.Header, err)
	}
	mu.Lock()
	identity = "other"
	mu.Unlock()
	expect("GET", status, nil, false, 404)
	mu.Lock()
	identity, operationKind = account, "logout"
	mu.Unlock()
	expect("GET", qr+"2", nil, false, 404)
	mu.Lock()
	operationKind, savedState = "login", "expired"
	mu.Unlock()
	expect("GET", qr+"2", nil, false, 404)
	mu.Lock()
	generationID = "fixture-replaced"
	if created != 1 || starts != 1 || qrReads != 1 {
		t.Error("unexpected native write/read counts", created, starts, qrReads)
	}
	mu.Unlock()
	expect("GET", status, nil, false, 503)
}
