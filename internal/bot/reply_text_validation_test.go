package bot

import (
	"context"
	"errors"
	"testing"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/sending"
)

func TestPausedConnectionKeepsPreparedDeliveryAndResumesOriginalKey(t *testing.T) {
	w, _, n, in, _ := memberReplyTextFixture(t)
	row := &delivery{Trace: in.TraceID, Thread: in.Thread, Input: in, Text: "Beta，晚安。", Key: "original-key", State: "prepared"}
	w.runtime.Deliveries = []*delivery{row}
	w.config.BeforeSubmit = func(context.Context) error { return errors.New("paused") }
	if _, err := w.submitDelivery(t.Context(), n, row, true); !errors.Is(err, sending.ErrOutcomeUnknown) || row.State != "prepared" || len(n.submissions) != 0 {
		t.Fatal("paused worker lost its prepared intent or submitted it", row.State, err)
	}
	w.config.BeforeSubmit = nil
	if _, err := w.submitDelivery(t.Context(), n, row, true); err != nil || row.State != "accepted" ||
		len(n.submissions) != 1 || n.submissions[0].IdempotencyKey != row.Key {
		t.Fatal("resume did not submit the retained intent", row.State, err, n.submissions)
	}
}

func memberReplyTextFixture(t *testing.T) (*Worker, *companion.Engine, *acceptingProgressNative, companion.Input, *planReviewModel) {
	t.Helper()
	m := &planReviewModel{}
	w, e, n, in := planWorker(t, m)
	in.Thread, in.TraceID, in.Sender = "room@chatroom", "beta-source", "member-beta"
	in.IsGroup, in.Text, in.SenderName, in.SenderNameSource = true, "晚安", "Beta", "native"
	in = observeSyntheticMediaRequest(t, w, e, in)
	other := in
	other.TraceID, other.Sender, other.SenderName, other.Text = "alpha-source", "member-alpha", "Alpha", "合成参与者消息"
	observeSyntheticMediaRequest(t, w, e, other)
	return w, e, n, in, m
}

func TestFinalTextSubmissionRejectsWrongNamedMemberWithoutModelCall(t *testing.T) {
	w, _, n, in, m := memberReplyTextFixture(t)
	if _, err := w.sendText(context.Background(), in, 0, "Alpha，晚安"); err == nil || len(n.submissions) != 0 {
		t.Fatal("wrong member vocative reached native submit", err, n.submissions)
	}
	d := &delivery{Trace: in.TraceID, Thread: in.Thread, Input: in, Text: "Alpha，晚安", Key: "synthetic-prepared-text", State: "prepared"}
	if _, err := w.submitDelivery(context.Background(), n, d, true); err == nil || len(n.submissions) != 0 {
		t.Fatal("prepared text bypassed final text check", err, n.submissions)
	}
	if m.calls != 0 {
		t.Fatal("deterministic text admission added a model call", m.calls)
	}
}

func TestTextAdmissionKeepsQuotationAndAcceptedReceiptSemantics(t *testing.T) {
	w, _, n, in, m := memberReplyTextFixture(t)
	text := "Beta，晚安。这里转述一句“Alpha，晚安”。"
	if _, err := w.sendText(context.Background(), in, 0, text); err != nil {
		t.Fatal("quoted narration was treated as an outgoing addressee", err)
	}
	if _, err := w.sendText(context.Background(), in, 0, "Alpha，晚安"); err != nil || len(n.submissions) != 1 {
		t.Fatal("accepted key was revalidated or resent", err, n.submissions)
	}
	if m.calls != 0 {
		t.Fatal("text validation invoked a model")
	}
}

func TestProgressAndMediaCaptionUseFinalTextAdmission(t *testing.T) {
	for _, path := range []string{"progress", "caption"} {
		t.Run(path, func(t *testing.T) {
			w, e, n, in, _ := memberReplyTextFixture(t)
			if path == "progress" {
				sent, err := w.toolProgress(e, in)(context.Background(), "Alpha，稍等一下")
				if err != nil || sent {
					t.Fatal("invalid progress was accepted", sent, err)
				}
			} else {
				w.deferMediaReply(in, []string{"Alpha，给你这段合成素材"}, 1)
				w.runtime.Media = []*mediaDelivery{{Trace: in.TraceID, Input: in, Kind: "voice", State: "accepted", Status: linuxnative.ToolStatus{State: "accepted", ServerID: "synthetic-receipt"}}}
				w.runtime.MediaReplies[0].Next = time.Now().Add(-time.Second)
				w.reconcileMediaReplies(context.Background())
				if w.runtime.MediaReplies[0].State != "stopped" {
					t.Fatal("invalid caption did not stop", w.runtime.MediaReplies[0].State)
				}
			}
			if len(n.submissions) != 0 {
				t.Fatal("invalid text path submitted", n.submissions)
			}
		})
	}
}
