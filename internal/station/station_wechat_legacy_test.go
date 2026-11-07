package station

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

func TestLocalWechatLegacyImportKeepsEvidenceWithoutReplay(t *testing.T) {
	base := t.TempDir()
	host, work, snapshot := filepath.Join(base, "host"), filepath.Join(base, "workspace"), filepath.Join(base, "snapshot")
	// The WeChat cutover runs on the same station that already imported its
	// model, stable identity, persistent project and both private voice types.
	ownerIdentity, speakingVoice := prepareLegacyCutover(t, base, host, work)
	legacyHost := filepath.Join(base, "old-host")
	app := filepath.Join(host, "apps", "wechat")
	native := filepath.Join(app, "native")
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	j := func(v any) []byte { b, _ := json.Marshal(v); return b }
	write(filepath.Join(native, "api-token"), []byte("synthetic-legacy-native-token"))
	sourceConfig := filepath.Join(app, legacyImportSourceFile)
	source := legacyImportSource{Root: filepath.ToSlash(snapshot), LegacyHostRoot: filepath.ToSlash(legacyHost), Primary: "one", Stopped: true}
	marker := legacyStopMarker{Version: 1, State: "stopped", Epoch: strings.Repeat("a", 32), AccountID: "one", DataRoot: legacyHost, NativeServerID: "old-dataset", At: time.Now().UTC().Add(-time.Minute)}
	liveMarker := filepath.Join(legacyHost, legacyStopMarkerFile)
	write(liveMarker, j(marker))
	write(filepath.Join(snapshot, legacyStopMarkerFile), j(marker))
	profile := filepath.Join(native, "accounts", "one", "profile")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(app, "native-import-"+wechatapp.ConnectionID("one")+".json")
	receiptProfile := profile
	if os.PathSeparator == '\\' {
		receiptProfile = `\\?\` + profile
	}
	receiptJSON := j(map[string]string{"id": "one", "storageMode": "managed", "profilePath": receiptProfile})
	write(receiptPath, receiptJSON)
	write(sourceConfig, j(source))
	const rawConfig = `{"enabled":true,"connection_paused":false,"api_key":"synthetic-do-not-copy-legacy-key","private_enabled":true,"private_all_friends":true,"direct_ids":["friend-one"],"filehelper_enabled":true,"companion":{"enabled":true,"permissions":{"reply_text":true}},"group_rules":[{"id":"123@chatroom","mode":"mention"},{"id":"124@chatroom","mode":"all","prefix":"/group ","persona":"synthetic scoped role","force_reply_when_mentioned":true},{"id":"125@chatroom","mode":"social"}]}`
	configPath := filepath.Join(snapshot, "admin", "config.json")
	write(configPath, []byte(rawConfig))
	write(filepath.Join(snapshot, "admin", "accounts.json"), j([]wechatapp.LegacyAccountRecord{{ID: "two", Removed: true}, {ID: "three"}, {ID: "four"}, {ID: "five"}}))
	write(filepath.Join(snapshot, "admin", "accounts", wechatapp.AccountDirectory("three"), "config.json"), []byte(rawConfig))
	write(filepath.Join(snapshot, "admin", "accounts", wechatapp.AccountDirectory("four"), "config.json"), []byte(rawConfig))
	write(filepath.Join(snapshot, "admin", "accounts", wechatapp.AccountDirectory("five"), "config.json"), []byte(rawConfig))
	queuePath := filepath.Join(snapshot, "bot-state", "messages-v2.json")
	const rawQueue = `{"version":1,"server":"old-dataset","account":"one","inbox":[{"phase":"received"}],"deliveries":[{"key":"legacy-accepted","id":"job-accepted","thread":"filehelper","state":"accepted","server_id":"69123","text":"do-not-import-old-body","input":{"account":"one","thread":"filehelper"}},{"key":"legacy-operation-unknown","id":"","thread":"filehelper","state":"waiting","text":"do-not-import-old-unknown","input":{"account":"one","thread":"filehelper"}},{"key":"legacy-pending","id":"job-pending","thread":"filehelper","state":"waiting","text":"do-not-import-old-draft","input":{"account":"one","thread":"filehelper"}}]}`
	write(queuePath, []byte(rawQueue))
	write(filepath.Join(snapshot, "bot-state", "checkpoint.json"), []byte(`{"version":1,"server_id":"old-dataset","account":"one","cursor":"62"}`))
	write(filepath.Join(snapshot, "bot-state", "accounts", wechatapp.AccountDirectory("three"), "messages-v2.json"), []byte(strings.ReplaceAll(rawQueue, `"account":"one"`, `"account":"two"`)))
	sameQueue := strings.ReplaceAll(strings.ReplaceAll(rawQueue, `"account":"one"`, `"account":"four"`), "old-dataset", "new-dataset")
	write(filepath.Join(snapshot, "bot-state", "accounts", wechatapp.AccountDirectory("four"), "messages-v2.json"), []byte(sameQueue))

	var sideEffects, receiptReads, eventReads, guardedWrites atomic.Int32
	var pushAfterInvalid atomic.Bool
	var daemonOffline atomic.Bool
	var loginOnline atomic.Bool
	var settledDelivery atomic.Bool
	writeStarted, allowWriteFinish := make(chan struct{}), make(chan struct{})
	var holdAuditLookup atomic.Bool
	auditLookupStarted, allowAuditLookup := make(chan struct{}), make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-legacy-native-token" {
			t.Error("wrong daemon credential")
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/v1/accounts/one/threads/123@chatroom/notifications" {
			guardedWrites.Add(1)
			if r.Header.Get("Idempotency-Key") == "rollback-inflight" {
				close(writeStarted)
				w.Header().Set("Content-Type", "application/json")
				w.(http.Flusher).Flush()
				select {
				case <-allowWriteFinish:
				case <-r.Context().Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "synthetic-action", "accountId": "one", "threadId": "123@chatroom", "kind": "mute", "state": "succeeded"})
			return
		}
		if r.Method != http.MethodGet {
			sideEffects.Add(1)
			w.WriteHeader(400)
			return
		}
		if daemonOffline.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"pid": 9432, "dataDir": native, "serverId": "new-dataset", "ownershipGeneration": "synthetic-generation"})
		case "/v1/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{{"id": "one"}, {"id": "two"}, {"id": "three"}, {"id": "four"}, {"id": "five"}}})
		case "/v1/accounts/one", "/v1/accounts/four", "/v1/accounts/five":
			account := filepath.Base(r.URL.Path)
			connection := "offline"
			online := account == "one" && loginOnline.Load()
			if online {
				connection = "online"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": account, "storageMode": "managed", "authenticated": online, "connection": connection})
		case "/v1/accounts/four/sends/job-pending":
			receiptReads.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-pending", "accountId": "four", "threadId": "filehelper", "kind": "text", "state": "accepted", "serverId": "69234"})
		case "/v1/accounts/one/sends/by-idempotency-key":
			if r.URL.Query().Get("key") != "b-active" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if holdAuditLookup.Load() {
				close(auditLookupStarted)
				select {
				case <-allowAuditLookup:
				case <-r.Context().Done():
					return
				}
			}
			status := map[string]string{"id": "job-active", "accountId": "one", "threadId": "filehelper", "kind": "text", "state": "running"}
			if settledDelivery.Load() {
				status["state"], status["serverId"] = "accepted", "91235"
			}
			_ = json.NewEncoder(w).Encode(status)
		case "/v1/accounts/one/events":
			eventReads.Add(1)
			page := linuxnative.EventPage{Items: []linuxnative.Event{}, NextCursor: "0"}
			if r.URL.Query().Get("after") == "1" {
				page.NextCursor = "1"
			} else if pushAfterInvalid.Load() {
				event := linuxnative.Event{Cursor: "1", Kind: "message.committed"}
				event.Data.MessageID, event.Data.ThreadID, event.Data.Direction = "must-not-read", "filehelper", "incoming"
				page.Items, page.NextCursor = []linuxnative.Event{event}, "1"
			}
			_ = json.NewEncoder(w).Encode(page)
		default:
			t.Error("import contacted an unexpected native endpoint", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer daemon.Close()
	entry, err := wechatapp.StationRegistration(t.Context(), daemon.URL, 9432, host)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := apphost.New(entry)
	if err != nil {
		t.Fatal(err)
	}
	open := func() (*LocalHomeHandler, *localHomeFixture) {
		t.Helper()
		handler, err := NewLocalHomeHandlerWithApps(host, work, "synthetic-local-password", "", registry)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		t.Cleanup(func() {
			server.Close()
			if err := handler.Close(); err != nil {
				t.Errorf("close cutover fixture: %v", err)
			}
		})
		jar, _ := cookiejar.New(nil)
		return handler, &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}, dir: host, root: work, handler: handler}
	}
	h, f := open()
	route := func(id string) string { return "/api/apps/wechat/connections/" + wechatapp.ConnectionID(id) }
	planRoute := func(id string) string { return route(id) + "/legacy-import" }
	legacyReceiptRoute := func(id string) string { return planRoute(id) + "/receipts" }
	activationRoute := planRoute("one") + "/activation"
	req := func(method, path string, input any, csrf bool, want int) map[string]any {
		t.Helper()
		code, body := f.request(method, path, input, csrf)
		if code != want {
			t.Fatalf("%s %s: got %d want %d: %#v", method, path, code, want, body)
		}
		return body
	}
	memoryRoute := func(account string) string { return planRoute(account) + "/memory" }
	ruleRoute := func(account, thread string) string { return planRoute(account) + "/groups/" + thread + "/rule" }
	req("GET", planRoute("one"), nil, false, 401)
	req("GET", legacyReceiptRoute("one"), nil, false, 401)
	req("GET", activationRoute, nil, false, 401)
	req("GET", memoryRoute("one"), nil, false, 401)
	req("GET", memoryRoute("one")+"/123@chatroom/preference", nil, false, 401)
	req("GET", ruleRoute("one", "124@chatroom"), nil, false, 401)
	f.login()
	guardedSender, err := h.chat.wechat.WorkerSender(t.Context(), wechatapp.ConnectionID("one"))
	if err != nil {
		t.Fatal(err)
	}
	guardedAction := linuxnative.AdminAction{Kind: "mute", ThreadID: "123@chatroom", Muted: true}
	if _, err := guardedSender.ExecuteAction(t.Context(), guardedAction, "pre-archive"); !errors.Is(err, linuxnative.ErrWriteNotAdmitted) || guardedWrites.Load() != 0 {
		t.Fatal("imported native profile could write before the archive was applied", err)
	}
	assertCutoverOwnerState(t, f, ownerIdentity, speakingVoice)
	req("GET", memoryRoute("two"), nil, false, 404)
	req("GET", memoryRoute("unknown"), nil, false, 404)
	if report := req("GET", memoryRoute("three"), nil, false, 200); report["status"] != "missing" {
		t.Fatal("secondary account fell back to primary memory", report)
	}
	legacyMemoryDir := filepath.Join(snapshot, "admin", "companion")
	write(filepath.Join(legacyMemoryDir, "memory.json"), []byte("{}"))
	if report := req("GET", memoryRoute("one"), nil, false, 200); report["status"] != "unsupported" || report["backend"] != "legacy_json" {
		t.Fatal("legacy JSON treated as a completed audit", report)
	}
	memory, err := companion.NewStore(legacyMemoryDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := companion.Defaults()
	cfg.Permissions.Remember = true
	now := time.Now().UTC()
	for _, in := range []companion.Input{
		{Channel: "wechat", Account: "one", Thread: "123@chatroom", TraceID: "chat", Sender: "synthetic-sender", Text: "synthetic-private-body", Source: &companion.SourceRef{Integrity: "observed", MessageType: 1, ServerID: "synthetic-native-original"}},
		{Channel: "wechat", Account: "one", Thread: "filehelper", TraceID: "helper", Sender: "synthetic-sender", Text: "synthetic-private-body", Source: &companion.SourceRef{Integrity: "observed", MessageType: 1, ServerID: "synthetic-native-original"}},
		{Channel: "wechat", Account: "other", Thread: "foreign-private-thread", TraceID: "foreign", Sender: "synthetic-sender", Text: "synthetic-private-body", Source: &companion.SourceRef{Integrity: "observed", MessageType: 1, ServerID: "synthetic-native-original"}},
	} {
		in.MessageTime = now.Format(time.RFC3339Nano)
		if err := memory.Observe(in, cfg, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := memory.Clear(companion.ScopeID("wechat", "one", "filehelper")); err != nil {
		t.Fatal(err)
	}
	const oldGroupNote = "合成群偏好：收到分享时默认总结。"
	if err := memory.SetPreferences(companion.ScopeID("wechat", "one", "123@chatroom"), oldGroupNote, 0); err != nil {
		t.Fatal(err)
	}
	if err := memory.Close(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(legacyMemoryDir, "memory.sqlite")
	dbBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// A mistakenly staged primary database cannot expose its scope IDs under a secondary account.
	secondaryMemory := filepath.Join(snapshot, "admin", "accounts", wechatapp.AccountDirectory("three"), "companion", "memory.sqlite")
	write(secondaryMemory, dbBefore)
	if report := req("GET", memoryRoute("three"), nil, false, 200); report["status"] != "ready" ||
		report["matched_scopes"] != float64(0) || report["foreign_scopes"] != float64(3) || len(report["conversations"].([]any)) != 0 {
		t.Fatal("wrong-account snapshot disclosed conversations", report)
	}
	write(dbPath+"-wal", []byte("synthetic WAL not a standalone snapshot"))
	if report := req("GET", memoryRoute("one"), nil, false, 200); report["status"] != "snapshot_incomplete" {
		t.Fatal("WAL-bearing snapshot treated as standalone", report)
	}
	if err := os.Remove(dbPath + "-wal"); err != nil {
		t.Fatal(err)
	}
	req("GET", memoryRoute("one")+"?limit=201", nil, false, 400)
	req("GET", memoryRoute("one")+"?after=bad", nil, false, 400)
	audit := req("GET", memoryRoute("one")+"?limit=1", nil, false, 200)
	if audit["status"] != "ready" || audit["backend"] != "sqlite" || audit["matched_scopes"] != float64(2) || audit["foreign_scopes"] != float64(1) {
		t.Fatal(audit)
	}
	first := audit["conversations"].([]any)[0].(map[string]any)
	if first["thread_id"] != "123@chatroom" || first["event_count"] != float64(1) || first["history_granted"] != false || first["has_preference"] != true {
		t.Fatal(first)
	}
	for _, private := range []string{"synthetic-private-body", "synthetic-sender", "foreign-private-thread", "memory.sqlite", oldGroupNote} {
		if strings.Contains(string(j(audit)), private) {
			t.Fatal("memory preview exposed private data", private)
		}
	}
	cursor := url.QueryEscape(audit["next_after"].(string))
	second := req("GET", memoryRoute("one")+"?after="+cursor, nil, false, 200)["conversations"].([]any)[0].(map[string]any)
	if second["thread_id"] != "filehelper" || second["event_count"] != float64(0) || second["withheld_events"] != float64(1) {
		t.Fatal(second)
	}
	if data, err := os.ReadFile(dbPath); err != nil || !bytes.Equal(dbBefore, data) {
		t.Fatal("read-only preview changed the source database", err)
	}
	for _, sidecar := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dbPath + sidecar); !os.IsNotExist(err) {
			t.Fatal("read-only preview created a sidecar", sidecar, err)
		}
	}
	req("GET", planRoute("unknown"), nil, false, 404)
	req("GET", planRoute("two"), nil, false, 404)
	req("GET", planRoute("three"), nil, false, 422)
	detailFor := func(account string) map[string]any {
		return req("GET", route(account), nil, false, 200)["connection"].(map[string]any)
	}
	if detailFor("one")["legacy_import"] != "available" || detailFor("two")["legacy_import"] != nil {
		t.Fatal("legacy entry must be scoped to the staged account")
	}
	five := req("GET", planRoute("five"), nil, false, 200)
	req("PUT", route("five")+"/policy", map[string]any{"reply_threads": []string{"existing-target-thread"}}, true, 200)
	req("POST", planRoute("five"), planDigest(five), true, 409)
	req("GET", planRoute("five"), nil, false, 409)
	existing := detailFor("five")
	if existing["legacy_import"] != "conflict" || !bytes.Equal(j(existing["configured_policy"].(map[string]any)["reply_threads"]), []byte(`["existing-target-thread"]`)) {
		t.Fatal("import modified the existing target policy", existing)
	}
	write(sourceConfig, j(legacyImportSource{Root: host, Primary: "one", Stopped: true}))
	req("GET", planRoute("one"), nil, false, 422)
	if detailFor("one")["legacy_import"] != "invalid" {
		t.Fatal("invalid snapshot was advertised as available")
	}
	write(sourceConfig, j(source))
	req("POST", route("one")+"/lifecycle", map[string]string{"action": "start"}, true, 409)
	req("POST", route("one")+"/lifecycle", map[string]string{"action": "stop"}, true, 200)
	assertCutover := func(plan map[string]any, host, profile, login string) {
		t.Helper()
		want := legacyCutover{LegacyHost: host, NativeProfile: profile, TargetLogin: login, TargetIdentity: "owner_check_pending"}
		var got legacyCutover
		if err := json.Unmarshal(j(plan["cutover"]), &got); err != nil || got != want {
			t.Fatalf("cutover = %#v, want %#v (%v)", got, want, err)
		}
	}
	plan := req("GET", planRoute("one"), nil, false, 200)
	assertCutover(plan, "stop_record_matched", "import_receipt_matched", "signed_out")
	if strings.Contains(string(j(plan)), "old-host") || strings.Contains(string(j(plan)), marker.Epoch) {
		t.Fatal("cutover exposed private evidence")
	}
	starting := marker
	starting.State, starting.Epoch = "starting", strings.Repeat("b", 32)
	write(liveMarker, j(starting))
	req("POST", planRoute("one"), planDigest(plan), true, 409)
	assertCutover(req("GET", planRoute("one"), nil, false, 200), "changed", "import_receipt_matched", "signed_out")
	write(liveMarker, j(marker))
	write(receiptPath, j(map[string]string{"id": "four", "storageMode": "managed", "profilePath": profile}))
	req("POST", planRoute("one"), planDigest(plan), true, 409)
	assertCutover(req("GET", planRoute("one"), nil, false, 200), "stop_record_matched", "mismatch", "signed_out")
	write(receiptPath, receiptJSON)
	if plan["legacy_enabled"] != true || plan["same_native_dataset"] != false || plan["receipt_records"] != float64(3) || plan["unresolved_receipts"] != float64(2) ||
		!bytes.Equal(j(plan["suggested_reply_threads"]), []byte(`["123@chatroom","filehelper","friend-one"]`)) || strings.Contains(string(j(plan)), "synthetic-do-not-copy") {
		t.Fatal(plan)
	}
	if _, err := os.Stat(filepath.Join(app, "workers", wechatapp.ConnectionID("one"))); !os.IsNotExist(err) {
		t.Fatal("preview created worker data", err)
	}
	req("POST", planRoute("one"), planDigest(plan), false, 403)
	write(queuePath, []byte(rawQueue+"\n"))
	req("POST", planRoute("one"), planDigest(plan), true, 409)
	write(queuePath, []byte(rawQueue))
	applied := req("POST", planRoute("one"), planDigest(plan), true, 200)
	if applied["applied"] != true || req("POST", planRoute("one"), planDigest(plan), true, 200)["applied"] != true {
		t.Fatal(applied)
	}
	detail := req("GET", route("one"), nil, false, 200)["connection"].(map[string]any)
	if detail["legacy_import"] != "applied" || detail["worker"] != "paused" || detail["configured_policy"].(map[string]any)["conversation_enabled"] != false {
		t.Fatal(detail)
	}
	req("POST", route("one")+"/lifecycle", map[string]string{"action": "start"}, true, 409)
	accepted := req("GET", route("one")+"/receipts/legacy-accepted", nil, false, 200)
	if accepted["state"] != "accepted" || accepted["lookup"] != "legacy_snapshot" || accepted["server_id"] != "69123" {
		t.Fatal(accepted)
	}
	pending := req("GET", route("one")+"/receipts/legacy-pending", nil, false, 200)
	if pending["state"] != "submission_unknown" || pending["lookup"] != "legacy_snapshot" || receiptReads.Load() != 0 {
		t.Fatal(pending)
	}
	// The legacy archive is an independent, local snapshot. Listing it never
	// reaches the native client, and its cursor is scoped to both the imported
	// connection and the selected filter.
	req("GET", legacyReceiptRoute("one")+"?limit=0", nil, false, 400)
	req("GET", legacyReceiptRoute("one")+"?limit=201", nil, false, 400)
	req("GET", legacyReceiptRoute("one")+"?after=not-a-cursor", nil, false, 400)
	legacyPage := req("GET", legacyReceiptRoute("one")+"?limit=2", nil, false, 200)
	if legacyPage["coverage"] != "legacy_snapshot" || legacyPage["receipt_count"] != float64(3) || legacyPage["pending_count"] != float64(2) || legacyPage["snapshot"] == nil {
		t.Fatal("legacy receipt page omitted snapshot metadata", legacyPage)
	}
	legacyItems, ok := legacyPage["items"].([]any)
	if !ok || len(legacyItems) != 2 {
		t.Fatal("legacy receipt page did not paginate", legacyPage)
	}
	firstLegacy := legacyItems[0].(map[string]any)
	if firstLegacy["idempotency_key"] != "legacy-accepted" || firstLegacy["state"] != "accepted" || firstLegacy["lookup"] != "legacy_snapshot" {
		t.Fatal("terminal receipt was not preserved as an archive snapshot", firstLegacy)
	}
	if unknown := legacyItems[1].(map[string]any); unknown["idempotency_key"] != "legacy-operation-unknown" || unknown["state"] != "submission_unknown" || unknown["lookup"] != "legacy_snapshot" {
		t.Fatal("old retry key was not retained as a pending archive snapshot", unknown)
	}
	legacyCursor, ok := legacyPage["next_after"].(string)
	if !ok || legacyCursor == "" {
		t.Fatal("legacy receipt cursor missing", legacyPage)
	}
	req("GET", legacyReceiptRoute("one")+"?after="+url.QueryEscape(legacyCursor)+"&pending=1", nil, false, 409)
	legacySecond := req("GET", legacyReceiptRoute("one")+"?limit=2&after="+url.QueryEscape(legacyCursor), nil, false, 200)
	secondItems := legacySecond["items"].([]any)
	if len(secondItems) != 1 || secondItems[0].(map[string]any)["idempotency_key"] != "legacy-pending" || secondItems[0].(map[string]any)["state"] != "submission_unknown" || legacySecond["next_after"] != nil {
		t.Fatal("legacy receipt pagination did not retain unresolved snapshot", legacySecond)
	}
	pendingOnly := req("GET", legacyReceiptRoute("one")+"?pending=1", nil, false, 200)
	if pendingOnly["receipt_count"] != float64(3) || pendingOnly["pending_count"] != float64(2) || len(pendingOnly["items"].([]any)) != 2 || pendingOnly["items"].([]any)[0].(map[string]any)["idempotency_key"] != "legacy-operation-unknown" {
		t.Fatal("pending legacy receipt filter was not local and stable", pendingOnly)
	}
	if unresolved := req("GET", legacyReceiptRoute("one")+"/legacy-operation-unknown", nil, false, 200); unresolved["state"] != "submission_unknown" || unresolved["lookup"] != "legacy_operation_unknown" {
		t.Fatal("archive record without its original operation was not retained locally", unresolved)
	}
	if mismatched := req("GET", legacyReceiptRoute("one")+"/legacy-pending", nil, false, 200); mismatched["state"] != "submission_unknown" || mismatched["lookup"] != "legacy_dataset_mismatch" || receiptReads.Load() != 0 {
		t.Fatal("foreign native dataset changed an imported receipt", mismatched)
	}
	daemonOffline.Store(true)
	if offline := req("GET", legacyReceiptRoute("one")+"/legacy-pending", nil, false, 200); offline["state"] != "submission_unknown" || offline["lookup"] != "legacy_native_unavailable" || receiptReads.Load() != 0 {
		t.Fatal("native outage changed the imported receipt", offline)
	}
	daemonOffline.Store(false)
	if receiptReads.Load() != 0 || sideEffects.Load() != 0 {
		t.Fatal("legacy receipt list replayed an old delivery", receiptReads.Load(), sideEffects.Load())
	}
	four := req("GET", planRoute("four"), nil, false, 200)
	if four["same_native_dataset"] != true {
		t.Fatal(four)
	}
	req("POST", planRoute("four"), planDigest(four), true, 200)
	// A valid cursor from another imported connection is stale for this archive,
	// rather than a license to enumerate a different account's old receipts.
	req("GET", legacyReceiptRoute("four")+"?after="+url.QueryEscape(legacyCursor), nil, false, 409)
	if recovered := req("GET", route("four")+"/receipts/legacy-pending", nil, false, 200); recovered["lookup"] != "platform" || recovered["server_id"] != "69234" {
		t.Fatal(recovered)
	}
	// A current checkpoint can hold the same key with a different native job.
	// The dedicated archive path must still resolve only the old operation.
	currentFour := filepath.Join(app, "workers", wechatapp.ConnectionID("four"), "queue", "messages-v2.json")
	write(currentFour, []byte(`{"version":1,"server":"new-dataset","account":"four","deliveries":[{"key":"legacy-pending","id":"current-job","thread":"filehelper","state":"accepted","server_id":"70000","input":{"account":"four","thread":"filehelper"}}]}`))
	if current := req("GET", route("four")+"/receipts/legacy-pending", nil, false, 200); current["lookup"] != "stored" || current["operation_id"] != "current-job" || current["server_id"] != "70000" {
		t.Fatal("current receipt lookup did not use its own checkpoint", current)
	}
	if archivePending := req("GET", legacyReceiptRoute("four")+"/legacy-pending", nil, false, 200); archivePending["lookup"] != "platform" || archivePending["server_id"] != "69234" {
		t.Fatal("matching legacy operation did not use the bounded lookup", archivePending)
	}
	if archiveAccepted := req("GET", legacyReceiptRoute("four")+"/legacy-accepted", nil, false, 200); archiveAccepted["lookup"] != "legacy_snapshot" || archiveAccepted["state"] != "accepted" {
		t.Fatal("terminal legacy receipt consulted the native queue", archiveAccepted)
	}
	if receiptReads.Load() != 2 || sideEffects.Load() != 0 {
		t.Fatal("legacy receipt lookup ran an unexpected operation", receiptReads.Load(), sideEffects.Load())
	}
	unknownCode, unknown := f.request("GET", legacyReceiptRoute("four")+"/missing-legacy-operation", nil, false)
	if unknownCode != http.StatusNotFound || receiptReads.Load() != 2 {
		t.Fatal("unknown legacy key was not contained by the archive guard", unknownCode, unknown)
	}
	req("GET", route("three")+"/receipts/legacy-accepted", nil, false, 404)
	target := filepath.Join(app, "workers", wechatapp.ConnectionID("one"))
	files, err := os.ReadDir(target)
	if err != nil || len(files) != 2 {
		t.Fatal("migration installed unexpected worker files", files, err)
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(target, file.Name()))
		if err != nil || bytes.Contains(data, []byte("do-not-import")) || bytes.Contains(data, []byte("do-not-copy")) {
			t.Fatal("migration copied raw source content", file.Name(), err)
		}
	}
	for path, expected := range map[string]string{configPath: rawConfig, queuePath: rawQueue} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != expected {
			t.Fatal("migration changed the snapshot", err)
		}
	}
	selectedRoute := memoryRoute("one") + "/123@chatroom"
	noteRoute := selectedRoute + "/preference"
	req("GET", memoryRoute("one")+"/filehelper/preference", nil, false, 400)
	groupReplyRoute := ruleRoute("one", "124@chatroom")
	req("GET", ruleRoute("one", "filehelper"), nil, false, 400)
	if row := req("GET", groupReplyRoute, nil, false, 200); row["status"] != "not_ready" || row["reply_settings"] != nil {
		t.Fatal("ungranted group disclosed its reply rule", row)
	}
	if row := req("GET", noteRoute, nil, false, 200); row["status"] != "not_ready" || row["note"] != nil {
		t.Fatal("ungranted group disclosed old preference", row)
	}
	otherRoute := memoryRoute("one") + "/filehelper"
	if row := req("GET", selectedRoute, nil, false, 200); row["status"] != "not_ready" {
		t.Fatal("selected history was not explicitly authorized", row)
	}
	setHistory := func(threads ...string) {
		req("PUT", route("one")+"/policy", map[string]any{
			"reply_threads":   []string{"123@chatroom", "filehelper"},
			"history_threads": threads,
		}, true, 200)
	}
	setHistory("123@chatroom", "filehelper")
	req("GET", memoryRoute("one")+"?after="+cursor, nil, false, 409)
	audit = req("GET", memoryRoute("one")+"?limit=1", nil, false, 200)
	if row := audit["conversations"].([]any)[0].(map[string]any); row["history_granted"] != true {
		t.Fatal("preview did not show configured target authorization", row)
	}
	if row := req("GET", selectedRoute, nil, false, 200); row["status"] != "not_ready" {
		t.Fatal("signed-out target passed cutover", row)
	}
	if row := req("GET", noteRoute, nil, false, 200); row["status"] != "not_ready" || row["note"] != nil {
		t.Fatal("signed-out target disclosed old preference", row)
	}
	loginOnline.Store(true)
	selected := req("GET", selectedRoute, nil, false, 200)
	if selected["status"] != "ready" || selected["event_count"] != float64(1) || bytes.Contains(j(selected), []byte("synthetic-private-body")) {
		t.Fatal("selected scope preview was wrong or leaked content", selected)
	}
	if row := req("GET", otherRoute, nil, false, 200); row["status"] != "empty" {
		t.Fatal("cleared old scope was treated as importable", row)
	}
	bundlePath := filepath.Join(target, stationLegacyMemoryFile("one", "123@chatroom"))
	if _, err := os.Stat(bundlePath); !os.IsNotExist(err) {
		t.Fatal("read-only scope preview created a bundle", err)
	}
	postMemory := func(plan map[string]any, sameIdentity bool) map[string]any {
		return map[string]any{"digest": plan["digest"], "same_identity": sameIdentity}
	}
	req("POST", selectedRoute, postMemory(selected, true), false, 403)
	req("POST", selectedRoute, postMemory(selected, false), true, 400)
	oldNotePreview := req("GET", noteRoute, nil, false, 200)
	if oldNotePreview["status"] != "ready" || oldNotePreview["note"] != oldGroupNote {
		t.Fatal("old note should be readable before importing the history bundle", oldNotePreview)
	}
	setHistory("filehelper")
	req("POST", selectedRoute, postMemory(selected, true), true, 409)
	req("POST", noteRoute, postMemory(oldNotePreview, true), true, 409)
	setHistory("123@chatroom", "filehelper")
	req("POST", selectedRoute, postMemory(selected, true), true, 409)
	req("POST", noteRoute, postMemory(oldNotePreview, true), true, 409)
	selected = req("GET", selectedRoute, nil, false, 200)

	targetMemory, err := companion.NewStore(filepath.Join(target, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if err := targetMemory.Observe(companion.Input{Channel: "wechat", Account: "one", Thread: "filehelper", Sender: "synthetic-target", TraceID: "target-local-message", Text: "existing target history"}, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err := targetMemory.Close(); err != nil {
		t.Fatal(err)
	}
	if row := req("GET", otherRoute, nil, false, 200); row["status"] != "conflict" {
		t.Fatal("existing target scope was not protected", row)
	}
	write(liveMarker, j(starting))
	req("POST", selectedRoute, postMemory(selected, true), true, 409)
	write(liveMarker, j(marker))
	oldMemory, err := companion.NewStore(legacyMemoryDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldMemory.Observe(companion.Input{Channel: "wechat", Account: "one", Thread: "123@chatroom", Sender: "synthetic-sender",
		TraceID: "added-after-preview", Text: "synthetic newer original", MessageTime: now.Format(time.RFC3339Nano),
		Source: &companion.SourceRef{Integrity: "observed", MessageType: 1, ServerID: "synthetic-second-original"}}, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err := oldMemory.Close(); err != nil {
		t.Fatal(err)
	}
	req("POST", selectedRoute, postMemory(selected, true), true, 409)
	selected = req("GET", selectedRoute, nil, false, 200)
	if selected["status"] != "ready" || selected["event_count"] != float64(2) {
		t.Fatal("updated source was not reprojected", selected)
	}
	confirmed := req("POST", selectedRoute, postMemory(selected, true), true, 200)
	if confirmed["status"] != "applied" || confirmed["event_count"] != float64(2) || bytes.Contains(j(confirmed), []byte("synthetic-private-body")) {
		t.Fatal("unexpected apply result", confirmed)
	}
	rawBundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	req("POST", selectedRoute, postMemory(selected, true), true, 200)
	if data, err := os.ReadFile(bundlePath); err != nil || !bytes.Equal(rawBundle, data) {
		t.Fatal("idempotent apply changed the saved bundle", err)
	}
	if _, err := os.Stat(filepath.Join(target, stationLegacyMemoryFile("one", "filehelper"))); !os.IsNotExist(err) {
		t.Fatal("apply wrote a second conversation", err)
	}
	var bundle stationLegacyMemory
	if err := json.Unmarshal(rawBundle, &bundle); err != nil || bundle.Account != "one" || bundle.Thread != "123@chatroom" || len(bundle.Selection.Events) != 2 {
		t.Fatal("saved bundle scope mismatch", err)
	}
	if _, err := readStationLegacyMemory(target, "one", "123@chatroom", bundle.Grant); err != nil {
		t.Fatal("current grant could not open the selected bundle", err)
	}
	setNote := func(note string, summarize bool) {
		t.Helper()
		req("PUT", route("one")+"/policy", map[string]any{
			"reply_threads": []string{"123@chatroom", "filehelper"}, "history_threads": []string{"123@chatroom", "filehelper"},
			"attachment_threads": []string{"123@chatroom"},
			"group_rules":        []apphost.ConnectionGroupRule{{ThreadID: "123@chatroom", Note: note, SummarizeShares: summarize}},
		}, true, 200)
	}
	checkNote := func(note string, summarize bool) {
		t.Helper()
		detail := detailFor("one")
		rows, _ := detail["configured_policy"].(map[string]any)["group_rules"].([]any)
		if note == "" && !summarize {
			if len(rows) != 0 {
				t.Fatal("cleared target note reappeared", rows)
			}
			return
		}
		if len(rows) != 1 {
			t.Fatal("unexpected group rules", rows)
		}
		rule := rows[0].(map[string]any)
		if rule["note"] != note || rule["summarize_shares"] != summarize || detail["worker"] != "paused" {
			t.Fatal("target note or independent share grant changed", rule)
		}
	}
	notePreview := req("GET", noteRoute, nil, false, 200)
	if notePreview["status"] != "ready" || notePreview["note"] != oldGroupNote || notePreview["can_enable_channels_summary"] != nil {
		t.Fatal("old scoped note unavailable after history import", notePreview)
	}
	postNoteWithSummary := func(plan map[string]any) map[string]any {
		body := postMemory(plan, true)
		body["enable_channels_summary"] = true
		return body
	}
	req("POST", noteRoute, postNoteWithSummary(notePreview), true, 409)
	req("POST", noteRoute, postMemory(notePreview, true), false, 403)
	req("POST", noteRoute, postMemory(notePreview, false), true, 400)
	setNote("合成新站备注", true)
	req("POST", noteRoute, postMemory(notePreview, true), true, 409)
	checkNote("合成新站备注", true)
	setNote("", false)
	notePreview = req("GET", noteRoute, nil, false, 200)
	if notePreview["can_enable_channels_summary"] != true {
		t.Fatal("existing attachment grant did not allow an explicit opt-in", notePreview)
	}
	req("PUT", route("one")+"/policy", map[string]any{
		"reply_threads": []string{"123@chatroom", "filehelper"}, "attachment_threads": []string{},
	}, true, 200)
	req("POST", noteRoute, postNoteWithSummary(notePreview), true, 409)
	setNote("", false)
	req("POST", noteRoute, postNoteWithSummary(notePreview), true, 409)
	notePreview = req("GET", noteRoute, nil, false, 200)
	write(liveMarker, j(starting))
	req("POST", noteRoute, postMemory(notePreview, true), true, 409)
	write(liveMarker, j(marker))
	notePreview = req("GET", noteRoute, nil, false, 200)
	beforeSummary := time.Now().UTC().Add(-time.Second)
	req("POST", noteRoute, postNoteWithSummary(notePreview), true, 200)
	req("POST", noteRoute, postMemory(notePreview, true), true, 409)
	checkNote(oldGroupNote, true)
	summaryWorker, err := h.chat.wechat.worker(wechatapp.ConnectionID("one"), false)
	if err != nil || summaryWorker == nil {
		t.Fatal("could not read imported group policy", err)
	}
	summaryWorker.mu.Lock()
	allowedBefore := summaryWorker.policy.groupShareAllowed("123@chatroom", beforeSummary.Format(time.RFC3339Nano))
	allowedAfter := summaryWorker.policy.groupShareAllowed("123@chatroom", time.Now().UTC().Format(time.RFC3339Nano))
	summaryWorker.mu.Unlock()
	if allowedBefore || !allowedAfter {
		t.Fatal("the imported summary preference must only apply from confirmation", allowedBefore, allowedAfter)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, f = open()
	f.login()
	assertCutoverOwnerState(t, f, ownerIdentity, speakingVoice)
	checkNote(oldGroupNote, true)
	setNote("", false)
	req("POST", noteRoute, postNoteWithSummary(notePreview), true, 200)
	checkNote("", false)
	// This group has a config rule and no SQLite scope. A reply grant, but
	// no attachment or history grant, is required to inspect its old settings.
	setRule := func(includeGroup bool, settings any) {
		t.Helper()
		threads := []string{"123@chatroom", "125@chatroom", "filehelper"}
		body := map[string]any{"reply_threads": threads}
		if includeGroup {
			body["reply_threads"] = append(threads, "124@chatroom")
			row := map[string]any{"thread_id": "124@chatroom", "note": "independent note"}
			if settings != nil {
				row["reply_settings"] = settings
			}
			body["group_rules"] = []any{row}
		}
		req("PUT", route("one")+"/policy", body, true, 200)
	}
	setRule(true, nil)
	socialPreview := req("GET", ruleRoute("one", "125@chatroom"), nil, false, 200)
	if socialPreview["status"] != "ready" || socialPreview["reply_settings"].(map[string]any)["mode"] != "social" {
		t.Fatal("social rule was not offered for explicit confirmation", socialPreview)
	}
	req("POST", ruleRoute("one", "125@chatroom"), postMemory(socialPreview, false), true, 400)
	req("POST", ruleRoute("one", "125@chatroom"), postMemory(socialPreview, true), true, 200)
	socialWorker, err := h.chat.wechat.worker(wechatapp.ConnectionID("one"), false)
	if err != nil || socialWorker == nil {
		t.Fatal("could not read imported social policy", err)
	}
	socialWorker.mu.Lock()
	socialMode := socialWorker.policy.GroupRules["125@chatroom"].Mode
	socialWorker.mu.Unlock()
	if socialMode != "social" {
		t.Fatal("confirmed social rule was lost", socialMode)
	}
	if row := req("GET", ruleRoute("four", "124@chatroom"), nil, false, 200); row["status"] != "not_ready" || row["reply_settings"] != nil {
		t.Fatal("another connection reused a group grant", row)
	}
	rulePreview := req("GET", groupReplyRoute, nil, false, 200)
	expectedRule := map[string]any{"mode": "all", "prefix": "/group ", "persona": "synthetic scoped role", "force_reply_when_mentioned": true}
	if rulePreview["status"] != "ready" || !bytes.Equal(j(rulePreview["reply_settings"]), j(expectedRule)) || bytes.Contains(j(rulePreview), []byte("do-not-copy")) {
		t.Fatal("config-only rule preview was wrong", rulePreview)
	}
	req("POST", groupReplyRoute, postMemory(rulePreview, true), false, 403)
	req("POST", groupReplyRoute, postMemory(rulePreview, false), true, 400)
	write(configPath, []byte(strings.Replace(rawConfig, "synthetic scoped role", "changed synthetic role", 1)))
	req("POST", groupReplyRoute, postMemory(rulePreview, true), true, 409)
	write(configPath, []byte(rawConfig))
	setRule(false, nil)
	req("POST", groupReplyRoute, postMemory(rulePreview, true), true, 409)
	setRule(true, nil)
	req("POST", groupReplyRoute, postMemory(rulePreview, true), true, 409)
	rulePreview = req("GET", groupReplyRoute, nil, false, 200)
	req("POST", groupReplyRoute, postMemory(rulePreview, true), true, 200)
	checkRule := func(expect any) {
		t.Helper()
		detail := detailFor("one")
		rows := detail["configured_policy"].(map[string]any)["group_rules"].([]any)
		if len(rows) != 1 || detail["worker"] != "paused" {
			t.Fatal("unexpected policy or worker state after rule import", detail)
		}
		row := rows[0].(map[string]any)
		if row["thread_id"] != "124@chatroom" || row["note"] != "independent note" || row["summarize_shares"] != false || !bytes.Equal(j(row["reply_settings"]), j(expect)) {
			t.Fatal("rule import overwrote independent settings", row)
		}
	}
	checkRule(expectedRule)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, f = open()
	f.login()
	checkRule(expectedRule)
	setRule(true, nil)
	checkRule(expectedRule)
	setRule(true, map[string]string{"mode": "mention"})
	req("POST", groupReplyRoute, postMemory(rulePreview, true), true, 200)
	checkRule(nil)
	if row := req("GET", selectedRoute, nil, false, 200); row["status"] != "applied" {
		t.Fatal("applied scope was not restored after restart", row)
	}
	activeNative, err := h.chat.wechat.WorkerSender(t.Context(), wechatapp.ConnectionID("one"))
	if err != nil {
		t.Fatal(err)
	}
	liveMemory, err := companion.NewStore(filepath.Join(target, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer liveMemory.Close()
	tools := &stationAttachmentTools{dir: target, native: activeNative, store: liveMemory, blocked: func() bool { return false },
		reads: map[string]time.Time{"123@chatroom": bundle.Grant}}
	trace := companion.Input{Channel: "wechat", Account: "one", Thread: "123@chatroom", Sender: "synthetic-sender",
		TraceID: "after-import", Text: "synthetic new question", MessageTime: bundle.Grant.Add(time.Second).Format(time.RFC3339Nano)}
	tools.SetTrace(trace)
	stamp := tools.MemoryResultAuthorization()
	if stamp == "" || !tools.MemoryResultAuthorized(stamp) || !tools.EphemeralToolResult("wechat_read_legacy") {
		t.Fatal("imported evidence was not bound to the live history grant")
	}
	list, err := tools.List(t.Context())
	if err != nil || !slices.ContainsFunc(list, func(tool mcptools.Tool) bool { return tool.Name == "wechat_read_legacy" }) {
		t.Fatal("ordinary tool discovery did not expose the current authorized scope", err)
	}
	call := json.RawMessage(`{"kind":"events","max_bytes":1000}`)
	if raw, err := tools.Call(t.Context(), "wechat_read_legacy", call); err != nil ||
		!strings.Contains(raw, "synthetic-private-body") || strings.Contains(raw, "foreign-private-thread") {
		t.Fatal("ordinary tool access to imported evidence was incorrect", err)
	}
	if err := liveMemory.Observe(trace, cfg, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := liveMemory.Clear(companion.ScopeID("wechat", "one", trace.Thread)); err != nil {
		t.Fatal(err)
	}
	if raw, err := tools.Call(t.Context(), "wechat_read_legacy", call); err == nil || raw != "" {
		t.Fatal("scope clear did not block never-observed imported sources")
	}
	if tools.MemoryResultAuthorized(stamp) {
		t.Fatal("a queued reply can still use the cleared evidence authority")
	}
	setHistory("filehelper")
	req("POST", selectedRoute, postMemory(selected, true), true, 409)
	setHistory("123@chatroom", "filehelper")
	if row := req("GET", selectedRoute, nil, false, 200); row["status"] != "conflict" {
		t.Fatal("regrant reactivated the bundle from the old grant", row)
	}
	start := map[string]string{"action": "start"}
	req("POST", route("one")+"/lifecycle", start, true, 409)
	if eventReads.Load() != 0 {
		t.Fatal("an unconfirmed worker read native events")
	}
	if row := req("GET", activationRoute, nil, false, 200); row["status"] != "blocked" || row["legacy_host"] != "stop_record_matched" {
		t.Fatal("a v1 stopped marker must remain inspectable without permitting activation", row)
	}
	marker.Version = 2
	write(liveMarker, j(marker))
	write(filepath.Join(snapshot, legacyStopMarkerFile), j(marker))
	guardedSender, err = h.chat.wechat.WorkerSender(t.Context(), wechatapp.ConnectionID("one"))
	if err != nil {
		t.Fatal(err)
	}
	activation := req("GET", activationRoute, nil, false, 200)
	if activation["status"] != "ready" || activation["pending_receipts"] != float64(2) || activation["model_ready"] != true ||
		bytes.Contains(j(activation), []byte("old-host")) || bytes.Contains(j(activation), []byte(marker.Epoch)) {
		t.Fatal("activation preview was incomplete or exposed private evidence", activation)
	}
	confirm := map[string]any{"digest": activation["digest"], "same_identity": true, "reviewed_receipts": true}
	req("POST", activationRoute, confirm, false, 403)
	req("POST", activationRoute, map[string]any{"digest": activation["digest"]}, true, 400)
	req("POST", activationRoute, map[string]any{"digest": activation["digest"], "same_identity": true}, true, 409)
	write(queuePath, []byte(rawQueue+"\n"))
	req("POST", activationRoute, confirm, true, 409)
	write(queuePath, []byte(rawQueue))
	write(liveMarker, j(starting))
	assertCutover(req("GET", planRoute("one"), nil, false, 200), "changed", "import_receipt_matched", "online")
	req("POST", activationRoute, confirm, true, 409)
	req("POST", route("one")+"/lifecycle", start, true, 409)
	write(liveMarker, j(marker))
	claimPath := filepath.Join(legacyHost, legacyCutoverClaimFile)
	if _, err := os.Lstat(claimPath); !os.IsNotExist(err) {
		t.Fatal("preview or refused confirmation wrote a claim", err)
	}
	unlock, err := hostfile.TryLock(filepath.Join(legacyHost, legacyCutoverLockFile))
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer unlock()
		req("POST", activationRoute, confirm, true, 409)
	}()
	write(claimPath, j("different-synthetic-target"))
	req("POST", activationRoute, confirm, true, 409)
	if raw, err := os.ReadFile(claimPath); err != nil || !bytes.Equal(raw, j("different-synthetic-target")) {
		t.Fatal("conflicting claim was overwritten", err)
	}
	if err := os.Remove(claimPath); err != nil {
		t.Fatal(err)
	}
	if activated := req("POST", activationRoute, confirm, true, 200); activated["status"] != "confirmed" || activated["target_identity"] != "owner_confirmed" {
		t.Fatal("owner confirmation did not persist the exact cutover", activated)
	}
	if eventReads.Load() != 0 || detailFor("one")["worker"] != "paused" {
		t.Fatal("confirming the cutover started the worker")
	}
	write(receiptPath, append(bytes.Clone(receiptJSON), '\n'))
	req("POST", route("one")+"/lifecycle", start, true, 409)
	write(receiptPath, receiptJSON)
	assertCutover(req("POST", planRoute("one"), planDigest(plan), true, 200), "stop_record_matched", "import_receipt_matched", "signed_out")
	if err := os.Remove(sourceConfig); err != nil {
		t.Fatal(err)
	}
	assertCutover(req("GET", planRoute("one"), nil, false, 200), "pending", "import_receipt_matched", "online")
	if row := req("GET", activationRoute, nil, false, 200); row["status"] != "confirmed" {
		t.Fatal("detaching the copied snapshot discarded saved activation", row)
	}
	if detailFor("one")["legacy_import"] != "applied" || req("GET", planRoute("one"), nil, false, 200)["applied"] != true ||
		req("POST", planRoute("one"), planDigest(plan), true, 200)["applied"] != true {
		t.Fatal("saved migration report was not available after detaching the source")
	}
	if req("GET", route("one")+"/receipts/legacy-accepted", nil, false, 200)["state"] != "accepted" ||
		req("GET", legacyReceiptRoute("one")+"?limit=200", nil, false, 200)["receipt_count"] != float64(3) ||
		req("GET", route("one"), nil, false, 200)["connection"].(map[string]any)["worker"] != "paused" ||
		sideEffects.Load() != 0 || receiptReads.Load() != 2 {
		t.Fatal("migration was not durable or triggered native side effects")
	}
	req("POST", route("one")+"/lifecycle", start, true, 200)
	waitAppLifecycle(t, func() bool { return detailFor("one")["worker"] == "running" })
	// The sender issued before activation must see the committed proof at dispatch.
	if _, err := guardedSender.ExecuteAction(t.Context(), guardedAction, "confirmed"); err != nil || guardedWrites.Load() != 1 {
		t.Fatal("confirmed action did not reach native exactly once", err)
	}
	claimBytes, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(claimPath); err != nil {
		t.Fatal(err)
	}
	if _, err := guardedSender.ExecuteAction(t.Context(), guardedAction, "claim-lost"); !errors.Is(err, linuxnative.ErrWriteNotAdmitted) || guardedWrites.Load() != 1 {
		t.Fatal("previously issued sender still wrote after the claim was removed", err)
	}
	pushAfterInvalid.Store(true)
	waitAppLifecycle(t, func() bool { return detailFor("one")["worker"] == "cutover_blocked" })
	if sideEffects.Load() != 0 {
		t.Fatal("invalidated worker reached native submission")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	readsBefore := eventReads.Load()
	h, f = open()
	f.login()
	waitAppLifecycle(t, func() bool { return detailFor("one")["worker"] == "cutover_blocked" })
	req("POST", route("one")+"/lifecycle", start, true, 409)
	if eventReads.Load() != readsBefore || sideEffects.Load() != 0 {
		t.Fatal("restart resumed the invalidated legacy worker")
	}
	req("POST", route("one")+"/lifecycle", map[string]string{"action": "stop"}, true, 200)
	write(claimPath, claimBytes)
	rollbackRoute := planRoute("one") + "/rollback"
	auditRoute := rollbackRoute + "/audit"
	req("GET", auditRoute, nil, false, 409)
	req("POST", rollbackRoute, map[string]any{}, false, 403)
	if view := req("GET", rollbackRoute, nil, false, 200); view["eligible"] != true || view["state"] != "not_frozen" {
		t.Fatal("read-only rollback status unexpectedly froze the connection", view)
	}
	guardedSender, err = h.chat.wechat.WorkerSender(t.Context(), wechatapp.ConnectionID("one"))
	if err != nil {
		t.Fatal(err)
	}
	writeResult := make(chan error, 1)
	writeCtx, cancelWrite := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelWrite()
	go func() {
		_, err := guardedSender.ExecuteAction(writeCtx, guardedAction, "rollback-inflight")
		writeResult <- err
	}()
	select {
	case <-writeStarted:
	case <-writeCtx.Done():
		t.Fatal("synthetic action never reached the native response body", writeCtx.Err())
	}
	if view := req("POST", rollbackRoute, map[string]any{}, true, 202); view["state"] != "draining" {
		t.Fatal("freeze did not wait for the existing native HTTP response body", view)
	}
	if _, err := guardedSender.ExecuteAction(t.Context(), guardedAction, "after-freeze"); !errors.Is(err, linuxnative.ErrWriteNotAdmitted) {
		t.Fatal("new native action passed the durable freeze", err)
	}
	close(allowWriteFinish)
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
	if view := req("GET", rollbackRoute, nil, false, 200); view["state"] != "frozen" {
		t.Fatal("drain did not finish after the in-flight response body closed", view)
	}
	if guardedWrites.Load() != 2 || sideEffects.Load() != 0 {
		t.Fatal("rollback changed the number of native writes", guardedWrites.Load(), sideEffects.Load())
	}
	newQueuePath := filepath.Join(target, "queue", "messages-v2.json")
	newQueue := `{"version":2,"server":"new-dataset","account":"one","deliveries":[{"key":"a-accepted","id":"job-sent","thread":"filehelper","state":"accepted","server_id":"91234","text":"synthetic-private-text","input":{"account":"one","thread":"filehelper"}},{"key":"b-active","id":"job-active","thread":"filehelper","state":"waiting","input":{"account":"one","thread":"filehelper"}},{"key":"c-unknown","thread":"filehelper","state":"submitting","input":{"account":"one","thread":"filehelper"}}]}`
	write(newQueuePath, []byte(newQueue))
	write(filepath.Join(host, stationNativeStopFile), j(stationNativeStop{Version: 1, State: "starting", Epoch: strings.Repeat("c", 32), DataRoot: host, At: time.Now().UTC()}))
	worker, err := h.chat.wechat.worker(wechatapp.ConnectionID("one"), false)
	if err != nil || worker == nil {
		t.Fatal("missing frozen worker", err)
	}
	worker.mu.Lock()
	ledger, err := worker.actionLedgerLocked()
	worker.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	ref := h.chat.wechat.connectionRef(wechatapp.ConnectionID("one"), "one")
	if err := ledger.Save(wechatapp.ActionProposal{ID: "synthetic-rollback-recall", Account: "one", State: "unknown",
		Request:   linuxnative.AdminAction{Kind: "recall", ThreadID: "filehelper", Text: "synthetic-private-action-payload"},
		AppAction: &apphost.ActionIntent{ConnectionRef: ref, Action: "recall", Target: "filehelper", IdempotencyKey: "synthetic-rollback-recall"}}); err != nil {
		t.Fatal(err)
	}
	req("GET", auditRoute, nil, false, 404)
	req("POST", auditRoute, map[string]any{}, false, 403)
	savedAudit := req("POST", auditRoute, map[string]any{}, true, 200)
	auditPath := filepath.Join(target, rollbackAuditFile)
	auditBytes, err := os.ReadFile(auditPath)
	if err != nil || savedAudit["digest"] != hexDigest(auditBytes) || savedAudit["local_current"] != true || savedAudit["desktop_bound"] != true ||
		savedAudit["delivery_count"] != float64(3) || savedAudit["action_count"] != float64(1) || savedAudit["active_count"] != float64(1) || savedAudit["unknown_count"] != float64(2) ||
		bytes.Contains(auditBytes, []byte("synthetic-private-text")) || bytes.Contains(auditBytes, []byte("synthetic-private-action-payload")) {
		t.Fatal("saved rollback audit did not match scoped, scrubbed evidence", savedAudit, err)
	}
	var sealed rollbackAudit
	if err := json.Unmarshal(auditBytes, &sealed); err != nil || sealed.DesktopEpoch != strings.Repeat("c", 32) || sealed.NativeGeneration != "synthetic-generation" ||
		sealed.Deliveries[0].ServerID != "91234" || sealed.Actions[0].OperationID != "synthetic-rollback-recall" {
		t.Fatal("saved audit did not retain original receipts and runtime binding", err)
	}
	rollbackFirst := req("GET", rollbackRoute+"?limit=2", nil, false, 200)
	items := rollbackFirst["items"].([]any)
	accepted, active := items[0].(map[string]any), items[1].(map[string]any)
	if rollbackFirst["native"] != "available" || accepted["disposition"] != "accepted" || accepted["server_id"] != "91234" ||
		active["disposition"] != "active" || bytes.Contains(j(rollbackFirst), []byte("synthetic-private-text")) {
		t.Fatal("rollback report lost or exposed delivery evidence", rollbackFirst)
	}
	rollbackNextPath := rollbackRoute + "?limit=2&after=" + url.QueryEscape(rollbackFirst["next_after"].(string))
	rollbackSecond := req("GET", rollbackNextPath, nil, false, 200)
	if row := rollbackSecond["items"].([]any)[0].(map[string]any); row["disposition"] != "unknown" || row["lookup"] != "not_found" || rollbackSecond["snapshot"] != rollbackFirst["snapshot"] {
		t.Fatal("missing native receipt was not retained as unknown", rollbackSecond)
	}
	holdAuditLookup.Store(true)
	auditStatus := make(chan int, 1)
	go func(fixture *localHomeFixture) {
		status, _ := fixture.request("POST", auditRoute, map[string]any{}, true)
		auditStatus <- status
	}(f)
	select {
	case <-auditLookupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("audit did not query the active native operation")
	}
	write(newQueuePath, []byte(newQueue+"\n"))
	close(allowAuditLookup)
	if status := <-auditStatus; status != 409 {
		t.Fatal("capture overwrote audit using changed source", status)
	}
	holdAuditLookup.Store(false)
	if stale := req("GET", auditRoute, nil, false, 200); stale["local_current"] != false || stale["digest"] != savedAudit["digest"] {
		t.Fatal("stale evidence was replaced or remained marked current", stale)
	}
	req("GET", rollbackNextPath, nil, false, 409)
	write(newQueuePath, []byte(strings.Replace(newQueue, "new-dataset", "other-dataset", 1)))
	if mismatch := req("GET", rollbackRoute+"?limit=2", nil, false, 200); mismatch["native"] != "dataset_mismatch" ||
		mismatch["items"].([]any)[1].(map[string]any)["disposition"] != "unknown" {
		t.Fatal("rollback queried a checkpoint against a different native dataset", mismatch)
	}
	write(newQueuePath, []byte(newQueue))
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, f = open()
	f.login()
	req("POST", route("one")+"/lifecycle", start, true, 409)
	daemonOffline.Store(true)
	offline := req("GET", rollbackRoute+"?limit=3", nil, false, 200)
	storedAudit := req("GET", auditRoute, nil, false, 200)
	offlineAudit := req("POST", auditRoute, map[string]any{}, true, 200)
	historicalAudit := req("GET", auditRoute+"?digest="+savedAudit["digest"].(string), nil, false, 200)
	repeated := req("POST", rollbackRoute, map[string]any{}, true, 200)
	daemonOffline.Store(false)
	if storedAudit["local_current"] != true || storedAudit["digest"] != savedAudit["digest"] || storedAudit["active_count"] != savedAudit["active_count"] {
		t.Fatal("audit did not survive restart without native lookup", storedAudit)
	}
	if offlineAudit["digest"] == savedAudit["digest"] || offlineAudit["desktop_bound"] != false || offlineAudit["active_count"] != float64(0) ||
		offlineAudit["unknown_count"] != float64(3) || historicalAudit["digest"] != savedAudit["digest"] || historicalAudit["active_count"] != float64(1) {
		t.Fatal("offline recapture did not preserve historical platform evidence", offlineAudit, historicalAudit)
	}
	if offline["state"] != "frozen" || offline["snapshot"] != rollbackFirst["snapshot"] || offline["native"] != "unavailable" ||
		repeated["frozen_at"] != rollbackFirst["frozen_at"] ||
		offline["items"].([]any)[1].(map[string]any)["disposition"] != "unknown" {
		t.Fatal("restart or native outage lost the frozen audit or misclassified pending work", offline)
	}
	if saved, err := os.ReadFile(claimPath); err != nil || !bytes.Equal(saved, claimBytes) {
		t.Fatal("rollback audit removed or changed the claim", err)
	}
	if runtime.GOOS != "windows" {
		return
	}
	settledDelivery.Store(true)
	settled := req("POST", auditRoute, map[string]any{}, true, 200)
	if settled["active_count"] != float64(0) || settled["unknown_count"] != float64(2) || settled["desktop_bound"] != true {
		t.Fatal("release fixture did not reach a settled state", settled)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	stopPath := filepath.Join(host, stationNativeStopFile)
	stop := stationNativeStop{Version: 1, State: "stopped", Epoch: strings.Repeat("c", 32), DataRoot: host,
		NativeServerID: "new-dataset", NativeOwnershipGeneration: "synthetic-generation", At: time.Now().UTC()}
	write(stopPath, j(stop))
	stationLease := filepath.Join(host, stationNativeRunLockFile)
	write(stationLease, []byte{0})
	intentPath := filepath.Join(host, stationNativeReleaseFile)
	releaseReq := LegacyWechatReleaseRequest{DataDir: host, ConnectionID: wechatapp.ConnectionID("one"), AuditDigest: settled["digest"].(string)}
	release := func() (LegacyWechatReleaseResult, error) { return ReleaseLegacyWechat(t.Context(), releaseReq) }
	rejected := func(message string) {
		t.Helper()
		if _, err := release(); err == nil || !strings.Contains(err.Error(), message) {
			t.Fatalf("expected release error containing %q, got %v", message, err)
		}
	}
	rejected("结果未知")
	releaseReq.AcceptUnknown = true
	releaseReq.AuditDigest = savedAudit["digest"].(string)
	rejected("还有在途")
	releaseReq.AuditDigest = settled["digest"].(string)
	for _, lease := range []string{stationLease, filepath.Join(legacyHost, legacyCutoverLockFile)} {
		unlock, err := hostfile.TryLock(lease)
		if err != nil {
			t.Fatal(err)
		}
		_, releaseErr := release()
		unlock()
		if releaseErr == nil || !strings.Contains(releaseErr.Error(), "仍在使用") {
			t.Fatal("release bypassed a held desktop lease", releaseErr)
		}
	}
	preview, err := release()
	if err != nil || preview.State != "ready" || preview.Unknown != 2 || preview.NativeFenced || !legacyMemoryDigestValid(preview.ApplyDigest) {
		t.Fatal("unexpected offline release preview", preview, err)
	}
	if _, err := os.Lstat(intentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview wrote a release intent", err)
	}
	releaseReq.ApplyDigest = preview.ApplyDigest
	changedStop := stop
	changedStop.Epoch = strings.Repeat("d", 32)
	write(stopPath, j(changedStop))
	rejected("停机记录不匹配")
	write(stopPath, j(stop))
	write(newQueuePath, []byte(newQueue+"\n"))
	rejected("本地投递或群操作记录发生变化")
	write(newQueuePath, []byte(newQueue))
	releaseReq.ApplyDigest = strings.Repeat("f", 64)
	rejected("令牌已变化")
	if _, err := os.Lstat(intentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected apply wrote a release intent", err)
	}
	releaseReq.ApplyDigest = preview.ApplyDigest
	if done, err := release(); err != nil || done.State != "released" || !done.NativeFenced {
		t.Fatal("first offline release failed", done, err)
	}
	// Re-create the interruption point between fencing and claim archival.
	archivedPath := filepath.Join(legacyHost, "legacy-cutover-released-"+preview.ApplyDigest+".json")
	if err := os.Rename(archivedPath, claimPath); err != nil {
		t.Fatal(err)
	}
	releaseReq.ApplyDigest = ""
	if pending, err := release(); err != nil || pending.State != "resume_required" || !pending.NativeFenced || pending.ApplyDigest != preview.ApplyDigest {
		t.Fatal("interrupted release lost its original token", pending, err)
	}
	releaseReq.ApplyDigest = preview.ApplyDigest
	for range 2 {
		if done, err := release(); err != nil || done.State != "released" || !done.NativeFenced || done.ApplyDigest != preview.ApplyDigest {
			t.Fatal("release or idempotent repeat failed", done, err)
		}
	}
	if _, err := os.Lstat(claimPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old startup claim is still active", err)
	}
	if archived, err := os.ReadFile(archivedPath); err != nil || !bytes.Equal(archived, claimBytes) {
		t.Fatal("release did not preserve original claim evidence", err)
	}
	if fenced, err := NewLocalHomeHandlerWithApps(host, work, "synthetic-local-password", "", registry); err == nil || !strings.Contains(err.Error(), "微信原生启动已禁用") {
		if fenced != nil {
			_ = fenced.Close()
		}
		t.Fatal("released root still admitted the native app", err)
	}
	if localOnly, err := NewLocalHomeHandlerWithApps(host, work, "synthetic-local-password", "", apphost.Registry{}); err != nil {
		t.Fatal("release fence blocked local chat", err)
	} else if err := localOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if guardedWrites.Load() != 2 || sideEffects.Load() != 0 {
		t.Fatal("offline release made a native write", guardedWrites.Load(), sideEffects.Load())
	}
}

func planDigest(plan map[string]any) map[string]any { return map[string]any{"digest": plan["digest"]} }
