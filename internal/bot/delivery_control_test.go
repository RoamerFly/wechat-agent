package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type controlTextNative struct{ *mediaFake }

func (n controlTextNative) Catalog(context.Context, string, string, string, string) (linuxnative.CatalogPage, error) {
	return linuxnative.CatalogPage{}, nil
}

func TestHostStopCancelsFutureMediaAndCaptionAcrossRestart(t *testing.T) {
	w, n, engine, in := mediaWorker(t)
	stopped := false
	w.config.DeliveryCancelled = func(request companion.Input) (bool, error) {
		return stopped && request.Account == in.Account && request.Thread == in.Thread && request.TraceID == in.TraceID, nil
	}
	w.queueMedia(t.Context(), engine, in, companion.GeneratedMedia{ID: "first-song", Kind: "voice"})
	d := w.runtime.Media[0]
	n.status.VoiceChunks = []linuxnative.VoiceChunkStatus{
		{Index: 0, State: "accepted", ServerID: "100", HasReceipt: true},
		{Index: 1, State: "submitting"}, {Index: 2, State: "cancelled"},
	}
	d.Status = n.status
	d.Next = time.Now().Add(time.Hour) // Host stop must bypass pacing/backoff.
	w.runtime.Deliveries = append(w.runtime.Deliveries, &delivery{Trace: in.TraceID, Key: "caption", State: "prepared", Thread: in.Thread, Input: in})
	other := in
	other.Sender, other.TraceID = "other-person", "other-turn"
	w.runtime.Deliveries = append(w.runtime.Deliveries, &delivery{Trace: other.TraceID, Key: "other", State: "prepared", Thread: in.Thread, Input: other})
	stopped = true
	w.applyDeliveryCancellations()
	if !d.UserCancelled || n.cancels != 1 || w.runtime.Deliveries[0].State != "stopped" || w.runtime.Deliveries[1].Cancelled {
		t.Fatal("host stop missed original turn or crossed request", d, n.cancels, w.runtime.Deliveries)
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	w.applyDeliveryCancellations()
	if w.taskActive(in) || !w.taskActive(other) {
		t.Fatal("restart changed cancellation scope")
	}
	if !w.mediaTraceCancelled(in.TraceID) {
		t.Fatal("late media callback can revive old turn")
	}
	w.runtime.Media[0].Next = time.Time{}
	w.reconcileMedia(t.Context())
	if n.sends != 1 || n.retries != 0 || !w.runtime.Media[0].Status.VoiceChunks[0].HasReceipt {
		t.Fatal("cancelled send was replayed or receipt lost", n.sends, n.retries)
	}
}

func TestHostStopBetweenCheckpointAndNativeSubmitNeverSends(t *testing.T) {
	w, n, engine, in := mediaWorker(t)
	w.config.WithDeliverySubmit = func(_ context.Context, got companion.Input, _ func() error) error {
		if got.TraceID != in.TraceID {
			t.Fatal("lost originating turn")
		}
		return ErrDeliveryCancelled
	}
	w.queueMedia(t.Context(), engine, in, companion.GeneratedMedia{ID: "song", Kind: "voice"})
	if n.sends != 0 || !w.runtime.Media[0].UserCancelled || w.runtime.Media[0].State != "stopped" {
		t.Fatal("native submit ran after host fence", n.sends, w.runtime.Media[0])
	}
	// The same boundary protects text without converting a blocked call into
	// an unknown native submission that could later be replayed.
	d := &delivery{Trace: in.TraceID, Key: "text", Thread: in.Thread, Input: in, Text: "caption", State: "prepared"}
	w.runtime.Deliveries = append(w.runtime.Deliveries, d)
	w.runtime.SendGates = nil
	_, _ = w.submitDelivery(t.Context(), controlTextNative{n}, d, false)
	if n.sends != 0 || !d.Cancelled || d.SubmittedText != "" || d.State != "stopped" {
		t.Fatal("text fence inferred a native submission", n.sends, d)
	}
}

func TestHostControlReadFailureBlocksNewWork(t *testing.T) {
	w, n, engine, in := mediaWorker(t)
	w.config.DeliveryCancelled = func(companion.Input) (bool, error) { return false, errors.New("synthetic disk read failure") }
	if w.taskActive(in) || w.persistErr == nil {
		t.Fatal("unreadable durable stop fence was ignored")
	}
	w.queueMedia(t.Context(), engine, in, companion.GeneratedMedia{ID: "song", Kind: "voice"})
	if n.sends != 0 {
		t.Fatal("corrupt control journal allowed submission")
	}
}

func TestMediaAuthorityChangesInsideSubmitFence(t *testing.T) {
	w, native, engine, in := mediaWorker(t)
	revoked := false
	w.config.WithDeliverySubmit = func(_ context.Context, _ companion.Input, submit func() error) error {
		revoked = true
		return submit()
	}
	w.config.AuthorizeMediaSubmit = func(_ context.Context, _ companion.Input, _, _ string) bool {
		if revoked {
			t.Fatal("native source lookup held the cancellation fence")
		}
		return true
	}
	w.config.AuthorizeMediaAsset = func(got companion.Input, kind, asset string) bool {
		if got.TraceID != in.TraceID || kind != "voice" || asset != "prepared-media" {
			t.Fatalf("submission lost media or origin: %s %s %s", got.TraceID, kind, asset)
		}
		return !revoked
	}
	w.queueMedia(t.Context(), engine, in, companion.GeneratedMedia{ID: "test-media", Kind: "voice", AssetID: "prepared-media"})
	d := w.runtime.Media[0]
	if native.sends != 0 || d.State != "stopped" || d.Reason != "media_source_or_authorization_changed" || d.UserCancelled {
		t.Fatal("revoked media was submitted or reported as a user cancellation", native.sends, d)
	}
}
