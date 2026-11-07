package admin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type attachmentAdapterHost struct {
	allowed    bool
	cached     bool
	downloads  int
	onDownload func(context.Context)
}

func (h *attachmentAdapterHost) CurrentThread() string                { return "current" }
func (h *attachmentAdapterHost) Allowed(context.Context, string) bool { return h.allowed }
func (h *attachmentAdapterHost) InvalidAttachment(s string) error     { return errors.New(s) }
func (h *attachmentAdapterHost) ResolveAttachment(ctx context.Context, thread, message string) (wechatapp.Attachment, error) {
	if thread != "current" || message != "source" || ctx.Err() != nil {
		return wechatapp.Attachment{}, wechatapp.ErrAttachmentDenied
	}
	if !h.cached {
		return wechatapp.Attachment{}, wechatapp.ErrAttachmentNotCached
	}
	return wechatapp.Attachment{AssetID: "asset", MessageID: "source", Filename: "notes.txt", MIME: "text/plain", Bytes: 12}, nil
}
func (h *attachmentAdapterHost) DownloadAttachment(ctx context.Context, thread, message string) error {
	h.downloads++
	if h.onDownload != nil {
		h.onDownload(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.cached = true
	return nil
}

func TestWechatAttachmentAdapter(t *testing.T) {
	raw := json.RawMessage(`{"message_id":"source"}`)
	t.Run("cached", func(t *testing.T) {
		h := &attachmentAdapterHost{allowed: true, cached: true}
		got, err := wechatapp.ReadAttachment(t.Context(), raw, h)
		if err != nil || h.downloads != 0 || got["asset_id"] != "asset" || got["message_id"] != "source" || got["status"] != "original_available" {
			t.Fatalf("cached result: %#v %v", got, err)
		}
	})
	t.Run("uncached downloads only verified ordinary source", func(t *testing.T) {
		h := &attachmentAdapterHost{allowed: true}
		got, err := wechatapp.ReadAttachment(t.Context(), raw, h)
		if err != nil || h.downloads != 1 || got["filename"] != "notes.txt" {
			t.Fatalf("uncached result: %#v %v", got, err)
		}
	})
	t.Run("source mismatch has no download", func(t *testing.T) {
		h := &attachmentAdapterHost{allowed: false}
		if _, err := wechatapp.ReadAttachment(t.Context(), json.RawMessage(`{"message_id":"other"}`), h); !errors.Is(err, wechatapp.ErrAttachmentDenied) || h.downloads != 0 {
			t.Fatalf("mismatch leaked or downloaded: %v %d", err, h.downloads)
		}
	})
	t.Run("revoked while downloading returns no preview", func(t *testing.T) {
		h := &attachmentAdapterHost{allowed: true}
		h.onDownload = func(context.Context) { h.allowed = false }
		if got, err := wechatapp.ReadAttachment(t.Context(), raw, h); !errors.Is(err, wechatapp.ErrAttachmentDenied) || got != nil {
			t.Fatalf("revoked result: %#v %v", got, err)
		}
	})
	t.Run("cancelled download returns no preview", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		h := &attachmentAdapterHost{allowed: true}
		h.onDownload = func(context.Context) { cancel() }
		if got, err := wechatapp.ReadAttachment(ctx, raw, h); !errors.Is(err, context.Canceled) || got != nil {
			t.Fatalf("cancelled result: %#v %v", got, err)
		}
	})

	// This is a host-level regression: optional observation/ASR plugins are not
	// involved in retrieving a received ordinary file original.
	t.Run("no observation plugin required", func(t *testing.T) {
		_, p, native := forwardFixture(t)
		native.message = linuxnative.Message{ID: "pluginless-file", AccountID: p.trace.Account, ThreadID: p.trace.Thread, SenderID: p.trace.Sender, Type: 49, Direction: "incoming", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Body: `<msg><appmsg><type>6</type><title>notes.txt</title><appattach><cdnattachurl>synthetic</cdnattachurl></appattach></appmsg></msg>`}
		result, err := p.readWechatAttachment(t.Context(), json.RawMessage(`{"message_id":"pluginless-file"}`))
		if err != nil || native.downloads != 1 || result == "" {
			t.Fatalf("pluginless receive: %q %v downloads=%d", result, err, native.downloads)
		}
	})
	t.Run("bound host cannot switch declared source", func(t *testing.T) {
		_, p, native := forwardFixture(t)
		native.message = linuxnative.Message{ID: "bound-source", AccountID: p.trace.Account, ThreadID: p.trace.Thread, SenderID: p.trace.Sender, Type: 49, Direction: "incoming", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Body: `<msg><appmsg><type>6</type><title>notes.txt</title><appattach><cdnattachurl>synthetic</cdnattachurl></appattach></appmsg></msg>`}
		host, err := newWechatAttachmentHost(p, json.RawMessage(`{"message_id":"bound-source"}`))
		if err != nil {
			t.Fatal(err)
		}
		got, err := wechatapp.ReadAttachment(t.Context(), json.RawMessage(`{"message_id":"other-source"}`), host)
		if !errors.Is(err, wechatapp.ErrAttachmentDenied) || got != nil || native.downloads != 0 {
			t.Fatalf("host accepted substituted source: %#v %v downloads=%d", got, err, native.downloads)
		}
	})
	t.Run("successful cross source enables private reply route", func(t *testing.T) {
		f, p, native := forwardFixture(t)
		f.manager.mu.Lock()
		f.manager.config.GroupRules = append(f.manager.config.GroupRules, GroupRule{ID: "request@chatroom", Mode: "mention"})
		f.manager.mu.Unlock()
		p.SetTrace(companion.Input{Channel: "wechat", Account: "test", Thread: "request@chatroom", Sender: "wxid_owner", TraceID: "attachment-cross", IsGroup: true, Mode: "direct"})
		native.message = linuxnative.Message{ID: "cross-source", AccountID: "test", ThreadID: "source@chatroom", SenderID: "wxid_owner", Type: 49, Direction: "incoming", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Body: `<msg><appmsg><type>6</type><title>notes.txt</title><appattach><cdnattachurl>synthetic</cdnattachurl></appattach></appmsg></msg>`}
		if _, err := p.readWechatAttachment(t.Context(), json.RawMessage(`{"message_id":"cross-source","thread_id":"source@chatroom"}`)); err != nil {
			t.Fatal(err)
		}
		if !p.PrivateReplyRequired() || p.PrivateReplyTarget() != "wxid_owner" {
			t.Fatalf("cross attachment did not bind private route: required=%v target=%q", p.PrivateReplyRequired(), p.PrivateReplyTarget())
		}
	})
}
