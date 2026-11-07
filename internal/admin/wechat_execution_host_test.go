package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

func TestWechatExecutionHostBindsOriginalTextClaimAndReceipt(t *testing.T) {
	f, p := trustedAgent(t)
	defer p.hub.Close()
	native := &crossTextNative{&fakeTools{fakeNative: f.native}}
	f.manager.native = native
	settings := f.manager.agent.config()
	settings.CrossConversation = true
	if err := f.manager.agent.save(settings); err != nil {
		t.Fatal(err)
	}
	f.manager.mu.Lock()
	f.manager.config.Behavior.RandomDelay = false
	f.manager.mu.Unlock()
	observeOutboundSource(t, f, p, "发送合成结果")
	raw := json.RawMessage(`{"thread_id":"wxid_owner","text":"合成结果","request_key":"bound-key"}`)
	h := &wechatTextHost{p: p, raw: raw}
	if _, err := h.Prepare(t.Context(), "filehelper", "合成结果"); err == nil {
		t.Fatal("adapter retargeted original request")
	}
	prepared, err := h.Prepare(t.Context(), "wxid_owner", "合成结果")
	if err != nil {
		t.Fatal(err)
	}
	request := linuxnative.ToolSend{ThreadID: "wxid_owner", Kind: "text", Text: prepared.Text, IdempotencyKey: p.operationID("wechat_send_text", raw), Quote: prepared.Quote}
	sender := h.Sender()
	if _, err := sender.LookupSend(t.Context(), request.IdempotencyKey); err == nil {
		t.Fatal("unclaimed application queried receipt")
	}
	if _, err := sender.Submit(t.Context(), request); err == nil || native.submits.Load() != 0 {
		t.Fatal("submission bypassed durable claim", err)
	}
	result, err := h.Claim(t.Context(), raw, func() (any, error) {
		if _, err := sender.LookupSend(t.Context(), "another-operation-key"); err == nil {
			t.Fatal("adapter read another operation")
		}
		if err := h.BeforeSubmit(t.Context(), request.ThreadID); err != nil {
			t.Fatal(err)
		}
		changed := request
		changed.IdempotencyKey = "new-key"
		if _, err := sender.Submit(t.Context(), changed); err == nil || native.submits.Load() != 0 {
			t.Fatal("original key was replaced", err)
		}
		p.trace.TaskID = "cancelled-or-missing-task"
		if _, err := sender.Submit(t.Context(), request); err == nil || native.submits.Load() != 0 {
			t.Fatal("cancelled task submitted", err)
		}
		p.trace.TaskID = ""
		status, err := sender.Submit(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sender.Submit(t.Context(), request); err == nil || native.submits.Load() != 1 {
			t.Fatal("second submission inside original claim", err)
		}
		return status, nil
	})
	if err != nil || !strings.Contains(result, "queued") || native.submits.Load() != 1 {
		t.Fatal(result, err)
	}
	if _, err := h.Claim(t.Context(), raw, func() (any, error) { t.Fatal("completed original claim replayed"); return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

type recoveredTextNative struct {
	*fakeTools
	status linuxnative.ToolStatus
}

func (n recoveredTextNative) LookupSend(context.Context, string) (linuxnative.ToolStatus, error) {
	return n.status, nil
}

func (n recoveredTextNative) ToolSendStatus(context.Context, string) (linuxnative.ToolStatus, error) {
	return n.status, nil
}

func TestWechatExecutionHostRecoversReceiptWithoutInventingLostBody(t *testing.T) {
	f, p := trustedAgent(t)
	defer p.hub.Close()
	n := recoveredTextNative{fakeTools: &fakeTools{fakeNative: f.native}, status: linuxnative.ToolStatus{ID: "original-platform-job", ThreadID: "wxid_owner", Kind: "text", State: "accepted", ServerID: "123"}}
	f.manager.native = n
	f.manager.mu.Lock()
	f.manager.config.Companion.Permissions.Remember = true
	f.manager.mu.Unlock()
	settings := f.manager.agent.config()
	settings.CrossConversation = true
	if err := f.manager.agent.save(settings); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"thread_id":"wxid_owner","text":"current arguments are not proof of old body","request_key":"original-key"}`)
	result, err := p.Call(t.Context(), "wechat_send_text", raw)
	if err != nil || !strings.Contains(result, `"state":"accepted"`) || n.submits.Load() != 0 {
		t.Fatal(result, err)
	}
	operations := f.manager.agent.operations()
	if len(operations) != 1 || !strings.Contains(operations[0].Result, "original-platform-job") {
		t.Fatal("receipt not persisted", operations)
	}
	statusArgs, err := json.Marshal(map[string]string{"operation_id": operations[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		polled, err := p.Call(t.Context(), "wechat_operation_status", statusArgs)
		if err != nil || !strings.Contains(polled, `"original_body_unverified":true`) {
			t.Fatal("poll dropped the original receipt evidence boundary", polled, err)
		}
	}
	found := false
	for _, row := range f.manager.Logs() {
		if row.Reason == "original_receipt_recovered_body_unverified" {
			found = true
			if row.Content != "" || row.Status != "accepted" || row.SendID != "original-platform-job" {
				t.Fatal(row)
			}
		}
	}
	if !found {
		t.Fatal("recovered receipt missing audit")
	}
	ctx := f.manager.memory.Context(companion.Input{Channel: "wechat", Account: "test", Thread: "wxid_owner"}, f.manager.Config().Companion, time.Now())
	for _, message := range ctx.Messages {
		if strings.Contains(message.Text, "current arguments are not proof") {
			t.Fatal("current draft became invented old delivery", message)
		}
	}
}

func TestWechatExecutionHostCannotRetargetOrInventRecallEvidence(t *testing.T) {
	f, p := trustedAgent(t)
	defer p.hub.Close()
	settings := f.manager.agent.config()
	settings.AccountActions = []string{"recall", "rename"}
	if err := f.manager.agent.save(settings); err != nil {
		t.Fatal(err)
	}
	native := &recallNative{actionNative: &actionNative{fakeTools: &fakeTools{fakeNative: f.native}}, msg: linuxnative.Message{ID: "original", AccountID: "test", ThreadID: "wxid_owner", Direction: "outgoing", ServerID: "321"}}
	f.manager.native = native
	h := &wechatActionHost{p: p, tool: "wechat_recall", raw: json.RawMessage(`{"message_id":"original","request_key":"recall-original"}`)}
	action := linuxnative.AdminAction{Kind: "recall", ThreadID: "wxid_owner", MessageID: "original"}
	changedRequest := action
	changedRequest.MessageID = "another-message"
	if _, err := h.AuthorizeAction(t.Context(), changedRequest, true); err == nil {
		t.Fatal("adapter replaced original recall request")
	}
	submit, err := h.AuthorizeAction(t.Context(), action, true)
	if err != nil {
		t.Fatal(err)
	}
	action.ServerID = "321"
	if _, err := submit(t.Context(), action); err == nil || native.actions != 0 {
		t.Fatal("application invented recall evidence", err)
	}
	if _, err := h.ReadMessage(t.Context(), "different"); err == nil {
		t.Fatal("reader escaped bound message")
	}
	if _, err := h.ReadMessage(t.Context(), "original"); err != nil {
		t.Fatal(err)
	}
	changed := action
	changed.ThreadID = "filehelper"
	if _, err := submit(t.Context(), changed); err == nil || native.actions != 0 {
		t.Fatal("recall target changed", err)
	}
	changed = action
	changed.ServerID = "999"
	if _, err := submit(t.Context(), changed); err == nil || native.actions != 0 {
		t.Fatal("recall server ID changed", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := submit(ctx, action); err == nil || native.actions != 0 {
		t.Fatal("cancelled context submitted", err)
	}
}
