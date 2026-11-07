package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
)

// wechatAttachmentHost exposes no Manager, native client, filesystem path or
// cache handle to the connected application. Its methods repeat the live
// authorization and source identity checks at each transfer boundary.
type wechatAttachmentHost struct {
	p       *permittedTools
	thread  string
	message string
}

func newWechatAttachmentHost(p *permittedTools, raw json.RawMessage) (wechatAttachmentHost, error) {
	var in wechatapp.AttachmentRequest
	if p == nil || json.Unmarshal(raw, &in) != nil || in.Message == "" {
		return wechatAttachmentHost{}, toolInputError("invalid attachment request")
	}
	if in.Thread == "" {
		in.Thread = p.trace.Thread
	}
	return wechatAttachmentHost{p: p, thread: in.Thread, message: in.Message}, nil
}

func (h wechatAttachmentHost) CurrentThread() string                  { return h.p.trace.Thread }
func (h wechatAttachmentHost) InvalidAttachment(message string) error { return toolInputError(message) }
func (h wechatAttachmentHost) Allowed(ctx context.Context, thread string) bool {
	return ctx.Err() == nil && thread == h.thread && h.p.agentToolEnabled("wechat_file_read") && h.p.conversationReadAllowed(thread)
}

func (h wechatAttachmentHost) input(thread string) companion.Input {
	in := h.p.trace
	in.Thread = thread
	return in
}

func (h wechatAttachmentHost) ResolveAttachment(ctx context.Context, thread, message string) (wechatapp.Attachment, error) {
	if thread != h.thread || message != h.message || !h.Allowed(ctx, thread) {
		return wechatapp.Attachment{}, wechatapp.ErrAttachmentDenied
	}
	asset, msg, err := h.p.manager.resolveInboundArtifact(ctx, h.input(thread), message)
	if strings.Contains(errorText(err), "inbound_artifact_original_not_cached") {
		if !h.Allowed(ctx, thread) {
			return wechatapp.Attachment{}, wechatapp.ErrAttachmentDenied
		}
		return wechatapp.Attachment{}, wechatapp.ErrAttachmentNotCached
	}
	if err != nil {
		return wechatapp.Attachment{}, err
	}
	in := h.input(thread)
	if !h.Allowed(ctx, thread) || msg.ID != message || msg.AccountID != in.Account || msg.ThreadID != thread || asset.Account != in.Account || asset.Scope != assetScope(in) {
		return wechatapp.Attachment{}, wechatapp.ErrAttachmentDenied
	}
	return wechatapp.Attachment{AssetID: asset.ID, MessageID: msg.ID, Filename: asset.Filename, MIME: asset.MIME, Bytes: asset.Bytes}, nil
}

func (h wechatAttachmentHost) DownloadAttachment(ctx context.Context, thread, message string) error {
	if thread != h.thread || message != h.message || !h.Allowed(ctx, thread) {
		return wechatapp.ErrAttachmentDenied
	}
	// Resolve first: it establishes account/thread/direction and the
	// same-sender or exact-quote constraint before a download is attempted.
	_, _, err := h.p.manager.resolveInboundArtifact(ctx, h.input(thread), message)
	if !strings.Contains(errorText(err), "inbound_artifact_original_not_cached") {
		return err
	}
	reader, ok := h.p.manager.native.(nativeMessageReader)
	if !ok {
		return errors.New("inbound_artifact_lookup_unavailable")
	}
	msg, err := reader.Message(ctx, message)
	in := h.input(thread)
	quoted := in.QuotedSource != nil && in.QuotedSource.MessageID == message && in.QuotedSource.Account == in.Account && in.QuotedSource.Thread == thread && in.QuotedSource.Sender == msg.SenderID
	if err != nil || !h.Allowed(ctx, thread) || msg.ID != message || msg.AccountID != in.Account || msg.ThreadID != thread ||
		msg.Direction != "incoming" || msg.IsQuotedText() || (!quoted && (in.Sender == "" || in.Sender != msg.SenderID)) || msg.FileAttachmentName() == "" {
		return wechatapp.ErrAttachmentDenied
	}
	if _, err = (mediaUnderstanding{manager: h.p.manager}).receiveFile(ctx, msg, msg.FileAttachmentName()); err != nil {
		return err
	}
	if !h.Allowed(ctx, thread) {
		return wechatapp.ErrAttachmentDenied
	}
	return nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// readWechatAttachment is the host entry point used by the agent tool router.
func (p *permittedTools) readWechatAttachment(ctx context.Context, raw json.RawMessage) (string, error) {
	host, err := newWechatAttachmentHost(p, raw)
	if err != nil {
		return "", err
	}
	result, err := wechatapp.ReadAttachment(ctx, raw, host)
	if errors.Is(err, wechatapp.ErrAttachmentDenied) {
		return "", toolInputError("attachment conversation denied")
	}
	if err != nil {
		return "", err
	}
	// Route using the source captured from the original request, never an
	// application-returned message field. This keeps a successful cross-group
	// acquisition private to the requesting owner on the final response path.
	if !host.Allowed(ctx, host.thread) {
		return "", toolInputError("attachment conversation denied")
	}
	p.routeCrossConversationThread(host.thread)
	b, err := json.Marshal(result)
	return string(b), err
}
