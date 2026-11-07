package admin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
)

func (p *permittedTools) callWechatOperation(ctx context.Context, raw json.RawMessage) (string, error) {
	boundRaw := append(json.RawMessage(nil), raw...)
	return wechatapp.OperationStatus(ctx, raw, &wechatOperationHost{p: p, raw: boundRaw})
}

// The adapter gets one immutable operation view; this host selects the only
// permitted native ID and is the only component that can persist or remember.
type wechatOperationHost struct {
	p      *permittedTools
	raw    json.RawMessage
	bound  *agentOperation
	send   *linuxnative.ToolStatus
	action *linuxnative.ActionStatus
}

func (h *wechatOperationHost) Invalid(s string) error { return toolInputError(s) }
func sameWechatOperation(a, b agentOperation) bool {
	if a.ID != b.ID || a.Tool != b.Tool || a.Thread != b.Thread || a.State != b.State || string(a.Arguments) != string(b.Arguments) || a.Result != b.Result || (a.AppAction == nil) != (b.AppAction == nil) {
		return false
	}
	return a.AppAction == nil || *a.AppAction == *b.AppAction
}
func (h *wechatOperationHost) view() wechatapp.Operation {
	o := *h.bound
	target := ""
	if o.Tool == "wechat_forward_file" {
		target, _ = forwardReceiptDescription(o, h.p.trace.Thread)
	}
	return wechatapp.Operation{ID: o.ID, Tool: o.Tool, Thread: o.Thread, Target: target, Arguments: append(json.RawMessage(nil), o.Arguments...), Result: o.Result}
}
func sameWechatView(a, b wechatapp.Operation) bool {
	return a.ID == b.ID && a.Tool == b.Tool && a.Thread == b.Thread && a.Target == b.Target && string(a.Arguments) == string(b.Arguments) && a.Result == b.Result
}
func (h *wechatOperationHost) live() bool {
	if h.bound == nil {
		return false
	}
	a := h.p.manager.agent
	a.mu.Lock()
	now, ok := a.data.Operations[h.bound.ID]
	a.mu.Unlock()
	return ok && sameWechatOperation(now, *h.bound)
}
func (h *wechatOperationHost) Load(_ context.Context, raw json.RawMessage) (wechatapp.Operation, error) {
	if string(raw) != string(h.raw) || h.bound != nil {
		return wechatapp.Operation{}, toolInputError("查询原请求已变化")
	}
	var in struct {
		ID string `json:"operation_id"`
	}
	if json.Unmarshal(raw, &in) != nil || in.ID == "" {
		return wechatapp.Operation{}, toolInputError("操作编号无效")
	}
	a := h.p.manager.agent
	a.mu.Lock()
	op, ok := a.data.Operations[in.ID]
	a.mu.Unlock()
	if !ok || op.ID != in.ID || op.Thread != h.p.trace.Thread {
		return wechatapp.Operation{}, toolInputError("只能查询本会话发起的操作")
	}
	if op.AppAction != nil && (op.AppAction.ConnectionRef != wechatConnectionRef(h.p.manager.native.Account()) || op.AppAction.RequesterID != h.p.trace.Sender) {
		return wechatapp.Operation{}, toolInputError("操作回执不属于当前请求者")
	}
	op.Arguments = append(json.RawMessage(nil), op.Arguments...)
	if op.AppAction != nil {
		intent := *op.AppAction
		op.AppAction = &intent
	}
	h.bound = &op
	return h.view(), nil
}
func (h *wechatOperationHost) Allowed(op wechatapp.Operation) bool {
	if h.bound == nil || !sameWechatView(op, h.view()) {
		return false
	}
	return h.live() && h.p.privileged() && h.p.agentToolEnabled("wechat_operation_status") && h.p.conversationReadAllowed(h.bound.Thread)
}
func (h *wechatOperationHost) ConversationAllowed(thread string) bool {
	return h.p.conversationReadAllowed(thread)
}
func (h *wechatOperationHost) CrossMediaStatus(ctx context.Context) (string, error) {
	if h.bound == nil || h.bound.Tool != "wechat_send_media" || !h.Allowed(h.view()) {
		return "", toolInputError("操作回执不属于当前请求者")
	}
	r, e := h.p.crossMediaStatus(ctx, h.bound.ID)
	if e != nil {
		return "", e
	}
	if !h.Allowed(h.view()) {
		return "", toolInputError("操作回执不属于当前请求者")
	}
	return r, nil
}
func (h *wechatOperationHost) SendStatus(ctx context.Context) (linuxnative.ToolStatus, error) {
	if h.bound == nil || (h.bound.Tool != "wechat_send_text" && h.bound.Tool != "wechat_forward_file") || !h.Allowed(h.view()) {
		return linuxnative.ToolStatus{}, toolInputError("操作回执不属于当前请求者")
	}
	var old linuxnative.ActionStatus
	_ = json.Unmarshal([]byte(h.bound.Result), &old)
	if old.ID == "" {
		return linuxnative.ToolStatus{}, toolInputError("原平台任务编号缺失")
	}
	n, ok := h.p.manager.native.(nativeTools)
	if !ok {
		return linuxnative.ToolStatus{}, toolInputError("后台不支持回执读取")
	}
	s, e := n.ToolSendStatus(ctx, old.ID)
	if e != nil {
		return s, e
	}
	if s.ID != old.ID {
		return linuxnative.ToolStatus{}, toolInputError("发送回执与原平台任务不匹配")
	}
	var original struct {
		Thread string `json:"thread_id"`
	}
	_ = json.Unmarshal(h.bound.Arguments, &original)
	if h.bound.Tool == "wechat_forward_file" {
		original.Thread, _ = forwardReceiptDescription(*h.bound, h.p.trace.Thread)
		var staged struct {
			Source string `json:"source_id"`
		}
		_ = json.Unmarshal([]byte(h.bound.Result), &staged)
		if staged.Source != "" && s.SourceID != "" && staged.Source != s.SourceID {
			return linuxnative.ToolStatus{}, toolInputError("原文件转发回执来源不匹配")
		}
	}
	if s.ThreadID != original.Thread || !h.ConversationAllowed(original.Thread) {
		return linuxnative.ToolStatus{}, toolInputError("回执目标不匹配或授权已取消")
	}
	if !h.Allowed(h.view()) {
		return linuxnative.ToolStatus{}, toolInputError("操作回执不属于当前请求者")
	}
	observed := s
	observed.VoiceChunks = slices.Clone(s.VoiceChunks)
	if s.VoiceFailedOffset != nil {
		offset := *s.VoiceFailedOffset
		observed.VoiceFailedOffset = &offset
	}
	h.send = &observed
	return s, nil
}
func (h *wechatOperationHost) ActionStatus(ctx context.Context) (linuxnative.ActionStatus, error) {
	if h.bound == nil || (h.bound.Tool != "wechat_account_action" && h.bound.Tool != "wechat_recall") || !h.Allowed(h.view()) {
		return linuxnative.ActionStatus{}, toolInputError("操作回执不属于当前请求者")
	}
	var old linuxnative.ActionStatus
	_ = json.Unmarshal([]byte(h.bound.Result), &old)
	if old.ID == "" {
		return linuxnative.ActionStatus{}, toolInputError("原平台任务编号缺失")
	}
	n, ok := h.p.manager.native.(nativeActions)
	if !ok {
		return linuxnative.ActionStatus{}, toolInputError("后台不支持动作查询")
	}
	s, e := n.InspectAction(ctx, old.ID)
	if e != nil {
		return s, e
	}
	if !matchesWechatAction(old, s) || !h.Allowed(h.view()) {
		return linuxnative.ActionStatus{}, toolInputError("账号动作回执与原任务不匹配")
	}
	h.action = &s
	return s, nil
}
func (h *wechatOperationHost) Commit(ctx context.Context, old wechatapp.Operation, result string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.bound == nil || !sameWechatView(old, h.view()) {
		return errors.New("operation_status_binding_changed")
	}
	var out map[string]any
	if json.Unmarshal([]byte(result), &out) != nil || out["operation_id"] != h.bound.ID {
		return errors.New("operation_status_projection_invalid")
	}
	bodyUnverified := h.bound.Tool == "wechat_send_text" && wechatapp.OriginalTextBodyUnverified(h.bound.Result)
	var expected map[string]any
	if h.send != nil {
		status := *h.send
		if h.bound.Tool == "wechat_forward_file" {
			status.Kind = "file"
		}
		b, _ := json.Marshal(status)
		_ = json.Unmarshal(b, &expected)
		if h.bound.Tool == "wechat_forward_file" {
			var before map[string]any
			_ = json.Unmarshal([]byte(h.bound.Result), &before)
			for _, field := range []string{"filename", "source_thread_id", "source_message_id"} {
				if value, exists := before[field]; exists {
					expected[field] = value
				}
			}
		}
	} else if h.action != nil {
		b, _ := json.Marshal(*h.action)
		_ = json.Unmarshal(b, &expected)
	} else {
		return errors.New("operation_status_not_observed")
	}
	if bodyUnverified {
		expected["original_body_unverified"] = true
	}
	expected["operation_id"] = h.bound.ID
	if !reflect.DeepEqual(out, expected) {
		return errors.New("operation_status_projection_not_observed")
	}
	if !h.Allowed(h.view()) || (h.send != nil && !h.ConversationAllowed(h.send.ThreadID)) {
		return toolInputError("操作回执不属于当前请求者")
	}
	a := h.p.manager.agent
	a.mu.Lock()
	current, ok := a.data.Operations[h.bound.ID]
	if !ok || !sameWechatOperation(current, *h.bound) {
		a.mu.Unlock()
		return errors.New("operation_status_changed_during_lookup")
	}
	current.Result = result
	a.data.Operations[current.ID] = current
	e := a.persist()
	if e != nil {
		a.data.Operations[current.ID] = *h.bound
	}
	a.mu.Unlock()
	if e != nil {
		return e
	}
	if h.send != nil && h.send.State == "accepted" && h.send.ServerID != "" && !bodyUnverified && h.ConversationAllowed(h.send.ThreadID) {
		if h.bound.Tool == "wechat_forward_file" {
			thread, text := forwardReceiptDescription(*h.bound, h.p.trace.Thread)
			h.p.rememberAgentSend(thread, text, h.bound.ID)
		} else {
			var in struct {
				Thread string `json:"thread_id"`
				Text   string `json:"text"`
			}
			if json.Unmarshal(h.bound.Arguments, &in) == nil && in.Thread == h.send.ThreadID {
				h.p.rememberAgentSend(in.Thread, in.Text, h.bound.ID)
			}
		}
	}
	return nil
}
