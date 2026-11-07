package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type applicationReadNative struct {
	*fakeNative
	read func(context.Context, string, string, string, string) (linuxnative.CatalogPage, error)
}

func (n applicationReadNative) Catalog(ctx context.Context, kind, thread, search, cursor string) (linuxnative.CatalogPage, error) {
	return n.read(ctx, kind, thread, search, cursor)
}

func TestWechatApplicationReadRegistersOnlySuccessfulMessageSources(t *testing.T) {
	for _, kind := range []string{"messages", "members"} {
		t.Run(kind, func(t *testing.T) {
			f, p := trustedAgent(t)
			defer p.hub.Close()
			settings := f.manager.agent.config()
			settings.CrossConversation = true
			if err := f.manager.agent.save(settings); err != nil {
				t.Fatal(err)
			}
			f.manager.mu.Lock()
			f.manager.config.GroupRules = append(f.manager.config.GroupRules, GroupRule{ID: "source@chatroom", Mode: "mention"})
			f.manager.mu.Unlock()
			calls := 0
			f.manager.native = applicationReadNative{f.native, func(ctx context.Context, gotKind, thread, search, cursor string) (linuxnative.CatalogPage, error) {
				calls++
				if gotKind != kind || thread != "source@chatroom" || search != "" || cursor != "opaque-original-cursor" {
					t.Fatalf("changed native read %s %s %s %s", gotKind, thread, search, cursor)
				}
				return linuxnative.CatalogPage{Items: []map[string]any{{"id": "source-1", "threadId": thread}}}, nil
			}}
			result, err := p.Call(t.Context(), "wechat_read", json.RawMessage(`{"kind":"`+kind+`","cursor":"opaque-original-cursor","thread_id":"source@chatroom"}`))
			if err != nil || calls != 1 || !strings.Contains(result, "source-1") {
				t.Fatal(calls, result, err)
			}
			if got := p.crossSourceMessages["source@chatroom"]; (kind == "messages") != (len(got) == 1) {
				t.Fatal("member names became message/task evidence", kind, got)
			}
			for _, tool := range p.workspaceAgentTools() {
				if wechatapp.ToolName(tool.Name) {
					t.Fatal("application tool leaked into project tools", tool.Name)
				}
			}
		})
	}
}

func TestWechatApplicationReadRechecksGrantBeforeReturningPage(t *testing.T) {
	f := setup(t)
	called := false
	f.manager.native = applicationReadNative{f.native, func(context.Context, string, string, string, string) (linuxnative.CatalogPage, error) {
		called = true
		settings := f.manager.agent.config()
		settings.ReadConversations = false
		if err := f.manager.agent.save(settings); err != nil {
			t.Fatal(err)
		}
		return linuxnative.CatalogPage{Items: []map[string]any{{"id": "late-private-source", "body": "synthetic late data"}}}, nil
	}}
	p, _, err := f.manager.toolAccess()
	if err != nil {
		t.Fatal(err)
	}
	defer p.hub.Close()
	p.SetTrace(companion.Input{Channel: "wechat", Account: "test", Thread: "filehelper", Sender: "owner", TraceID: "revoked-read"})
	result, err := p.Call(t.Context(), "wechat_read", json.RawMessage(`{"kind":"messages"}`))
	if !called || err == nil || result != "" || len(p.crossSourceMessages) != 0 || p.agentToolEnabled("wechat_read") {
		t.Fatal("revoked page escaped or became task evidence", result, err)
	}
	if p.agentToolEnabled("wechat_unknown") || workspaceAgentToolName("wechat_unknown") {
		t.Fatal("an undeclared application tool became callable")
	}
}

func TestWechatApplicationSourceBoundaryExcludesHostBookkeeping(t *testing.T) {
	src := &companion.SourceRef{InstanceID: "dataset", EventID: "event", MessageID: "message", ServerID: "123",
		Account: "account", Thread: "room", Sender: "sender", SenderName: "private-display-name", BodyHash: "private-body-hash",
		Integrity: "private-import-detail", MessageType: 49, OccurredAt: "2026-01-01", ReceivedAt: "2026-01-02", Sequence: 8, Revision: 2}
	view := wechatSourceMetadata(src)
	if view.EventID != src.EventID || view.InstanceID != src.InstanceID || view.Sender != src.Sender || view.Revision != 2 || view.Sequence != 8 || view.MessageType != 49 {
		t.Fatal("source identity lost at application boundary", view)
	}
	data, err := json.Marshal(view)
	if err != nil || strings.Contains(string(data), "private-") || wechatSourceMetadata(nil) != nil {
		t.Fatal("host-only source fields crossed boundary", string(data), err)
	}
}
