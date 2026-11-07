package bot

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

func TestVoiceAndFileSerializeAndPersistRandomGap(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	c.Config.Permissions.SendFile = true
	w.config.Options.RandomDelay = true
	w.config.Options.DelayMinSeconds = 3
	w.config.Options.DelayMaxSeconds = 8
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "voice-a", Kind: "voice", Title: "song.wav"})
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "file-b", Kind: "file", Title: "song.flac"})
	if n.sends != 1 || w.runtime.Media[1].State != "prepared" {
		t.Fatal("formats sent concurrently", n.sends)
	}
	first, second := w.runtime.Media[0], w.runtime.Media[1]
	n.status.State = "accepted"
	n.status.ServerID = "verified-pacing-receipt"
	first.Next = time.Time{}
	w.reconcileMedia(context.Background())
	gap := second.NotBefore.Sub(first.CompletedAt)
	if n.sends != 1 || gap < 3*time.Second || gap > 8*time.Second {
		t.Fatal("random gap missing", gap, n.sends)
	}
	deadline := second.NotBefore
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	second = w.runtime.Media[1]
	if !second.NotBefore.Equal(deadline) {
		t.Fatal("restart lost pacing deadline")
	}
	second.NotBefore = time.Now().Add(-time.Second)
	second.Next = time.Time{}
	w.runtime.SendGates = nil
	w.reconcileMedia(context.Background())
	if n.sends != 2 {
		t.Fatal("file not delivered after gap", n.sends)
	}
}

func TestSuccessfulSpeechDeliveryRetainsVerifiedSynthesisText(t *testing.T) {
	w, n, engine, in := mediaWorker(t)
	if !w.queueMedia(context.Background(), engine, in, companion.GeneratedMedia{ID: "speech-source", Kind: "voice", Title: "speech.wav", SynthesisText: "cloth。fabric。", SynthesisTextKnown: true}) {
		t.Fatal("speech was not queued")
	}
	if len(w.runtime.Media) != 1 || !w.runtime.Media[0].SynthesisTextKnown {
		t.Fatal("delivery lost synthesis provenance", w.runtime.Media)
	}
	n.status.State, n.status.ServerID = "accepted", "speech-receipt"
	w.runtime.Media[0].Next = time.Time{}
	w.reconcileMedia(context.Background())
	media := []companion.MediaMemory{}
	for _, turn := range engine.Store.Context(in, engine.Config, time.Now()).Messages {
		if turn.Media != nil {
			media = append(media, *turn.Media)
		}
	}
	if len(media) != 1 || media[0].State != "accepted" || !media[0].SynthesisTextKnown || media[0].SynthesisText != "cloth。fabric。" {
		t.Fatal("receipt or verified script missing", media)
	}
}

type mediaFake struct {
	onStatus   func()
	lookupErrs []error
	fakeSender
	status                  linuxnative.ToolStatus
	resumeStatus            linuxnative.ToolStatus
	resumeErr               error
	sends, retries, cancels int
	resumes                 int
	keys                    map[string]bool
}

func TestUnknownOldSurfDoesNotBlockExplicitRequestButPreservesBatchOrder(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	old := in
	old.TraceID = "old-surf"
	old.Surf = true
	w.runtime.Media = append(w.runtime.Media, &mediaDelivery{Key: "old-key", Trace: old.TraceID, Input: old, ID: "old-job", Kind: "voice", State: "waiting", Status: linuxnative.ToolStatus{State: "needs_review"}, Next: time.Now().Add(time.Hour)})
	w.runtime.Media = append(w.runtime.Media, &mediaDelivery{Key: "unsent-surf", Trace: "another-surf", Input: old, Kind: "voice", State: "prepared"})
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "explicit-source", Kind: "voice", Title: "requested.wav"})
	if n.sends != 1 || n.status.SourceID != "explicit-source" || w.runtime.Media[0].State != "waiting" {
		t.Fatal("old surf blocked unrelated request", n.sends, n.status)
	}
	first := w.runtime.Media[2]
	first.State = "waiting"
	first.Status.State = "needs_review"
	first.Next = time.Now().Add(time.Hour)
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "part-two", Kind: "voice", Title: "part2.wav"})
	if n.sends != 1 {
		t.Fatal("same batch skipped unknown predecessor", n.sends)
	}
}

func (n *mediaFake) Submit(_ context.Context, r linuxnative.ToolSend) (linuxnative.ToolStatus, error) {
	n.sends++
	n.keys[r.IdempotencyKey] = true
	n.status = linuxnative.ToolStatus{ID: "job-test", State: "running", Phase: "pacing", ThreadID: r.ThreadID, SourceID: r.SourceID}
	return n.status, nil
}
func (n *mediaFake) LookupSend(_ context.Context, key string) (linuxnative.ToolStatus, error) {
	if len(n.lookupErrs) > 0 {
		err := n.lookupErrs[0]
		n.lookupErrs = n.lookupErrs[1:]
		return linuxnative.ToolStatus{}, err
	}
	if !n.keys[key] {
		return linuxnative.ToolStatus{}, linuxnative.ErrToolNotFound
	}
	return n.status, nil
}
func (n *mediaFake) ToolSendStatus(context.Context, string) (linuxnative.ToolStatus, error) {
	if n.onStatus != nil {
		n.onStatus()
	}
	return n.status, nil
}
func TestUpdateDrainWaitsForMediaWithoutCancellingOrTakingNewInbox(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "source", Kind: "voice", Title: "track.wav"})
	w.runtime.Media[0].Next = time.Time{}
	w.runtime.Inbox = append(w.runtime.Inbox, inboxItem{Event: fixtureEvent("1", "next", "filehelper", "incoming", "wechat"), Phase: "received", Updated: time.Now().UTC()})
	if e := w.saveRuntime(); e != nil {
		t.Fatal(e)
	}
	reads := 0
	n.onStatus = func() {
		reads++
		if reads >= 2 {
			n.status.State = "accepted"
			n.status.ServerID = "123"
		}
	}
	w.config.Draining = func() bool { return true }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := w.runDurable(ctx, filepath.Join(w.config.StateDir, "checkpoint.json"), checkpoint{ServerID: "server-1"})
	if e != nil || ctx.Err() != nil || reads < 2 || n.cancels != 0 || w.runtime.Media[0].State != "accepted" {
		t.Fatal("update interrupted media", e, reads, n.cancels)
	}
	if w.runtime.Inbox[0].Phase != "received" || w.source.(*fakeSource).reads != 0 {
		t.Fatal("drain consumed next message")
	}
}

func TestUpdateDrainParksPreparedMediaWithoutSubmissionOrCancellation(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	w.config.Draining = func() bool { return true }
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "source", Kind: "voice", Title: "prepared.wav"})
	if w.pendingMedia() {
		t.Fatal("never-submitted media blocked executable update")
	}
	w.suspendMedia()
	if n.cancels != 0 || w.runtime.Media[0].State != "prepared" || w.runtime.Media[0].ID != "" {
		t.Fatal("prepared work was lost during update")
	}
}
func (n *mediaFake) RetryMediaJob(context.Context, string) (linuxnative.ToolStatus, error) {
	n.retries++
	n.status.State = "running"
	n.status.Retryable = false
	return n.status, nil
}
func (n *mediaFake) CancelMediaJob(context.Context, string) (linuxnative.ToolStatus, error) {
	n.cancels++
	n.status.State = "cancelled"
	n.status.Retryable = true
	return n.status, nil
}
func (n *mediaFake) ResumeMissingMediaJob(context.Context, string, string) (linuxnative.ToolStatus, error) {
	n.resumes++
	return n.resumeStatus, n.resumeErr
}
func mediaWorker(t *testing.T) (*Worker, *mediaFake, *companion.Engine, companion.Input) {
	t.Helper()
	store, e := companion.NewStore(t.TempDir())
	if opened := store; opened != nil {
		t.Cleanup(func() { _ = opened.Close() })
	}
	if e != nil {
		t.Fatal(e)
	}
	c := companion.Defaults()
	c.Enabled = true
	c.Permissions.Remember = true
	engine, e := companion.New(c, store, generatedModel{}, "test")
	if e != nil {
		t.Fatal(e)
	}
	n := &mediaFake{keys: map[string]bool{}}
	opts := DefaultOptions()
	w, e := New(Config{StateDir: t.TempDir(), Options: &opts, AllowedThreads: []string{"filehelper", "room@chatroom"}}, &fakeSource{}, engine, n)
	if e != nil {
		t.Fatal(e)
	}
	if e = w.loadRuntime("server-1"); e != nil {
		t.Fatal(e)
	}
	in := companion.Input{Channel: "wechat", Account: "test", Thread: "filehelper", TraceID: "incoming", Sender: "requester", Mode: "direct", Text: "发音乐"}
	original := fixtureMessage(in.TraceID, in.Thread, in.Text, "incoming")
	original.SenderID = in.Sender
	original.ServerID = "77"
	in.MessageTime = original.CreatedAt
	in.OriginalText = &original.Body
	bindInputSource(&in, original, engine)
	if e = store.Observe(in, c, time.Now()); e != nil {
		t.Fatal(e)
	}
	r, e := engine.BindReplyEvidence(context.Background(), in)
	if e != nil {
		t.Fatal(e)
	}
	in.ReplyContext = &r
	return w, n, engine, in
}

func TestModelFileIntentCancelsOnlyRequestersOldVoiceAndSurvivesRestart(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	in.Thread, in.Sender = "room@chatroom", "requester"
	in = observeSyntheticMediaRequest(t, w, c, in)
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "voice-source", Kind: "voice", Title: "song.wav"})
	other := in
	other.Sender, other.TraceID = "other-user", "other-trace"
	other = observeSyntheticMediaRequest(t, w, c, other)
	w.queueMedia(context.Background(), c, other, companion.GeneratedMedia{ID: "other-source", Kind: "voice", Title: "other.wav"})
	// A legacy group record without a requester cannot be safely attributed to
	// the current member's new request.
	anonymous := other
	anonymous.Sender, anonymous.TraceID = "", "anonymous-trace"
	w.runtime.Media = append(w.runtime.Media, &mediaDelivery{Trace: anonymous.TraceID, Input: anonymous, ID: "anonymous-job", Kind: "voice", State: "running"})
	newRequest := in
	newRequest.TraceID, newRequest.Text = "new-request", "还是原版吧"
	w.cancelVoiceForIntent(context.Background(), newRequest, "file")
	if n.cancels != 1 || !w.runtime.Media[0].UserCancelled || w.runtime.Media[0].State != "cancelling" || w.runtime.Media[1].UserCancelled || w.runtime.Media[2].UserCancelled {
		t.Fatal("file intent failed or interrupted someone else", n.cancels, w.runtime.Media)
	}
	w.runtime.Media[0].Next = time.Time{}
	n.status.SourceID = "voice-source"
	w.reconcileMedia(context.Background())
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	if !w.runtime.Media[0].UserCancelled || w.runtime.Media[0].State != "stopped" {
		t.Fatal("cancellation lost on restart")
	}
}
func TestMediaPacingDoesNotBlockStopAndPersistsCancelledMemory(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	start := time.Now()
	if !w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "alias", SourceID: "cached-source", AssetID: "asset-test", Kind: "voice", Title: "song.wav"}) {
		t.Fatal("not handled")
	}
	if time.Since(start) > time.Second || n.sends != 1 || w.runtime.Media[0].State != "running" {
		t.Fatal("voice blocked or not queued")
	}
	m := fixtureMessage("stop", "filehelper", "发我音乐文件吧，不用发语音了", "outgoing")
	if !w.stopRequestedVoice(context.Background(), m) || n.cancels != 1 {
		t.Fatal("stop ignored")
	}
	d := w.runtime.Media[0]
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if d.State != "stopped" || !d.UserCancelled || n.retries != 0 || n.sends != 1 {
		t.Fatal("cancelled voice replayed")
	}
	scope := c.Store.Context(in, c.Config, time.Now())
	found := false
	for _, turn := range scope.Messages {
		if turn.Media != nil {
			found = true
			if turn.Media.State != "stopped" || turn.Media.SourceID != "cached-source" || turn.Media.AssetID != "asset-test" {
				t.Fatalf("wrong memory: %+v", turn.Media)
			}
		}
	}
	if !found {
		t.Fatal("voice missing from memory")
	}
	// Reload the real durable file: cancellation survives service restart.
	if e := w.loadRuntime("server-1"); e != nil {
		t.Fatal(e)
	}
	w.runtime.Media[0].Next = time.Time{}
	w.reconcileMedia(context.Background())
	if n.retries != 0 {
		t.Fatal("restart retried user stop")
	}
}

func TestNewSongCancelsOldVoiceAndItsLateCaptionAcrossRestart(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	w.deferMediaReply(in, []string{"找到 Cloud Dancer，分几段发你"}, 1)
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "old", Kind: "voice", Title: "Cloud Dancer"})
	message := fixtureMessage("new-song", "filehelper", "重新找一首，traveling light", "outgoing")
	message.SenderID = in.Sender
	if !w.stopRequestedVoice(context.Background(), message) || n.cancels != 1 || w.runtime.MediaReplies[0].State != "stopped" {
		t.Fatal("new request did not supersede old voice/caption")
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	w.reconcileMediaReplies(context.Background())
	w.runtime.Media[0].Next = time.Time{}
	w.reconcileMedia(context.Background())
	if n.calls != 0 || n.retries != 0 || !w.runtime.Media[0].UserCancelled {
		t.Fatal("stale caption or audio resumed")
	}
	for _, text := range []string{"不要换歌", "不用重新找，继续这首", "你们换首歌吧"} {
		if text != "你们换首歌吧" && replacesVoice(text) {
			t.Fatal("negated instruction cancelled current song")
		}
	}
}

func TestUnmentionedGroupStopCancelsOnlyRequestersVoiceTrace(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	in.Thread, in.Sender, in.TraceID = "room@chatroom", "requester", "requester-song"
	other := in
	other.Sender, other.TraceID = "other-member", "other-song"
	w.runtime.Media = []*mediaDelivery{
		{Trace: in.TraceID, Input: in, ID: "requester-job", Kind: "voice", State: "running"},
		{Trace: other.TraceID, Input: other, Kind: "voice", State: "prepared"},
	}
	m := fixtureMessage("stop-wrong-song", in.Thread, "发错了，不是这首歌，后面别发了", "incoming")
	w.runtime.Wakes = []*conversationWake{
		{Input: in, Parent: in.TraceID, State: "waiting"},
		{Input: other, Parent: other.TraceID, State: "waiting"},
	}
	m.SenderID = in.Sender
	if !w.stopRequestedVoice(context.Background(), m) {
		t.Fatal("unmentioned requester stop was ignored")
	}
	if w.runtime.Wakes[0].State != "stopped" || w.runtime.Wakes[1].State != "waiting" {
		t.Fatal("cancelled delivery and its future reassessment must share the same scope")
	}
	if n.cancels != 1 || !w.runtime.Media[0].UserCancelled || w.runtime.Media[0].CancelTrace != m.ID || w.runtime.Media[1].UserCancelled {
		t.Fatalf("wrong group delivery was stopped: cancels=%d first=%+v second=%+v", n.cancels, w.runtime.Media[0], w.runtime.Media[1])
	}
	// A late callback with a different source cannot resurrect the cancelled
	// request under the old trace.
	w.queueMedia(context.Background(), nil, in, companion.GeneratedMedia{ID: "late-plugin-source", Kind: "voice", Title: "late.wav"})
	if len(w.runtime.Media) != 2 {
		t.Fatal("late callback revived cancelled trace", w.runtime.Media)
	}
}

func TestGroupStopAmbiguityPausesOnlyRequestersCandidateTraces(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	in.Thread, in.Sender, in.TraceID = "room@chatroom", "requester", "first-song"
	second := in
	second.TraceID = "second-song"
	other := in
	other.Sender, other.TraceID = "other-member", "other-song"
	w.runtime.Media = []*mediaDelivery{
		{Trace: in.TraceID, Input: in, ID: "first-job", Kind: "voice", State: "running"},
		{Trace: second.TraceID, Input: second, Kind: "voice", State: "prepared"},
		{Trace: other.TraceID, Input: other, Kind: "voice", State: "prepared"},
	}
	m := fixtureMessage("ambiguous-stop", in.Thread, "发错了", "incoming")
	m.SenderID = in.Sender
	if !w.stopRequestedVoice(context.Background(), m) {
		t.Fatal("ambiguous stop not recognised")
	}
	if !w.runtime.Media[0].Paused || !w.runtime.Media[1].Paused || w.runtime.Media[0].UserCancelled || w.runtime.Media[1].UserCancelled || w.runtime.Media[2].Paused || w.runtime.Media[2].UserCancelled {
		t.Fatalf("ambiguity affected the wrong deliveries: %+v %+v %+v", w.runtime.Media[0], w.runtime.Media[1], w.runtime.Media[2])
	}
	if n.cancels != 1 { // only the first native job had begun; prepared work is simply held.
		t.Fatal("ambiguous pause submitted unnecessary cancels", n.cancels)
	}
	if len(w.runtime.Plans) != 1 || w.runtime.Plans[0].Input.TraceID != m.ID || w.runtime.Plans[0].State != "queued" {
		t.Fatal("ambiguous interruption did not queue one clarification", w.runtime.Plans)
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	if !w.runtime.Media[0].Paused || !w.runtime.Media[1].Paused || w.runtime.Media[0].UserCancelled || w.runtime.Media[1].UserCancelled {
		t.Fatalf("restart changed ambiguous pause into cancellation or resume: %+v %+v", w.runtime.Media[0], w.runtime.Media[1])
	}
}

func TestQuotedGroupStopSelectsExactlyOneConcurrentTrace(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	in.Thread, in.Sender, in.TraceID = "room@chatroom", "requester", "first-song"
	second := in
	second.TraceID = "second-song"
	w.runtime.Media = []*mediaDelivery{
		{Trace: in.TraceID, Input: in, ID: "first-job", Kind: "voice", State: "running"},
		{Trace: second.TraceID, Input: second, Kind: "voice", State: "prepared"},
	}
	m := fixtureMessage("quoted-stop", in.Thread, "这首发错了", "incoming")
	m.SenderID = "requester"
	m.ResolvedQuote = &linuxnative.Message{ID: second.TraceID}
	if !w.stopRequestedVoice(context.Background(), m) {
		t.Fatal("quoted stop ignored")
	}
	if n.cancels != 0 || w.runtime.Media[0].UserCancelled || !w.runtime.Media[1].UserCancelled {
		t.Fatalf("quote did not select exactly one prepared trace: cancels=%d first=%+v second=%+v", n.cancels, w.runtime.Media[0], w.runtime.Media[1])
	}
}

func TestQuotedUnrelatedMessageNeverFallsBackToUniqueVoiceTask(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	in.Thread, in.Sender, in.TraceID = "room@chatroom", "requester", "only-song"
	w.runtime.Media = []*mediaDelivery{{Trace: in.TraceID, Input: in, ID: "only-native-job", Kind: "voice", State: "running"}}
	m := fixtureMessage("unrelated-quoted-stop", in.Thread, "停", "incoming")
	m.SenderID = in.Sender
	m.ResolvedQuote = &linuxnative.Message{ID: "an-unrelated-message"}
	if w.stopRequestedVoice(context.Background(), m) || n.cancels != 0 || w.runtime.Media[0].UserCancelled {
		t.Fatalf("unrelated quote fell back to the only voice task: cancels=%d delivery=%+v", n.cancels, w.runtime.Media[0])
	}
}

func TestShortVoiceStopsAreExactAndDoNotMatchOrdinaryWords(t *testing.T) {
	for _, text := range []string{"停", "停一下", "停下", "先停", "先停一下", "先停下"} {
		if !parseVoiceInterrupt(text).stop {
			t.Fatal("short stop was not recognised", text)
		}
	}
	for _, text := range []string{"停车场", "停电了", "先停靠一下", "这首不停"} {
		if parseVoiceInterrupt(text).stop {
			t.Fatal("ordinary text was interpreted as a stop", text)
		}
	}
}

func TestUserCancelRecoversLostNativeIDByIdempotencyKey(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	d := &mediaDelivery{Key: "lost-native-key", Trace: in.TraceID, Input: in, SourceID: "source", Kind: "voice", State: "submitting", Created: time.Now().UTC()}
	w.runtime.Media = []*mediaDelivery{d}
	n.keys[d.Key] = true
	n.status = linuxnative.ToolStatus{ID: "native-job-created-before-crash", State: "running", ThreadID: in.Thread, SourceID: d.SourceID}
	w.cancelMedia(n, d, true, "media_cancel_requested_by_user")
	if n.cancels != 1 || d.ID != "native-job-created-before-crash" || !d.UserCancelled || d.State != "cancelling" {
		t.Fatalf("lost native id was not recovered and cancelled: cancels=%d delivery=%+v", n.cancels, d)
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	d = w.runtime.Media[0]
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if d.State != "stopped" || n.sends != 0 || n.retries != 0 {
		t.Fatalf("cancelled lost-id job revived after restart: %+v sends=%d retries=%d", d, n.sends, n.retries)
	}
}

func TestUserCancelUnknownSubmittingKeyNeverSubmitsAgain(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	d := &mediaDelivery{Key: "never-created-key", Trace: in.TraceID, Input: in, SourceID: "source", Kind: "voice", State: "submitting", Created: time.Now().UTC()}
	w.runtime.Media = []*mediaDelivery{d}
	w.cancelMedia(n, d, true, "media_cancel_requested_by_user")
	if d.State != "stopped" || !d.UserCancelled || n.cancels != 0 {
		t.Fatalf("unknown submission was not safely stopped: cancels=%d delivery=%+v", n.cancels, d)
	}
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if n.sends != 0 || n.retries != 0 {
		t.Fatal("cancelled unknown submission was recreated", n.sends, n.retries)
	}
}

func TestCancelledUnknownKeyNeverSubmitsAfterLookupFailureAndRestart(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	d := &mediaDelivery{Key: "interrupted-lookup-key", Trace: in.TraceID, Input: in, SourceID: "source", Kind: "voice", State: "cancelling", UserCancelled: true, Created: time.Now().UTC()}
	w.runtime.Media = []*mediaDelivery{d}
	n.lookupErrs = []error{errors.New("temporary native disconnect")}
	w.reconcileMedia(context.Background())
	if d.State != "cancelling" || n.sends != 0 || n.retries != 0 {
		t.Fatalf("lookup outage changed a cancelled task into a send: %+v sends=%d retries=%d", d, n.sends, n.retries)
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	d = w.runtime.Media[0]
	d.Next = time.Time{}
	w.reconcileMedia(context.Background()) // native now reports idempotency key absent
	if d.State != "stopped" || !d.UserCancelled || n.sends != 0 || n.retries != 0 {
		t.Fatalf("404 after restart recreated cancelled task: %+v sends=%d retries=%d", d, n.sends, n.retries)
	}
}

func TestCancelledUnknownKeyAcceptsOnlyLateOriginalReceipt(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	d := &mediaDelivery{Key: "late-receipt-key", Trace: in.TraceID, Input: in, SourceID: "source", Kind: "voice", State: "cancelling", UserCancelled: true, Created: time.Now().UTC()}
	w.runtime.Media = []*mediaDelivery{d}
	n.keys[d.Key] = true
	n.status = linuxnative.ToolStatus{ID: "already-submitted", State: "accepted", ServerID: "accepted-before-cancel", ThreadID: in.Thread, SourceID: d.SourceID}
	w.reconcileMedia(context.Background())
	if d.State != "accepted" || !d.UserCancelled || d.ID != "already-submitted" || n.sends != 0 || n.retries != 0 {
		t.Fatalf("late original receipt was not reconciled safely: %+v sends=%d retries=%d", d, n.sends, n.retries)
	}
}

func TestCancelledKnownUnknownJobKeepsPollingOnlyForLateReceipt(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	d := &mediaDelivery{Key: "known-unknown-key", Trace: in.TraceID, Input: in, SourceID: "source", ID: "known-job", Kind: "voice", State: "cancelling", UserCancelled: true, Created: time.Now().UTC()}
	w.runtime.Media = []*mediaDelivery{d}
	n.status = linuxnative.ToolStatus{ID: d.ID, State: "unknown", ThreadID: in.Thread, SourceID: d.SourceID}
	w.reconcileMedia(context.Background())
	if d.State != "cancelling" || d.Reason != "media_cancel_receipt_pending" || n.sends != 0 || n.retries != 0 {
		t.Fatalf("unknown cancelled job was not retained for receipt only: %+v sends=%d retries=%d", d, n.sends, n.retries)
	}
	n.status = linuxnative.ToolStatus{ID: d.ID, State: "accepted", ServerID: "late-server-receipt", ThreadID: in.Thread, SourceID: d.SourceID}
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if d.State != "accepted" || !d.UserCancelled || n.sends != 0 || n.retries != 0 {
		t.Fatalf("late receipt was not recorded without retry: %+v sends=%d retries=%d", d, n.sends, n.retries)
	}
}

func TestHandleUnmentionedGroupStopRunsBeforeEligibility(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	in.Thread, in.Sender, in.TraceID = "room@chatroom", "requester", "active-song"
	w.runtime.Media = []*mediaDelivery{{Trace: in.TraceID, Input: in, ID: "native-active", Kind: "voice", State: "running"}}
	m := fixtureMessage("unmentioned-stop", in.Thread, "发错了，后面别发", "incoming")
	m.SenderID = in.Sender
	w.source = &fakeSource{messages: map[string]linuxnative.Message{m.ID: m}}
	w.handle(context.Background(), fixtureEvent("1", m.ID, m.ThreadID, m.Direction, "remote"))
	if n.cancels != 1 || !w.runtime.Media[0].UserCancelled || n.calls != 0 {
		t.Fatalf("normal inbound path did not use the local stop path: cancels=%d text_calls=%d media=%+v", n.cancels, n.calls, w.runtime.Media[0])
	}
}

func TestUnmentionedReplacementPromotesOnlyNewSongTurn(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	in.Thread, in.Sender, in.TraceID = "room@chatroom", "requester", "old-song"
	w.runtime.Media = []*mediaDelivery{{Trace: in.TraceID, Input: in, ID: "native-active", Kind: "voice", State: "running"}}
	model := &fakeModel{answer: "已换成新歌。"}
	w.model = model
	m := fixtureMessage("replace-song", in.Thread, "发错了，换成《正确的歌》", "incoming")
	m.SenderID = in.Sender
	w.source = &fakeSource{messages: map[string]linuxnative.Message{m.ID: m}}
	w.handle(context.Background(), fixtureEvent("1", m.ID, m.ThreadID, m.Direction, "remote"))
	if n.cancels != 1 || !w.runtime.Media[0].UserCancelled || model.calls != 1 {
		t.Fatalf("unmentioned replacement did not cancel then start the new turn: cancels=%d model_calls=%d media=%+v", n.cancels, model.calls, w.runtime.Media[0])
	}
}

func TestRecoveredUnmentionedGroupStopCancelsBeforeGenerationResumes(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	in.Thread, in.Sender, in.TraceID = "room@chatroom", "requester", "active-song"
	w.runtime.Media = []*mediaDelivery{{Trace: in.TraceID, Input: in, ID: "native-active", Kind: "voice", State: "running"}}
	m := fixtureMessage("recovered-stop", in.Thread, "不是这首歌", "incoming")
	m.SenderID = in.Sender
	w.source = &fakeSource{events: []linuxnative.Event{fixtureEvent("2", m.ID, m.ThreadID, m.Direction, "remote")}, messages: map[string]linuxnative.Message{m.ID: m}}
	w.peekVoiceInterruptions(context.Background(), "1", map[string]bool{})
	if n.cancels != 1 || !w.runtime.Media[0].UserCancelled {
		t.Fatalf("recovered stop did not cancel native delivery: cancels=%d media=%+v", n.cancels, w.runtime.Media[0])
	}
}

func TestOrdinaryMessageDoesNotPauseMediaQueue(t *testing.T) {
	w, _, _, in := mediaWorker(t)
	in.Thread, in.Sender, in.TraceID = "room@chatroom", "requester", "active-song"
	w.runtime.Media = []*mediaDelivery{{Trace: in.TraceID, Input: in, ID: "native-active", Kind: "voice", State: "running"}}
	m := fixtureMessage("ordinary-group-chat", in.Thread, "这首歌挺好听", "incoming")
	m.SenderID = in.Sender
	m.Mentions.Self = new(bool)
	*m.Mentions.Self = true
	w.noticeConversationMessage(m, true)
	if w.runtime.Media[0].Paused || w.runtime.Media[0].UserCancelled {
		t.Fatalf("ordinary chat changed an active delivery: %+v", w.runtime.Media[0])
	}
}

func TestUpdateDrainCanPeekStopWithoutConsumingNewTask(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "old", Kind: "voice"})
	event := fixtureEvent("2", "new-song", "filehelper", "incoming", "live")
	message := fixtureMessage("new-song", "filehelper", "重新找一首，traveling light", "incoming")
	message.SenderID = in.Sender
	w.source = &fakeSource{events: []linuxnative.Event{event}, messages: map[string]linuxnative.Message{"new-song": message}}
	before := len(w.runtime.Inbox)
	w.peekVoiceInterruptions(context.Background(), "1", map[string]bool{})
	if n.cancels != 1 || len(w.runtime.Inbox) != before || n.sends != 1 || n.calls != 0 {
		t.Fatal("update either blocked stop or consumed a new model task")
	}
}
func TestMediaRetryUsesOriginalJobPreservesReceiptAndWaits(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "source", Kind: "voice", Title: "track.wav"})
	d := w.runtime.Media[0]
	n.status.State = "cancelled"
	n.status.Reason = "media_worker_stopping"
	n.status.Retryable = true
	n.status.VoiceChunks = []linuxnative.VoiceChunkStatus{{Index: 0, State: "accepted", HasReceipt: true, DurationMS: 40000}, {Index: 1, State: "prepared", DurationMS: 40000}}
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if d.State != "retry_wait" || n.retries != 0 || time.Until(d.Next) < 110*time.Second {
		t.Fatal("retry did not wait")
	}
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if n.retries != 0 {
		t.Fatal("only one check before retry")
	}
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if n.retries != 1 || n.sends != 1 || d.Status.VoiceChunks[0].State != "accepted" {
		t.Fatal("new job or accepted part replayed")
	}
	n.status.State = "accepted"
	n.status.ServerID = "123"
	n.status.VoiceChunks[1].State = "accepted"
	n.status.VoiceChunks[1].HasReceipt = true
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if d.State != "accepted" {
		t.Fatal("receipt missing")
	}
	scope := c.Store.Context(in, c.Config, time.Now())
	count := 0
	for _, turn := range scope.Messages {
		if turn.Media != nil {
			count++
			if turn.Media.AcceptedParts != 2 || turn.Media.DurationMS != 80000 {
				t.Fatal("chunk memory missing")
			}
		}
	}
	if count != 1 {
		t.Fatal("duplicate memory")
	}
}
func TestUncertainNativeGuardAndDrainDoNotResubmit(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "source", Kind: "voice"})
	d := w.runtime.Media[0]
	n.status.State = "needs_review"
	n.status.Retryable = false
	for i := 0; i < 3; i++ {
		d.Next = time.Time{}
		w.reconcileMedia(context.Background())
	}
	if d.State != "waiting" || n.retries != 0 || n.sends != 1 {
		t.Fatal("unsafe resubmit")
	}
	if w.pendingConversationThread(in.Thread) || w.pendingMedia() {
		t.Fatal("read-only uncertain receipt starved recovery tasks or updates")
	}
	w.config.Draining = func() bool { return true }
	w.suspendMedia()
	if n.cancels != 0 {
		t.Fatal("drain cancelled completed/reviewed job")
	}
	w.config.Draining = nil
	n.status.State = "accepted"
	n.status.ServerID = "123"
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if d.State != "accepted" || n.sends != 1 || n.retries != 0 {
		t.Fatal("late receipt was not reconciled", d)
	}
}

func TestCrossSeedsRestartTargetAndStrictBatchOrder(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	origin, err := w.prepareMediaEvidence(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.Thread = "target@chatroom"
	w.allowed[in.Thread] = true
	seeds := []MediaSeed{}
	for i := 0; i < 4; i++ {
		seeds = append(seeds, MediaSeed{OriginInput: &origin, Key: fmt.Sprint("cross-key-", i), Trace: "cross-batch", Input: in, Requester: "owner", OriginThread: "owner", SourceID: fmt.Sprint("source-", i), Media: companion.GeneratedMedia{Kind: "voice", Title: "part.wav"}})
	}
	w.config.MediaSeeds = func() []MediaSeed { return seeds }
	w.config.MediaSeedAllowed = func(MediaSeed) bool { return true }
	w.adoptMediaSeeds()
	w.adoptMediaSeeds()
	if len(w.runtime.Media) != 4 {
		t.Fatal("seed duplicate or loss")
	}
	w.reconcileMedia(context.Background())
	if n.sends != 1 || n.status.ThreadID != in.Thread {
		t.Fatal("wrong destination", n.sends, n.status)
	}
	if e := w.loadRuntime("server-1"); e != nil {
		t.Fatal(e)
	}
	w.adoptMediaSeeds()
	if len(w.runtime.Media) != 4 {
		t.Fatal("restart repeated seeds")
	}
	// Even if an unrelated send interleaves, the unknown first part blocks later
	// parts in this batch. An uncertain receipt cannot advance audio order.
	first := w.runtime.Media[0]
	first.State = "waiting"
	first.Status.State = "unknown"
	first.Next = time.Now().Add(time.Hour)
	unrelated := &mediaDelivery{Key: "unrelated", Trace: "other", Input: in, State: "accepted", CompletedAt: time.Now().Add(-time.Minute)}
	w.runtime.Media = append(w.runtime.Media[:1], append([]*mediaDelivery{unrelated}, w.runtime.Media[1:]...)...)
	w.reconcileMedia(context.Background())
	if n.sends != 1 {
		t.Fatal("advanced before earlier part accepted")
	}
	w.config.MediaSeedAllowed = func(MediaSeed) bool { return false }
	w.reconcileMedia(context.Background())
	if n.sends != 1 {
		t.Fatal("revoked owner sent")
	}
}

func TestCancelledCrossSeedWithUnknownSubmissionStopsAfterRestart(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	in.Thread = "target@chatroom"
	seed := MediaSeed{Key: "cancelled-cross-key", Trace: "cancelled-batch", Input: in, Requester: "owner", OriginThread: "owner", SourceID: "old-source", Media: companion.GeneratedMedia{Kind: "voice", Title: "old.wav"}}
	w.config.MediaSeeds = func() []MediaSeed { return []MediaSeed{seed} }
	w.config.MediaSeedAllowed = func(MediaSeed) bool { return true }
	w.adoptMediaSeeds()
	d := w.runtime.Media[0]
	d.State = "submitting"
	d.Next = time.Time{}
	if err := w.saveRuntime(); err != nil {
		t.Fatal(err)
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	// The management journal has cancelled this batch after native lookup
	// proved its key was absent. A stale worker checkpoint must not submit it.
	w.config.MediaSeedAllowed = func(MediaSeed) bool { return false }
	w.reconcileMedia(context.Background())
	d = w.runtime.Media[0]
	if d.State != "stopped" || n.sends != 0 || n.retries != 0 {
		t.Fatal("cancelled stale submission was retried", d, n.sends, n.retries)
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	w.reconcileMedia(context.Background())
	if w.runtime.Media[0].State != "stopped" || n.sends != 0 {
		t.Fatal("cancelled cross seed restarted", w.runtime.Media[0], n.sends)
	}
}

func TestOwnerCrossSeedCanTargetNonReplyConversationWithoutOpeningIt(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	origin, err := w.prepareMediaEvidence(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.Thread = "no-auto-reply@chatroom"
	seed := MediaSeed{OriginInput: &origin, Key: "owner-cross-key", Trace: "owner-cross-batch", Input: in, Requester: "owner", OriginThread: "owner", SourceID: "owner-cross-source", Media: companion.GeneratedMedia{Kind: "voice", Title: "synthetic.wav"}}
	w.config.MediaSeeds = func() []MediaSeed { return []MediaSeed{seed} }
	w.config.MediaSeedAllowed = func(s MediaSeed) bool {
		return s.Trace == seed.Trace && s.Requester == seed.Requester && s.Input.Thread == seed.Input.Thread
	}
	w.adoptMediaSeeds()
	if len(w.runtime.Media) != 1 || w.allowed[in.Thread] {
		t.Fatal("owner seed lost or changed auto-reply scope")
	}
	w.reconcileMedia(context.Background())
	if n.sends != 1 || n.status.ThreadID != in.Thread || w.allowed[in.Thread] {
		t.Fatal("wrong target or reply scope changed", n.sends, n.status)
	}
	ordinary := *w.runtime.Media[0]
	ordinary.CrossRequester = ""
	if w.mediaAllowed(&ordinary) {
		t.Fatal("ordinary media inherited temporary owner scope")
	}
	w.config.MediaSeedAllowed = func(MediaSeed) bool { return false }
	if w.mediaAllowed(w.runtime.Media[0]) {
		t.Fatal("owner authority revocation ignored")
	}
}

func TestCrossReceiptJournalOverridesStaleWorkerWithoutNativeResend(t *testing.T) {
	w, n, _, in := mediaWorker(t)
	in.Thread = "target@chatroom"
	w.allowed[in.Thread] = true
	seed := MediaSeed{Key: "cross-key", Trace: "cross-batch", Input: in, Requester: "owner", OriginThread: "owner", SourceID: "cross-source", Media: companion.GeneratedMedia{Kind: "voice", Title: "part.wav"}}
	w.config.MediaSeeds = func() []MediaSeed { return []MediaSeed{seed} }
	w.config.MediaSeedAllowed = func(s MediaSeed) bool { return s.Trace == seed.Trace && s.SourceID == seed.SourceID }
	w.adoptMediaSeeds()
	// A durable accepted receipt exists, while the checkpoint is still prepared.
	// Do not depend on native remembering its key or permission staying enabled.
	w.config.MediaSeedAllowed = func(MediaSeed) bool { return false }
	w.config.MediaSeedReceipt = func(trace, key string) (linuxnative.ToolStatus, bool) {
		return linuxnative.ToolStatus{ID: "verified-job", ThreadID: in.Thread, SourceID: seed.SourceID, State: "accepted", ServerID: "verified-receipt"}, trace == seed.Trace && key == seed.Key
	}
	settled := 0
	w.config.MediaSeedSettled = func(trace, key string, s linuxnative.ToolStatus) error { settled++; return nil }
	w.reconcileMedia(context.Background())
	d := w.runtime.Media[0]
	if n.sends != 0 || n.retries != 0 || n.cancels != 0 || d.State != "accepted" || !d.CrossSettled || settled != 1 {
		t.Fatal("receipt fact lost across checkpoint", d, n.sends, n.retries, n.cancels, settled)
	}
	if e := w.loadRuntime("server-1"); e != nil {
		t.Fatal(e)
	}
	w.reconcileMedia(context.Background())
	if n.sends != 0 || settled != 1 {
		t.Fatal("restart replayed settled receipt")
	}
}

func TestAcceptedStatusWithoutReceiptDoesNotCompleteMedia(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "source", Kind: "voice", Title: "song.wav"})
	d := w.runtime.Media[0]
	n.status.State = "accepted"
	n.status.ServerID = ""
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if d.State == "accepted" || d.Reason != "media_receipt_id_missing" {
		t.Fatal("unverified accepted status treated as delivered", d)
	}
	n.status.ServerID = "late-receipt"
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if d.State != "accepted" || n.sends != 1 {
		t.Fatal("late receipt lost or duplicate send", d, n.sends)
	}
}

func TestCompleteChunkReceiptsNormalizeAcceptedParentStatus(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	w.queueMedia(context.Background(), c, in, companion.GeneratedMedia{ID: "source", Kind: "voice", Title: "song.wav"})
	d := w.runtime.Media[0]
	n.status.State = "running"
	n.status.VoiceChunks = []linuxnative.VoiceChunkStatus{{Index: 1, State: "accepted", HasReceipt: true, ServerID: "chunk-1"}, {Index: 2, State: "accepted", HasReceipt: true, ServerID: "chunk-2"}}
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if d.State != "accepted" || d.Status.State != "accepted" || d.Status.ServerID != "chunk-2" || n.sends != 1 {
		t.Fatal("complete voice receipts not normalized", d, n.sends)
	}
}

type scopedMediaFake struct {
	*mediaFake
	loseReply bool
}

func (n scopedMediaFake) Submit(ctx context.Context, req linuxnative.ToolSend) (linuxnative.ToolStatus, error) {
	status, err := n.mediaFake.Submit(ctx, req)
	status.AccountID, status.Kind = "test", "send"
	n.status = status
	if n.loseReply {
		return linuxnative.ToolStatus{}, errors.New("synthetic submission response lost")
	}
	return status, err
}

func TestScopedMediaUsesOriginalJobAcrossUnknownReceiptAndCancellation(t *testing.T) {
	for _, latePart := range []bool{false, true} {
		t.Run(fmt.Sprint(latePart), func(t *testing.T) {
			w, n, c, in := mediaWorker(t)
			w.sender = scopedMediaFake{mediaFake: n, loseReply: true}
			w.config.AuthorizeMedia = func(request companion.Input) bool {
				return request.Account == in.Account && request.Thread == in.Thread
			}
			staged := 0
			w.config.PrepareMediaSource = func(_ context.Context, request companion.Input, kind, asset string) (string, error) {
				staged++
				if request.TraceID != in.TraceID || kind != "voice" || asset != "original" {
					return "", errors.New("upload received wrong request identity")
				}
				return "verified-upload", nil
			}
			media := companion.GeneratedMedia{ID: "prepared-voice", AssetID: "original", Kind: "voice"}
			w.queueMedia(t.Context(), c, in, media)
			if w.mediaPreparation == nil || n.sends != 0 {
				t.Fatal("staging did not defer the submit")
			}
			select {
			case prepared := <-w.mediaPreparation.result:
				w.mediaPreparation.result <- prepared
			case <-time.After(5 * time.Second):
				t.Fatal("staging did not finish")
			}
			w.reconcileMedia(t.Context())
			d := w.runtime.Media[0]
			if staged != 1 || n.sends != 1 || d.State != "checking_receipt" || d.SourceID != "verified-upload" {
				t.Fatal("did not checkpoint unknown submission", staged, n.sends, d.State)
			}
			key := d.Key
			if err := w.loadRuntime("server-1"); err != nil {
				t.Fatal(err)
			}
			d = w.runtime.Media[0]
			n.lookupErrs = []error{linuxnative.ErrToolNotFound}
			d.Next = time.Time{}
			w.reconcileMedia(t.Context())
			if d.State != "checking_receipt" || n.sends != 1 || d.Key != key {
				t.Fatal("absent lookup repeated submission", d.State, n.sends)
			}
			n.status.VoiceChunks = []linuxnative.VoiceChunkStatus{
				{Index: 0, State: "accepted", HasReceipt: true, ServerID: "123", DurationMS: 60000},
				{Index: 1, State: "cancelled", DurationMS: 60000},
				{Index: 2, State: "cancelled", DurationMS: 10000},
			}
			if latePart {
				n.status.VoiceChunks[1].State = "submitting"
			}
			interrupt := fixtureMessage("stop", in.Thread, "发错了，不是这首", "incoming")
			interrupt.SenderID = in.Sender
			n.lookupErrs = []error{linuxnative.ErrToolNotFound, linuxnative.ErrToolNotFound}
			if !w.stopRequestedVoice(t.Context(), interrupt) || !d.UserCancelled || d.State != "cancelling" || n.cancels != 0 {
				t.Fatal("cancel was not retained while its native job was still undiscoverable", d.State, n.cancels)
			}
			d.Next = time.Time{}
			w.reconcileMedia(t.Context())
			if d.State != "cancelling" || time.Until(d.Next) > 2*time.Second || n.sends != 1 {
				t.Fatal("late native cancel target was not checked promptly", d.State, d.Next, n.sends)
			}
			d.Next = time.Time{}
			w.reconcileMedia(t.Context())
			if n.cancels != 1 || d.ID != "job-test" {
				t.Fatal("cancel did not locate original job by key", n.cancels, d.State)
			}
			d.Next = time.Time{}
			w.reconcileMedia(t.Context())
			if latePart {
				if d.State != "cancelling" {
					t.Fatal("in-flight second segment was treated as cancelled", d.State)
				}
				n.status.VoiceChunks[1].State = "accepted"
				n.status.VoiceChunks[1].HasReceipt, n.status.VoiceChunks[1].ServerID = true, "124"
				d.Next = time.Time{}
				w.reconcileMedia(t.Context())
			}
			if d.State != "stopped" || !d.Status.VoiceChunks[0].HasReceipt || d.Status.VoiceChunks[1].HasReceipt != latePart || d.Status.VoiceChunks[2].State != "cancelled" {
				t.Fatal("partial receipts or pending cancellation lost", d.State, d.Status.VoiceChunks)
			}
			w.queueMedia(t.Context(), c, in, media)
			if staged != 1 || n.sends != 1 || n.retries != 0 {
				t.Fatal("cancelled delivery revived", staged, n.sends, n.retries)
			}
		})
	}
}

func TestScopedMediaStopDuringUploadDiscardsLateStaging(t *testing.T) {
	w, n, c, in := mediaWorker(t)
	started := make(chan struct{})
	w.config.PrepareMediaSource = func(ctx context.Context, _ companion.Input, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "late-native-source", nil
	}
	media := companion.GeneratedMedia{ID: "prepared-voice", AssetID: "original", Kind: "voice"}
	w.queueMedia(t.Context(), c, in, media)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	interrupt := fixtureMessage("stop", in.Thread, "后面别发了", "incoming")
	interrupt.SenderID = in.Sender
	if !w.stopRequestedVoice(t.Context(), interrupt) {
		t.Fatal("worker could not accept stop during upload")
	}
	select {
	case <-w.mediaPreparation.result:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not observe cancellation")
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	w.reconcileMedia(t.Context())
	w.queueMedia(t.Context(), c, in, media)
	if n.sends != 0 || n.cancels != 0 || w.runtime.Media[0].State != "stopped" {
		t.Fatal("stopping during staging submitted a native job", n.sends, n.cancels, w.runtime.Media[0].State)
	}
}
