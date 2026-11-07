package station

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

func TestLocalWechatWorkersIsolatePauseRevokeAndRestore(t *testing.T) {
	base := t.TempDir()
	host, workspace := filepath.Join(base, "host"), filepath.Join(base, "workspace")
	nativeRoot := filepath.Join(host, "apps", "wechat", "native")
	if err := os.MkdirAll(nativeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeRoot, "api-token"), []byte("synthetic-worker-native-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var nativeMu sync.Mutex
	messages := map[string][]linuxnative.Message{}
	sends := map[string][]string{}
	submitKeys := map[string]string{}
	twoSubmitted := make(chan struct{}, 1)
	var foreignReads atomic.Int32
	var replaced, lateAccepted atomic.Bool
	var replacementTraffic atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeMu.Lock()
		defer nativeMu.Unlock()
		if r.Header.Get("Authorization") != "Bearer synthetic-worker-native-token" {
			t.Error("missing worker credential")
			w.WriteHeader(401)
			return
		}
		out := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.URL.Path {
		case "/v1/status":
			gen := "worker-generation"
			if replaced.Load() {
				gen = "replaced-worker-generation"
			}
			out(map[string]any{"pid": 7654, "dataDir": nativeRoot, "serverId": "worker-station", "ownershipGeneration": gen})
			return
		case "/v1/accounts":
			out(map[string]any{"items": []map[string]any{
				{"id": "one", "authenticated": true, "connection": "online"},
				{"id": "two", "authenticated": true, "connection": "online"},
			}})
			return
		}
		if replaced.Load() {
			replacementTraffic.Add(1)
		}
		fields := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/accounts/"), "/")
		account := fields[0]
		if account != "one" && account != "two" {
			t.Error("unexpected account", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		path := strings.Join(fields[1:], "/")
		switch {
		case r.Method == http.MethodGet && path == "":
			out(map[string]any{"id": account, "authenticated": true, "connection": "online"})
		case r.Method == http.MethodGet && path == "events":
			after, _ := strconv.Atoi(r.URL.Query().Get("after"))
			events := []linuxnative.Event{}
			for i, message := range messages[account] {
				if i < after {
					continue
				}
				event := linuxnative.Event{Cursor: strconv.Itoa(i + 1), Kind: "message.committed"}
				event.Data.MessageID, event.Data.ThreadID, event.Data.Direction = message.ID, message.ThreadID, message.Direction
				events = append(events, event)
			}
			out(linuxnative.EventPage{Items: events, NextCursor: strconv.Itoa(after + len(events))})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "messages/"):
			for _, m := range messages[account] {
				if m.ID == strings.TrimPrefix(path, "messages/") {
					if m.ThreadID == "foreign-thread" {
						foreignReads.Add(1)
					}
					out(m)
					return
				}
			}
			w.WriteHeader(404)
		case r.Method == http.MethodPost && path == "sends":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			thread, _ := body["threadId"].(string)
			sends[account] = append(sends[account], thread)
			submitKeys[account] = r.Header.Get("Idempotency-Key")
			if account == "two" {
				select {
				case twoSubmitted <- struct{}{}:
				default:
				}
			}
			w.WriteHeader(202)
			out(map[string]any{"id": "send-" + account, "accountId": account, "threadId": thread, "kind": "text", "state": "queued"})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "sends"):
			if account != "two" || submitKeys[account] == "" {
				w.WriteHeader(404)
				return
			}
			row := map[string]any{"id": "send-two", "accountId": account, "threadId": "person-two", "kind": "text", "state": "queued"}
			if lateAccepted.Load() {
				row["state"], row["serverId"] = "accepted", "67001"
			}
			out(row)
		default:
			// Native display-name discovery is optional. Return an empty page.
			if r.Method == http.MethodGet {
				out(map[string]any{"items": []any{}})
			} else {
				t.Error("unexpected mutation", r.Method, r.URL.Path)
				w.WriteHeader(404)
			}
		}
	}))
	defer daemon.Close()
	appendMessageAt := func(account, thread, text string, at time.Time) {
		nativeMu.Lock()
		defer nativeMu.Unlock()
		index := len(messages[account]) + 1
		messages[account] = append(messages[account], linuxnative.Message{
			ID: fmt.Sprintf("%s-msg-%d", account, index), AccountID: account, ThreadID: thread, SenderID: thread,
			ServerID: strconv.Itoa(62000 + index), Direction: "incoming", Type: 1,
			Body: text, CreatedAt: at.UTC().Format(time.RFC3339Nano),
		})
	}
	appendMessage := func(account, thread, text string) { appendMessageAt(account, thread, "/ai "+text, time.Now()) }
	reopen := func() (*LocalHomeHandler, *localHomeFixture) {
		t.Helper()
		entry, err := wechatapp.StationRegistration(t.Context(), daemon.URL, 7654, host)
		if err != nil {
			t.Fatal(err)
		}
		registry, err := apphost.New(entry)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := NewLocalHomeHandlerWithApps(host, workspace, "synthetic-local-password", "", registry)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		t.Cleanup(func() { _ = handler.Close() })
		jar, _ := cookiejar.New(nil)
		f := &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}}
		f.login()
		return handler, f
	}
	h, f := reopen()
	blockStarted, blockStopped := make(chan struct{}, 1), make(chan struct{}, 1)
	groupPrompt := make(chan string, 4)
	h.chat.wechat.model = localModelFunc(func(ctx context.Context, system string, history []map[string]string) (string, error) {
		raw, _ := json.Marshal(history)
		if strings.Contains(system, "fixture scoped group persona") {
			groupPrompt <- string(raw)
			return `{"action":"silent"}`, nil
		}
		if strings.Contains(string(raw), "block-synthetic-first") {
			blockStarted <- struct{}{}
			<-ctx.Done()
			blockStopped <- struct{}{}
			return "", ctx.Err()
		}
		return `{"action":"reply","messages":["合成回复"]}`, nil
	})
	path := func(account string) string { return "/api/apps/wechat/connections/" + wechatapp.ConnectionID(account) }
	request := func(f *localHomeFixture, method, path string, body any, csrf bool, code int) map[string]any {
		t.Helper()
		got, result := f.request(method, path, body, csrf)
		if got != code {
			t.Fatalf("%s %s: status %d want %d, %#v", method, path, got, code, result)
		}
		return result
	}
	policy := func(f *localHomeFixture, account string, threads ...string) {
		t.Helper()
		request(f, "PUT", path(account)+"/policy", map[string]any{"reply_threads": append([]string{}, threads...)}, true, 200)
	}
	lifecycle := func(f *localHomeFixture, account, action string, code int) map[string]any {
		t.Helper()
		return request(f, "POST", path(account)+"/lifecycle", map[string]string{"action": action}, true, code)
	}
	state := func(f *localHomeFixture, account string) map[string]any {
		t.Helper()
		return request(f, "GET", path(account), nil, false, 200)["connection"].(map[string]any)
	}
	waitState := func(f *localHomeFixture, account, expected string) {
		t.Helper()
		waitAppLifecycle(t, func() bool { return state(f, account)["worker"] == expected })
	}
	await := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(14 * time.Second):
			t.Fatal("worker did not reach expected stage")
		}
	}
	lifecycle(f, "one", "start", 409)
	request(f, "PUT", path("one")+"/policy", map[string]any{"reply_threads": []string{"person-one"}}, false, 403)
	request(f, "PUT", path("one")+"/policy", map[string]any{"reply_threads": nil}, true, 400)
	request(f, "PUT", path("other")+"/policy", map[string]any{"reply_threads": []string{"person-one"}}, true, 404)
	policy(f, "one", "person-one")
	policy(f, "two", "person-two")
	shareOne, shareTwo := path("one")+"/share-reader", path("two")+"/share-reader"
	h.chat.wechat.shareOptions.CatalogDirectory = filepath.Join("..", "..", "plugins")
	request(f, "PUT", shareOne, map[string]any{"action": "install", "grant_network": true}, false, 403)
	request(f, "PUT", shareOne, map[string]any{"action": "install"}, true, 400)
	installedShare := request(f, "PUT", shareOne, map[string]any{"action": "install", "grant_network": true}, true, 200)
	if installedShare["installed"] != true || installedShare["enabled"] != false {
		t.Fatal(installedShare)
	}
	shareRev := installedShare["revision"]
	request(f, "PUT", shareOne, map[string]any{"action": "configure", "revision": shareRev, "configuration": map[string]any{"values": map[string]any{"object_bridge_url": "http://192.0.2.1:1234"}}}, true, 400)
	configuredShare := request(f, "PUT", shareOne, map[string]any{"action": "configure", "revision": shareRev, "configuration": map[string]any{"secrets": map[string]string{"yuanbao_cookie": "synthetic-fixture-cookie"}}}, true, 200)
	encodedShare, _ := json.Marshal(configuredShare)
	if strings.Contains(string(encodedShare), "synthetic-fixture-cookie") || !strings.Contains(string(encodedShare), "yuanbao_cookie") {
		t.Fatal("share settings did not redact credential", configuredShare)
	}
	if other := request(f, "GET", shareTwo, nil, false, 200); other["installed"] != false || other["configuration"] != nil {
		t.Fatal("cross-connection share tool leaked", other)
	}
	mediaOne, mediaTwo := path("one")+"/media-understanding", path("two")+"/media-understanding"
	request(f, "PUT", mediaOne, map[string]any{"action": "install", "grant_media": true}, false, 403)
	request(f, "PUT", mediaOne, map[string]any{"action": "install", "grant_network": true}, true, 400)
	media := request(f, "PUT", mediaOne, map[string]any{"action": "install", "grant_media": true}, true, 200)
	if media["installed"] != true || media["enabled"] != false {
		t.Fatal(media)
	}
	mediaRevision := media["revision"]
	request(f, "PUT", mediaOne, map[string]any{"action": "configure", "revision": mediaRevision, "configuration": map[string]any{"values": map[string]any{"asr_backend": "local"}}}, true, 409)
	request(f, "PUT", mediaOne, map[string]any{"action": "configure", "revision": mediaRevision, "configuration": map[string]any{"values": map[string]any{"local_asr_profile_id": "caller-controlled"}}}, true, 400)
	configuredMedia := request(f, "PUT", mediaOne, map[string]any{"action": "configure", "revision": mediaRevision, "configuration": map[string]any{
		"values":  map[string]string{"vision_model": "synthetic-vision", "vision_endpoint": "https://vision.example/chat/completions"},
		"secrets": map[string]string{"vision_key": "synthetic-vision-test-value"},
	}}, true, 200)
	encodedMedia, _ := json.Marshal(configuredMedia)
	if strings.Contains(string(encodedMedia), "synthetic-vision-test-value") || !strings.Contains(string(encodedMedia), "vision_key") {
		t.Fatal("media settings did not redact credential", configuredMedia)
	}
	mediaWorker, err := h.chat.wechat.worker(wechatapp.ConnectionID("one"), false)
	if err != nil || mediaWorker == nil {
		t.Fatal("synthetic media worker unavailable", err)
	}
	mediaHost, err := h.chat.wechat.shareHost(mediaWorker, "one")
	if err != nil {
		t.Fatal(err)
	}
	mediaHost.localASR = stationLocalASR{
		version: func(context.Context) (string, error) { return "synthetic-owner-asr-version", nil },
		transcribe: func(context.Context, []byte, func() error) (string, error) {
			return "", fmt.Errorf("unexpected transcription while saving settings")
		},
	}
	configured := configuredMedia["configuration"].(map[string]any)
	localMedia := request(f, "PUT", mediaOne, map[string]any{"action": "configure", "revision": configuredMedia["revision"], "configuration": map[string]any{
		"revision": configured["revision"], "values": map[string]string{"asr_backend": "local", "vision_model": "synthetic-vision", "vision_endpoint": "https://vision.example/chat/completions"},
	}}, true, 200)
	if values := localMedia["configuration"].(map[string]any)["values"].(map[string]any); values["asr_backend"] != "local" || values["vision_model"] != "synthetic-vision" {
		t.Fatal("local backend choice replaced the vision configuration", localMedia)
	}
	if localMedia["local_asr_ready"] != true {
		t.Fatal("ready local ASR was not reported", localMedia)
	}
	availableASR := mediaHost.localASR
	mediaHost.localASR = stationLocalASR{}
	request(f, "PUT", mediaOne, map[string]any{"action": "enable", "revision": localMedia["revision"]}, true, 409)
	unavailableMedia := request(f, "GET", mediaOne, nil, false, 200)
	if unavailableMedia["enabled"] != false || unavailableMedia["local_asr_ready"] != false || unavailableMedia["local_asr_reason"] == nil || unavailableMedia["revision"] != localMedia["revision"] {
		t.Fatal("unavailable local ASR was enabled or reported ready", unavailableMedia)
	}
	mediaHost.localASR = availableASR
	enabledMedia := request(f, "PUT", mediaOne, map[string]any{"action": "enable", "revision": localMedia["revision"]}, true, 200)
	mediaHost.localASR = stationLocalASR{}
	if view := request(f, "GET", mediaOne, nil, false, 200); view["enabled"] != true || view["local_asr_ready"] != false {
		t.Fatal("ASR revocation did not change the connection readiness", view)
	}
	request(f, "PUT", mediaOne, map[string]any{"action": "disable", "revision": enabledMedia["revision"]}, true, 200)
	mediaHost.localASR = availableASR
	if other := request(f, "GET", mediaTwo, nil, false, 200); other["installed"] != false || other["configuration"] != nil {
		t.Fatal("cross-connection media tool leaked", other)
	}
	request(f, "PUT", path("one")+"/policy", map[string]any{"reply_threads": []string{"person-one"}, "history_threads": []string{"foreign-thread"}}, true, 400)
	request(f, "PUT", path("two")+"/policy", map[string]any{"reply_threads": []string{"person-two"}, "history_threads": []string{"person-two"}}, true, 200)
	const room = "fixture-room@chatroom"
	group := map[string]any{"thread_id": room, "note": "synthetic group preference", "summarize_shares": true}
	groupInput := map[string]any{"reply_threads": []string{"person-one", room}, "group_rules": []any{group}}
	request(f, "PUT", path("one")+"/policy", groupInput, true, 400)
	groupInput["attachment_threads"] = []string{room}
	groupInput["auto_observe_images"] = true
	groupInput["auto_observe_videos"] = true
	request(f, "PUT", path("one")+"/policy", groupInput, false, 403)
	request(f, "PUT", path("one")+"/policy", groupInput, true, 200)
	policy(f, "one", "person-one", room)
	oneRules := state(f, "one")["configured_policy"].(map[string]any)["group_rules"].([]any)
	if len(oneRules) != 1 || oneRules[0].(map[string]any)["summarize_shares"] != true || oneRules[0].(map[string]any)["note"] != "synthetic group preference" ||
		state(f, "two")["configured_policy"].(map[string]any)["group_rules"] != nil {
		t.Fatal("group settings were lost or shared between accounts", oneRules)
	}
	if state(f, "one")["configured_policy"].(map[string]any)["auto_observe_images"] != true || state(f, "two")["configured_policy"].(map[string]any)["auto_observe_images"] != nil {
		t.Fatal("automatic image permission was lost or shared between connections")
	}
	if state(f, "one")["configured_policy"].(map[string]any)["auto_observe_videos"] != true || state(f, "two")["configured_policy"].(map[string]any)["auto_observe_videos"] != nil {
		t.Fatal("automatic video permission was lost or shared between connections")
	}
	request(f, "PUT", path("one")+"/policy", map[string]any{"reply_threads": []string{"person-one", room}, "attachment_threads": []string{}}, true, 200)
	if rule := state(f, "one")["configured_policy"].(map[string]any)["group_rules"].([]any)[0].(map[string]any); rule["summarize_shares"] != false || rule["note"] != "synthetic group preference" {
		t.Fatal("attachment revocation retained summary authorization or erased note", rule)
	}
	if state(f, "one")["configured_policy"].(map[string]any)["auto_observe_images"] != nil {
		t.Fatal("automatic image processing retained permission after removing all attachment grants")
	}
	if state(f, "one")["configured_policy"].(map[string]any)["auto_observe_videos"] != nil {
		t.Fatal("automatic video processing retained permission after removing all attachment grants")
	}
	policy(f, "one", "person-one")
	if rules := state(f, "one")["configured_policy"].(map[string]any)["group_rules"]; rules != nil {
		t.Fatal("reply revocation retained group settings", rules)
	}
	policy(f, "two", "person-two") // Old clients omitting the field retain the explicit grant.
	if grants := state(f, "two")["configured_policy"].(map[string]any)["history_threads"].([]any); len(grants) != 1 || grants[0] != "person-two" {
		t.Fatal("history authorization was not retained", grants)
	}
	settings := defaultLocalModelSettings()
	settings.BaseURL = "https://127.0.0.1:1"
	if _, err := h.chat.models.Save(LocalModelInput{LocalModelSettings: settings, APIKey: "synthetic-worker-model-not-real"}); err != nil {
		t.Fatal(err)
	}
	lifecycle(f, "one", "start", 200)
	lifecycle(f, "two", "start", 200)
	waitState(f, "one", "running")
	waitState(f, "two", "running")
	appendMessage("one", "foreign-thread", "must-not-read")
	appendMessage("one", "person-one", "block-synthetic-first")
	await(blockStarted)
	appendMessage("two", "person-two", "synthetic-second")
	// A model result still passes the real reply delay and the worker poll.
	// Await the actual submission barrier, using the processing-stage budget
	// rather than the shorter connection-status polling budget.
	await(twoSubmitted)
	if row := lifecycle(f, "one", "stop", 200); row["worker"] != "paused" || row["submissions_paused"] != true {
		t.Fatal(row)
	}
	await(blockStopped)
	if detail := state(f, "one"); detail["login"] != "authenticated" || detail["runtime"] != "online" || detail["worker"] != "paused" {
		t.Fatal(detail)
	}
	waitState(f, "two", "running")
	nativeMu.Lock()
	receiptKey := submitKeys["two"]
	nativeMu.Unlock()
	if receiptKey == "" {
		t.Fatal("missing stable delivery key")
	}
	request(f, "POST", path("two")+"/receipts/"+receiptKey+"/cancel", map[string]any{}, false, 403)
	request(f, "POST", path("one")+"/receipts/"+receiptKey+"/cancel", map[string]any{}, true, 404)
	for range 2 {
		stopped := request(f, "POST", path("two")+"/receipts/"+receiptKey+"/cancel", map[string]any{}, true, 202)
		if stopped["state"] != "cancellation_requested" || stopped["scope"] != "originating_turn" {
			t.Fatal(stopped)
		}
	}
	if row := request(f, "GET", path("two")+"/receipts/"+receiptKey, nil, false, 200); row["cancel_requested"] != true || row["state"] == "cancelled" {
		t.Fatal("in-flight text was treated as cancelled", row)
	}
	policy(f, "two")
	waitState(f, "two", "paused")
	if grants := state(f, "two")["configured_policy"].(map[string]any)["history_threads"]; grants != nil {
		t.Fatal("reply revocation retained history access", grants)
	}
	lateAccepted.Store(true)
	waitAppLifecycle(t, func() bool {
		code, row := f.request("GET", path("two")+"/receipts/"+receiptKey, nil, false)
		return code == 200 && row["state"] == "accepted"
	})
	lifecycle(f, "two", "start", 409)
	request(f, "GET", path("two")+"/receipts/"+receiptKey, nil, false, 200)
	request(f, "GET", path("one")+"/receipts/"+receiptKey, nil, false, 404)
	listed := request(f, "GET", path("two")+"/receipts?limit=1", nil, false, 200)
	items, _ := listed["items"].([]any)
	if len(items) != 1 || listed["coverage"] != "current_worker_checkpoint" || listed["snapshot"] == "" {
		t.Fatal("missing scoped delivery list", listed)
	}
	item := items[0].(map[string]any)
	if item["idempotency_key"] != receiptKey || item["conversation_id"] != "person-two" || item["lookup"] != "stored" {
		t.Fatal("delivery list lost original task identity", listed)
	}
	request(f, "GET", path("two")+"/receipts?limit=0", nil, false, 400)
	request(f, "GET", path("two")+"/receipts?after=../foreign", nil, false, 400)
	request(f, "GET", path("two")+"/receipts?after="+receiptKey, nil, false, 200)
	anonymous, err := http.Get(f.server.URL + path("two") + "/receipts")
	if err != nil {
		t.Fatal(err)
	}
	anonymous.Body.Close()
	if anonymous.StatusCode != 401 {
		t.Fatal("anonymous receipt listing", anonymous.StatusCode)
	}
	// Regranting the same thread requires a new request. The interrupted
	// earlier message must not be regenerated using the new authorization.
	policy(f, "one")
	policy(f, "one", "person-one")
	lifecycle(f, "one", "start", 200)
	waitState(f, "one", "running")
	select {
	case <-blockStarted:
		t.Fatal("revoked source was regenerated on regrant")
	case <-time.After(3 * time.Second):
	}
	// Configure a scoped trigger on a group that was already granted. A
	// pre-change message cannot become eligible when the new worker resumes.
	policy(f, "one", "person-one", room)
	preChange := time.Now()
	request(f, "PUT", path("one")+"/policy", map[string]any{
		"reply_threads": []string{"person-one", room},
		"group_rules": []any{map[string]any{"thread_id": room, "reply_settings": map[string]any{
			"mode": "all", "prefix": "!fish ", "persona": "fixture scoped group persona",
		}}},
	}, true, 200)
	waitState(f, "one", "paused")
	appendMessageAt("one", room, "!fish pre-change-group-message", preChange)
	appendMessageAt("one", room, "wrong-prefix-group-message", time.Now())
	appendMessageAt("one", room, "!fish new-group-message", time.Now())
	lifecycle(f, "one", "start", 200)
	select {
	case prompt := <-groupPrompt:
		if !strings.Contains(prompt, "new-group-message") || strings.Contains(prompt, "pre-change-group-message") {
			t.Fatal("scoped rule did not enforce the activation epoch and exact prefix")
		}
	case <-time.After(14 * time.Second):
		t.Fatal("new scoped group rule did not reach the synthetic model")
	}
	select {
	case <-groupPrompt:
		t.Fatal("pre-change or unprefixed group message triggered another turn")
	case <-time.After(time.Second):
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h2, f2 := reopen()
	waitState(f2, "one", "running")
	waitState(f2, "two", "paused")
	if row := request(f2, "GET", path("two")+"/receipts/"+receiptKey, nil, false, 200); row["cancel_requested"] != true || row["state"] != "accepted" {
		t.Fatal("restart lost cancellation or late accepted receipt", row)
	}
	sender, err := h2.chat.wechat.WorkerSender(t.Context(), wechatapp.ConnectionID("one"))
	if err != nil {
		t.Fatal(err)
	}
	replaced.Store(true)
	if _, err := sender.Status(t.Context()); err == nil {
		t.Fatal("existing worker admitted an account request after daemon replacement")
	}
	// A native outage/replacement cannot prevent a local stop from being saved.
	request(f2, "POST", path("two")+"/receipts/"+receiptKey+"/cancel", map[string]any{}, true, 202)
	// Insert a new synthetic pending turn only after this worker was stopped.
	pausedWorker, err := h2.chat.wechat.worker(wechatapp.ConnectionID("two"), false)
	if err != nil {
		t.Fatal(err)
	}
	queuePath := filepath.Join(pausedWorker.dir, "queue", "messages-v2.json")
	raw, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if json.Unmarshal(raw, &snapshot) != nil {
		t.Fatal("synthetic queue unreadable")
	}
	snapshot["deliveries"] = append(snapshot["deliveries"].([]any), map[string]any{
		"key": "offline-stop-target", "trace": "offline-origin", "thread": "person-two", "state": "prepared",
		"input": map[string]any{"account": "two", "thread": "person-two", "trace_id": "offline-origin"},
	})
	raw, _ = json.Marshal(snapshot)
	if err := os.WriteFile(queuePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	request(f2, "POST", path("two")+"/receipts/offline-stop-target/cancel", map[string]any{}, true, 202)
	stops, err := pausedWorker.readDeliveryStops()
	if err != nil || len(stops) != 2 {
		t.Fatal("new offline stop not persisted", stops, err)
	}
	if page := request(f2, "GET", path("two")+"/receipts", nil, false, 200); len(page["items"].([]any)) != 2 {
		t.Fatal("offline local delivery history unavailable", page)
	}
	if err := h2.Close(); err != nil {
		t.Fatal(err)
	}
	if replacementTraffic.Load() != 0 {
		t.Fatal("worker contacted an unverified account endpoint")
	}
	nativeMu.Lock()
	defer nativeMu.Unlock()
	if foreignReads.Load() != 0 || len(sends["one"]) != 0 || len(sends["two"]) != 1 || sends["two"][0] != "person-two" {
		t.Fatal("conversation or delivery escaped its worker scope", foreignReads.Load(), sends)
	}
}
