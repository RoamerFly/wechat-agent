package admin

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/linuxnative"
)

// Application execution receives request-bound services, never the Manager,
// credentials, memory database or operation ledger itself.
type wechatActionHost struct {
	p              *permittedTools
	tool           string
	raw            json.RawMessage
	bound          *linuxnative.AdminAction
	checkedMessage *linuxnative.Message
}

func (h wechatActionHost) Account() string              { return h.p.trace.Account }
func (h wechatActionHost) Thread() string               { return h.p.trace.Thread }
func (h wechatActionHost) Owner() bool                  { return h.p.privileged() }
func (h wechatActionHost) ValidID(id string) bool       { return validID(id) }
func (h wechatActionHost) Invalid(message string) error { return toolInputError(message) }
func (h *wechatActionHost) ReadMessage(ctx context.Context, id string) (linuxnative.Message, error) {
	if h.bound == nil || h.bound.Kind != "recall" || h.bound.MessageID != id {
		return linuxnative.Message{}, toolInputError("撤回来源不属于当前授权动作")
	}
	msg, err := h.p.manager.native.Message(ctx, id)
	if err == nil && wechatapp.OwnMessage(msg, h.Account(), h.bound.ThreadID, id) {
		h.checkedMessage = &msg
	}
	return msg, err
}

func (h *wechatActionHost) AuthorizeAction(ctx context.Context, action linuxnative.AdminAction, cross bool) (wechatapp.ActionSubmission, error) {
	p := h.p
	var expected linuxnative.AdminAction
	if h.tool == "wechat_recall" && cross {
		var original struct {
			Thread  string `json:"thread_id"`
			Message string `json:"message_id"`
			Key     string `json:"request_key"`
		}
		if json.Unmarshal(h.raw, &original) != nil || !validID(original.Key) {
			return nil, toolInputError("撤回原请求无效")
		}
		if original.Thread == "" {
			original.Thread = p.trace.Thread
		}
		expected = linuxnative.AdminAction{Kind: "recall", ThreadID: original.Thread, MessageID: original.Message}
	} else if h.tool == "wechat_account_action" && !cross {
		if json.Unmarshal(h.raw, &expected) != nil {
			return nil, toolInputError("账号动作原请求无效")
		}
	} else {
		return nil, toolInputError("账号动作入口不匹配")
	}
	if !reflect.DeepEqual(action, expected) {
		return nil, toolInputError("账号动作不属于原请求")
	}
	settings := p.manager.agent.config()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.wechatActionAllowed(action, cross, settings) {
		if cross {
			return nil, toolInputError("recall_target_not_authorized")
		}
		return nil, toolInputError("账号动作未授权或参数无效")
	}
	// Copy the selected IDs before invoking any reader. The returned closure is
	// restricted to this action; only an exact recall can resolve its server ID.
	bound := action
	bound.Members = slices.Clone(action.Members)
	h.bound, h.checkedMessage = &bound, nil
	key := agentID(p.trace.Account, p.trace.TraceID, h.tool, string(h.raw))
	if cross {
		var in struct {
			Key string `json:"request_key"`
		}
		_ = json.Unmarshal(h.raw, &in)
		key = agentID(p.trace.Account, action.ThreadID, in.Key, "recall")
	}
	return func(ctx context.Context, verified linuxnative.AdminAction) (string, error) {
		compare := verified
		if cross && bound.Kind == "recall" {
			compare.ServerID = bound.ServerID
		}
		if !reflect.DeepEqual(compare, bound) {
			return "", toolInputError("账号动作目标在核验后发生变化，尚未提交操作")
		}
		if bound.Kind == "recall" && (h.checkedMessage == nil || !wechatapp.OwnMessage(*h.checkedMessage, h.Account(), bound.ThreadID, bound.MessageID) || h.checkedMessage.ServerID != verified.ServerID) {
			return "", toolInputError("撤回消息尚未由宿主核验，未提交操作")
		}
		return p.submitWechatAction(ctx, h.tool, h.raw, verified, key, settings, cross)
	}, nil
}

type wechatTextHost struct {
	p         *permittedTools
	raw       json.RawMessage
	prepared  *linuxnative.ToolSend
	validate  func(context.Context) error
	claimed   bool
	ready     bool
	attempted bool
}

func (h wechatTextHost) Invalid(message string) error { return toolInputError(message) }

func (h wechatTextHost) Allowed(thread string) bool {
	p := h.p
	return p.privileged() && p.agentToolEnabled("wechat_send_text") && p.conversationReadAllowed(thread) && p.manager.Config().Companion.Permissions.ReplyText && p.manager.agentTaskActive(p.trace)
}
func (h wechatTextHost) InvalidTextRequest() error {
	return localAgentError(errors.New("发送目标、正文或request_key无效，或文字发送权限未开启；可发送到当前或其他已授权会话"))
}
func (h *wechatTextHost) Claim(ctx context.Context, raw json.RawMessage, fn func() (any, error)) (string, error) {
	if string(raw) != string(h.raw) {
		return "", toolInputError("发送原请求已变化，未提交")
	}
	return h.p.privilegedCall(ctx, "wechat_send_text", raw, func() (any, error) {
		h.claimed = true
		defer func() { h.claimed, h.ready = false, false }()
		return fn()
	})
}
func (h wechatTextHost) OperationKey(raw json.RawMessage) string {
	return h.p.operationID("wechat_send_text", h.raw)
}
func (h *wechatTextHost) Sender() wechatapp.TextSender {
	if native, ok := h.p.manager.native.(nativeTools); ok {
		return wechatTextSender{host: h, native: native}
	}
	return nil
}

type wechatTextSender struct {
	host   *wechatTextHost
	native nativeTools
}

func (s wechatTextSender) LookupSend(ctx context.Context, key string) (linuxnative.ToolStatus, error) {
	if !s.host.claimed || key != s.host.OperationKey(s.host.raw) {
		return linuxnative.ToolStatus{}, toolInputError("只能查询本次发送的原幂等键")
	}
	status, err := s.native.LookupSend(ctx, key)
	if err != nil {
		return status, err
	}
	var original struct {
		Thread string `json:"thread_id"`
	}
	if json.Unmarshal(s.host.raw, &original) != nil || status.ID == "" || status.ThreadID != original.Thread || (status.Kind != "" && status.Kind != "send" && status.Kind != "text") {
		return linuxnative.ToolStatus{}, errors.New("outbound_receipt_identity_mismatch")
	}
	// The native receipt proves delivery status, not the original text body.
	// Claim persists this result. Audit recovery without inventing chat content
	// from the current arguments when the earlier local record was lost.
	p := s.host.p
	p.manager.record(bot.Record{Event: "agent_send_receipt_recovered", Category: "tool", ThreadID: status.ThreadID, Sender: p.trace.Sender,
		Status: status.State, SendID: status.ID, MessageID: p.trace.TraceID, Origin: "agent", Reason: "original_receipt_recovered_body_unverified"})
	return status, nil
}
func (s wechatTextSender) Submit(ctx context.Context, request linuxnative.ToolSend) (linuxnative.ToolStatus, error) {
	if !s.host.claimed || !s.host.ready || s.host.attempted || s.host.prepared == nil || !reflect.DeepEqual(request, *s.host.prepared) || s.host.validate == nil {
		return linuxnative.ToolStatus{}, rejectOutboundEvidence(errors.New("outbound_request_binding_changed"))
	}
	if !s.host.Allowed(request.ThreadID) {
		return linuxnative.ToolStatus{}, rejectOutboundEvidence(errors.New("outbound_destination_authorization_revoked"))
	}
	if err := s.host.validate(ctx); err != nil {
		return linuxnative.ToolStatus{}, err
	}
	s.host.ready = false
	s.host.attempted = true
	status, err := s.host.p.manager.submitWechat(ctx, s.native, request)
	if err == nil {
		s.host.submitted(request.ThreadID, request.Text, request.IdempotencyKey, status)
	}
	return status, err
}

func (h *wechatTextHost) Prepare(ctx context.Context, thread, text string) (wechatapp.PreparedText, error) {
	p := h.p
	var original struct {
		Thread string `json:"thread_id"`
		Text   string `json:"text"`
	}
	if json.Unmarshal(h.raw, &original) != nil || original.Thread != thread || original.Text != text {
		return wechatapp.PreparedText{}, toolInputError("发送原目标或正文已变化，未准备")
	}
	text, binding, err := p.prepareOutboundText(ctx, thread, text)
	if err != nil {
		return wechatapp.PreparedText{}, err
	}
	out := wechatapp.PreparedText{Text: text}
	out.Validate = func(ctx context.Context) error { return p.validateOutboundText(ctx, thread, binding, text) }
	if text != "" && binding.QuoteRequired && thread == p.trace.Thread {
		ref := p.outboundInput(thread).Source
		if p.trace.QuotedSource != nil && p.trace.QuotedSource.MessageID == binding.AnchorMessageID {
			ref = p.trace.QuotedSource
		}
		if ref == nil || ref.ServerID == "" || ref.MessageID != binding.AnchorMessageID {
			return wechatapp.PreparedText{}, errors.New("outbound_quote_source_unavailable")
		}
		out.Quote = &linuxnative.ToolQuote{MessageID: ref.MessageID, ServerID: ref.ServerID}
	}
	h.validate = out.Validate
	h.prepared = &linuxnative.ToolSend{ThreadID: thread, Kind: "text", Text: text, IdempotencyKey: h.OperationKey(h.raw)}
	if out.Quote != nil {
		quote := *out.Quote
		h.prepared.Quote = &quote
	}
	return out, nil
}

func (h *wechatTextHost) BeforeSubmit(ctx context.Context, thread string) error {
	if !h.claimed || h.prepared == nil || h.prepared.ThreadID != thread || h.ready || h.attempted {
		return rejectOutboundEvidence(errors.New("outbound_operation_not_claimed"))
	}
	p := h.p
	cfg := p.manager.Config()
	if strings.HasSuffix(thread, "@chatroom") && cfg.Companion.Quiet(time.Now()) {
		return errors.New("目标群处于安静时段")
	}
	if cfg.Behavior.RandomDelay {
		delay := cfg.Behavior.DelayMinSeconds
		if n := cfg.Behavior.DelayMaxSeconds - delay; n > 0 {
			delay += rand.IntN(n + 1)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(delay) * time.Second):
		}
	}
	if !h.Allowed(thread) {
		return rejectOutboundEvidence(errors.New("outbound_destination_authorization_revoked"))
	}
	if err := p.manager.memory.ClaimReply(cfg.Companion, time.Now()); err != nil {
		return err
	}
	h.ready = true
	return nil
}

func (h wechatTextHost) submitted(thread, text, key string, status linuxnative.ToolStatus) {
	p := h.p
	if status.State == "accepted" && status.ServerID != "" {
		p.rememberAgentSend(thread, text, key)
	}
	p.manager.record(bot.Record{Event: "agent_cross_send", Category: "sent", ThreadID: thread, Sender: p.trace.Sender,
		Content: text, Status: status.State, SendID: status.ID, MessageID: p.trace.TraceID, Origin: "agent"})
}
