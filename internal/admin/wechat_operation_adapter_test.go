package admin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
)

type operationAdapterHost struct {
	op           wechatapp.Operation
	allowed      bool
	conversation bool
	send         linuxnative.ToolStatus
	action       linuxnative.ActionStatus
	remembered   string
	commitOld    string
	stale        bool
}

func (h *operationAdapterHost) Invalid(s string) error { return errors.New(s) }
func (h *operationAdapterHost) Load(_ context.Context, raw json.RawMessage) (wechatapp.Operation, error) {
	var in struct {
		ID string `json:"operation_id"`
	}
	if json.Unmarshal(raw, &in) != nil || in.ID != h.op.ID {
		return wechatapp.Operation{}, errors.New("wrong operation")
	}
	return h.op, nil
}
func (h *operationAdapterHost) Allowed(wechatapp.Operation) bool { return h.allowed }
func (h *operationAdapterHost) ConversationAllowed(string) bool  { return h.conversation }
func (h *operationAdapterHost) CrossMediaStatus(context.Context) (string, error) {
	return `{"operation_id":"media","state":"queued"}`, nil
}
func (h *operationAdapterHost) SendStatus(context.Context) (linuxnative.ToolStatus, error) {
	return h.send, nil
}
func (h *operationAdapterHost) ActionStatus(context.Context) (linuxnative.ActionStatus, error) {
	return h.action, nil
}
func (h *operationAdapterHost) Commit(_ context.Context, old wechatapp.Operation, result string) error {
	h.commitOld = old.Result
	if h.stale {
		return errors.New("operation_status_changed_during_lookup")
	}
	h.op.Result = result
	return nil
}

func TestWechatOperationAdapter(t *testing.T) {
	newHost := func(tool, args, result string) *operationAdapterHost {
		return &operationAdapterHost{op: wechatapp.Operation{ID: "operation-original", Tool: tool, Thread: "current", Arguments: json.RawMessage(args), Result: result}, allowed: true, conversation: true}
	}
	t.Run("text receipt retains bound id and remembers verified original body", func(t *testing.T) {
		h := newHost("wechat_send_text", `{"thread_id":"target","text":"original body"}`, `{"id":"native-original"}`)
		h.send = linuxnative.ToolStatus{ID: "native-original", State: "accepted", ThreadID: "target", ServerID: "server"}
		got, err := wechatapp.OperationStatus(t.Context(), json.RawMessage(`{"operation_id":"operation-original"}`), h)
		if err != nil || h.remembered != "" || h.commitOld != `{"id":"native-original"}` {
			t.Fatalf("result=%s err=%v remembered=%q commit=%q", got, err, h.remembered, h.commitOld)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(got), &out)
		if out["operation_id"] != "operation-original" || out["id"] != "native-original" {
			t.Fatalf("wrong projection: %s", got)
		}
	})
	t.Run("forward receipt carries original file metadata without inventing text", func(t *testing.T) {
		h := newHost("wechat_forward_file", `{"source_thread_id":"source","message_id":"message","target_thread_id":"target"}`, `{"id":"native-original","source_id":"asset","filename":"a.pdf","source_thread_id":"source","source_message_id":"message","target_thread_id":"target"}`)
		h.op.Target = "target"
		h.send = linuxnative.ToolStatus{ID: "native-original", State: "accepted", ThreadID: "target", SourceID: "asset", ServerID: "server"}
		got, err := wechatapp.OperationStatus(t.Context(), json.RawMessage(`{"operation_id":"operation-original"}`), h)
		if err != nil || h.remembered != "" {
			t.Fatalf("result=%s err=%v remembered=%q", got, err, h.remembered)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(got), &out)
		if out["filename"] != "a.pdf" || out["source_message_id"] != "message" || out["kind"] != "file" {
			t.Fatalf("metadata missing: %s", got)
		}
	})
	t.Run("late result cannot overwrite a newer operation", func(t *testing.T) {
		h := newHost("wechat_send_text", `{"thread_id":"target","text":"original"}`, `{"id":"native-original"}`)
		h.stale = true
		h.send = linuxnative.ToolStatus{ID: "native-original", ThreadID: "target"}
		if _, err := wechatapp.OperationStatus(t.Context(), json.RawMessage(`{"operation_id":"operation-original"}`), h); err == nil {
			t.Fatal("late result overwrote operation")
		}
	})
	t.Run("revoked target is not projected", func(t *testing.T) {
		h := newHost("wechat_send_text", `{"thread_id":"target","text":"original"}`, `{"id":"native-original"}`)
		h.conversation = false
		h.send = linuxnative.ToolStatus{ID: "native-original", ThreadID: "target"}
		if _, err := wechatapp.OperationStatus(t.Context(), json.RawMessage(`{"operation_id":"operation-original"}`), h); err == nil {
			t.Fatal("revoked receipt was returned")
		}
	})
}
