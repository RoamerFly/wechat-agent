package station

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

func TestStationHistoryScopedPagingAndSourceChecks(t *testing.T) {
	const thread = "synthetic-room@chatroom"
	now := time.Now().UTC()
	messages := map[string]linuxnative.Message{}
	longID := "msg_" + strings.Repeat("abcdef", 50)
	for i, id := range []string{"old", "new", "share", "quote", "foreign", longID} {
		messages[id] = linuxnative.Message{ID: id, AccountID: "one", ThreadID: thread, SenderID: "member", ServerID: id + "-server", Type: 1, Direction: "incoming", Body: strings.Repeat("中文", 8), CreatedAt: now.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)}
	}
	large := messages[longID]
	large.Body = strings.Repeat("中文", 15000)
	messages[longID] = large
	m := messages["share"]
	m.Type, m.Link = 49, &linuxnative.LinkCard{Title: "synthetic video", Description: "publisher copy", URL: "https://example.com/watch?token=secret", SourceName: "Channels"}
	messages["share"] = m
	m = messages["quote"]
	m.Type, m.Quote, m.Body = 49, &linuxnative.QuoteReference{ServerID: "old-server", Type: 1}, `<msg><appmsg><title>quoted reply</title></appmsg></msg>`
	messages["quote"] = m
	m = messages["foreign"]
	m.ThreadID = "another-room@chatroom"
	messages["foreign"] = m
	store, err := companion.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var calls, writes atomic.Int32
	var blocked, forgetDuringRead atomic.Bool
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			writes.Add(1)
			w.WriteHeader(405)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-read-token" {
			w.WriteHeader(401)
			return
		}
		out := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch r.URL.Path {
		case "/v1/accounts/one/threads/" + thread + "/messages":
			if r.URL.Query().Get("limit") != "75" {
				t.Error("explicit page size lost", r.URL)
			}
			if r.URL.Query().Get("cursor") != "" && r.URL.Query().Get("cursor") != "native-next" {
				t.Error("native cursor changed")
			}
			out(map[string]any{"items": []linuxnative.Message{messages["new"], messages["old"]}, "nextCursor": "native-next", "coverage": "native-page"})
		case "/v1/accounts/one/groups/" + thread + "/members":
			out(map[string]any{"items": []map[string]any{{"id": "member", "roomDisplayName": "synthetic name", "secret": "hidden"}}})
		default:
			id := strings.TrimPrefix(r.URL.Path, "/v1/accounts/one/messages/")
			row, ok := messages[id]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if id == "new" && forgetDuringRead.Load() {
				if err := store.Clear(companion.ScopeID("wechat", "one", thread)); err != nil {
					t.Error(err)
				}
			}
			out(row)
		}
	}))
	defer daemon.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err = os.WriteFile(token, []byte("synthetic-read-token"), 0600); err != nil {
		t.Fatal(err)
	}
	native, err := linuxnative.New(linuxnative.Config{URL: daemon.URL, Account: "one", TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	tools := &stationAttachmentTools{native: native, store: store, blocked: blocked.Load}
	trace := companion.Input{Channel: "wechat", Account: "one", Thread: thread, Sender: "member", TraceID: "current", MessageTime: now.Add(time.Minute).Format(time.RFC3339Nano)}
	tools.SetTrace(trace)
	if available, _ := tools.List(t.Context()); len(available) != 0 {
		t.Fatal("history on by default")
	}
	tools.reads = map[string]time.Time{thread: now}
	if available, _ := tools.List(t.Context()); len(available) != 2 {
		t.Fatal("missing history tools", available)
	}
	call := func(name, raw string) map[string]any {
		t.Helper()
		result, err := tools.Call(t.Context(), name, json.RawMessage(raw))
		if err != nil {
			t.Fatal(name, err)
		}
		var out map[string]any
		if err = json.Unmarshal([]byte(result), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	deny := func(name, raw string) {
		t.Helper()
		result, err := tools.Call(t.Context(), name, json.RawMessage(raw))
		if err == nil || result != "" {
			t.Fatal("unexpected disclosure", name, result, err)
		}
	}
	page := call("wechat_read", `{"kind":"messages","limit":75}`)
	rows := page["items"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["message_id"] != "old" || rows[1].(map[string]any)["sender_id"] != "member" || page["coverage"] != "native-page" {
		t.Fatal(page)
	}
	next := page["next_cursor"].(string)
	if !strings.HasPrefix(next, "wc-") || next == "native-next" {
		t.Fatal(page)
	}
	deny("wechat_read", `{"kind":"messages","cursor":"forged","limit":75}`)
	deny("wechat_read", `{"kind":"members","cursor":"`+next+`"}`)
	deny("wechat_read", `{"kind":"messages","thread_id":"another-room@chatroom"}`)
	// Returning the same native cursor cannot produce an endless valid continuation.
	deny("wechat_read", `{"kind":"messages","cursor":"`+next+`","limit":75}`)
	full := call("wechat_message_read", `{"message_id":"old","length":7}`)
	if full["text"] != "中文" || full["next_offset"] != float64(6) || full["total_bytes"] != float64(48) {
		t.Fatal(full)
	}
	if long := call("wechat_message_read", `{"message_id":"`+longID+`"}`); long["message_id"] != longID {
		t.Fatal("native message ID was treated as a short account ID", long)
	}
	largePage := call("wechat_message_read", `{"message_id":"`+longID+`","length":100000}`)
	if largePage["requested_length"] != float64(100000) || largePage["effective_length"] != float64(65535) || largePage["next_offset"] != float64(65535) || largePage["total_bytes"] != float64(90000) {
		t.Fatal("transport boundary hid continuation", largePage)
	}
	continued := call("wechat_message_read", `{"message_id":"old","offset":6,"expected_sha256":"`+full["sha256"].(string)+`","length":100000}`)
	if continued["next_offset"] != nil || continued["requested_length"] != float64(100000) || continued["text"] != strings.Repeat("中文", 7) {
		t.Fatal(continued)
	}
	deny("wechat_message_read", `{"message_id":"old","offset":6}`)
	deny("wechat_message_read", `{"message_id":"old","expected_sha256":"changed"}`)
	deny("wechat_message_read", `{"message_id":"foreign"}`)
	card := call("wechat_message_read", `{"message_id":"share"}`)
	if card["read_scope"] != "publisher_card_only" || !strings.Contains(card["text"].(string), "publisher copy") || strings.Contains(card["text"].(string), "token=secret") {
		t.Fatal(card)
	}
	quote := call("wechat_message_read", `{"message_id":"quote"}`)
	if quote["quote_server_id"] != "old-server" || quote["text"] != "quoted reply" {
		t.Fatal(quote)
	}
	members := call("wechat_read", `{"kind":"members"}`)
	if members["items"].([]any)[0].(map[string]any)["secret"] != nil {
		t.Fatal(members)
	}
	// Native re-reading cannot resurrect a known locally forgotten event.
	in := trace
	in.TraceID, in.Text, in.MessageTime = "old", messages["old"].Body, messages["old"].CreatedAt
	cfg := companion.Defaults()
	cfg.Permissions.Remember = true
	if err = store.Observe(in, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err = store.Clear(companion.ScopeID("wechat", "one", thread)); err != nil {
		t.Fatal(err)
	}
	deny("wechat_read", `{"kind":"messages","cursor":"`+next+`","limit":75}`)
	deny("wechat_message_read", `{"message_id":"old"}`)
	page = call("wechat_read", `{"kind":"messages","limit":75}`)
	if page["filtered_count"] != float64(1) || page["returned_count"] != float64(1) {
		t.Fatal(page)
	}
	forgetDuringRead.Store(true)
	deny("wechat_read", `{"kind":"messages","limit":75}`)
	forgetDuringRead.Store(false)
	blocked.Store(true)
	before := calls.Load()
	deny("wechat_message_read", `{"message_id":"new"}`)
	if calls.Load() != before || writes.Load() != 0 {
		t.Fatal("revoked or write traffic", calls.Load(), before, writes.Load())
	}
	blocked.Store(false)
	tools.SetTrace(trace)
	deny("wechat_read", `{"kind":"messages","cursor":"`+next+`","limit":75}`)
}
