package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	wechatadapter "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
)

type protocolTextSender struct {
	lookup               linuxnative.ToolStatus
	lookupErr, submitErr error
	last                 linuxnative.ToolSend
	lookups, submits     int
}

func (s *protocolTextSender) LookupSend(context.Context, string) (linuxnative.ToolStatus, error) {
	s.lookups++
	return s.lookup, s.lookupErr
}
func (s *protocolTextSender) Submit(_ context.Context, in linuxnative.ToolSend) (linuxnative.ToolStatus, error) {
	s.submits++
	s.last = in
	if s.submitErr != nil {
		return linuxnative.ToolStatus{}, s.submitErr
	}
	return linuxnative.ToolStatus{ID: "native-send", State: "accepted", ThreadID: in.ThreadID, ServerID: "99"}, nil
}

type protocolTextHost struct {
	sender                             *protocolTextSender
	claimed                            map[string]string
	prepareErr, beforeErr, validateErr error
	preparedText                       string
	quote                              *linuxnative.ToolQuote
	prepares, before, invalids         int
}

func (h *protocolTextHost) Allowed(string) bool { return true }
func (h *protocolTextHost) InvalidTextRequest() error {
	h.invalids++
	return errors.New("invalid text request")
}
func (*protocolTextHost) Invalid(string) error { return errors.New("invalid host request") }
func (h *protocolTextHost) Claim(_ context.Context, raw json.RawMessage, fn func() (any, error)) (string, error) {
	key := string(raw)
	if old, ok := h.claimed[key]; ok {
		return old, nil
	}
	value, err := fn()
	if err != nil {
		// Keep an uncertain original operation from being re-entered by the same raw request.
		h.claimed[key] = `{"state":"submission_unknown"}`
		return "", err
	}
	b, _ := json.Marshal(value)
	h.claimed[key] = string(b)
	return string(b), nil
}
func (*protocolTextHost) OperationKey(json.RawMessage) string { return "original-key" }
func (h *protocolTextHost) Sender() wechatadapter.TextSender  { return h.sender }
func (h *protocolTextHost) Prepare(context.Context, string, string) (wechatadapter.PreparedText, error) {
	h.prepares++
	if h.prepareErr != nil {
		return wechatadapter.PreparedText{}, h.prepareErr
	}
	return wechatadapter.PreparedText{Text: h.preparedText, Quote: h.quote, Validate: func(context.Context) error { return h.validateErr }}, nil
}
func (h *protocolTextHost) BeforeSubmit(context.Context, string) error {
	h.before++
	return h.beforeErr
}

func newProtocolTextHost() *protocolTextHost {
	return &protocolTextHost{sender: &protocolTextSender{lookupErr: linuxnative.ErrToolNotFound}, claimed: map[string]string{}, preparedText: "reviewed text"}
}

func TestWechatExecutionProtocolTextReceiptAndTemporaryLookupNeverSubmit(t *testing.T) {
	raw := json.RawMessage(`{"thread_id":"target","text":"original","request_key":"request"}`)
	t.Run("existing receipt", func(t *testing.T) {
		h := newProtocolTextHost()
		h.sender.lookup = linuxnative.ToolStatus{ID: "old", State: "accepted", ThreadID: "target", ServerID: "88"}
		h.sender.lookupErr = nil
		if _, err := wechatadapter.SendText(t.Context(), raw, h); err != nil || h.prepares != 0 || h.sender.submits != 0 {
			t.Fatalf("existing receipt was not returned without a send: err=%v prepares=%d submits=%d", err, h.prepares, h.sender.submits)
		}
	})
	t.Run("temporary lookup failure", func(t *testing.T) {
		h := newProtocolTextHost()
		h.sender.lookupErr = errors.New("temporary lookup failure")
		if _, err := wechatadapter.SendText(t.Context(), raw, h); err == nil || h.prepares != 0 || h.sender.submits != 0 {
			t.Fatalf("transient lookup failure dispatched a send: err=%v prepares=%d submits=%d", err, h.prepares, h.sender.submits)
		}
	})
}

func TestWechatExecutionProtocolTextDoesNotReplayUnknownSubmit(t *testing.T) {
	h := newProtocolTextHost()
	h.sender.submitErr = errors.New("acknowledgement lost")
	raw := json.RawMessage(`{"thread_id":"target","text":"original","request_key":"request"}`)
	if _, err := wechatadapter.SendText(t.Context(), raw, h); err == nil {
		t.Fatal("lost acknowledgement unexpectedly succeeded")
	}
	if _, err := wechatadapter.SendText(t.Context(), raw, h); err != nil {
		t.Fatalf("durable result retry failed: %v", err)
	}
	if h.sender.submits != 1 || h.prepares != 1 {
		t.Fatalf("same raw request re-entered submission: prepares=%d submits=%d", h.prepares, h.sender.submits)
	}
}

func TestWechatExecutionProtocolTextValidatesBeforeSubmitAndPreservesBoundArguments(t *testing.T) {
	for name, configure := range map[string]func(*protocolTextHost){
		"prepare failure":           func(h *protocolTextHost) { h.prepareErr = errors.New("review failed") },
		"pre-submit cancellation":   func(h *protocolTextHost) { h.beforeErr = context.Canceled },
		"source validation failure": func(h *protocolTextHost) { h.validateErr = errors.New("source changed") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newProtocolTextHost()
			configure(h)
			_, err := wechatadapter.SendText(t.Context(), json.RawMessage(`{"thread_id":"target","text":"original","request_key":"request"}`), h)
			if err == nil || h.sender.submits != 0 {
				t.Fatalf("failed validation dispatched a send: err=%v submits=%d", err, h.sender.submits)
			}
		})
	}
	h := newProtocolTextHost()
	h.quote = &linuxnative.ToolQuote{MessageID: "source", ServerID: "42"}
	if _, err := wechatadapter.SendText(t.Context(), json.RawMessage(`{"thread_id":"target","text":"original","request_key":"request"}`), h); err != nil {
		t.Fatal(err)
	}
	if got := h.sender.last; got.ThreadID != "target" || got.IdempotencyKey != "original-key" || got.Text != "reviewed text" || got.Quote == nil || *got.Quote != *h.quote || h.sender.submits != 1 {
		t.Fatalf("submitted arguments lost their binding: %#v submits=%d", got, h.sender.submits)
	}
}

func TestWechatExecutionProtocolTextRejectsInvalidInput(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"thread_id":"target","text":" ","request_key":"request"}`),
		json.RawMessage(`{"thread_id":"target","text":"original","request_key":""}`),
		json.RawMessage(`{"thread_id":"target","text":"` + strings.Repeat("x", 3001) + `","request_key":"request"}`),
	} {
		h := newProtocolTextHost()
		if _, err := wechatadapter.SendText(t.Context(), raw, h); err == nil || h.sender.lookups != 0 || h.sender.submits != 0 || h.invalids != 1 {
			t.Fatalf("invalid input reached sender: err=%v lookups=%d submits=%d invalids=%d", err, h.sender.lookups, h.sender.submits, h.invalids)
		}
	}
}

type protocolActionHost struct {
	account, thread                  string
	owner                            bool
	authorizeErr, submitErr, readErr error
	message                          linuxnative.Message
	valid                            map[string]bool
	cross                            []bool
	submits                          int
	last                             linuxnative.AdminAction
	onRead                           func()
}

func (h *protocolActionHost) Account() string        { return h.account }
func (h *protocolActionHost) Thread() string         { return h.thread }
func (h *protocolActionHost) Owner() bool            { return h.owner }
func (h *protocolActionHost) ValidID(id string) bool { return h.valid[id] }
func (*protocolActionHost) Invalid(string) error     { return errors.New("invalid action") }
func (h *protocolActionHost) AuthorizeAction(_ context.Context, in linuxnative.AdminAction, cross bool) (wechatadapter.ActionSubmission, error) {
	h.cross = append(h.cross, cross)
	if h.authorizeErr != nil {
		return nil, h.authorizeErr
	}
	return func(_ context.Context, got linuxnative.AdminAction) (string, error) {
		h.submits++
		h.last = got
		return "submitted", h.submitErr
	}, nil
}
func (h *protocolActionHost) ReadMessage(context.Context, string) (linuxnative.Message, error) {
	if h.onRead != nil {
		h.onRead()
	}
	return h.message, h.readErr
}
func newProtocolActionHost() *protocolActionHost {
	return &protocolActionHost{account: "account", thread: "current", owner: true, valid: map[string]bool{"request": true, "member": true}, message: linuxnative.Message{ID: "message", AccountID: "account", ThreadID: "current", Direction: "outgoing", ServerID: "7"}}
}

func TestWechatExecutionProtocolActionsUseDistinctCrossAuthorityAndExactRecall(t *testing.T) {
	h := newProtocolActionHost()
	if _, err := wechatadapter.AccountAction(t.Context(), json.RawMessage(`{"kind":"rename","thread_id":"room@chatroom","text":"name"}`), h); err != nil {
		t.Fatal(err)
	}
	if _, err := wechatadapter.Recall(t.Context(), json.RawMessage(`{"message_id":"message","request_key":"request"}`), h); err != nil {
		t.Fatal(err)
	}
	if len(h.cross) != 2 || h.cross[0] || !h.cross[1] || h.last.ThreadID != "current" || h.last.MessageID != "message" || h.last.ServerID != "7" {
		t.Fatalf("authority or exact recall binding changed: cross=%v action=%#v", h.cross, h.last)
	}
}

func TestWechatExecutionProtocolRecallRequiresExactOwnOutgoingMessage(t *testing.T) {
	for name, alter := range map[string]func(*linuxnative.Message){
		"other account": func(m *linuxnative.Message) { m.AccountID = "other" },
		"other thread":  func(m *linuxnative.Message) { m.ThreadID = "other" },
		"incoming":      func(m *linuxnative.Message) { m.Direction = "incoming" },
		"other message": func(m *linuxnative.Message) { m.ID = "other" },
		"no server id":  func(m *linuxnative.Message) { m.ServerID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newProtocolActionHost()
			alter(&h.message)
			if _, err := wechatadapter.Recall(t.Context(), json.RawMessage(`{"message_id":"message","request_key":"request"}`), h); err == nil || h.submits != 0 {
				t.Fatalf("non-exact message reached recall submission: err=%v submits=%d", err, h.submits)
			}
		})
	}
}

func TestWechatExecutionProtocolActionsRejectBeforeMutation(t *testing.T) {
	t.Run("revoked during read", func(t *testing.T) {
		h := newProtocolActionHost()
		h.onRead = func() { h.submitErr = errors.New("grant revoked") }
		if _, err := wechatadapter.Recall(t.Context(), json.RawMessage(`{"message_id":"message","request_key":"request"}`), h); err == nil || h.submits != 1 {
			t.Fatalf("revocation was not enforced by bound submission: err=%v submits=%d", err, h.submits)
		}
	})
	for name, raw := range map[string]json.RawMessage{
		"invalid member":   json.RawMessage(`{"kind":"add_members","thread_id":"room@chatroom","members":["bad"]}`),
		"rename non-group": json.RawMessage(`{"kind":"rename","thread_id":"person","text":"name"}`),
	} {
		t.Run(name, func(t *testing.T) {
			h := newProtocolActionHost()
			if _, err := wechatadapter.AccountAction(t.Context(), raw, h); err == nil || h.submits != 0 {
				t.Fatalf("invalid action mutated state: err=%v submits=%d", err, h.submits)
			}
		})
	}
	t.Run("host rejects unsupported permission", func(t *testing.T) {
		h := newProtocolActionHost()
		h.authorizeErr = errors.New("unsupported permission")
		if _, err := wechatadapter.AccountAction(t.Context(), json.RawMessage(`{"kind":"mute","thread_id":"person"}`), h); err == nil || h.submits != 0 {
			t.Fatalf("host rejection mutated state: err=%v submits=%d", err, h.submits)
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		h := newProtocolActionHost()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := wechatadapter.AccountAction(ctx, json.RawMessage(`{"kind":"mute","thread_id":"person"}`), h); err == nil || h.submits != 0 {
			t.Fatalf("cancelled action submitted: err=%v submits=%d", err, h.submits)
		}
	})
}
