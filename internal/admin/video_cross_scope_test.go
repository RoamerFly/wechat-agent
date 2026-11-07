package admin

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

func TestCrossConversationVideoUsesOriginalCacheAndExplicitDestinationScope(t *testing.T) {
	f, p, n := forwardFixture(t)
	f.manager.config.Behavior.ReadVideos = true
	n.message = linuxnative.Message{ID: "cached-video", CreatedAt: "2026-10-30T12:00:00Z", SenderID: "source-member", Direction: "incoming", AccountID: "test", ThreadID: "source@chatroom", Type: 43}
	trace := companion.Input{Channel: "wechat", Account: "test", Thread: "source@chatroom", TraceID: n.message.ID, Sender: n.message.SenderID, MessageTime: n.message.CreatedAt}
	a, err := f.manager.rememberAssetFormats(trace, assetOriginal{InboundMessage: &n.message, Data: inspectionFixture(t), MIME: "video/mp4", Filename: "synthetic.mp4"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []map[string]string{{"message_id": n.message.ID}, {"asset_id": a.ID, "source_thread_id": "source@chatroom"}} {
		source["action"] = "info"
		raw, _ := json.Marshal(source)
		result, err := p.Call(context.Background(), "video_inspect", raw)
		if err != nil || !strings.Contains(result, `"source_thread_id":"source@chatroom"`) || !strings.Contains(result, `"origin_thread_id":"wxid_owner"`) || n.downloads != 0 {
			t.Fatal(result, err, n.downloads)
		}
	}
}

func TestOwnerCanInspectAnotherAuthorizedConversationWithoutChangingOrigin(t *testing.T) {
	f, p, n := forwardFixture(t)
	f.manager.config.Behavior.ReadVideos = true
	origin := p.trace.Thread
	n.message = linuxnative.Message{ID: "video-message", AccountID: "test", ThreadID: "source@chatroom", Type: 43}
	in := videoInspectionInput{Message: n.message.ID}
	data, _, err := p.inspectionSource(context.Background(), &in)
	if err != nil || len(data) == 0 || in.SourceThread != "source@chatroom" || p.trace.Thread != origin {
		t.Fatal(in, err)
	}
	// Explicit source is binding; it cannot be silently replaced by metadata.
	in.SourceThread = origin
	if _, _, err = p.inspectionSource(context.Background(), &in); err == nil {
		t.Fatal("explicit source ignored")
	}
	in.SourceThread = ""
	n.message.AccountID = "another-account"
	if _, _, err = p.inspectionSource(context.Background(), &in); err == nil {
		t.Fatal("another account exposed")
	}
	n.message.AccountID = "test"
	p.trace.Sender = "ordinary-member"
	in.SourceThread = ""
	if _, _, err = p.inspectionSource(context.Background(), &in); err == nil {
		t.Fatal("member cross-read")
	}
	p.trace.Sender = "wxid_owner"
	cfg := f.manager.agent.config()
	cfg.CrossConversation = false
	f.manager.agent.save(cfg)
	in.SourceThread = ""
	if _, _, err = p.inspectionSource(context.Background(), &in); err == nil {
		t.Fatal("disabled cross-read")
	}
}

func TestOwnerDiagnosticsAreScopedPaginatedAndExcludeRawSecrets(t *testing.T) {
	f, p, _ := forwardFixture(t)
	f.manager.record(bot.Record{Event: "video_audio_no_words", ThreadID: "source@chatroom", MessageID: "target", Status: "no_recognizable_speech", ToolCode: "ASR_RESPONSE_HAVE_NO_WORDS", ToolHTTPStatus: 400, Content: "synthetic private content", Reason: "synthetic private credential", Origin: "speech"})
	result, err := p.Call(context.Background(), "agent_diagnostics", json.RawMessage(`{"thread_id":"source@chatroom","message_id":"target","limit":1}`))
	if err != nil || !strings.Contains(result, "ASR_RESPONSE_HAVE_NO_WORDS") || strings.Contains(result, "private") {
		t.Fatal(result, err)
	}
	p.trace.Account = "different-account"
	if _, err = p.Call(context.Background(), "agent_diagnostics", json.RawMessage(`{}`)); err == nil {
		t.Fatal("different account")
	}
	p.trace.Account = "test"
	p.trace.Sender = "ordinary-member"
	if _, err = p.Call(context.Background(), "agent_diagnostics", json.RawMessage(`{}`)); err == nil {
		t.Fatal("member diagnostics")
	}
}

func TestVideoRetryReusedLocatorResolvesCurrentShare(t *testing.T) {
	f, p, n := forwardFixture(t)
	f.manager.config.Behavior.ReadVideos = true
	old := linuxnative.Message{ID: "reused-row", AccountID: "test", ThreadID: "source@chatroom", SenderID: "same-member", Direction: "incoming", Type: 43, CreatedAt: "2026-10-28T12:00:00Z", ServerID: "101"}
	trace := companion.Input{Channel: "wechat", Account: old.AccountID, Thread: old.ThreadID, TraceID: old.ID, Sender: old.SenderID, MessageTime: old.CreatedAt}
	cache := func(original *linuxnative.Message, data string) mediaAsset {
		t.Helper()
		item, err := f.manager.rememberAssetFormats(trace, assetOriginal{InboundMessage: original, Data: []byte(data), MIME: "video/mp4", Filename: "fixture.mp4"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	oldAsset := cache(&old, "old bytes")
	n.message = old
	subtype := 51
	n.message.Type, n.message.AppMessageType = 49, &subtype
	n.message.ServerID, n.message.CreatedAt = "202", "2026-11-04T06:24:04Z"
	trace.MessageTime = n.message.CreatedAt
	// A legacy or generated asset carrying even the current request's ID
	// and timestamp is not evidence that its bytes came from this share.
	cache(nil, "unverified bytes")
	source := videoInspectionInput{Message: old.ID}
	if b, _, err := p.inspectionSource(t.Context(), &source); err == nil || len(b) != 0 {
		t.Fatalf("unavailable Channels source returned a substitute: %q, %v", b, err)
	}
	if _, err := p.inspectionTranscriptionMessage(t.Context(), videoInspectionInput{Asset: oldAsset.ID, SourceThread: old.ThreadID}); err == nil {
		t.Fatal("old audio was attributed to the new message")
	}
	cache(&n.message, "actual share")
	if b, _, err := p.inspectionSource(t.Context(), &source); err != nil || string(b) != "actual share" {
		t.Fatal(string(b), err)
	}
	n.message.ServerID = "203"
	if b, _, err := p.inspectionSource(t.Context(), &source); err == nil || len(b) != 0 {
		t.Fatal("a different server message matched the same timestamp")
	}
}

type replacedChannelsNative struct {
	*forwardNative
	source  linuxnative.ChannelsSource
	lookups []string
}

func (n *replacedChannelsNative) ChannelsMedia(_ context.Context, id string) (linuxnative.ChannelsSource, error) {
	n.lookups = append(n.lookups, id)
	return n.source, nil
}

func TestVideoInspectionRejectsOlderChannelsCardFromSameRoom(t *testing.T) {
	f, p, native := forwardFixture(t)
	defer p.hub.Close()
	f.manager.config.Behavior.ReadVideos = true
	kind := 51
	native.message = linuxnative.Message{ID: "target-card", AccountID: "test", ThreadID: "source@chatroom", SenderID: "member", Direction: "incoming", Type: 49, AppMessageType: &kind}
	channels := &replacedChannelsNative{forwardNative: native, source: linuxnative.ChannelsSource{AccountID: "test", ThreadID: "source@chatroom", MessageID: "older-card", ObjectID: "101", State: "available", MediaURL: "https://invalid.example/synthetic"}}
	f.manager.native = channels
	for range 2 {
		in := videoInspectionInput{Message: native.message.ID}
		data, _, err := p.inspectionSource(t.Context(), &in)
		if err == nil || err.Error() != "channels_source_scope_mismatch" || len(data) != 0 {
			t.Fatal("read accepted a locator for another message", err)
		}
	}
	if !slices.Equal(channels.lookups, []string{"target-card", "target-card"}) {
		t.Fatal("retry retargeted native reads", channels.lookups)
	}
}
