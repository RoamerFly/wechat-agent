package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

func wechatAppTestPath(account string) string {
	return "/api/apps/wechat/connections/" + wechatConnectionID(account)
}

func requireAppLogin(t *testing.T, f *webFixture, path string) {
	t.Helper()
	client := &http.Client{Transport: f.client.Transport}
	response, err := client.Get(f.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("application evidence exposed without owner session", response.StatusCode)
	}
}

func allowAppTestThread(t *testing.T, m *Manager) Config {
	t.Helper()
	cfg := m.Config().Config
	cfg.GroupRules = []GroupRule{{ID: "room@chatroom", Mode: "mention"}}
	cfg.Companion.Permissions.Remember = true
	if _, err := m.Save(ConfigInput{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestApplicationEventUsesExactStoredSourceAndForgetting(t *testing.T) {
	f, registry := setupAccounts(t)
	second := registry.lookup("second").manager
	const room = "room@chatroom"
	const original = "msg-original"
	const referenced = "msg-quoted"
	const literal = "synthetic literal body only used as evidence"
	observe := func(m *Manager, cfg Config, id, server, sender string, seq int64, quote *companion.SourceRef) companion.SourceRef {
		t.Helper()
		now := time.Date(2026, 1, 1, 10, int(seq), 0, 0, time.UTC)
		const storedTime = "2006-01-02T15:04:05.000000000Z"
		src := companion.SourceRef{Account: m.native.Account(), Thread: room, InstanceID: m.memory.SourceInstanceID(),
			MessageID: id, ServerID: server, Sender: sender, MessageType: 1,
			OccurredAt: now.Format(storedTime), ReceivedAt: now.Add(time.Second).Format(storedTime),
			Sequence: seq, Revision: 1, Integrity: "observed"}
		originalBody := literal
		in := companion.Input{Channel: "wechat", Account: src.Account, Thread: src.Thread, Sender: sender,
			TraceID: id, Text: literal, MessageTime: src.OccurredAt, Source: &src, OriginalText: &originalBody, QuotedSource: quote}
		if quote != nil {
			in.Addressees = []string{quote.Sender}
			in.IncomingMedia = []companion.IncomingMediaRef{{Source: src, Kind: "image", UploaderID: sender, AssetID: "synthetic-asset", ObservationState: "observed", Observation: "synthetic observation stays private"}}
		}
		if err := m.memory.Observe(in, cfg.Companion, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		return src
	}
	primaryCfg, cfg := allowAppTestThread(t, f.manager), allowAppTestThread(t, second)
	observe(f.manager, primaryCfg, original, "301", "first-user", 3, nil)
	anchor := observe(second, cfg, referenced, "702", "quoted-user", 8, nil)
	src := observe(second, cfg, original, "703", "second-user", 9, &anchor)
	scope := companion.ScopeID("wechat", "second", room)
	wantID, err := second.memory.OriginalEventID(t.Context(), scope, original)
	if err != nil {
		t.Fatal(err)
	}
	path := "/events/" + original + "?conversation=" + url.QueryEscape(room)
	requireAppLogin(t, f, wechatAppTestPath("second")+path)
	read := func(account string) apphost.Event {
		t.Helper()
		code, body, _ := f.request("GET", wechatAppTestPath(account)+path+"&account=test", nil, true, false)
		var ev apphost.Event
		if code != http.StatusOK || json.Unmarshal(body, &ev) != nil || strings.Contains(string(body), literal) ||
			strings.Contains(string(body), "synthetic observation stays private") || ev.AccountID != account ||
			ev.ConnectionID != wechatConnectionID(account) || ev.AppID != "wechat" || ev.InstallationID != "wechat-legacy" {
			t.Fatalf("invalid source response %d %s", code, body)
		}
		return ev
	}
	a, b := read("test"), read("second")
	if a.SenderID != "first-user" || a.ServerID != "301" || a.EventID == b.EventID || a.DatasetID == b.DatasetID ||
		b.SenderID != src.Sender || b.EventID != wantID || b.ServerID != src.ServerID || b.Revision != 1 || b.ConversationID != room ||
		b.OccurredAt != src.OccurredAt || b.ReceivedAt != src.ReceivedAt || b.SourceSequence != 9 || b.StoredSequence < 1 ||
		len(b.AddresseeIDs) != 1 || b.AddresseeIDs[0] != anchor.Sender || b.Quote == nil || b.Quote.MessageID != referenced ||
		b.Quote.ServerID != anchor.ServerID || len(b.Media) != 1 || b.Media[0].AssetID != "synthetic-asset" || b.ReadScope != "source_metadata" {
		t.Fatalf("original evidence identity changed: %+v / %+v", a, b)
	}
	if code, _, _ := f.request("GET", wechatAppTestPath("test")+"/events/"+referenced+"?conversation="+room, nil, true, false); code != http.StatusNotFound {
		t.Fatal("quote crossed accounts", code)
	}
	cfg.GroupRules = nil
	if _, err := second.Save(ConfigInput{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.request("GET", wechatAppTestPath("second")+path, nil, true, false); code != http.StatusNotFound {
		t.Fatal("revoked scope remained readable", code)
	}
	allowAppTestThread(t, second)
	if err := second.memory.Clear(scope); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.request("GET", wechatAppTestPath("second")+path, nil, true, false); code != http.StatusNotFound {
		t.Fatal("app route revived a forgotten source", code)
	}
	if read("test").EventID != a.EventID {
		t.Fatal("forgetting affected another connection")
	}
}

func TestApplicationReceiptQueriesOnlyOriginalKeyAndPreservesAcceptedEvidence(t *testing.T) {
	const key, room = "synthetic-app-operation", "room@chatroom"
	var submits, lookups, phase atomic.Int32
	f, registry := setupAccountsWithNativeRoutes(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/accounts/second/sends":
			submits.Add(1)
			if r.Header.Get("Idempotency-Key") != key {
				t.Error("wrong operation key on initial submission")
			}
			http.Error(w, "synthetic lost response", http.StatusBadGateway)
			return true
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/sends/by-idempotency-key"):
			lookups.Add(1)
			if r.URL.Path != "/v1/accounts/second/sends/by-idempotency-key" || r.URL.Query().Get("key") != key {
				t.Error("lookup crossed scope or changed the operation key")
			}
			switch phase.Load() {
			case 0:
				http.NotFound(w, r)
			case 1:
				respond(w, 200, map[string]any{"id": "wrong-thread-send", "accountId": "second", "threadId": "elsewhere@chatroom", "state": "accepted", "serverId": "700"})
			case 2:
				respond(w, 200, map[string]any{"id": "original-send", "accountId": "second", "threadId": room, "state": "accepted", "serverId": "701", "errorCode": "late_receipt", "reason": "synthetic private detail"})
			default:
				http.Error(w, "synthetic native outage", http.StatusServiceUnavailable)
			}
			return true
		}
		return false
	})
	m := registry.lookup("second").manager
	allowAppTestThread(t, m)
	input := linuxnative.ToolSend{Kind: "text", ThreadID: room, IdempotencyKey: key, Text: "synthetic send body"}
	if code, body, _ := f.request("POST", "/api/tools/send?account=second", input, true, true); code != http.StatusAccepted ||
		!strings.Contains(string(body), "submission_result_unknown_check_same_key") || submits.Load() != 1 {
		t.Fatalf("legacy entry did not retain unknown original submission: %d %s", code, body)
	}
	path := "/receipts/" + key
	for _, account := range []string{"test", "unregistered"} {
		if code, _, _ := f.request("GET", wechatAppTestPath(account)+path+"?account=second", nil, true, false); code != http.StatusNotFound {
			t.Fatal("foreign receipt lookup succeeded", account, code)
		}
	}
	requireAppLogin(t, f, wechatAppTestPath("second")+path)
	if lookups.Load() != 0 {
		t.Fatal("foreign or unauthenticated read contacted platform", lookups.Load())
	}
	read := func(wantLookup, wantState string) apphost.Receipt {
		t.Helper()
		code, body, _ := f.request("GET", wechatAppTestPath("second")+path, nil, true, false)
		var v apphost.Receipt
		if code != http.StatusOK || json.Unmarshal(body, &v) != nil || v.Lookup != wantLookup || v.State != wantState ||
			v.IdempotencyKey != key || v.AccountID != "second" || v.ConversationID != room || v.ConnectionID != wechatConnectionID("second") ||
			strings.Contains(string(body), "synthetic private detail") {
			t.Fatalf("unexpected receipt %d %s", code, body)
		}
		return v
	}
	if v := read("not_found", "submission_unknown"); v.ServerID != "" {
		t.Fatal(v)
	}
	phase.Store(1)
	if v := read("identity_mismatch", "submission_unknown"); v.ServerID != "" || v.OperationID != "" {
		t.Fatal(v)
	}
	phase.Store(2)
	if v := read("platform", "accepted"); v.ServerID != "701" || v.OperationID != "original-send" || v.PlatformCode != "late_receipt" {
		t.Fatal(v)
	}
	if code, _, _ := f.request("GET", "/api/tools/lookup?account=second&key="+key, nil, true, false); code != http.StatusOK {
		t.Fatal("authoritative legacy receipt persistence failed", code)
	}
	record, ok, err := m.tools.record(key, "second")
	if err != nil || !ok || !verifiedSend(record) {
		t.Fatalf("legacy receipt was not verified: %v", err)
	}
	if err := m.tools.archiveSend(record); err != nil {
		t.Fatal(err)
	}
	if err := m.tools.change(func(data *toolData) error {
		delete(data.Sends, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.tools, err = newToolStore(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	phase.Store(3)
	if v := read("stored", "accepted"); v.ServerID != "701" || submits.Load() != 1 || lookups.Load() != 4 {
		t.Fatal("saved acceptance changed or receipt lookup submitted again", v, submits.Load(), lookups.Load())
	}
}
