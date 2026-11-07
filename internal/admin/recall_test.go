package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type recallNative struct {
	*actionNative
	msg  linuxnative.Message
	last linuxnative.AdminAction
}

func (n *recallNative) Message(context.Context, string) (linuxnative.Message, error) {
	return n.msg, nil
}
func (n *recallNative) ExecuteAction(ctx context.Context, a linuxnative.AdminAction, key string) (linuxnative.ActionStatus, error) {
	n.last = a
	return n.actionNative.ExecuteAction(ctx, a, key)
}

func TestOwnerRecallIsDiscoverableScopedIdempotentAndPollsNative(t *testing.T) {
	f, p := trustedAgent(t)
	c := f.manager.agent.config()
	c.CrossConversation = true
	c.PrivilegedThreads = []string{"*"}
	c.AccountActions = []string{"recall", "rename"}
	if e := f.manager.agent.save(c); e != nil {
		t.Fatal(e)
	}
	f.manager.mu.Lock()
	f.manager.config.GroupRules = []GroupRule{{ID: "room@chatroom", Mode: "mention"}}
	f.manager.mu.Unlock()
	n := &recallNative{actionNative: &actionNative{fakeTools: &fakeTools{fakeNative: f.native}}, msg: linuxnative.Message{ID: "message-one", AccountID: "test", ThreadID: "room@chatroom", Direction: "outgoing", ServerID: "server-message-one"}}
	f.manager.native = n
	if !p.agentToolEnabled("wechat_recall") {
		t.Fatal("hidden recall")
	}
	args := json.RawMessage(`{"thread_id":"room@chatroom","message_id":"message-one","request_key":"recall-one"}`)
	result, e := p.Call(context.Background(), "wechat_recall", args)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Call(context.Background(), "wechat_recall", args); e != nil {
		t.Fatal(e)
	}
	if n.actions != 1 || n.last.ServerID != "server-message-one" || n.last.ThreadID != "room@chatroom" {
		t.Fatal("wrong/repeated recall", n.actions, n.last)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(result), &out)
	poll, _ := json.Marshal(map[string]any{"operation_id": out["operation_id"]})
	result, e = p.Call(context.Background(), "wechat_operation_status", poll)
	if e != nil || !strings.Contains(result, "succeeded") {
		t.Fatal("did not poll native recall", result, e)
	}
	n.msg.Direction = "incoming"
	if _, e = p.Call(context.Background(), "wechat_recall", json.RawMessage(`{"thread_id":"room@chatroom","message_id":"incoming","request_key":"another"}`)); e == nil {
		t.Fatal("recalled other user's message")
	}
	n.msg.Direction = "outgoing"
	c = f.manager.agent.config()
	c.AccountActions = []string{"rename"}
	c.OwnerIDs = append(c.OwnerIDs, "other_owner")
	if e = f.manager.agent.save(c); e != nil {
		t.Fatal(e)
	}
	if p.agentToolEnabled("wechat_recall") || !p.agentToolEnabled("wechat_account_action") {
		t.Fatal("revocation changed the wrong tools")
	}
	for tool, params := range map[string]json.RawMessage{
		"wechat_recall":         args,
		"wechat_account_action": json.RawMessage(`{"kind":"recall","thread_id":"room@chatroom","message_id":"message-one","server_id":"server-message-one"}`),
	} {
		if _, e = p.Call(context.Background(), tool, params); e == nil || n.actions != 1 {
			t.Fatal("revoked recall was submitted", tool, e)
		}
	}
	if result, e = p.Call(context.Background(), "wechat_operation_status", poll); e != nil || !strings.Contains(result, "succeeded") {
		t.Fatal("revocation blocked readback of the existing receipt", result, e)
	}
	f.login()
	code, body, _ := f.request("GET", wechatAppTestPath("test")+"/operations/"+out["operation_id"].(string), nil, true, false)
	if code != 200 || !strings.Contains(string(body), `"state":"succeeded"`) || strings.Contains(string(body), "wxid_owner") {
		t.Fatal("app action query did not project a scoped receipt", code, string(body))
	}
	persisted, e := newAgentControl(f.manager.dir)
	if e != nil {
		t.Fatal(e)
	}
	op := persisted.data.Operations[out["operation_id"].(string)]
	if op.AppAction == nil || op.AppAction.RequesterID != "wxid_owner" || op.AppAction.Target != "room@chatroom" ||
		op.AppAction.SourceThread != "wxid_owner" || op.AppAction.SourceRequestID != "turn-1" ||
		op.AppAction.ConnectionRef != wechatConnectionRef("test") || op.AppAction.IdempotencyKey != agentID("test", "room@chatroom", "recall-one", "recall") {
		t.Fatalf("original action scope was not persisted: %+v", op.AppAction)
	}
	p.SetTrace(companion.Input{Account: "test", Thread: "wxid_owner", Sender: "other_owner"})
	if _, e = p.Call(context.Background(), "wechat_operation_status", poll); e == nil {
		t.Fatal("another trusted sender could read the original requester's action")
	}
	p.SetTrace(companion.Input{Account: "test", Thread: "room@chatroom", Sender: "ordinary"})
	if p.agentToolEnabled("wechat_recall") {
		t.Fatal("member got owner recall")
	}
	if _, e = p.Call(context.Background(), "wechat_recall", args); e == nil {
		t.Fatal("member recalled")
	}
}

func TestGroupOwnerHasPrivateVoiceButNoIncompatibleDefaultSubmission(t *testing.T) {
	f, p, _, _ := voiceAgentFixture(t)
	c := f.manager.agent.config()
	c.PrivilegedThreads = []string{"*"}
	if e := f.manager.agent.save(c); e != nil {
		t.Fatal(e)
	}
	f.manager.mu.Lock()
	f.manager.config.GroupRules = []GroupRule{{ID: "room@chatroom", Mode: "mention"}}
	f.manager.mu.Unlock()
	p.SetTrace(companion.Input{Account: "test", Thread: "room@chatroom", Sender: "wxid_owner", TraceID: "group-voice"})
	if result, e := p.Call(context.Background(), "voice_library_read", json.RawMessage(`{"voice_id":"owner-voice"}`)); e != nil || !strings.Contains(result, "engine_compatibility") {
		t.Fatal(result, e)
	}
	if result, e := p.Call(context.Background(), "voice_library_prepare", json.RawMessage(`{"voice_id":"owner-voice"}`)); e != nil || !strings.Contains(result, `"global":true`) {
		t.Fatal(result, e)
	}
	_, e := p.startRVCCover(context.Background(), json.RawMessage(`{"title":"test","reference_asset_id":"original","voice_id":"owner-voice"}`))
	if e == nil || !strings.Contains(e.Error(), "selected_voice_engine_not_rvc") {
		t.Fatal("silently substituted default voice", e)
	}
	cfg, e := p.scopedHostConfig(json.RawMessage(`{"global":true,"gpu":true}`))
	if e != nil || !cfg.Isolation.GPU {
		t.Fatal("GPU scope not granted", e)
	}
	p.trace.Sender = "member"
	if _, e = p.Call(context.Background(), "voice_library_read", json.RawMessage(`{"voice_id":"owner-voice"}`)); e == nil {
		t.Fatal("private voice leaked")
	}
}

func TestTechnicalDiagnosticQueuesOnePrivateReport(t *testing.T) {
	f, p := trustedAgent(t)
	args := json.RawMessage(`{"text":"synthetic diagnostic: GPU mount needed"}`)
	for i := 0; i < 2; i++ {
		if _, e := p.Call(context.Background(), "agent_owner_report", args); e != nil {
			t.Fatal(e)
		}
	}
	tasks := f.manager.agent.list("test", "wxid_owner")
	if len(tasks) != 1 || !tasks[0].ReportOnly || tasks[0].Thread != "wxid_owner" {
		t.Fatal("private report missing or duplicated", tasks)
	}
}

func TestGroupInitialContextDoesNotLoadPrivateChatOrTasks(t *testing.T) {
	f, p := trustedAgent(t)
	f.manager.mu.Lock()
	f.manager.config.Companion.Permissions.Remember = true
	f.manager.mu.Unlock()
	scope, e := p.workspaceScope(false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.manager.workspace.Write(scope, "MEMORY.md", "synthetic-private-chat-only-token", ""); e != nil {
		t.Fatal(e)
	}
	if e = f.manager.agent.task(agentTask{ID: agentID("private-task"), Account: "test", Thread: "wxid_owner", State: "waiting", Checkpoint: "synthetic-private-task-only-token"}); e != nil {
		t.Fatal(e)
	}
	cfg := f.manager.agent.config()
	cfg.PrivilegedThreads = []string{"*"}
	if e = f.manager.agent.save(cfg); e != nil {
		t.Fatal(e)
	}
	f.manager.mu.Lock()
	f.manager.config.GroupRules = []GroupRule{{ID: "room@chatroom", Mode: "mention"}}
	f.manager.mu.Unlock()
	p.SetTrace(companion.Input{Channel: "wechat", Account: "test", Thread: "room@chatroom", Sender: "wxid_owner"})
	ctx := p.AgentContext(context.Background())
	tasks, e := p.Call(context.Background(), "agent_tasks", json.RawMessage(`{}`))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(ctx+tasks, "synthetic-private-") {
		t.Fatal("private conversation automatically loaded in group")
	}
	if !strings.Contains(ctx, "workspace_policy") || !strings.Contains(ctx, "requester_is_trusted_owner") {
		t.Fatal("missing scoped workspace authorization context")
	}
}
