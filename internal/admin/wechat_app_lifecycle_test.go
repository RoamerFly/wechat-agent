package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/linuxnative"
)

func TestApplicationConnectionStopKeepsLoginReceiptsAndOriginalQueue(t *testing.T) {
	var sends, accountActions, queueLookups, logouts atomic.Int32
	f, registry := setupAccountsWithNativeRoutes(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/accounts/second/sends":
			sends.Add(1)
			respond(w, 202, map[string]string{"id": "manual-original", "accountId": "second", "threadId": "filehelper", "state": "queued"})
		case r.Method == "GET" && r.URL.Path == "/v1/accounts/second/sends/by-idempotency-key" && r.URL.Query().Get("key") == "manual-original-key":
			respond(w, 200, map[string]string{"id": "manual-original", "accountId": "second", "threadId": "filehelper", "state": "accepted", "serverId": "730"})
		case r.Method == "GET" && r.URL.Path == "/v1/accounts/second/sends/queued-original":
			queueLookups.Add(1)
			respond(w, 200, map[string]string{"id": "queued-original", "accountId": "second", "threadId": "filehelper", "state": "accepted", "serverId": "731"})
		case r.Method == "PATCH" && r.URL.Path == "/v1/accounts/second/groups/room@chatroom":
			accountActions.Add(1)
			respond(w, 202, map[string]string{"id": "would-change", "accountId": "second", "state": "running"})
		case r.Method == "POST" && (strings.HasSuffix(r.URL.Path, "/logout") || strings.HasSuffix(r.URL.Path, "/forget-credentials")):
			logouts.Add(1)
			http.Error(w, "unexpected login mutation", 400)
		default:
			return false
		}
		return true
	})
	second := registry.lookup("second").manager
	for _, m := range []*Manager{f.manager, second} {
		cfg := allowAppTestThread(t, m)
		cfg.APIKey = "synthetic-model-key-not-real-0001"
		cfg.Enabled = true
		if _, err := m.Save(ConfigInput{Config: cfg}); err != nil {
			t.Fatal(err)
		}
	}
	waitAppLifecycle(t, func() bool { return f.manager.BotStatus().State == "running" && second.BotStatus().State == "running" })
	path := wechatAppTestPath("second")
	manual := linuxnative.ToolSend{Kind: "text", ThreadID: "filehelper", Text: "synthetic original text", IdempotencyKey: "manual-original-key"}
	if code, body, _ := f.request("POST", "/api/tools/send?account=second", manual, true, true); code != http.StatusAccepted {
		t.Fatalf("original manual send failed: %d %s", code, body)
	}
	draft := linuxnative.AdminAction{Kind: "rename", ThreadID: "room@chatroom", Text: "synthetic change"}
	code, body, _ := f.request("POST", "/api/actions/prepare?account=second", draft, true, true)
	var proposal wechatapp.ActionProposal
	if code != http.StatusCreated || json.Unmarshal(body, &proposal) != nil || proposal.ID == "" {
		t.Fatalf("draft unavailable: %d %s", code, body)
	}
	transition := func(action, worker string, paused bool) {
		t.Helper()
		code, body, _ := f.request("POST", path+"/lifecycle?account=test", map[string]string{"action": action}, true, true)
		var v apphost.ConnectionLifecycle
		if code != http.StatusOK || json.Unmarshal(body, &v) != nil || v.ConnectionRef != wechatConnectionRef("second") ||
			v.SubmissionsPaused != paused || !v.WorkerEnabled || (worker != "" && v.Worker != worker) {
			t.Fatalf("wrong lifecycle result: %d %s", code, body)
		}
	}
	if code, _, _ := f.request("POST", path+"/lifecycle", map[string]string{"action": "stop"}, true, false); code != http.StatusForbidden {
		t.Fatal("lifecycle write did not require CSRF", code)
	}
	transition("stop", "paused", true)
	transition("stop", "paused", true)
	if f.manager.BotStatus().State != "running" || logouts.Load() != 0 {
		t.Fatal("stopping one connection changed its peer or native login")
	}
	code, body, _ = f.request("GET", path, nil, true, false)
	var detail struct{ Connection apphost.ConnectionStatus }
	if code != http.StatusOK || json.Unmarshal(body, &detail) != nil || !detail.Connection.SubmissionsPaused ||
		detail.Connection.Login != "authenticated" || detail.Connection.Runtime != "online" {
		t.Fatalf("native login and host pause were conflated: %d %s", code, body)
	}
	manual.IdempotencyKey = "blocked-key"
	if code, _, _ := f.request("POST", "/api/tools/send?account=second", manual, true, true); code != http.StatusConflict {
		t.Fatal("legacy send bypassed connection pause", code)
	}
	if code, _, _ := f.request("POST", "/api/actions/"+proposal.ID+"/approve?account=second", map[string]string{"confirm": proposal.ID}, true, true); code != http.StatusConflict || accountActions.Load() != 0 {
		t.Fatal("account action bypassed connection pause", code)
	}
	if code, _, _ := f.request("POST", "/api/actions/"+proposal.ID+"/cancel?account=second", map[string]any{}, true, true); code != http.StatusOK {
		t.Fatal("connection pause blocked cancellation of a draft", code)
	}
	code, body, _ = f.request("GET", path+"/receipts/manual-original-key", nil, true, false)
	var receipt apphost.Receipt
	if code != http.StatusOK || json.Unmarshal(body, &receipt) != nil || receipt.State != "accepted" || receipt.ServerID != "730" || sends.Load() != 1 {
		t.Fatalf("receipt unavailable or replayed while paused: %d %s", code, body)
	}
	cfg := second.Config().Config
	cfg.ConnectionPaused = false // Old clients may omit the host-owned field.
	if _, err := second.Save(ConfigInput{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	onDisk, err := loadConfig(second.dir)
	if err != nil || !onDisk.ConnectionPaused || second.BotStatus().State != "paused" {
		t.Fatal("config round trip cleared durable pause", err)
	}
	if code, _, _ := f.request("POST", "/api/bot/start?account=second", map[string]any{}, true, true); code != http.StatusBadRequest {
		t.Fatal("old start path could bypass app pause", code)
	}
	code, body, _ = f.request("POST", "/api/runtime/drain", map[string]any{}, true, true)
	var lease struct{ Ticket string }
	if code != http.StatusOK || json.Unmarshal(body, &lease) != nil || lease.Ticket == "" {
		t.Fatalf("drain unavailable: %d %s", code, body)
	}
	if code, _, _ := f.request("POST", path+"/lifecycle", map[string]string{"action": "start"}, true, true); code != http.StatusConflict {
		t.Fatal("lifecycle write cleared maintenance barrier", code)
	}
	if code, _, _ := f.request("POST", "/api/runtime/undrain", map[string]string{"ticket": lease.Ticket}, true, true); code != http.StatusOK {
		t.Fatal("global drain did not release", code)
	}
	if !second.connectionPaused() || second.BotStatus().State != "paused" {
		t.Fatal("releasing the updater resumed a manually paused connection")
	}
	if err := os.MkdirAll(second.stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	const checkpoint = `{"version":1,"server":"synthetic-server","account":"second","inbox":[],"deliveries":[{"trace":"original-trace","thread":"filehelper","text":"synthetic pending text","submitted_text":"synthetic pending text","key":"queued-original-key","id":"queued-original","state":"waiting","input":{"account":"second","thread":"filehelper","channel":"wechat"}}]}`
	if err := writePrivate(filepath.Join(second.stateDir, "messages-v2.json"), []byte(checkpoint)); err != nil {
		t.Fatal(err)
	}
	transition("start", "", false)
	waitAppLifecycle(t, func() bool {
		rows, err := bot.ReadDeliveryEvidence(second.stateDir, "second", []string{"queued-original-key"})
		return err == nil && second.BotStatus().State == "running" && queueLookups.Load() > 0 &&
			rows["queued-original-key"].Status.State == "accepted"
	})
	transition("start", "running", false)
	code, body, _ = f.request("GET", path+"/receipts/queued-original-key", nil, true, false)
	if code != http.StatusOK || json.Unmarshal(body, &receipt) != nil || receipt.State != "accepted" ||
		receipt.ServerID != "731" || receipt.OperationID != "queued-original" || sends.Load() != 1 {
		t.Fatalf("original worker receipt not recovered: %d %s", code, body)
	}
}

func TestApplicationConnectionPausePersistsAcrossManagerRestart(t *testing.T) {
	root := t.TempDir()
	native := &fakeNative{}
	m, err := NewManager(root, filepath.Join(root, "inbox"), native)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if m != nil {
			_ = m.Close()
		}
	}()
	if err := m.setConnectionRunning(t.Context(), true); !errors.Is(err, apphost.ErrLifecycleNotReady) {
		t.Fatal("app start bypassed existing setup requirements", err)
	}
	cfg := m.runtimeConfig()
	cfg.Enabled = true
	cfg.APIKey = "synthetic-model-key-not-real-0001"
	if _, err := m.Save(ConfigInput{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if err := m.setConnectionRunning(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = NewManager(root, filepath.Join(root, "inbox"), native)
	if err != nil {
		t.Fatal(err)
	}
	if m.BotStatus().State != "paused" || !m.Config().Enabled || m.done != nil || native.loginCalls.Load() != 0 {
		t.Fatal("restart discarded host pause or mutated the native login")
	}
}

func waitAppLifecycle(t *testing.T, ready func() bool) {
	t.Helper()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ready() {
			return
		}
	}
	t.Fatal("expected connection lifecycle state not reached")
}
