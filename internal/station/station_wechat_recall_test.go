package station

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
)

func TestStationWechatOwnerRecallFromDelivery(t *testing.T) {
	base := t.TempDir()
	host, home := filepath.Join(base, "host"), filepath.Join(base, "home")
	nativeDir := filepath.Join(host, "apps", "wechat", "native")
	if err := os.MkdirAll(nativeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "api-token"), []byte("synthetic-recall-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var submits, statusReads atomic.Int32
	var statusMode atomic.Int32
	nativeReceipt := func(id, state, outcome, message, server string) map[string]any {
		return map[string]any{"id": id, "accountId": "one", "kind": "message_recall", "state": state,
			"result": map[string]string{"messageId": message, "threadId": "room@chatroom", "serverId": server, "remoteOutcome": outcome, "status": "synthetic_recall"}}
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/status":
			write(map[string]any{"pid": 8009, "dataDir": nativeDir, "serverId": "synthetic-recall", "ownershipGeneration": "first"})
		case "GET /v1/accounts":
			write(map[string]any{"items": []map[string]any{
				{"id": "one", "authenticated": true, "connection": "online"},
				{"id": "two", "authenticated": true, "connection": "online"},
			}})
		case "GET /v1/accounts/one/threads/room@chatroom/messages":
			if r.URL.Query().Get("cursor") == "" {
				write(map[string]any{"items": []map[string]any{{"id": "msg_bystander", "serverId": "101", "threadId": "room@chatroom", "direction": "incoming", "body": "synthetic-private-text"}}, "nextCursor": "private-native-cursor"})
				return
			}
			if r.URL.Query().Get("cursor") != "private-native-cursor" {
				t.Error("wrong native cursor")
			}
			write(map[string]any{"items": []map[string]any{
				{"id": "msg_original", "serverId": "101", "threadId": "room@chatroom", "direction": "outgoing", "body": "synthetic-private-text"},
				{"id": "msg_voice", "serverId": "102", "threadId": "room@chatroom", "direction": "outgoing"},
			}})
		case "GET /v1/accounts/one/messages/msg_original", "GET /v1/accounts/one/messages/msg_voice", "GET /v1/accounts/one/messages/msg_bystander", "GET /v1/accounts/one/messages/msg_otherroom":
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			thread, server, direction := "room@chatroom", "101", "outgoing"
			switch id {
			case "msg_voice":
				server = "102"
			case "msg_bystander":
				direction = "incoming"
			case "msg_otherroom":
				thread = "other@chatroom"
			}
			write(map[string]any{"id": id, "accountId": "one", "threadId": thread, "serverId": server, "direction": direction})
		case "POST /v1/accounts/one/messages/msg_original/recall", "POST /v1/accounts/one/messages/msg_voice/recall":
			submits.Add(1)
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || !strings.HasPrefix(r.Header.Get("Idempotency-Key"), "station-recall-") {
				t.Error("unexpected recall request")
			}
			if strings.Contains(r.URL.Path, "msg_voice") {
				if body["serverId"] != "102" {
					t.Error("wrong voice segment")
				}
				w.WriteHeader(503)
				return
			}
			if body["serverId"] != "101" {
				t.Error("wrong text server ID")
			}
			write(nativeReceipt("recall-job-one", "queued", "not_attempted", "msg_original", "101"))
		case "GET /v1/accounts/one/operations/recall-job-one":
			statusReads.Add(1)
			if statusMode.Load() == 0 {
				write(nativeReceipt("recall-job-one", "succeeded", "accepted", "msg_original", "999"))
			} else if statusMode.Load() == 1 {
				write(nativeReceipt("recall-job-one", "succeeded", "unknown", "msg_original", "101"))
			} else {
				write(nativeReceipt("recall-job-one", "succeeded", "confirmed", "msg_original", "101"))
			}
		default:
			t.Error("unexpected native request", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer daemon.Close()
	open := func() *localHomeFixture {
		t.Helper()
		entry, err := wechatapp.StationRegistration(t.Context(), daemon.URL, 8009, host)
		if err != nil {
			t.Fatal(err)
		}
		registry, err := apphost.New(entry)
		if err != nil {
			t.Fatal(err)
		}
		h, err := NewLocalHomeHandlerWithApps(host, home, "synthetic-local-password", "", registry)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(h)
		t.Cleanup(func() { server.Close(); _ = h.Close() })
		jar, _ := cookiejar.New(nil)
		return &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}, handler: h}
	}
	f := open()
	connection := wechatapp.ConnectionID("one")
	w, err := f.handler.chat.wechat.worker(connection, true)
	if err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(w.dir, "queue")
	if err := os.MkdirAll(queue, 0700); err != nil {
		t.Fatal(err)
	}
	const delivery = `{"version":2,"account":"one","deliveries":[
	{"key":"text-send","id":"text-job","state":"accepted","thread":"room@chatroom","input":{"account":"one","thread":"room@chatroom"},"server_id":"101"}],
	"media":[{"key":"voice-send","id":"voice-job","kind":"voice","input":{"account":"one","thread":"room@chatroom"},"status":{"state":"cancelled","voice_chunks":[{"index":0,"state":"accepted","has_receipt":true,"server_id":"102"},{"index":1,"state":"cancelled","has_receipt":false}]}}]}`
	if err := os.WriteFile(filepath.Join(queue, "messages-v2.json"), []byte(delivery), 0600); err != nil {
		t.Fatal(err)
	}
	basePath := "/api/apps/wechat/connections/" + connection
	textPath := basePath + "/receipts/text-send/recall"
	voicePath := basePath + "/receipts/voice-send/recall"
	call := func(method, path string, body any, csrf bool, want int) map[string]any {
		t.Helper()
		if code, value := f.request(method, path, body, csrf); code == want {
			return value
		} else {
			t.Fatalf("%s %s: %d != %d (%v)", method, path, code, want, value)
			return nil
		}
	}
	params := func(message, server string) map[string]string {
		return map[string]string{"message_id": message, "server_id": server}
	}
	call("GET", textPath+"?server_id=101", nil, false, 401)
	f.login()
	call("POST", "/api/local/home", map[string]string{"action": "initialize"}, true, 200)
	call("POST", textPath, params("msg_original", "101"), false, 403)
	call("GET", strings.Replace(textPath, connection, wechatapp.ConnectionID("two"), 1)+"?server_id=101", nil, false, 404)
	first := call("GET", textPath+"?server_id=101", nil, false, 200)
	cursor := first["next_cursor"].(string)
	if first["message_id"] != nil || len(first) != 1 || strings.Contains(cursor, "private-native-cursor") || submits.Load() != 0 {
		t.Fatal("first page exposed data or performed a mutation", first)
	}
	call("GET", voicePath+"?server_id=102&cursor="+cursor, nil, false, 400)
	target := call("GET", textPath+"?server_id=101&cursor="+cursor, nil, false, 200)
	if target["message_id"] != "msg_original" || len(target) != 1 {
		t.Fatal("wrong exact candidate or exposed message body", target)
	}
	call("POST", textPath, params("msg_bystander", "101"), true, 409)
	call("POST", textPath, params("msg_otherroom", "101"), true, 409)
	call("POST", textPath, params("msg_voice", "102"), true, 404)
	textResult := call("POST", textPath, params("msg_original", "101"), true, 202)
	if textResult["state"] != "submitted_pending" || textResult["action"] != "recall" {
		t.Fatal(textResult)
	}
	call("POST", textPath, params("msg_original", "101"), true, 202)
	for mode := int32(0); mode < 2; mode++ {
		statusMode.Store(mode)
		view := call("GET", textPath+"?server_id=101", nil, false, 200)["operation"].(map[string]any)
		if view["state"] != "submitted_pending" || view["lookup"] != "identity_mismatch" {
			t.Fatal("misreported recall confirmation", view)
		}
	}
	statusMode.Store(2)
	actionPath := basePath + "/operations/" + textResult["operation_id"].(string)
	if result := call("GET", actionPath, nil, false, 200); result["state"] != "succeeded" || result["lookup"] != "platform" {
		t.Fatal(result)
	}
	voiceResult := call("POST", voicePath, params("msg_voice", "102"), true, 202)
	if voiceResult["state"] != "submission_unknown" {
		t.Fatal(voiceResult)
	}
	f.server.Close()
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	f = open()
	f.login()
	if result := call("GET", actionPath, nil, false, 200); result["state"] != "succeeded" || result["lookup"] != "stored" {
		t.Fatal("confirmed recall was lost on restart", result)
	}
	voice := call("GET", voicePath+"?server_id=102", nil, false, 200)["operation"].(map[string]any)
	if voice["operation_id"] != voiceResult["operation_id"] || voice["state"] != "submission_unknown" {
		t.Fatal("unknown recall was lost on restart", voice)
	}
	call("POST", voicePath, params("msg_voice", "102"), true, 202)
	if submits.Load() != 2 || statusReads.Load() != 3 {
		t.Fatal("unexpected mutation or lookup", submits.Load(), statusReads.Load())
	}
}
