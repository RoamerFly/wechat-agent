package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"wechat-bot/internal/linuxnative"
)

// OriginalTextBodyUnverified is preserved when polling a recovered receipt.
func OriginalTextBodyUnverified(result string) bool {
	var metadata struct {
		Unverified bool `json:"original_body_unverified"`
	}
	return json.Unmarshal([]byte(result), &metadata) == nil && metadata.Unverified
}

type TextSender interface {
	LookupSend(context.Context, string) (linuxnative.ToolStatus, error)
	Submit(context.Context, linuxnative.ToolSend) (linuxnative.ToolStatus, error)
}

// PreparedText carries host-verified text/quote and a final source-version
// check. No memory handle, model context or mutable permission is exported.
type PreparedText struct {
	Text     string
	Quote    *linuxnative.ToolQuote
	Validate func(context.Context) error
}

type TextHost interface {
	Allowed(string) bool
	InvalidTextRequest() error
	Invalid(string) error
	Claim(context.Context, json.RawMessage, func() (any, error)) (string, error)
	OperationKey(json.RawMessage) string
	Sender() TextSender
	Prepare(context.Context, string, string) (PreparedText, error)
	BeforeSubmit(context.Context, string) error
}

// SendText owns the platform's lookup/prepare/submit sequence. The host owns
// durable claiming, policy, cancellation and results. Unknown lookups are never
// interpreted as permission to issue a new submission.
func SendText(ctx context.Context, raw json.RawMessage, host TextHost) (string, error) {
	var in struct {
		Thread string `json:"thread_id"`
		Text   string `json:"text"`
		Key    string `json:"request_key"`
	}
	if json.Unmarshal(raw, &in) != nil || !host.Allowed(in.Thread) || strings.TrimSpace(in.Text) == "" || len(in.Text) > 3000 || in.Key == "" || len(in.Key) > 128 {
		return "", host.InvalidTextRequest()
	}
	native := host.Sender()
	if native == nil {
		return "", host.Invalid("后台不支持幂等发送")
	}
	return host.Claim(ctx, raw, func() (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := host.OperationKey(raw)
		old, err := native.LookupSend(ctx, key)
		if err == nil {
			if old.ThreadID != in.Thread || old.ID == "" {
				return nil, errors.New("outbound_receipt_identity_mismatch")
			}
			return struct {
				linuxnative.ToolStatus
				BodyUnverified bool `json:"original_body_unverified"`
			}{old, true}, nil
		}
		if !errors.Is(err, linuxnative.ErrToolNotFound) {
			return nil, err
		}
		prepared, err := host.Prepare(ctx, in.Thread, in.Text)
		if err != nil {
			return nil, err
		}
		if prepared.Text == "" {
			return map[string]any{"state": "not_sent", "reason": "grounding_review_no_supported_reply"}, nil
		}
		if len(prepared.Text) > 3000 {
			return nil, errors.New("reviewed_text_too_long")
		}
		if err := host.BeforeSubmit(ctx, in.Thread); err != nil {
			return nil, err
		}
		if prepared.Validate == nil {
			return nil, errors.New("outbound_source_validation_unavailable")
		}
		if err := prepared.Validate(ctx); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return native.Submit(ctx, linuxnative.ToolSend{ThreadID: in.Thread, Kind: "text", Text: prepared.Text, IdempotencyKey: key, Quote: prepared.Quote})
	})
}
