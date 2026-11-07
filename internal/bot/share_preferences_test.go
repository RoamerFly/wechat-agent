package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

func TestSharePreferenceConsidersCardsWithoutInventingMention(t *testing.T) {
	w, e, _ := backgroundWorker(t, generationModelFunc(func(context.Context, string, []map[string]string) (string, error) {
		return `{"action":"silent","messages":[]}`, nil
	}))
	room := "synthetic@chatroom"
	w.allowed[room] = true
	w.config.GroupRules = map[string]GroupRule{room: {Mode: "mention"}}
	w.config.Analyzer = unavailableShareAnalyzer{}
	w.config.Options.ReadVideos = true
	e.Config.Permissions.ObserveGroups = true
	e.Config.Permissions.AutonomousGroups = true
	e.Config.Permissions.ReplyText = true
	input := companion.Input{Channel: w.config.Channel, Account: w.source.Account(), Thread: room, Sender: "a", Text: "hello", TraceID: "seed"}
	if err := e.Store.Observe(input, e.Config, time.Now()); err != nil {
		t.Fatal(err)
	}
	m := fixtureMessage("card", room, "", "incoming")
	m.Type = 49
	m.Link = &linuxnative.LinkCard{URL: "https://www.kuaishou.com/short-video/3xsynthetic12345"}
	if w.considerPreferredShare(m) {
		t.Fatal("empty preferences changed trigger")
	}
	if err := e.Store.SetPreferences(companion.ScopeID(input.Channel, input.Account, room), "分享默认总结后发送", 0); err != nil {
		t.Fatal(err)
	}
	if !w.considerPreferredShare(m) {
		t.Fatal("share not considered")
	}
	channels := m
	subtype := 51
	channels.AppMessageType = &subtype
	channels.Link = nil
	if !w.considerPreferredShare(channels) {
		t.Fatal("native Channels identity without parsed card ignored")
	}
	quoted := m
	subtypeQuote := 57
	quoted.AppMessageType = &subtypeQuote
	if w.considerPreferredShare(quoted) {
		t.Fatal("quoted text was treated as a new share")
	}
	working := m
	working.Type = 1
	working.Body = "分享内容"
	if _, ok := w.promptWithSharePreference(working, time.Now(), true); !ok {
		t.Fatal("authorized share blocked by mention mode")
	}
	if w.addressed(working) {
		t.Fatal("fabricated mention")
	}
	working.CreatedAt = time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	if _, ok := w.promptWithSharePreference(working, time.Now(), true); ok {
		t.Fatal("stale card bypassed age")
	}
	e.Config.Permissions.AutonomousGroups = false
	if w.considerPreferredShare(m) {
		t.Fatal("preference bypassed autonomous permission")
	}
	e.Config.Permissions.AutonomousGroups = true
	m.ThreadID = "other@chatroom"
	if w.considerPreferredShare(m) {
		t.Fatal("cross room preference")
	}
}

func TestHostGroupShareStartupBatchCarriesAnchorEvidenceToModel(t *testing.T) {
	var sawEvidence bool
	w, _, _ := backgroundWorker(t, generationModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
		for _, turn := range turns {
			if strings.Contains(turn["content"], "已读取的真实视频片段") && strings.Contains(turn["content"], `"observation_state":"observed"`) {
				sawEvidence = true
			}
		}
		return `{"action":"silent","messages":[]}`, nil
	}))
	room := "startup-share@chatroom"
	w.allowed[room] = true
	w.config.GroupRules = map[string]GroupRule{room: {Mode: "mention"}}
	share := fixtureMessage("startup-channels", room, "", "incoming")
	share.Type, share.SenderID = 49, "member-b"
	subtype := 51
	share.AppMessageType = &subtype
	w.source = &fakeSource{messages: map[string]linuxnative.Message{share.ID: share}}
	w.config.PreferredGroupShare = func(m linuxnative.Message) bool { return m.ID == share.ID }
	w.config.ReadPreferredGroupShare = func(_ context.Context, in companion.Input, m linuxnative.Message) (string, MediaObservationEvidence, error) {
		if m.ID != share.ID || in.Source == nil {
			return "", MediaObservationEvidence{}, errors.New("share_group_source_unavailable")
		}
		return "已读取的真实视频片段", MediaObservationEvidence{ObservedRange: "frames 0-5", Excerpt: "已读取的真实视频片段", Parts: []companion.ObservationPart{{Kind: companion.ObservationVisualFrames, Range: "frames 0-5", Excerpt: "已读取的真实视频片段"}}}, nil
	}
	w.runtime.Inbox = []inboxItem{{Event: fixtureEvent("1", share.ID, room, "incoming", "remote"), Phase: "received", Backlog: true, Updated: time.Now()}}
	if err := w.processConversationBatch(t.Context(), room, nil); err != nil {
		t.Fatal(err)
	}
	if !sawEvidence {
		t.Fatal("startup anchor reached the model without its actual share evidence")
	}
}

func TestHostGroupShareWorkerRegressionGatesAndEvidence(t *testing.T) {
	w, e, _ := backgroundWorker(t, generationModelFunc(func(context.Context, string, []map[string]string) (string, error) {
		return `{"action":"silent","messages":[]}`, nil
	}))
	room := "host-share@chatroom"
	w.allowed[room] = true
	w.config.GroupRules = map[string]GroupRule{room: {Mode: "mention"}}

	// An ordinary group bubble remains quiet. The host rule cannot turn any
	// unaddressed text into a share turn.
	plain := fixtureMessage("plain", room, "大家晚上好", "incoming")
	if w.preferredGroupShare(plain) {
		t.Fatal("plain group text was treated as a share")
	}
	if _, ok := w.promptWithSharePreference(plain, time.Now(), false); ok {
		t.Fatal("ordinary unaddressed group text became eligible")
	}

	channels := fixtureMessage("channels", room, "", "incoming")
	channels.Type, channels.SenderID = 49, "member-a"
	subtype := 51
	channels.AppMessageType = &subtype
	w.config.PreferredGroupShare = func(m linuxnative.Message) bool { return m.ID == channels.ID }
	callbackCalls := 0
	w.config.ReadPreferredGroupShare = func(_ context.Context, in companion.Input, m linuxnative.Message) (string, MediaObservationEvidence, error) {
		callbackCalls++
		if m.ID != channels.ID || in.Source == nil {
			return "", MediaObservationEvidence{}, errors.New("share_group_source_unavailable")
		}
		if _, ok := e.Store.SourceForInput(in); !ok {
			return "", MediaObservationEvidence{}, errors.New("share_group_source_unavailable")
		}
		return "实际音轨：这是一段已经解码的内容。", MediaObservationEvidence{ObservedRange: "audio 0-3 seconds", Excerpt: "这是一段已经解码的内容。", Parts: []companion.ObservationPart{{Kind: companion.ObservationAudioTranscript, Range: "0-3 seconds", Excerpt: "这是一段已经解码的内容。"}}}, nil
	}
	if !w.preferredGroupShare(channels) {
		t.Fatal("host-authorized Channels card was not eligible")
	}
	working := channels
	working.Type, working.Body = 1, "收到了一条分享，结合会话偏好判断是否读取内容并回复。"
	if _, ok := w.promptWithSharePreference(working, time.Now(), true); !ok {
		t.Fatal("host-authorized Channels card was blocked by mention mode")
	}
	old := working
	old.CreatedAt = time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	if _, ok := w.promptWithSharePreference(old, time.Now(), true); ok {
		t.Fatal("old share bypassed normal time gate")
	}

	original := ""
	in := companion.Input{Channel: w.config.Channel, Account: w.source.Account(), Thread: room, Sender: channels.SenderID, TraceID: channels.ID, Text: working.Body, OriginalText: &original, MessageTime: channels.CreatedAt, Mode: "direct", IsGroup: true}
	bindInputSource(&in, channels, e)
	if err := w.preparePreferredGroupShare(t.Context(), e, &in, channels); err != nil {
		t.Fatal(err)
	}
	if callbackCalls != 1 || len(in.IncomingMedia) != 1 || in.IncomingMedia[0].ObservationState != "observed" || !companion.HasFirsthandObservation(in.IncomingMedia[0].ObservationParts) {
		t.Fatalf("host evidence was not retained: calls=%d input=%+v", callbackCalls, in.IncomingMedia)
	}

	// Publisher/card text alone is not a source for an automatic reply. The
	// failure happens before any model invocation or outbound delivery.
	w.config.ReadPreferredGroupShare = func(context.Context, companion.Input, linuxnative.Message) (string, MediaObservationEvidence, error) {
		return "标题：仅卡片", MediaObservationEvidence{Parts: []companion.ObservationPart{{Kind: companion.ObservationPublisherDescription, Range: "card", Excerpt: "标题：仅卡片"}}}, nil
	}
	failure := channels
	failure.ID = "channels-card-only"
	failure.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	w.config.PreferredGroupShare = func(m linuxnative.Message) bool { return m.ID == failure.ID }
	in = companion.Input{Channel: w.config.Channel, Account: w.source.Account(), Thread: room, Sender: failure.SenderID, TraceID: failure.ID, Text: working.Body, OriginalText: &original, MessageTime: failure.CreatedAt, Mode: "direct", IsGroup: true}
	bindInputSource(&in, failure, e)
	if err := w.preparePreferredGroupShare(t.Context(), e, &in, failure); err == nil || observationReason(err) != "share_group_observation_not_firsthand" {
		t.Fatalf("card-only evidence was accepted: %v", err)
	}
}
