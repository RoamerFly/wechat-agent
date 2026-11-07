package bot

import (
	"context"
	"strings"
	"testing"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type planReviewModel struct {
	answer string
	calls  int
	turns  []map[string]string
}

func (m *planReviewModel) Chat(_ context.Context, _ string, turns []map[string]string) (string, error) {
	m.calls++
	m.turns = turns
	return m.answer, nil
}
func planWorker(t *testing.T, model *planReviewModel) (*Worker, *companion.Engine, *acceptingProgressNative, companion.Input) {
	t.Helper()
	c := companion.Defaults()
	c.Enabled = true
	c.Conversation = companion.ConversationDefaults()
	c.Permissions.Remember = true
	c.QuietStart = "00:00"
	c.QuietEnd = "00:00"
	c.ReplyMinMS = 0
	c.ReplyMaxMS = 0
	store, _ := companion.NewStore(t.TempDir())
	if opened := store; opened != nil {
		t.Cleanup(func() { _ = opened.Close() })
	}
	engine, err := companion.New(c, store, model, "test")
	if err != nil {
		t.Fatal(err)
	}
	n := &acceptingProgressNative{reliableFake: reliableFake{intents: map[string]linuxnative.ToolStatus{}}}
	o := DefaultOptions()
	o.RandomDelay = false
	w, err := New(Config{StateDir: t.TempDir(), Options: &o, AllowedThreads: []string{"filehelper", "room@chatroom", "friend"}}, &fakeSource{}, engine, n)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	in := companion.Input{Channel: "wechat", Account: "test", Thread: "filehelper", TraceID: "original", Sender: "one", Text: "聊一聊", Mode: "direct", Directed: true, QuoteServerID: "77"}
	original := fixtureMessage(in.TraceID, in.Thread, in.Text, "incoming")
	original.SenderID, original.ServerID = in.Sender, in.QuoteServerID
	in.MessageTime = original.CreatedAt
	in.OriginalText = &original.Body
	bindInputSource(&in, original, engine)
	if err = store.Observe(in, c, time.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := engine.BindReplyEvidence(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.ReplyContext = &r
	return w, engine, n, in
}
func readyPlans(w *Worker) {
	w.runtime.SendGates = nil // Advance the simulated pacing clock.
	for _, p := range w.runtime.Plans {
		p.InboxChecked = true
		p.Next = time.Time{}
	}
}
func TestSemanticPlanWaitsForFreshInboxThenCanReplaceOnlyUnsentTail(t *testing.T) {
	model := &planReviewModel{answer: `{"action":"replace","messages":["改成简短的补充"]}`}
	w, c, n, in := planWorker(t, model)
	w.queueUtterancePlan(c, in, companion.Decision{Action: "reply", Messages: []string{"这是第一条完整意思", "旧的第二条", "旧的第三条"}})
	w.reconcileUtterancePlans(context.Background())
	if len(n.submissions) != 0 {
		t.Fatal("sent before new inbox scan")
	}
	readyPlans(w)
	w.reconcileUtterancePlans(context.Background())
	if len(n.submissions) != 1 {
		t.Fatal(n.submissions)
	}
	m := fixtureMessage("new", "filehelper", "不用说这么多，简单点", "incoming")
	m.SenderID = "one"
	w.noticeConversationMessage(m, true)
	if w.runtime.Plans[0].State != "review_pending" {
		t.Fatal("tail not paused")
	}
	readyPlans(w)
	w.reconcileUtterancePlans(context.Background())
	if model.calls != 1 || len(n.submissions) != 1 || w.runtime.Plans[0].Messages[0] != "这是第一条完整意思" {
		t.Fatal("review rewrote accepted message or sent too early")
	}
	readyPlans(w)
	w.reconcileUtterancePlans(context.Background())
	if len(n.submissions) != 2 || !strings.Contains(n.submissions[1].Text, "简短的补充") || w.runtime.Plans[0].State != "accepted" {
		t.Fatal("stale tail still sent", n.submissions)
	}
}
func TestPlanRestartKeepsExactTraceQuoteAndDeduplicatesAcceptedMemory(t *testing.T) {
	w, c, n, in := planWorker(t, &planReviewModel{})
	w.queueUtterancePlan(c, in, companion.Decision{Messages: []string{"已接受的第一条", "下一条"}})
	readyPlans(w)
	// Simulate accepted native receipt and memory commit before plan advancement.
	_, err := w.sendText(context.Background(), in, 0, "已接受的第一条")
	if err != nil {
		t.Fatal(err)
	}
	p := w.runtime.Plans[0]
	if err = c.Store.AcceptedDelivery(in, "已接受的第一条", deliveryMemoryKey(w.planDelivery(p)), c.Config, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	if w.runtime.Plans[0].Input.TraceID != in.TraceID || w.runtime.Plans[0].Quote == nil || w.runtime.Plans[0].Quote.ServerID != "77" {
		t.Fatal("identity/reference lost on restart")
	}
	readyPlans(w)
	w.reconcileUtterancePlans(context.Background())
	if len(n.submissions) != 1 || w.runtime.Plans[0].Index != 1 {
		t.Fatal("accepted first bubble replayed")
	}
	scope := c.Store.Context(in, c.Config, time.Now())
	count := 0
	for _, turn := range scope.Messages {
		if turn.Text == "已接受的第一条" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("accepted memory duplicated", count)
	}
}
func TestSleepingPlanDoesNotBlockAnotherConversationAndCancelDoesNotCancelMedia(t *testing.T) {
	w, c, n, in := planWorker(t, &planReviewModel{answer: `{"action":"cancel"}`})
	w.queueUtterancePlan(c, in, companion.Decision{Messages: []string{"旧内容"}})
	w.runtime.Plans[0].Next = time.Now().Add(time.Hour)
	w.runtime.Plans[0].InboxChecked = true
	other := in
	other.Thread = "friend"
	other.TraceID = "friend-request"
	other = observeSyntheticMediaRequest(t, w, c, other)
	w.queueUtterancePlan(c, other, companion.Decision{Messages: []string{"另一个会话的回复"}})
	w.runtime.Plans[1].InboxChecked = true
	w.reconcileUtterancePlans(context.Background())
	if len(n.submissions) != 1 || n.submissions[0].ThreadID != "friend" {
		t.Fatal("sleep blocked another thread", n.submissions)
	}
	w.runtime.Media = []*mediaDelivery{{Kind: "file", State: "running", Input: in, Trace: in.TraceID}}
	stop := fixtureMessage("stop", "filehelper", "先别说了", "incoming")
	stop.SenderID = in.Sender
	w.noticeConversationMessage(stop, true)
	readyPlans(w)
	w.reconcileUtterancePlans(context.Background())
	if w.runtime.Plans[0].State != "stopped" || w.runtime.Media[0].State != "running" {
		t.Fatal("text cancellation cancelled independent file")
	}
}
func TestShortMessageAggregationPreservesEachIdentityAndDoesNotMixSpeakers(t *testing.T) {
	w, c, _, in := planWorker(t, &planReviewModel{})
	current := fixtureMessage("original", "room@chatroom", "帮我看看", "incoming")
	current.SenderID = "one"
	stamp := time.Now().Add(-time.Second)
	current.CreatedAt = stamp.Format(time.RFC3339Nano)
	peer := fixtureMessage("peer", "room@chatroom", "再解释这个词", "incoming")
	peer.SenderID = "one"
	peer.CreatedAt = stamp.Add(500 * time.Millisecond).Format(time.RFC3339Nano)
	other := peer
	other.ID = "other"
	other.SenderID = "two"
	w.source = &fakeSource{messages: map[string]linuxnative.Message{"peer": peer, "other": other}}
	w.runtime.Inbox = []inboxItem{{Event: fixtureEvent("1", "original", current.ThreadID, "incoming", "remote"), Phase: "processing"}, {Event: fixtureEvent("2", "peer", current.ThreadID, "incoming", "remote"), Phase: "received"}, {Event: fixtureEvent("3", "other", current.ThreadID, "incoming", "remote"), Phase: "received"}}
	in.Thread = current.ThreadID
	in.Text = current.Body
	in.OriginalText, in.MessageTime = &current.Body, current.CreatedAt
	bindInputSource(&in, current, c)
	w.aggregateConversation(context.Background(), c, current, &in)
	if !strings.Contains(in.Text, "再解释这个词") || w.runtime.Inbox[1].Phase != "aggregated" || w.runtime.Inbox[2].Phase != "received" {
		t.Fatal("aggregation dropped or mixed messages", in.Text, w.runtime.Inbox)
	}
	scope := c.Store.Context(in, c.Config, time.Now())
	seen := map[string]bool{}
	for _, turn := range scope.Messages {
		seen[turn.TraceID] = true
		if turn.TraceID == "peer" {
			if turn.Source == nil || turn.Source.MessageID != peer.ID || turn.Source.Sequence != 2 {
				t.Fatal("aggregated message inherited the anchor source", turn.Source)
			}
			at, err := time.Parse(time.RFC3339Nano, turn.Source.OccurredAt)
			if err != nil || !at.Equal(stamp.Add(500*time.Millisecond)) {
				t.Fatal("aggregated message inherited the anchor time", turn.Source, err)
			}
		}
	}
	if !seen["original"] || !seen["peer"] || seen["other"] {
		t.Fatal("individual memory identities lost", seen)
	}
}
func TestWakeIsIndependentBoundedAndOnlyForOwnerSelectedTargets(t *testing.T) {
	model := &planReviewModel{answer: `{"action":"silent","messages":[]}`}
	w, c, _, in := planWorker(t, model)
	c.Config.Conversation.AllowWake = true
	c.Config.Permissions.Proactive = true
	c.Config.ProactiveThreads = []string{"filehelper"}
	w.scheduleConversationWake(c, in, 30, true)
	if len(w.runtime.Wakes) != 1 {
		t.Fatal("no wake")
	}
	w.runtime.Wakes[0].Due = time.Now().Add(-time.Second)
	if !w.tickConversationWake(context.Background()) || model.calls != 1 || w.runtime.Wakes[0].State != "completed" {
		t.Fatal("wake required a new question")
	}
	if w.tickConversationWake(context.Background()) || model.calls != 1 {
		t.Fatal("wake replayed")
	}
	in.TraceID = "another"
	w.scheduleConversationWake(c, in, 30, true)
	reply := fixtureMessage("human-replied", "filehelper", "好呀", "incoming")
	reply.SenderID = in.Sender
	w.noticeConversationMessage(reply, true)
	if w.runtime.Wakes[1].State != "stopped" {
		t.Fatal("wait-for-reply not cancelled")
	}
	in.Thread = "friend"
	in.TraceID = "not-selected"
	w.scheduleConversationWake(c, in, 30, false)
	if len(w.runtime.Wakes) != 2 {
		t.Fatal("unselected wake target admitted")
	}
}

type countedReceiptReader struct {
	*reliableFake
	reads int
}

func (n *countedReceiptReader) LookupSend(ctx context.Context, key string) (linuxnative.ToolStatus, error) {
	n.reads++
	return n.reliableFake.LookupSend(ctx, key)
}

func (n *countedReceiptReader) ToolSendStatus(ctx context.Context, id string) (linuxnative.ToolStatus, error) {
	n.reads++
	return n.reliableFake.ToolSendStatus(ctx, id)
}

func TestPendingPlanSharesPersistedReceiptBackoff(t *testing.T) {
	w, c, _, in := planWorker(t, &planReviewModel{})
	n := &countedReceiptReader{reliableFake: &reliableFake{intents: map[string]linuxnative.ToolStatus{}}}
	w.sender = n
	w.queueUtterancePlan(c, in, companion.Decision{Messages: []string{"合成回复"}})
	readyPlans(w)
	d := &delivery{Trace: in.TraceID, Thread: in.Thread, Input: in, Bubble: 0, Text: "合成回复", State: "waiting", ID: "existing-job", Key: "stable-key", Created: time.Now()}
	w.runtime.Deliveries = append(w.runtime.Deliveries, d)
	w.config.Options.RetryDelaySeconds = 120
	for round := 1; round <= 3; round++ {
		d.Next = time.Time{} // advance to the next scheduled check
		at := time.Now()
		w.reconcileDeliveries(t.Context())
		base := time.Duration(120<<(round-1)) * time.Second
		if d.UncertainRounds != round || d.Next.Before(at.Add(base)) || d.Next.After(time.Now().Add(2*base)) {
			t.Fatal("incorrect backoff", round, d.Next.Sub(at))
		}
		reads, next := n.reads, d.Next
		w.reconcileUtterancePlans(t.Context())
		w.reconcileDeliveries(t.Context())
		if n.reads != reads || !d.Next.Equal(next) {
			t.Fatal("plan bypassed or resampled receipt deadline")
		}
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	saved := w.runtime.Deliveries[0]
	if !saved.Next.Equal(d.Next) || saved.UncertainRounds != 3 {
		t.Fatal("restart lost sampled deadline")
	}
	n.intents[d.Key] = linuxnative.ToolStatus{ID: d.ID, ThreadID: in.Thread, State: "accepted", ServerID: "123"}
	saved.Next = time.Time{}
	w.runtime.Plans[0].InboxChecked = true
	w.reconcileDeliveries(t.Context())
	w.reconcileUtterancePlans(t.Context())
	if len(n.submissions) != 0 || w.runtime.Plans[0].State != "accepted" || saved.State != "accepted" {
		t.Fatal("late receipt missed or duplicated", saved.State, w.runtime.Plans[0].State)
	}
}
func TestNewContextPreventsSubmissionOfPreparedOldText(t *testing.T) {
	w, c, n, in := planWorker(t, &planReviewModel{answer: `{"action":"cancel"}`})
	w.queueUtterancePlan(c, in, companion.Decision{Messages: []string{"不该继续的旧内容"}})
	readyPlans(w)
	w.runtime.Deliveries = append(w.runtime.Deliveries, &delivery{Trace: in.TraceID, Thread: in.Thread, Input: in, Bubble: 0, Text: "不该继续的旧内容", State: "prepared", Key: "never-submitted", Created: time.Now()})
	update := fixtureMessage("new-context", "filehelper", "不用说了", "incoming")
	update.SenderID = in.Sender
	w.noticeConversationMessage(update, true)
	w.reconcileDeliveries(context.Background())
	readyPlans(w)
	w.reconcileUtterancePlans(context.Background())
	if len(n.submissions) != 0 || w.runtime.Plans[0].State != "stopped" || w.runtime.Deliveries[0].State != "stopped" {
		t.Fatal("unsubmitted stale text escaped review")
	}
}
