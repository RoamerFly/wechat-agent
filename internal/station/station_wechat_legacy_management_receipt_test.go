package station

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
)

func TestLegacyManagementReceiptImportIsLocalAndSeparate(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the source snapshot is a Windows desktop artifact")
	}
	base := t.TempDir()
	legacy, snapshot := filepath.Join(base, "legacy"), filepath.Join(base, "snapshot")
	fixture := newLegacyWechatSnapshotFixture(t, legacy)
	mustWriteSnapshotFixture(t, filepath.Join(legacy, "admin", "tool-state.json"), []byte(`{
		"version":1,"sends":{
			"accepted-one":{"key":"accepted-one","hash":"synthetic","account":"one","thread":"manual-thread","kind":"text","status":{"id":"manual-job","state":"accepted","server_id":"72123"}},
			"management-only":{"key":"management-only","hash":"synthetic","account":"one","thread":"manual-thread","kind":"voice","status":{"id":"manual-voice-job","state":"running","voice_parts_incomplete":true,"voice_chunks":[{"index":0,"state":"accepted","server_id":"72124","has_receipt":true},{"index":1,"state":"prepared"}]}}
		},"uploads":{"private":"synthetic-not-a-receipt"}}`))
	capture := LegacyWechatSnapshotRequest{LegacyDataDir: legacy, SnapshotDir: snapshot, PrimaryAccount: fixture.primary}
	preview, err := CaptureLegacyWechatSnapshot(t.Context(), capture)
	if err != nil {
		t.Fatal(err)
	}
	capture.ApplyDigest = preview.ApplyDigest
	if _, err := CaptureLegacyWechatSnapshot(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	host, home := filepath.Join(base, "host"), filepath.Join(base, "home")
	app := filepath.Join(host, "apps", "wechat")
	native := filepath.Join(app, "native")
	mustWriteSnapshotFixture(t, filepath.Join(native, "api-token"), []byte("synthetic-legacy-native-token"))
	config, err := json.Marshal(legacyImportSource{Root: snapshot, LegacyHostRoot: legacy, Primary: fixture.primary, Stopped: true, ManifestDigest: preview.ManifestDigest})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteSnapshotFixture(t, filepath.Join(app, legacyImportSourceFile), config)
	var nativeCalls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		if r.Method != http.MethodGet {
			t.Error("receipt import attempted a native write")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch r.URL.Path {
		case "/v1/status":
			result = map[string]any{"pid": 9432, "dataDir": native, "serverId": "new-dataset", "ownershipGeneration": "synthetic-generation"}
		case "/v1/accounts":
			result = map[string]any{"items": []map[string]string{{"id": fixture.primary}, {"id": fixture.secondary}}}
		case "/v1/accounts/one", "/v1/accounts/two":
			result = map[string]any{"id": filepath.Base(r.URL.Path), "storageMode": "managed", "authenticated": false, "connection": "offline"}
		default:
			t.Errorf("receipt import attempted an unexpected native query %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	t.Cleanup(daemon.Close)
	entry, err := wechatapp.StationRegistration(t.Context(), daemon.URL, 9432, host)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := apphost.New(entry)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLocalHomeHandlerWithApps(host, home, "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	f := &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}, dir: host, root: home, handler: handler}
	req := func(method, path string, data any, expected int) map[string]any {
		t.Helper()
		code, result := f.request(method, path, data, method == "POST")
		if code != expected {
			t.Fatalf("%s %s: %d want %d: %#v", method, path, code, expected, result)
		}
		return result
	}
	route := func(account string) string {
		return "/api/apps/wechat/connections/" + wechatapp.ConnectionID(account) + "/legacy-import"
	}
	primary, secondary := route(fixture.primary), route(fixture.secondary)
	const origin = "?origin=management"
	req("GET", primary+"/receipts"+origin, nil, 401)
	f.login()
	plan := req("GET", primary, nil, 200)
	if plan["receipt_records"] != float64(1) || plan["management_receipt_records"] != float64(2) || plan["management_unresolved_receipts"] != float64(1) {
		t.Fatal("preview omitted the separate ledger counts", plan)
	}
	projection := filepath.Join(snapshot, "admin", "tool-state.json")
	valid := mustReadSnapshotFixture(t, projection)
	mustWriteSnapshotFixture(t, projection, append(valid, '\n'))
	req("POST", primary, map[string]any{"digest": plan["digest"]}, 422)
	mustWriteSnapshotFixture(t, projection, valid)
	req("POST", primary, map[string]any{"digest": plan["digest"]}, 200)
	other := req("GET", secondary, nil, 200)
	if other["management_receipt_records"] != nil {
		t.Fatal("secondary account read the primary ledger", other)
	}
	req("POST", secondary, map[string]any{"digest": other["digest"]}, 200)
	activation := req("GET", primary+"/activation", nil, 200)
	if activation["management_pending_receipts"] != float64(1) {
		t.Fatal("activation omitted unresolved management receipts", activation)
	}
	if err := os.Rename(snapshot, filepath.Join(base, "detached-snapshot")); err != nil {
		t.Fatal(err)
	}
	before := nativeCalls.Load()
	page := req("GET", primary+"/receipts"+origin+"&limit=1", nil, 200)
	first := page["items"].([]any)[0].(map[string]any)
	if page["coverage"] != legacyManagementReceiptLookup || first["lookup"] != legacyManagementReceiptLookup || first["state"] != "accepted" || first["operation_id"] != "manual-job" {
		t.Fatal("management history lost its own receipt for the shared key", page)
	}
	worker := req("GET", primary+"/receipts", nil, 200)["items"].([]any)[0].(map[string]any)
	if worker["idempotency_key"] != first["idempotency_key"] || worker["operation_id"] != "job-one" {
		t.Fatal("management receipt overwrote the worker ledger", worker)
	}
	after := "after=" + url.QueryEscape(page["next_after"].(string))
	req("GET", primary+"/receipts?"+after, nil, 409)
	req("GET", primary+"/receipts"+origin+"&"+after, nil, 200)
	filtered := req("GET", primary+"/receipts"+origin+"&pending=1", nil, 200)
	detail := req("GET", primary+"/receipts/management-only"+origin, nil, 200)
	if len(filtered["items"].([]any)) != 1 || detail["state"] != "submitted_pending" || detail["lookup"] != legacyManagementReceiptLookup ||
		len(detail["accepted_parts"].([]any)) != 1 || len(detail["unconfirmed_parts"].([]any)) != 1 {
		t.Fatal("management snapshot lost the uncertainty or accepted part", detail)
	}
	req("GET", primary+"/receipts/management-only", nil, 404)
	req("GET", secondary+"/receipts/management-only"+origin, nil, 404)
	req("GET", primary+"/receipts?origin=invalid", nil, 400)
	if nativeCalls.Load() != before {
		t.Fatal("offline archive read contacted the native service")
	}
	req("GET", "/api/apps/wechat/connections/"+wechatapp.ConnectionID(fixture.primary)+"/receipts/management-only", nil, 404)
}
