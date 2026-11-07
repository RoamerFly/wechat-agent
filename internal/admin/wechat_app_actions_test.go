package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

type actionDuringReadNative struct {
	*recallNative
	duringRead func()
}

func (n *actionDuringReadNative) Message(ctx context.Context, id string) (linuxnative.Message, error) {
	n.duringRead()
	return n.recallNative.Message(ctx, id)
}

func TestApplicationActionRechecksGrantAfterNativeMessageRead(t *testing.T) {
	f, p := trustedAgent(t)
	f.login()
	settings := f.manager.agent.config()
	settings.AccountActions = []string{"recall"}
	if err := f.manager.agent.save(settings); err != nil {
		t.Fatal(err)
	}
	native := &actionDuringReadNative{
		recallNative: &recallNative{actionNative: &actionNative{fakeTools: &fakeTools{fakeNative: f.native}},
			msg: linuxnative.Message{ID: "original", AccountID: "test", ThreadID: "wxid_owner", Direction: "outgoing", ServerID: "112"}},
		duringRead: func() {
			settings := f.manager.agent.config()
			settings.AccountActions = nil
			code, body, _ := f.request("POST", "/api/agent/config", settings, true, true)
			if code != http.StatusOK {
				t.Fatalf("revoke failed: %d %s", code, body)
			}
		},
	}
	f.manager.native = native
	raw := json.RawMessage(`{"message_id":"original","request_key":"same-request"}`)
	if _, err := p.Call(t.Context(), "wechat_recall", raw); err == nil || native.actions != 0 {
		t.Fatalf("stale permission dispatched a mutation: actions=%d err=%v", native.actions, err)
	}
	if len(f.manager.agent.operations()) != 0 {
		t.Fatal("denied operation was claimed as if submitted")
	}
}

func TestApplicationActionReadsExactConnectionAndKeepsCancelledDrafts(t *testing.T) {
	var submits, probes, wrongID, dropResponse atomic.Int32
	f, registry := setupAccountsWithNativeRoutes(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == "PATCH" && r.URL.Path == "/v1/accounts/second/groups/room@chatroom":
			submits.Add(1)
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("mutation lacked original claim key")
			}
			if dropResponse.Load() != 0 {
				http.Error(w, "synthetic lost acknowledgement", http.StatusBadGateway)
				return true
			}
			respond(w, 202, map[string]string{"id": "native-action", "accountId": "second", "state": "running", "kind": "rename"})
			return true
		case r.Method == "GET" && r.URL.Path == "/v1/accounts/second/operations/native-action":
			probes.Add(1)
			id := "native-action"
			if wrongID.Load() != 0 {
				id = "unrelated-action"
			}
			respond(w, 200, map[string]any{"id": id, "accountId": "second", "state": "succeeded", "kind": "rename", "result": map[string]string{"code": "confirmed"}})
			return true
		}
		return false
	})
	m := registry.lookup("second").manager
	cfg := allowAppTestThread(t, m)
	prepare := func() string {
		t.Helper()
		in := linuxnative.AdminAction{Kind: "rename", ThreadID: "room@chatroom", Text: "synthetic new name"}
		code, body, _ := f.request("POST", "/api/actions/prepare?account=second", in, true, true)
		var v wechatapp.ActionProposal
		if code != http.StatusCreated || json.Unmarshal(body, &v) != nil || v.AppAction != nil || v.ID == "" {
			t.Fatalf("draft not prepared: %d %s", code, body)
		}
		return v.ID
	}
	read := func(id, state, lookup string) apphost.ActionReceipt {
		t.Helper()
		code, body, _ := f.request("GET", wechatAppTestPath("second")+"/operations/"+id+"?account=test", nil, true, false)
		var v apphost.ActionReceipt
		if code != http.StatusOK || json.Unmarshal(body, &v) != nil || v.State != state || v.Lookup != lookup || v.OperationID != id ||
			v.ConnectionRef != wechatConnectionRef("second") || v.Action != "rename" || v.Target != "room@chatroom" {
			t.Fatalf("invalid action receipt: %d %s", code, body)
		}
		return v
	}
	approve := func(id string) (int, []byte) {
		t.Helper()
		code, body, _ := f.request("POST", "/api/actions/"+id+"/approve?account=second", map[string]string{"confirm": id}, true, true)
		return code, body
	}
	submitted, draft, unknown := prepare(), prepare(), prepare()
	requireAppLogin(t, f, wechatAppTestPath("second")+"/operations/"+draft)
	read(draft, "not_submitted", "stored")
	if code, body := approve(submitted); code != http.StatusAccepted {
		t.Fatalf("approve failed: %d %s", code, body)
	}
	dropResponse.Store(1)
	if code, body := approve(unknown); code != http.StatusAccepted {
		t.Fatalf("unknown approval failed: %d %s", code, body)
	}
	if code, _ := approve(unknown); code != http.StatusOK || submits.Load() != 2 {
		t.Fatal("unknown approval was replayed", code, submits.Load())
	}
	read(unknown, "submission_unknown", "stored")
	cfg.GroupRules = nil
	if _, err := m.Save(ConfigInput{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if code, _ := approve(draft); code != http.StatusForbidden || submits.Load() != 2 {
		t.Fatal("revoked destination allowed new submission", code)
	}
	if code, _, _ := f.request("GET", wechatAppTestPath("test")+"/operations/"+submitted, nil, true, false); code != http.StatusNotFound || probes.Load() != 0 {
		t.Fatal("another connection could inspect a receipt", code)
	}
	wrongID.Store(1)
	read(submitted, "submitted_pending", "identity_mismatch")
	wrongID.Store(0)
	if receipt := read(submitted, "succeeded", "platform"); receipt.PlatformCode != "confirmed" || receipt.PlatformOperationID != "native-action" {
		t.Fatal("platform confirmation missing", receipt)
	}
	if code, _, _ := f.request("POST", "/api/actions/"+draft+"/cancel?account=second", map[string]any{}, true, true); code != http.StatusOK {
		t.Fatal("revocation prevented cancelling a pending draft", code)
	}
	read(draft, "cancelled", "stored")
	saved, ok := m.actions.Read(submitted, "second")
	if !ok || saved.AppAction == nil || *saved.AppAction != ownerActionIntent(saved) || submits.Load() != 2 || probes.Load() != 2 {
		t.Fatal("scope or operation count changed", saved.AppAction, submits.Load(), probes.Load())
	}
}
