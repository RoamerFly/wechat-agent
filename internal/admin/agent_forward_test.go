package admin

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type forwardNative struct {
	*fakeTools
	message       linuxnative.Message
	catalogThread string
	downloads     int
	uploaded      []byte
	send          linuxnative.ToolSend
	onDownload    func()
}

func (n *forwardNative) Message(context.Context, string) (linuxnative.Message, error) {
	return n.message, nil
}
func (n *forwardNative) Catalog(_ context.Context, _, thread, _, _ string) (linuxnative.CatalogPage, error) {
	n.catalogThread = thread
	return linuxnative.CatalogPage{Items: []map[string]any{{"id": n.message.ID, "file_name": "notes.txt"}}}, nil
}
func (n *forwardNative) IncomingMedia(context.Context, string, int64) ([]byte, string, error) {
	n.downloads++
	if n.onDownload != nil {
		n.onDownload()
	}
	return []byte("synthetic attachment bytes"), "text/plain", nil
}
func (n *forwardNative) Upload(_ context.Context, name, mime string, r io.Reader, size int64) (linuxnative.UploadSource, error) {
	n.uploaded, _ = io.ReadAll(r)
	return linuxnative.UploadSource{ID: "source-forward", Filename: name, ContentType: mime, ByteLength: size}, nil
}
func (n *forwardNative) Submit(ctx context.Context, send linuxnative.ToolSend) (linuxnative.ToolStatus, error) {
	n.send = send
	return n.fakeTools.Submit(ctx, send)
}
func (n *forwardNative) LookupSend(ctx context.Context, key string) (linuxnative.ToolStatus, error) {
	if n.submits.Load() == 0 {
		return linuxnative.ToolStatus{}, linuxnative.ErrToolNotFound
	}
	return n.ToolSendStatus(ctx, key)
}
func (n *forwardNative) ToolSendStatus(context.Context, string) (linuxnative.ToolStatus, error) {
	return linuxnative.ToolStatus{ID: "send-one", ThreadID: n.send.ThreadID, Kind: "file", State: "accepted", ServerID: "123456"}, nil
}

func forwardFixture(t *testing.T) (*webFixture, *permittedTools, *forwardNative) {
	f, p := trustedAgent(t)
	n := &forwardNative{fakeTools: &fakeTools{fakeNative: f.native}, message: linuxnative.Message{ID: "file-message", CreatedAt: "2026-10-30T12:00:00Z", SenderID: "synthetic-sender", Direction: "incoming", AccountID: "test", ThreadID: "source@chatroom", Type: 49, Body: `<msg><appmsg><type>6</type><title>notes.txt</title><appattach><cdnattachurl>SECRET</cdnattachurl></appattach></appmsg></msg>`}}
	f.manager.native = n
	c := f.manager.agent.config()
	c.CrossConversation = true
	c.PrivilegedThreads = []string{"*"}
	if err := f.manager.agent.save(c); err != nil {
		t.Fatal(err)
	}
	f.manager.mu.Lock()
	f.manager.config.GroupRules = append(f.manager.config.GroupRules, GroupRule{ID: "source@chatroom", Mode: "mention"})
	f.manager.config.Companion.Permissions.SendFile = true
	f.manager.config.Companion.Permissions.Remember = true
	f.manager.config.Behavior.RandomDelay = false
	f.manager.mu.Unlock()
	p.trace.Text = "请把合成文件转发到指定会话"
	p.trace.OriginalText = &p.trace.Text
	p.trace.MessageTime = time.Now().UTC().Format(time.RFC3339Nano)
	p.trace.Source = &companion.SourceRef{InstanceID: f.manager.memory.SourceInstanceID(), MessageID: p.trace.TraceID, ServerID: "synthetic-forward-request", Account: p.trace.Account, Thread: p.trace.Thread, Sender: p.trace.Sender, MessageType: 1, OccurredAt: p.trace.MessageTime, Revision: 1, Integrity: "verified"}
	if err := f.manager.memory.Observe(p.trace, f.manager.Config().Companion, time.Now()); err != nil {
		t.Fatal(err)
	}
	if source, ok := f.manager.memory.SourceForInput(p.trace); ok {
		p.trace.Source = source
	} else {
		t.Fatal("missing synthetic forwarding request source")
	}
	return f, p, n
}
func TestOwnerCrossReadAndForwardUsesRealAttachmentAndReceipt(t *testing.T) {
	f, p, n := forwardFixture(t)
	if _, err := p.Call(context.Background(), "wechat_read", json.RawMessage(`{"kind":"messages","thread_id":"source@chatroom"}`)); err != nil || n.catalogThread != "source@chatroom" {
		t.Fatal("cross read failed", err)
	}
	args := json.RawMessage(`{"source_thread_id":"source@chatroom","message_id":"file-message","request_key":"forward-one"}`)
	result, err := p.Call(context.Background(), "wechat_forward_file", args)
	if err != nil {
		t.Fatal(err)
	}
	seeds := f.manager.takeMediaSeeds()
	if n.downloads != 1 || string(n.uploaded) != "synthetic attachment bytes" || len(seeds) != 1 || seeds[0].Input.Thread != "wxid_owner" || n.submits.Load() != 0 {
		t.Fatal("not original durable destination", result, len(seeds), n.submits.Load())
	}
	var submitted struct {
		Operation string `json:"operation_id"`
		State     string `json:"state"`
	}
	if json.Unmarshal([]byte(result), &submitted) != nil || submitted.Operation == "" || submitted.State != "queued" {
		t.Fatal(result)
	}
	if _, err = p.Call(context.Background(), "wechat_forward_file", args); err != nil || n.submits.Load() != 0 || n.downloads != 1 {
		t.Fatal("forward replay", err)
	}
	if _, err = p.Call(context.Background(), "wechat_forward_file", json.RawMessage(`{"source_thread_id":"source@chatroom","message_id":"file-message","target_thread_id":"filehelper","request_key":"forward-one"}`)); err == nil || n.submits.Load() != 0 {
		t.Fatal("changed target reused key")
	}
	// Synthetic worker submission, no real WeChat connection.
	n.Submit(context.Background(), linuxnative.ToolSend{ThreadID: seeds[0].Input.Thread, Kind: "file", SourceID: seeds[0].SourceID, IdempotencyKey: seeds[0].Key})
	f.login()
	receipt := readAppTestReceipt(t, f, submitted.Operation)
	if receipt.State != "accepted" || receipt.Kind != "file" || len(receipt.Deliveries) != 1 ||
		receipt.Deliveries[0].IdempotencyKey != seeds[0].Key || receipt.Deliveries[0].ServerID != "123456" {
		t.Fatal("forwarded file receipt did not match its original delivery", receipt)
	}
	statusArgs, _ := json.Marshal(map[string]string{"operation_id": submitted.Operation})
	status, err := p.Call(context.Background(), "wechat_operation_status", statusArgs)
	if err != nil || !strings.Contains(status, `"state":"accepted"`) {
		t.Fatal(status, err)
	}
	status, err = p.Call(context.Background(), "wechat_operation_status", statusArgs)
	if err != nil || n.submits.Load() != 1 {
		t.Fatal("poll lost filename or repeated send", status, err)
	}

}
func TestCrossConversationRejectsOtherUsersAndFalseSources(t *testing.T) {
	_, p, n := forwardFixture(t)
	p.trace.Sender = "wxid_imposter"
	if _, err := p.Call(context.Background(), "wechat_read", json.RawMessage(`{"kind":"messages","thread_id":"source@chatroom"}`)); err == nil || n.catalogThread != "" {
		t.Fatal("non-owner read another group")
	}
	args := json.RawMessage(`{"source_thread_id":"source@chatroom","message_id":"file-message","request_key":"forward-two"}`)
	if _, err := p.Call(context.Background(), "wechat_forward_file", args); err == nil {
		t.Fatal("non-owner offered forwarding")
	}
	p.trace.Sender = "wxid_owner"
	n.message.ThreadID = "not-source@chatroom"
	if _, err := p.Call(context.Background(), "wechat_forward_file", args); err == nil || n.downloads != 0 {
		t.Fatal("false source reached download")
	}
	n.message.ThreadID = "source@chatroom"
	n.message.Type = 1
	if _, err := p.Call(context.Background(), "wechat_forward_file", args); err == nil || n.downloads != 0 {
		t.Fatal("text substituted for file")
	}
	n.message.Type = 49
	if _, err := p.Call(context.Background(), "wechat_forward_file", json.RawMessage(`{"source_thread_id":"source@chatroom","message_id":"file-message","target_thread_id":"not-allowed@chatroom","request_key":"bad-target"}`)); err == nil || n.downloads != 0 {
		t.Fatal("unapproved target reached download")
	}
	if n.submits.Load() != 0 {
		t.Fatal("denied operation submitted")
	}
}
func TestForwardRevokedWhileDownloadingDoesNotSend(t *testing.T) {
	f, p, n := forwardFixture(t)
	n.onDownload = func() {
		c := f.manager.agent.config()
		c.CrossConversation = false
		if err := f.manager.agent.save(c); err != nil {
			t.Fatal(err)
		}
	}
	result, err := p.Call(context.Background(), "wechat_forward_file", json.RawMessage(`{"source_thread_id":"source@chatroom","message_id":"file-message","request_key":"revoke"}`))
	if err != nil || !strings.Contains(result, "authorization_revoked") || n.submits.Load() != 0 {
		t.Fatal(result, err, n.submits.Load())
	}
}
