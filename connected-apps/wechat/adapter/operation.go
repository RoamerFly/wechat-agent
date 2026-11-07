package adapter

import (
	"context"
	"encoding/json"
	"errors"

	"wechat-bot/internal/linuxnative"
)

// Operation is an immutable host-owned snapshot.  The adapter deliberately has
// no access to the host ledger, memory store or application manager.
type Operation struct {
	ID        string
	Tool      string
	Thread    string
	Target    string
	Arguments json.RawMessage
	Result    string
}

// OperationHost supplies only the narrowly scoped effects needed for a
// read-only receipt lookup. Load must bind raw to the exact stored operation;
// Commit must compare the supplied previous result before it persists, so a
// late lookup cannot overwrite another completed transition.
type OperationHost interface {
	Invalid(string) error
	Load(context.Context, json.RawMessage) (Operation, error)
	Allowed(Operation) bool
	ConversationAllowed(string) bool
	CrossMediaStatus(context.Context) (string, error)
	SendStatus(context.Context) (linuxnative.ToolStatus, error)
	ActionStatus(context.Context) (linuxnative.ActionStatus, error)
	Commit(context.Context, Operation, string) error
}

// OperationStatus reads an already-bound operation exactly once. It never
// submits or retries a native request. Permission is checked after the native
// read as well as by Load, preventing a revoked late receipt from escaping.
func OperationStatus(ctx context.Context, raw json.RawMessage, host OperationHost) (string, error) {
	var in struct {
		ID string `json:"operation_id"`
	}
	if json.Unmarshal(raw, &in) != nil || in.ID == "" {
		return "", host.Invalid("操作编号无效")
	}
	op, err := host.Load(ctx, raw)
	if err != nil {
		return "", err
	}
	if op.ID != in.ID || !host.Allowed(op) {
		return "", host.Invalid("只能查询当前请求者在本会话发起的操作")
	}
	if op.Tool == "wechat_send_media" {
		result, err := host.CrossMediaStatus(ctx)
		if err != nil {
			return "", err
		}
		if !host.Allowed(op) {
			return "", host.Invalid("操作回执不属于当前请求者")
		}
		return result, nil
	}
	var previous linuxnative.ActionStatus
	_ = json.Unmarshal([]byte(op.Result), &previous)
	if previous.ID == "" {
		return op.Result, nil
	}

	var value any
	var original struct {
		Thread string `json:"thread_id"`
		Text   string `json:"text"`
	}
	_ = json.Unmarshal(op.Arguments, &original)
	if op.Target != "" {
		original.Thread = op.Target
	}
	switch op.Tool {
	case "wechat_send_text", "wechat_forward_file":
		status, err := host.SendStatus(ctx)
		if err != nil {
			return "", err
		}
		if op.Tool == "wechat_forward_file" {
			var staged struct {
				Source        string `json:"source_id"`
				Target        string `json:"target_thread_id"`
				Filename      string `json:"filename"`
				SourceThread  string `json:"source_thread_id"`
				SourceMessage string `json:"source_message_id"`
			}
			_ = json.Unmarshal([]byte(op.Result), &staged)
			if staged.Source != "" && status.SourceID != "" && staged.Source != status.SourceID {
				return "", host.Invalid("原文件转发回执来源不匹配")
			}
			status.Kind = "file"
		}
		if status.ThreadID != original.Thread || !host.ConversationAllowed(original.Thread) || !host.Allowed(op) {
			return "", host.Invalid("回执目标不匹配或授权已取消")
		}
		value = status
	case "wechat_account_action", "wechat_recall":
		status, err := host.ActionStatus(ctx)
		if err != nil {
			return "", err
		}
		if status.ID != previous.ID || (previous.Kind != "" && status.Kind != "" && status.Kind != previous.Kind) {
			return "", host.Invalid("账号动作回执与原任务不匹配")
		}
		if !host.Allowed(op) {
			return "", host.Invalid("操作回执不属于当前请求者")
		}
		value = status
	default:
		return op.Result, nil
	}
	b, _ := json.Marshal(value)
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return "", errors.New("operation_status_projection_invalid")
	}
	if op.Tool == "wechat_forward_file" {
		var metadata map[string]any
		_ = json.Unmarshal([]byte(op.Result), &metadata)
		for _, field := range []string{"filename", "source_thread_id", "source_message_id"} {
			if v, ok := metadata[field]; ok {
				out[field] = v
			}
		}
	}
	if op.Tool == "wechat_send_text" && OriginalTextBodyUnverified(op.Result) {
		out["original_body_unverified"] = true
	}
	out["operation_id"] = op.ID
	b, _ = json.Marshal(out)
	if err := host.Commit(ctx, op, string(b)); err != nil {
		return "", err
	}
	return string(b), nil
}
