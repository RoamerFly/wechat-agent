package adapter

import (
	"encoding/json"
	"strings"
	"testing"

	"wechat-bot/internal/linuxnative"
)

func TestChannelsRouteBindsOneCardAndDoesNotClaimObservation(t *testing.T) {
	message := linuxnative.Message{ID: "card-one", AccountID: "account", ThreadID: "group", SenderID: "sender"}
	source := linuxnative.ChannelsSource{AccountID: "account", ThreadID: "group", MessageID: "card-one", ObjectID: "12345"}
	route, err := NewChannelsRoute(message, source, ChannelsEvent{Account: "account", Thread: "group", Message: "card-one", EventID: "event", Revision: 1}, "https://weixin.qq.com/sph/synthetic")
	if err != nil {
		t.Fatal(err)
	}
	parse := json.RawMessage(`{"route":"channels-parse","body":{"type":"video_channel_url","url":"https://weixin.qq.com/sph/synthetic","scene":1},"referer":""}`)
	if _, err := route.ExpectedRequest(parse); err != nil {
		t.Fatal(err)
	}
	foreign := json.RawMessage(strings.Replace(string(parse), "synthetic", "other", 1))
	if _, err := route.ExpectedRequest(foreign); err == nil {
		t.Fatal("a different share card reached the route")
	}
	if err := route.Accept("channels-parse", json.RawMessage(`{"code":0,"data":{"playable_url":"https://channels.weixin.qq.com/finder-preview/detail?token=token&eid=export-one"}}`)); err != nil {
		t.Fatal(err)
	}
	feed := json.RawMessage(`{"route":"channels-feed","body":{"baseReq":{"generalToken":"token"},"exportId":"export-one"},"referer":"https://channels.weixin.qq.com/finder-preview/detail?token=token&eid=export-one"}`)
	if _, err := route.ExpectedRequest(feed); err != nil {
		t.Fatal(err)
	}
	if err := route.Accept("channels-feed", json.RawMessage(`{"errCode":0,"data":{"authorInfo":{"nickname":"author"},"feedInfo":{"objectId":"12345","description":"title","videoUrl":"https://media.example/video","decodeKey":"8"}}}`)); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"ok":true,"platform":"channels","source_url":"https://weixin.qq.com/sph/synthetic","object_id":"12345","title":"title","author":"author","private_media_locators":["https://media.example/video"],"private_decode_key":"8","coverage":"media_locator_only","video_observed":false,"media_downloaded":false,"untrusted_external_content":true}`)
	checkpoint, handoff, err := route.ValidateResult(result)
	if err != nil || handoff.MessageID != "card-one" || strings.Contains(string(checkpoint), "private_media") || strings.Contains(string(checkpoint), "video_observed\":true") {
		t.Fatal(string(checkpoint), handoff, err)
	}
}
