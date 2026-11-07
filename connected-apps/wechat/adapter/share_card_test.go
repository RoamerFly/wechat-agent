package adapter

import (
	"strings"
	"testing"

	"wechat-bot/internal/linuxnative"
)

func TestCardDescriptionSeparatesPublisherCopyFromObservedContent(t *testing.T) {
	kind := 51
	message := linuxnative.Message{
		Type:           49,
		AppMessageType: &kind,
		Link: &linuxnative.LinkCard{
			Title:       "亲子活动视频",
			Description: "孩子们在公园放风筝",
			SourceName:  "测试发布者",
			URL:         "https://www.bilibili.com/video/BV1xx?token=secret&p=2#private",
		},
	}
	part, ok := PublisherCardDescription(message)
	if !ok || part.Range != "分享卡片元数据" || !strings.Contains(part.Excerpt, "亲子活动") {
		t.Fatal(part, ok)
	}
	card := CardDescription(message)
	if !strings.Contains(card, "未观看视频") || strings.Contains(card, "secret") || strings.Contains(card, "#private") || !strings.Contains(card, "p=2") {
		t.Fatal(card)
	}
	observed := ObservedCardDescription(message, "[实际内容已读取]")
	if !strings.HasPrefix(observed, "[实际内容已读取]") || strings.Contains(observed, "未观看视频") {
		t.Fatal(observed)
	}
}

func TestCardBodyDescriptionNeverReleasesURLOrInstructions(t *testing.T) {
	body := `<msg><appmsg><title>亲子活动视频</title><des>孩子们在公园放风筝</des><url>https://private.invalid/watch?token=secret</url><finderFeed><desc>户外运动</desc><nickname>作者</nickname></finderFeed></appmsg></msg>`
	text := CardBodyDescription(body)
	if !strings.Contains(text, "亲子活动") || !strings.Contains(text, "未观看") || strings.Contains(text, "secret") || strings.Contains(text, "private.invalid") {
		t.Fatal(text)
	}
	if !strings.Contains(CardBodyDescription("not XML"), "没有可读取") {
		t.Fatal("malformed card treated as video")
	}
}
