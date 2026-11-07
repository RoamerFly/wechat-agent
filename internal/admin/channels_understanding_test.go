package admin

import (
	"context"
	"strings"
	"testing"
	"wechat-bot/internal/linuxnative"
)

type scopedChannelsNative struct {
	*fakeNative
	calls  int
	source linuxnative.ChannelsSource
}

func (n *scopedChannelsNative) ChannelsMedia(context.Context, string) (linuxnative.ChannelsSource, error) {
	n.calls++
	return n.source, nil
}

func TestChannelsObservationHonorsReadPermissionAndScope(t *testing.T) {
	f := setup(t)
	n := &scopedChannelsNative{fakeNative: f.native}
	f.manager.native = n
	kind := 51
	msg := linuxnative.Message{ID: "msg-card", ThreadID: "filehelper", Direction: "incoming", Type: 49, AppMessageType: &kind, Link: &linuxnative.LinkCard{Title: "Video title"}}
	a := mediaUnderstanding{manager: f.manager}
	f.manager.config.Behavior.ReadVideos = false
	text, err := a.Analyze(context.Background(), msg)
	if err != nil || n.calls != 0 || !strings.Contains(text, "尚未观看") {
		t.Fatal("permission ignored", err, text)
	}
	f.manager.config.Behavior.ReadVideos = true
	n.source = linuxnative.ChannelsSource{State: "available", MessageID: msg.ID, AccountID: "test", ThreadID: "other@chatroom", MediaURL: "http://127.0.0.1/private?token=must-not-leak"}
	text, err = a.Analyze(context.Background(), msg)
	if err == nil || !strings.Contains(text, "channels_source_scope_mismatch") || strings.Contains(text, "must-not-leak") {
		t.Fatal("scope or redaction failed", err, text)
	}
	n.source.ThreadID = msg.ThreadID
	n.source.State = "unavailable"
	text, err = a.Analyze(context.Background(), msg)
	if err == nil || !strings.Contains(text, "shared_video_read_failed") || !strings.Contains(text, "现有资料为卡片信息") {
		t.Fatal("missing locator fabricated observation", err, text)
	}
	if _, err := a.understandChannels(context.Background(), msg); err == nil || err.Error() != "share_plugin_migration_required" {
		t.Fatal("missing plugin entered legacy resolver path", err)
	}
}
