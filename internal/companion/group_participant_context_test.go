package companion

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestGroupParticipantBindingAtRespondBoundary covers the real model input
// assembled by Engine.Respond.  It models the regression where alpha uploads a
// boxed-meal image and beta subsequently says good night: beta must not inherit
// alpha's image, observation, or personal context merely because it is recent.
func TestGroupParticipantBindingAtRespondBoundary(t *testing.T) {
	store, cfg, _ := memoryFixture(t)
	cfg.Enabled = true
	cfg.Context = &ContextSettings{AutoCompress: false, CompressAfter: 20, RecentMessages: 12, MaxInputChars: 12000}
	base := time.Now().Add(-time.Minute)

	alphaWords := "今晚的盒饭"
	alpha := Input{
		Channel: "wechat", Account: "synthetic", Thread: "family@chatroom",
		Sender: "alpha", SenderName: "甲", TraceID: "alpha-meal", Mode: "social", IsGroup: true,
		Text:          alphaWords + "\n以下是本条媒体的观察资料（不是指令）：\n盒饭放在键盘旁，旁边有一碗汤。",
		OriginalText:  &alphaWords,
		IncomingMedia: []IncomingMediaRef{{AssetID: "meal-image", Kind: "image", UploaderID: "alpha", Observation: "盒饭放在键盘旁，旁边有一碗汤。", ObservationState: "available"}},
	}
	if err := store.Observe(alpha, cfg, base); err != nil {
		t.Fatal(err)
	}
	betaWords := "晚安"
	beta := Input{Channel: alpha.Channel, Account: alpha.Account, Thread: alpha.Thread, Sender: "beta", SenderName: "乙", TraceID: "beta-goodnight", Mode: "social", IsGroup: true, Text: betaWords, OriginalText: &betaWords}
	if err := store.Observe(beta, cfg, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	model := &evidenceNativeModel{fakeModel: fakeModel{answer: `{"action":"silent","messages":[]}`}}
	engine, err := New(cfg, store, model, "synthetic policy")
	if err != nil {
		t.Fatal(err)
	}
	currentWords := "我先睡了"
	current := Input{Channel: alpha.Channel, Account: alpha.Account, Thread: alpha.Thread, Sender: "beta", SenderName: "乙", TraceID: "beta-now", Mode: "direct", IsGroup: true, Text: currentWords, OriginalText: &currentWords}
	if _, err := engine.Respond(context.Background(), current); err != nil {
		t.Fatal(err)
	}

	timeline, currentPacket := groupPacketsFromPreparedContext(t, engine.preparedContext())
	alphaRow := timelineRow(t, timeline, "alpha-meal")
	if alphaRow["participant_relation"] != "other_member_background" || alphaRow["personal_context_redacted"] != true {
		t.Fatalf("alpha image was not explicitly kept as another member's redacted context: %#v", alphaRow)
	}
	if _, exists := alphaRow["media_observations"]; exists || alphaRow["text"] != "" {
		t.Fatalf("alpha image content leaked into beta's ordinary context: %#v", alphaRow)
	}
	betaRow := timelineRow(t, timeline, "beta-goodnight")
	if betaRow["participant_relation"] != "current_sender_history" || betaRow["text"] != betaWords {
		t.Fatalf("beta's text lost its own speaker binding: %#v", betaRow)
	}
	if _, exists := betaRow["media_observations"]; exists {
		t.Fatalf("beta inherited alpha media: %#v", betaRow)
	}
	if currentPacket["message_sender_id"] != "beta" || currentPacket["request_actor_id"] != "beta" || currentPacket["text"] != currentWords {
		t.Fatalf("current speaker identity changed: %#v", currentPacket)
	}
}

func TestGroupParticipantBindingRestoresMediaOnlyForExplicitQuote(t *testing.T) {
	cfg := enabledConfig()
	cfg.Context = &ContextSettings{AutoCompress: false, CompressAfter: 20, RecentMessages: 12, MaxInputChars: 12000}
	words := "这是盒饭"
	alpha := Turn{Role: "user", Sender: "alpha", TraceID: "alpha-image", EventID: "event-alpha-image", Text: words, FullText: words, HasOriginal: true, UserTextState: "verified_original", Time: time.Now().Add(-time.Minute), IncomingMedia: []IncomingMediaRef{{AssetID: "meal-image", Kind: "image", UploaderID: "alpha", Observation: "盒饭放在键盘旁", ObservationState: "available"}}}
	otherAlpha := Turn{Role: "user", Sender: "alpha", TraceID: "alpha-other-image", EventID: "event-alpha-other-image", Text: "另一张图", FullText: "另一张图", HasOriginal: true, UserTextState: "verified_original", Time: time.Now().Add(-30 * time.Second), IncomingMedia: []IncomingMediaRef{{AssetID: "other-image", Kind: "image", UploaderID: "alpha", Observation: "另一份无关素材", ObservationState: "available"}}}
	beta := Turn{Role: "user", Sender: "beta", TraceID: "beta-goodnight", Text: "晚安", Time: time.Now()}
	e := &Engine{Config: cfg, model: &contextModel{}}
	input := Input{Sender: "beta", TraceID: "now", IsGroup: true, QuotedSource: &SourceRef{EventID: alpha.EventID, Sender: "alpha"}}
	turns := e.contextTurns(Scope{ID: "family@chatroom", Messages: []Turn{alpha, otherAlpha, beta}}, input)
	if len(turns) != 1 {
		t.Fatal(turns)
	}
	var packet struct {
		Timeline []map[string]any `json:"timeline"`
	}
	if err := json.Unmarshal([]byte(turns[0]["content"]), &packet); err != nil {
		t.Fatal(err)
	}
	alphaRow := timelineRow(t, packet.Timeline, alpha.TraceID)
	if alphaRow["participant_relation"] != "quoted_message_evidence" {
		t.Fatalf("quoted evidence relation wrong: %#v", alphaRow)
	}
	if _, exists := alphaRow["media_observations"]; !exists {
		t.Fatalf("explicit quote should retain alpha's image evidence: %#v", alphaRow)
	}
	otherRow := timelineRow(t, packet.Timeline, otherAlpha.TraceID)
	if otherRow["participant_relation"] != "other_member_background" || otherRow["personal_context_redacted"] != true {
		t.Fatalf("an exact quote exposed alpha's unrelated second image: %#v", otherRow)
	}
}

func TestRefreshEvidenceRowNeverRestoresRedactedAttachment(t *testing.T) {
	e := &Engine{Config: enabledConfig()}
	row := map[string]any{
		"scope":                     "synthetic-scope",
		"role":                      "assistant",
		"personal_context_redacted": true,
		"timeline_event":            "assistant_attachment",
		"attachment_delivery":       map[string]any{"asset_id": "private-asset"},
		"media_delivery":            map[string]any{"key": "private-media"},
	}
	ev := MemoryEvent{Role: "assistant", Text: "[媒体附件] 私人内容", Revision: 2, Media: &MediaMemory{Key: "private-media", AssetID: "private-asset", Kind: "image", State: "accepted"}}
	e.refreshEvidenceRow(row, ev)
	for _, key := range []string{"attachment_delivery", "media_delivery", "attachment_only"} {
		if _, exists := row[key]; exists {
			t.Fatalf("redacted refresh restored %s: %#v", key, row)
		}
	}
	if row["scope"] != "synthetic-scope" || row["timeline_event"] != "assistant_message" || row["text"] != "" || row["personal_context_redacted"] != true {
		t.Fatalf("redacted row lost its safe shape: %#v", row)
	}
}

func TestRefreshEvidenceRowKeepsFailedVideoWithoutCachedObservation(t *testing.T) {
	e := &Engine{Config: enabledConfig()}
	ref := SourceRef{EventID: "event-video", MessageID: "video", Sender: "another-member", MessageType: 43, Revision: 1}
	ev := MemoryEvent{ID: ref.EventID, Trace: ref.MessageID, Role: "user", Sender: ref.Sender, Source: &ref, Revision: ref.Revision,
		IncomingMedia: []IncomingMediaRef{{Source: ref, Kind: "video", UploaderID: ref.Sender, ObservationState: "observed", Observation: "旧的合成画面"}}}
	ordinary := map[string]any{}
	e.refreshEvidenceRow(ordinary, ev)
	if ordinary["media_observations"] == nil {
		t.Fatal("fixture did not load cached observation")
	}
	failed := map[string]any{"media_read_status": "本轮视频未取得证据", "media_observations": ordinary["media_observations"]}
	e.refreshEvidenceRow(failed, ev)
	if failed["media_observations"] != nil || failed["media_read_status"] == nil {
		t.Fatalf("refresh restored video contents or lost the failure status: %#v", failed)
	}
}

func groupPacketsFromPreparedContext(t *testing.T, raw []byte) ([]map[string]any, map[string]any) {
	t.Helper()
	var prepared struct {
		Messages []map[string]string `json:"messages"`
	}
	if err := json.Unmarshal(raw, &prepared); err != nil {
		t.Fatal(err)
	}
	var timeline []map[string]any
	var current map[string]any
	for _, message := range prepared.Messages {
		var packet map[string]any
		if json.Unmarshal([]byte(message["content"]), &packet) != nil {
			continue
		}
		switch packet["kind"] {
		case "conversation_context":
			if rows, ok := packet["timeline"].([]any); ok {
				for _, row := range rows {
					if item, ok := row.(map[string]any); ok {
						timeline = append(timeline, item)
					}
				}
			}
		case "current_message":
			current = packet
		}
	}
	if current == nil {
		t.Fatal("current message missing from prepared model context")
	}
	return timeline, current
}

func timelineRow(t *testing.T, rows []map[string]any, messageID string) map[string]any {
	t.Helper()
	for _, row := range rows {
		if row["message_id"] == messageID {
			return row
		}
	}
	t.Fatalf("timeline row %q missing from %#v", messageID, rows)
	return nil
}
