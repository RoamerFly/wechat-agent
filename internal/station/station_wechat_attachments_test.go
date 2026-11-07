package station

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

func TestLocalWechatAttachmentVerifiesOriginalAndScopesAccess(t *testing.T) {
	base := t.TempDir()
	host := filepath.Join(base, "host")
	nativeRoot := filepath.Join(host, "apps", "wechat", "native")
	if err := os.MkdirAll(nativeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	const token, content, sourceBody = "synthetic-attachment-token", "synthetic-image-original", "synthetic-source"
	if err := os.WriteFile(filepath.Join(nativeRoot, "api-token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	occurred := time.Now().UTC().Add(time.Minute)
	var transfers, uploads atomic.Int32
	var wrongUpload atomic.Bool
	var corrupt, wrongAsset, changedSource, thumbnailOnly atomic.Bool
	var blockContent atomic.Bool
	var requestReady atomic.Bool
	var workReady atomic.Bool
	var documentReady atomic.Bool
	var workAt atomic.Value
	workAt.Store("")
	var documentAt atomic.Value
	documentAt.Store("")
	const workContent = "synthetic-work-artifact"
	workHash := fmt.Sprintf("%x", sha256.Sum256([]byte(workContent)))
	documentSource := syntheticStationDOCX(t)
	documentPDF := syntheticStationPDF()
	documentSourceHash := fmt.Sprintf("%x", sha256.Sum256(documentSource))
	documentPDFHash := fmt.Sprintf("%x", sha256.Sum256(documentPDF))
	var requestAt atomic.Value
	requestAt.Store("")
	submitted := make(chan string, 2)
	contentStarted, releaseContent := make(chan struct{}, 1), make(chan struct{})
	originalMessage := func(body string) linuxnative.Message {
		return linuxnative.Message{ID: "msg", AccountID: "one", ThreadID: "room@chatroom", SenderID: "member", ServerID: "1234",
			Type: 3, Direction: "incoming", Body: body, CreatedAt: occurred.Format(time.RFC3339Nano)}
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		out := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/status":
			out(map[string]any{"pid": 7654, "dataDir": nativeRoot, "serverId": "attachment-dataset", "ownershipGeneration": "attachment-generation"})
		case "GET /v1/accounts":
			out(map[string]any{"items": []map[string]any{{"id": "one", "authenticated": true, "connection": "online"}, {"id": "two", "authenticated": true, "connection": "online"}}})
		case "GET /v1/accounts/one", "GET /v1/accounts/two":
			out(map[string]any{"id": filepath.Base(r.URL.Path), "authenticated": true, "connection": "online"})
		case "GET /v1/accounts/one/messages/msg":
			body := sourceBody
			if changedSource.Load() {
				body += "-revision"
			}
			out(originalMessage(body))
		case "GET /v1/accounts/one/events":
			after := r.URL.Query().Get("after")
			page := linuxnative.EventPage{NextCursor: after}
			if requestReady.Load() && after == "0" {
				event := linuxnative.Event{Cursor: "1", Kind: "message.committed"}
				event.Data.MessageID, event.Data.ThreadID, event.Data.Direction = "request", "room@chatroom", "incoming"
				page.Items, page.NextCursor = []linuxnative.Event{event}, "1"
			}
			if workReady.Load() && after == "1" {
				event := linuxnative.Event{Cursor: "2", Kind: "message.committed"}
				event.Data.MessageID, event.Data.ThreadID, event.Data.Direction = "work-request", "room@chatroom", "incoming"
				page.Items, page.NextCursor = []linuxnative.Event{event}, "2"
			}
			if documentReady.Load() && after == "2" {
				event := linuxnative.Event{Cursor: "3", Kind: "message.committed"}
				event.Data.MessageID, event.Data.ThreadID, event.Data.Direction = "document-request", "room@chatroom", "incoming"
				page.Items, page.NextCursor = []linuxnative.Event{event}, "3"
			}
			out(page)
		case "GET /v1/accounts/one/messages/work-request":
			yes := true
			m := linuxnative.Message{ID: "work-request", AccountID: "one", ThreadID: "room@chatroom", SenderID: "member", ServerID: "1236", Type: 1, Direction: "incoming", Body: "@大肥鱼 把给我的作品发过来", CreatedAt: workAt.Load().(string)}
			m.Mentions.State, m.Mentions.Self = "known", &yes
			out(m)
		case "GET /v1/accounts/one/messages/document-request":
			yes, subtype := true, 57
			m := linuxnative.Message{ID: "document-request", AccountID: "one", ThreadID: "room@chatroom", SenderID: "member", ServerID: "1238",
				Type: 49, AppMessageType: &subtype, Quote: &linuxnative.QuoteReference{ServerID: "1237", Type: 49}, Direction: "incoming",
				Body: "@大肥鱼 把引用的 Word 转成 PDF 发回这里", CreatedAt: documentAt.Load().(string)}
			m.Mentions.State, m.Mentions.Self = "known", &yes
			out(m)
		case "GET /v1/accounts/one/messages/document-request/quote":
			out(linuxnative.QuoteResolution{AccountID: "one", ThreadID: "room@chatroom", MessageID: "document-request", State: "resolved", Message: &linuxnative.Message{ID: "document-source", AccountID: "one", ThreadID: "room@chatroom", SenderID: "member", ServerID: "1237", Type: 49, Direction: "incoming", Body: "<msg><appmsg><type>6</type><title>source.docx</title></appmsg></msg>", CreatedAt: documentAt.Load().(string)}})
		case "GET /v1/accounts/one/messages/document-source":
			out(linuxnative.Message{ID: "document-source", AccountID: "one", ThreadID: "room@chatroom", SenderID: "member", ServerID: "1237", Type: 49, Direction: "incoming", Body: "<msg><appmsg><type>6</type><title>source.docx</title></appmsg></msg>", CreatedAt: documentAt.Load().(string)})
		case "GET /v1/accounts/one/messages/request":
			yes, subtype := true, 57
			m := linuxnative.Message{ID: "request", AccountID: "one", ThreadID: "room@chatroom", SenderID: "member", ServerID: "1235",
				Type: 49, AppMessageType: &subtype, Quote: &linuxnative.QuoteReference{ServerID: "1234", Type: 3}, Direction: "incoming",
				Body: "@大肥鱼 把引用图片发回这里", CreatedAt: requestAt.Load().(string)}
			m.Mentions.State, m.Mentions.Self = "known", &yes
			out(m)
		case "GET /v1/accounts/one/messages/request/quote":
			original := originalMessage(sourceBody)
			out(linuxnative.QuoteResolution{AccountID: "one", ThreadID: "room@chatroom", MessageID: "request", State: "resolved", Message: &original})
		case "GET /v1/accounts/one/messages/msg/media":
			items := []map[string]any{{"accountId": "one", "messageId": "msg", "kind": "image", "variant": "thumbnail", "actualVariant": "thumbnail", "state": "available"}}
			if !thumbnailOnly.Load() {
				items = append(items, map[string]any{"accountId": "one", "messageId": "msg", "kind": "image", "variant": "original", "state": "pending"})
			}
			out(map[string]any{"items": items})
		case "GET /v1/accounts/one/messages/document-source/media":
			out(map[string]any{"items": []map[string]any{{"accountId": "one", "messageId": "document-source", "kind": "file", "variant": "original", "state": "available"}}})
		case "POST /v1/accounts/one/messages/msg/media-transfers":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["quality"] != "original" {
				t.Error("transfer did not select full original", request, err)
			}
			transfers.Add(1)
			out(map[string]any{"id": "job", "accountId": "one", "kind": "download", "state": "available", "assetId": "native-asset"})
		case "POST /v1/accounts/one/messages/document-source/media-transfers":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["quality"] != "original" {
				t.Error("document transfer did not select full original", request, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			transfers.Add(1)
			out(map[string]any{"id": "document-job", "accountId": "one", "kind": "download", "state": "available", "assetId": "document-native-asset"})
		case "GET /v1/accounts/one/media/native-asset":
			message := "msg"
			if wrongAsset.Load() {
				message = "different-source"
			}
			out(map[string]any{"id": "native-asset", "accountId": "one", "messageId": message, "kind": "image",
				"state": "available", "variant": "original", "actualVariant": "original", "byteLength": len(content), "contentType": "image/png", "sha256": hash})
		case "GET /v1/accounts/one/media/native-asset/content":
			if blockContent.Load() {
				contentStarted <- struct{}{}
				select {
				case <-releaseContent:
				case <-r.Context().Done():
					return
				}
			}
			if corrupt.Load() {
				_, _ = io.WriteString(w, "X"+content[1:])
				return
			}
			_, _ = io.WriteString(w, content)
		case "GET /v1/accounts/one/media/document-native-asset":
			out(map[string]any{"id": "document-native-asset", "accountId": "one", "messageId": "document-source", "kind": "file", "state": "available", "variant": "original", "actualVariant": "original", "byteLength": len(documentSource), "contentType": localDOCXMIME, "sha256": documentSourceHash})
		case "GET /v1/accounts/one/media/document-native-asset/content":
			_, _ = w.Write(documentSource)
		case "POST /v1/accounts/one/uploads":
			uploads.Add(1)
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(file)
			file.Close()
			if header.Filename == "converted.pdf" {
				if err != nil || string(data) != string(documentPDF) {
					t.Error("uploaded PDF did not match verified document product", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				out(map[string]any{"id": "upload-document", "name": header.Filename, "byteLength": len(data), "sha256": documentPDFHash})
				return
			}
			if header.Filename == "作品.txt" {
				if err != nil || string(data) != workContent {
					t.Error("uploaded mutable home file instead of private snapshot", err)
				}
				out(map[string]any{"id": "upload-work", "name": header.Filename, "byteLength": len(data), "sha256": workHash})
				return
			}
			if err != nil || string(data) != content || header.Filename != "image.bin" {
				t.Error("upload did not use verified original", err)
			}
			digest := hash
			if wrongUpload.Load() {
				digest = "wrong"
			}
			out(map[string]any{"id": "upload-verified", "name": header.Filename, "byteLength": len(data), "sha256": digest})
		case "POST /v1/accounts/one/sends":
			var body map[string]any
			key := r.Header.Get("Idempotency-Key")
			err := json.NewDecoder(r.Body).Decode(&body)
			validMedia := body["sourceId"] == "upload-verified" && body["kind"] == "image" || body["sourceId"] == "upload-work" && body["kind"] == "file" || body["sourceId"] == "upload-document" && body["kind"] == "file"
			if err != nil || key == "" || !validMedia || body["threadId"] != "room@chatroom" {
				t.Error("unexpected native attachment submission", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body["sourceId"] == "upload-document" {
				out(map[string]any{"id": "synthetic-send-document", "accountId": "one", "threadId": "room@chatroom", "kind": "send", "sourceId": "upload-document", "state": "accepted", "serverMessageId": "2419"})
				submitted <- key
				return
			}
			out(map[string]any{"id": "synthetic-send-" + fmt.Sprint(body["kind"]), "accountId": "one", "threadId": "room@chatroom", "kind": "send", "sourceId": body["sourceId"], "state": "accepted", "serverMessageId": "2311"})
			submitted <- key
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	entry, err := wechatapp.StationRegistration(t.Context(), daemon.URL, 7654, host)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := apphost.New(entry)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLocalHomeHandlerWithApps(host, filepath.Join(base, "home"), "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	server := httptest.NewServer(handler)
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	f := &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}}
	const thread = "room@chatroom"
	connection := wechatapp.ConnectionID("one")
	path := "/api/apps/wechat/connections/" + connection
	req := func(method, route string, value any, csrf bool, want int) map[string]any {
		t.Helper()
		code, row := f.request(method, route, value, csrf)
		if code != want {
			t.Fatalf("%s %s: got %d want %d, %#v", method, route, code, want, row)
		}
		return row
	}
	acquire := func(code int) map[string]any {
		return req("POST", path+"/attachments", map[string]string{"thread_id": thread, "message_id": "msg"}, true, code)
	}
	policy := func(want int, attachments ...string) {
		t.Helper()
		req("PUT", path+"/policy", map[string]any{"reply_threads": []string{thread}, "attachment_threads": append([]string{}, attachments...)}, true, want)
	}
	get := func(route string, want int) *http.Response {
		t.Helper()
		response, err := f.client.Get(server.URL + route)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		if response.StatusCode != want {
			t.Fatalf("GET %s: got %d want %d", route, response.StatusCode, want)
		}
		return response
	}
	req("POST", path+"/attachments", map[string]string{}, false, 401)
	f.login()
	req("POST", path+"/attachments", map[string]string{}, false, 403)
	policy(400, "other-room")
	req("PUT", path+"/policy", map[string]any{"reply_threads": []string{thread}, "media_reply_threads": []string{thread}}, true, 400)
	policy(200)
	acquire(404)
	if transfers.Load() != 0 {
		t.Fatal("download submitted without grant")
	}
	policy(200, thread)
	thumbnailOnly.Store(true)
	acquire(502)
	thumbnailOnly.Store(false)
	wrongAsset.Store(true)
	acquire(502)
	wrongAsset.Store(false)
	corrupt.Store(true)
	acquire(502)
	corrupt.Store(false)
	worker, err := handler.chat.wechat.worker(connection, false)
	if err != nil || worker == nil {
		t.Fatal("worker policy missing", err)
	}
	entries, err := os.ReadDir(filepath.Join(worker.dir, "attachments"))
	if err != nil || len(entries) != 0 {
		t.Fatal("unverified bytes were published", entries, err)
	}
	row := acquire(200)
	asset, _ := row["asset_id"].(string)
	if !stationAttachmentID(asset) || row["sha256"] != hash || row["kind"] != "image" || row["variant"] != "original" || row["status"] != "original_available" || row["path"] != nil {
		t.Fatal(row)
	}
	if acquire(200)["asset_id"] != asset || transfers.Load() != 3 {
		t.Fatal("verified cached original was downloaded again")
	}
	contentRoute := "/attachments/" + asset + "/content"
	response := get(path+contentRoute, 200)
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(got) != content || response.Header.Get("Content-Disposition") != "attachment; filename=image.bin" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("verified download mismatch", err)
	}
	get("/api/apps/wechat/connections/"+wechatapp.ConnectionID("two")+contentRoute, 404)
	changedSource.Store(true)
	get(path+contentRoute, 404)
	changedSource.Store(false)

	native, err := handler.chat.wechat.WorkerSender(t.Context(), connection)
	if err != nil {
		t.Fatal(err)
	}
	memory, err := companion.NewStore(filepath.Join(worker.dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	worker.mu.Lock()
	grant := worker.policy.AttachmentGrants[thread]
	worker.mu.Unlock()
	tools := (&stationAttachmentTools{native: native, store: memory, dir: worker.dir, grants: map[string]time.Time{thread: grant}, blocked: func() bool { return false }}).ForkTools().(*stationAttachmentTools)
	in := companion.Input{Channel: "wechat", Account: "one", Thread: thread, Sender: "member", TraceID: "msg", Text: sourceBody, MessageTime: occurred.Format(time.RFC3339Nano),
		Source: &companion.SourceRef{InstanceID: memory.SourceInstanceID(), MessageID: "msg", Account: "one", Thread: thread, Sender: "member", ServerID: "1234", MessageType: 3, OccurredAt: occurred.Format(time.RFC3339Nano), BodyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(sourceBody)))}}
	config := companion.Defaults()
	config.Permissions.Remember = true
	if err := memory.Observe(in, config, occurred); err != nil {
		t.Fatal(err)
	}
	tools.SetTrace(in)
	if list, _ := tools.List(t.Context()); len(list) != 1 || list[0].Name != "wechat_file_read" {
		t.Fatal("current media tool unavailable", list)
	}
	if _, err := tools.Call(t.Context(), "wechat_file_read", json.RawMessage(`{"message_id":"unrelated"}`)); err == nil {
		t.Fatal("model substituted unrelated source")
	}
	if result, err := tools.Call(t.Context(), "wechat_file_read", json.RawMessage(`{"message_id":"msg"}`)); err != nil || !json.Valid([]byte(result)) {
		t.Fatal("current media call failed", err)
	}
	textTurn := in
	textTurn.TraceID, textTurn.Source = "new-request", &companion.SourceRef{MessageID: "new-request", MessageType: 1}
	tools.SetTrace(textTurn)
	if list, _ := tools.List(t.Context()); len(list) != 0 {
		t.Fatal("ordinary text exposed attachment tool", list)
	}
	textTurn.QuotedSource = in.Source
	tools.SetTrace(textTurn)
	if result, err := tools.Call(t.Context(), "wechat_file_read", json.RawMessage(`{"message_id":"msg"}`)); err != nil || !json.Valid([]byte(result)) {
		t.Fatal("verified quoted media call failed", err)
	}
	req("PUT", path+"/policy", map[string]any{"reply_threads": []string{thread}, "media_reply_threads": []string{thread}}, true, 200)
	worker.mu.Lock()
	tools.replies = worker.policy.MediaReplyGrants
	worker.mu.Unlock()
	textTurn.Mode, textTurn.Directed = "direct", true
	tools.SetTrace(textTurn)
	prepared, err := tools.Call(t.Context(), "wechat_attachment_reply_prepare", json.RawMessage(`{"message_id":"msg"}`))
	var responsePayload struct {
		Media []companion.GeneratedMedia `json:"media"`
	}
	if err != nil || json.Unmarshal([]byte(prepared), &responsePayload) != nil || len(responsePayload.Media) != 1 || uploads.Load() != 0 {
		t.Fatal("attachment preparation uploaded bytes or failed", prepared, err)
	}
	selection := responsePayload.Media[0]
	if selected, ok := tools.Media(selection.ID); !ok || selected.AssetID != asset || selected.SourceID != "" || selected.Kind != "image" {
		t.Fatal("unexpected prepared handle", selected)
	}
	if _, err := tools.stageReply(t.Context(), textTurn, "video", asset); err == nil || uploads.Load() != 0 {
		t.Fatal("format substitution reached native upload")
	}
	wrongUpload.Store(true)
	if _, err := tools.stageReply(t.Context(), textTurn, "image", asset); err == nil {
		t.Fatal("wrong staged digest accepted")
	}
	wrongUpload.Store(false)
	if source, err := tools.stageReply(t.Context(), textTurn, "image", asset); err != nil || source != "upload-verified" || uploads.Load() != 2 {
		t.Fatal("verified native upload failed", source, err)
	}
	tools.SetTrace(in)
	if _, ok := tools.Media(selection.ID); ok {
		t.Fatal("prepared handle persisted into another turn")
	}
	if err := memory.Close(); err != nil {
		t.Fatal(err)
	}
	settings := defaultLocalModelSettings()
	settings.BaseURL = "https://127.0.0.1:1"
	if _, err := handler.chat.models.Save(LocalModelInput{LocalModelSettings: settings, APIKey: "synthetic-offline-model"}); err != nil {
		t.Fatal(err)
	}
	handler.chat.wechat.model = stationAttachmentReplyModel{mediaID: selection.ID}
	req("POST", path+"/lifecycle", map[string]string{"action": "start"}, true, 200)
	waitAppLifecycle(t, func() bool {
		row := req("GET", path, nil, false, 200)
		return row["connection"].(map[string]any)["worker"] == "running"
	})
	requestAt.Store(time.Now().UTC().Format(time.RFC3339Nano))
	requestReady.Store(true)
	var deliveryKey string
	select {
	case deliveryKey = <-submitted:
	case <-time.After(35 * time.Second): // The real group collection window is 20 seconds.
		queue, _ := os.ReadFile(filepath.Join(worker.dir, "queue", "messages-v2.json"))
		t.Fatalf("worker did not submit the verified quoted attachment: %s", queue)
	}
	waitAppLifecycle(t, func() bool {
		code, receipt := f.request("GET", path+"/receipts/"+deliveryKey, nil, false)
		return code == 200 && strings.Contains(fmt.Sprint(receipt), "2311")
	})
	req("POST", path+"/lifecycle", map[string]string{"action": "stop"}, true, 200)
	if len(submitted) != 0 || uploads.Load() != 3 {
		t.Fatal("worker repeated upload or submit", uploads.Load(), len(submitted))
	}
	identity := req("POST", "/api/local/home", map[string]string{"action": "initialize"}, true, 200)
	workPath := filepath.Join(identity["directory"].(string), "作品.txt")
	if err := os.WriteFile(workPath, []byte(workContent), 0600); err != nil {
		t.Fatal(err)
	}
	registered := req("POST", "/api/local/artifacts", map[string]string{"path": "作品.txt", "expected_sha256": workHash, "request_key": "register-work"}, true, 200)
	if err := os.WriteFile(workPath, []byte("edited-after-registration"), 0600); err != nil {
		t.Fatal(err)
	}
	grantBody := map[string]any{"artifact_id": registered["artifact_id"], "thread_id": thread, "requester_id": "member", "request_key": "grant-work", "enabled": true}
	req("POST", path+"/work-grants", grantBody, false, 403)
	shared := req("POST", path+"/work-grants", grantBody, true, 200)
	workID := shared["delivery_id"].(string)
	listing := req("GET", path+"/work-grants?limit=1", nil, false, 200)
	if len(listing["items"].([]any)) != 1 {
		t.Fatal("saved disclosure not visible", listing)
	}
	workAccess := &stationWorkAccess{artifacts: handler.home.artifacts, dir: worker.dir, grants: tools.replies}
	foreign := textTurn
	foreign.Sender = "another-member"
	if _, err := workAccess.authorized(foreign, workID); err == nil {
		t.Fatal("another group member inherited private work disclosure")
	}
	handler.chat.wechat.model = stationAttachmentReplyModel{mediaID: "reply-" + workID, workID: workID}
	req("POST", path+"/lifecycle", map[string]string{"action": "start"}, true, 200)
	waitAppLifecycle(t, func() bool {
		return req("GET", path, nil, false, 200)["connection"].(map[string]any)["worker"] == "running"
	})
	workAt.Store(time.Now().UTC().Format(time.RFC3339Nano))
	workReady.Store(true)
	select {
	case deliveryKey = <-submitted:
	case <-time.After(35 * time.Second):
		queue, _ := os.ReadFile(filepath.Join(worker.dir, "queue", "messages-v2.json"))
		t.Fatalf("registered work not delivered: %s", queue)
	}
	waitAppLifecycle(t, func() bool {
		code, result := f.request("GET", path+"/receipts/"+deliveryKey, nil, false)
		return code == 200 && strings.Contains(fmt.Sprint(result), "2311")
	})
	grantBody["enabled"] = false
	req("POST", path+"/work-grants/"+workID+"/revoke", map[string]any{}, true, 200)
	if _, err := workAccess.authorized(textTurn, workID); err == nil {
		t.Fatal("revoked work still visible")
	}
	grantBody["enabled"] = true
	req("POST", path+"/work-grants", grantBody, true, 409)
	if uploads.Load() != 4 || len(submitted) != 0 {
		t.Fatal("work delivery duplicated", uploads.Load(), len(submitted))
	}
	// The normal worker must use the document exporter as a prepare-only tool;
	// its PDF becomes a native upload only after the model selects media_ids.
	// Use a local renderer fixture so this remains a no-model/no-WeChat test.
	handler.chat.wechat.documents = stationDocumentTools{
		available: func(context.Context, string) bool { return true },
		inspect:   func(context.Context, stationDocumentInput) (string, error) { return `{}`, nil },
		exportAvailable: func(context.Context) bool {
			return true
		},
		exportPDF: func(ctx context.Context, in stationDocumentInput, filename string, persist func(context.Context, *os.File, stationDocumentProduct) error) (stationDocumentProduct, error) {
			if in.extension != ".docx" || in.record.SHA256 != documentSourceHash || filename != "converted.pdf" {
				t.Fatalf("unexpected worker document export input: %#v filename=%q", in, filename)
			}
			product := stationDocumentProduct{Filename: filename, SHA256: documentPDFHash, Bytes: int64(len(documentPDF)), Pages: 1}
			path := filepath.Join(t.TempDir(), filename)
			if err := os.WriteFile(path, documentPDF, 0600); err != nil {
				return stationDocumentProduct{}, err
			}
			file, err := os.Open(path)
			if err != nil {
				return stationDocumentProduct{}, err
			}
			defer file.Close()
			if err := persist(ctx, file, product); err != nil {
				return stationDocumentProduct{}, err
			}
			return product, nil
		},
	}
	handler.chat.wechat.model = stationAttachmentReplyModel{documentID: "document-source"}
	req("POST", path+"/lifecycle", map[string]string{"action": "start"}, true, 200)
	waitAppLifecycle(t, func() bool {
		return req("GET", path, nil, false, 200)["connection"].(map[string]any)["worker"] == "running"
	})
	documentAt.Store(time.Now().UTC().Format(time.RFC3339Nano))
	documentReady.Store(true)
	select {
	case deliveryKey = <-submitted:
	case <-time.After(35 * time.Second):
		queue, _ := os.ReadFile(filepath.Join(worker.dir, "queue", "messages-v2.json"))
		t.Fatalf("worker did not submit exported PDF: %s", queue)
	}
	waitAppLifecycle(t, func() bool {
		code, receipt := f.request("GET", path+"/receipts/"+deliveryKey, nil, false)
		return code == 200 && strings.Contains(fmt.Sprint(receipt), "2419") && strings.Contains(fmt.Sprint(receipt), "synthetic-send-document")
	})
	req("POST", path+"/lifecycle", map[string]string{"action": "stop"}, true, 200)
	if len(submitted) != 0 || uploads.Load() != 5 {
		t.Fatal("worker duplicated PDF upload or send", uploads.Load(), len(submitted))
	}
	policy(200)
	get(path+contentRoute, 404)
	policy(200, thread)
	get(path+contentRoute, 404)
	newAsset := acquire(200)["asset_id"]
	if newAsset == asset {
		t.Fatal("new grant reused previously revoked asset handle")
	}

	policy(200)
	policy(200, thread)
	defer func() {
		select {
		case <-releaseContent:
		default:
			close(releaseContent)
		}
	}()
	entries, err = os.ReadDir(filepath.Join(worker.dir, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	knownAttachments := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		knownAttachments[entry.Name()] = struct{}{}
	}
	blockContent.Store(true)
	completed := make(chan int, 1)
	go func() {
		code, _ := f.request("POST", path+"/attachments", map[string]string{"thread_id": thread, "message_id": "msg"}, true)
		completed <- code
	}()
	select {
	case <-contentStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer did not start")
	}
	policy(200)
	close(releaseContent)
	select {
	case code := <-completed:
		if code != 404 {
			t.Fatal("revoked transfer returned status", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoked transfer did not stop")
	}
	entries, err = os.ReadDir(filepath.Join(worker.dir, "attachments"))
	if err != nil || len(entries) != len(knownAttachments) {
		t.Fatal("revoked transfer left a staged or published copy", entries, err)
	}
	for _, entry := range entries {
		if _, ok := knownAttachments[entry.Name()]; !ok {
			t.Fatal("revoked transfer left a new attachment entry", entry.Name())
		}
	}
}

type stationAttachmentReplyModel struct{ mediaID, workID, documentID string }

func (m stationAttachmentReplyModel) Chat(ctx context.Context, system string, history []map[string]string) (string, error) {
	return m.ChatFunctions(ctx, system, history, nil)
}

func (m stationAttachmentReplyModel) ChatFunctions(_ context.Context, _ string, history []map[string]string, tools []mcptools.Tool) (string, error) {
	for _, turn := range history {
		if m.mediaID != "" && strings.Contains(turn["content"], m.mediaID) {
			return fmt.Sprintf(`{"action":"reply","messages":[],"media_ids":[%q]}`, m.mediaID), nil
		}
		if m.documentID != "" {
			var packet struct {
				Tool   string `json:"tool"`
				Result string `json:"result"`
			}
			var prepared struct {
				Media []companion.GeneratedMedia `json:"media"`
			}
			if json.Unmarshal([]byte(turn["content"]), &packet) == nil && packet.Tool == "wechat_document_export_pdf" &&
				json.Unmarshal([]byte(packet.Result), &prepared) == nil && len(prepared.Media) == 1 &&
				strings.HasPrefix(prepared.Media[0].ID, "reply-docpdf-") {
				return fmt.Sprintf(`{"action":"reply","messages":[],"media_ids":[%q]}`, prepared.Media[0].ID), nil
			}
		}
	}
	if m.documentID != "" {
		for _, tool := range tools {
			if tool.Name == "wechat_document_export_pdf" {
				return fmt.Sprintf(`{"action":"tool","tool":"wechat_document_export_pdf","arguments":{"message_id":%q,"filename":"converted.pdf"}}`, m.documentID), nil
			}
		}
		return `{"action":"tool","tool":"discover_tools","arguments":{"query":"wechat_document_export_pdf"}}`, nil
	}
	for _, tool := range tools {
		if m.workID != "" && tool.Name == "wechat_work_reply" {
			return fmt.Sprintf(`{"action":"tool","tool":"wechat_work_reply","arguments":{"action":"prepare","delivery_id":%q}}`, m.workID), nil
		}
		if tool.Name == "wechat_attachment_reply_prepare" {
			return `{"action":"tool","tool":"wechat_attachment_reply_prepare","arguments":{"message_id":"msg"}}`, nil
		}
	}
	if m.workID != "" {
		return `{"action":"tool","tool":"discover_tools","arguments":{"query":"wechat_work_reply"}}`, nil
	}
	return `{"action":"tool","tool":"discover_tools","arguments":{"query":"wechat_attachment_reply_prepare"}}`, nil
}
