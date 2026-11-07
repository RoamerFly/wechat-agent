package admin

import (
	"encoding/json"
	"testing"

	"wechat-bot/internal/linuxnative"
)

func TestWechatOperationHostRejectsForgedReceiptsAndLateState(t *testing.T) {
	f, p := trustedAgent(t)
	defer p.hub.Close()
	status := linuxnative.ToolStatus{ID: "native-original", ThreadID: p.trace.Thread, Kind: "text", State: "accepted", ServerID: "321"}
	f.manager.native = &deliveryToolNative{fakeTools: &fakeTools{fakeNative: f.native}, status: status}
	args, _ := json.Marshal(map[string]string{"thread_id": p.trace.Thread, "text": "synthetic original body"})
	wantArgs := string(args)
	id := agentID("operation-status-original")
	old := agentOperation{ID: id, Tool: "wechat_send_text", Thread: p.trace.Thread, Arguments: args, Result: `{"id":"native-original","state":"queued"}`, State: "completed"}
	f.manager.agent.mu.Lock()
	f.manager.agent.data.Operations[id] = old
	f.manager.agent.mu.Unlock()
	raw, _ := json.Marshal(map[string]string{"operation_id": id})
	h := &wechatOperationHost{p: p, raw: raw}
	view, err := h.Load(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	view.Arguments[0] = 'x'
	f.manager.agent.mu.Lock()
	storedArgs := string(f.manager.agent.data.Operations[id].Arguments)
	f.manager.agent.mu.Unlock()
	if storedArgs != wantArgs {
		t.Fatal("application view mutated stored arguments")
	}
	view = h.view()
	if _, err := h.SendStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(status)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	result["operation_id"] = id
	for _, field := range []string{"server_id", "source_id", "extra"} {
		forged := map[string]any{}
		for k, v := range result {
			forged[k] = v
		}
		forged[field] = "forged"
		body, _ := json.Marshal(forged)
		if err := h.Commit(t.Context(), view, string(body)); err == nil {
			t.Fatal("application forged observed receipt", field)
		}
	}
	// Cancellation can change State without rewriting a result. A late receipt
	// must not replace that newer host transition.
	f.manager.agent.mu.Lock()
	changed := old
	changed.State = "cancelled"
	f.manager.agent.data.Operations[id] = changed
	f.manager.agent.mu.Unlock()
	body, _ := json.Marshal(result)
	if err := h.Commit(t.Context(), view, string(body)); err == nil {
		t.Fatal("late result overwrote cancelled operation")
	}
	f.manager.agent.mu.Lock()
	current := f.manager.agent.data.Operations[id]
	f.manager.agent.mu.Unlock()
	if current.State != "cancelled" || current.Result != old.Result {
		t.Fatal(current)
	}
}
