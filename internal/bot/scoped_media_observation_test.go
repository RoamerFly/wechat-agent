package bot

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type scopedTestAnalyzer struct {
	kind    int
	enabled bool
	analyze func(context.Context, linuxnative.Message) (string, error)
}

func (a scopedTestAnalyzer) Analyze(ctx context.Context, m linuxnative.Message) (string, error) {
	return a.analyze(ctx, m)
}

func (a scopedTestAnalyzer) HandlesType(kind int) bool { return kind == a.kind }

func (a scopedTestAnalyzer) SupportsMedia(_ context.Context, m linuxnative.Message) bool {
	return a.enabled && a.HandlesType(m.Type) && m.ID == "media-sample"
}

func TestScopedMediaObservationReachesPrivateAndGroupedTurns(t *testing.T) {
	for _, scenario := range []struct {
		name, thread   string
		group, backlog bool
		kind           int
		disabled       bool
	}{
		{name: "private voice", thread: "friend", kind: 34},
		{name: "voice in group live window", thread: "room@chatroom", group: true, kind: 34},
		{name: "voice in group after reconnect", thread: "room@chatroom", group: true, backlog: true, kind: 34},
		{name: "private image", thread: "friend", kind: 3},
		{name: "image in group live window", thread: "room@chatroom", group: true, kind: 3},
		{name: "image in group after reconnect", thread: "room@chatroom", group: true, backlog: true, kind: 3},
		{name: "image before opt-in", thread: "room@chatroom", group: true, kind: 3, disabled: true},
		{name: "private video", thread: "friend", kind: 43},
		{name: "video in group live window", thread: "room@chatroom", group: true, kind: 43},
		{name: "video in group after reconnect", thread: "room@chatroom", group: true, backlog: true, kind: 43},
		{name: "video before opt-in", thread: "room@chatroom", group: true, kind: 43, disabled: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const observation = "合成媒体观察记号"
			var modelCalls, reads atomic.Int32
			w, _, _ := backgroundWorker(t, generationModelFunc(func(_ context.Context, _ string, messages []map[string]string) (string, error) {
				modelCalls.Add(1)
				if strings.Contains(messages[len(messages)-1]["content"], observation) == scenario.disabled {
					t.Error("model received incorrect media observation coverage")
				}
				return `{"action":"silent"}`, nil
			}))
			media := fixtureMessage("media-sample", scenario.thread, "[媒体]", "incoming")
			media.SenderID, media.ServerID, media.Type = "sender", "9001", scenario.kind
			w.config.Options.ReadImages = false // The scoped host, not the legacy option, owns admission.
			w.config.ScopedAnalyzer = scopedTestAnalyzer{kind: scenario.kind, enabled: !scenario.disabled, analyze: func(ctx context.Context, m linuxnative.Message) (string, error) {
				reads.Add(1)
				ref, ok := companion.MediaSourceFromContext(ctx)
				if !ok || ref.MessageID != m.ID || ref.Sender != m.SenderID || ref.Thread != scenario.thread || ref.MessageType != scenario.kind {
					t.Error("media analyzer missing persisted source identity")
				}
				return observation, nil
			}}
			w.config.GroupRules = map[string]GroupRule{scenario.thread: {Mode: "all"}}
			w.source = &fakeSource{messages: map[string]linuxnative.Message{media.ID: media}}
			event := fixtureEvent("1", media.ID, scenario.thread, "incoming", "remote")
			w.runtime.Inbox = append(w.runtime.Inbox, inboxItem{Event: event, Phase: "received", Backlog: scenario.backlog})
			w.enableGenerations()
			defer w.closeGenerations()
			switch {
			case scenario.backlog:
				if err := w.processUnreadConversation(t.Context(), scenario.thread); err != nil {
					t.Fatal(err)
				}
			case scenario.group:
				if err := w.processConversationBatch(t.Context(), scenario.thread, map[string]bool{media.ID: true}); err != nil {
					t.Fatal(err)
				}
			default:
				w.handle(t.Context(), event)
			}
			pumpGeneration(t, w, func() bool { return w.generation.occupied() == 0 })
			wantReads := int32(1)
			if scenario.disabled {
				wantReads = 0
			}
			if reads.Load() != wantReads || modelCalls.Load() != 1 {
				t.Fatalf("media reads=%d decisions=%d, want %d and 1", reads.Load(), modelCalls.Load(), wantReads)
			}
		})
	}
}

func TestUnreadableScopedVideoDoesNotBlockIndependentGroupText(t *testing.T) {
	for _, scenario := range []struct {
		name                                       string
		backlog, quoteVideo, sameSender, videoOnly bool
		continues                                  bool
	}{
		{name: "separate directed text", continues: true},
		{name: "later video after reconnect", backlog: true, continues: true},
		{name: "text explicitly quotes unreadable video", quoteVideo: true},
		{name: "same requester may mean their video", sameSender: true},
		{name: "video is the only request", videoOnly: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const thread = "room@chatroom"
			const videoID = "media-sample"
			const textID = "text-request"
			var decisions atomic.Int32
			w, _, _ := backgroundWorker(t, generationModelFunc(func(_ context.Context, _ string, messages []map[string]string) (string, error) {
				decisions.Add(1)
				data := messages[len(messages)-1]["content"]
				if !strings.Contains(data, "请告诉我明天星期几") || !strings.Contains(data, "未取得内容证据") {
					t.Error("independent request or video failure missing from the decision")
				}
				return `{"action":"silent"}`, nil
			}))
			video := fixtureMessage(videoID, thread, "[视频]", "incoming")
			video.Type, video.SenderID, video.ServerID = 43, "video-sender", "10001"
			text := fixtureMessage(textID, thread, "请告诉我明天星期几", "incoming")
			text.SenderID, text.ServerID = "text-sender", "10002"
			yes := true
			text.Mentions.State, text.Mentions.Self = "known", &yes
			if scenario.quoteVideo {
				text.ResolvedQuote = &video
			}
			if scenario.sameSender {
				text.SenderID = video.SenderID
			}
			messages := []linuxnative.Message{video, text}
			if scenario.backlog {
				messages[0], messages[1] = text, video
			}
			if scenario.videoOnly {
				messages = messages[:1]
			}
			w.config.GroupRules = map[string]GroupRule{thread: {Mode: "all"}}
			w.config.ScopedAnalyzer = scopedTestAnalyzer{kind: 43, enabled: true, analyze: func(context.Context, linuxnative.Message) (string, error) {
				return "", errors.New("video_probe_invalid")
			}}
			source := &fakeSource{messages: map[string]linuxnative.Message{}}
			live := map[string]bool{}
			for i, m := range messages {
				m.CreatedAt = time.Now().Add(time.Duration(i-10) * time.Second).UTC().Format(time.RFC3339Nano)
				source.messages[m.ID] = m
				live[m.ID] = true
				w.runtime.Inbox = append(w.runtime.Inbox, inboxItem{Event: fixtureEvent("1", m.ID, thread, "incoming", "remote"), Phase: "received", Backlog: scenario.backlog})
			}
			w.source = source
			var activity []Record
			w.config.Record = func(r Record) { activity = append(activity, r) }
			w.enableGenerations()
			defer w.closeGenerations()
			if scenario.backlog {
				live = nil
			}
			if err := w.processConversationBatch(t.Context(), thread, live); err != nil {
				t.Fatal(err)
			}
			pumpGeneration(t, w, func() bool { return w.generation.occupied() == 0 })
			eventName, expectedDecisions := "group_input_read_failed", int32(0)
			if scenario.continues {
				eventName, expectedDecisions = "background_video_read_failed", 1
			}
			found := false
			for _, r := range activity {
				found = found || r.Event == eventName && r.RelatedMessageID == videoID && r.Reason == "video_probe_invalid"
			}
			if decisions.Load() != expectedDecisions || !found {
				t.Fatalf("decisions=%d, want %d and %s, activity=%+v", decisions.Load(), expectedDecisions, eventName, activity)
			}
		})
	}
}
