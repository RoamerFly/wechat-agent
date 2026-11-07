package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type pagedUnreadSource struct{ fakeSource }

func TestOwnerSingleMessageRetryIsNotConvertedToUnreadBatch(t *testing.T) {
	w, _, _ := backgroundWorker(t, generationModelFunc(func(context.Context, string, []map[string]string) (string, error) {
		t.Fatal("capture must not call a model")
		return "", nil
	}))
	w.source = &fakeSource{}
	w.runtime.Inbox = []inboxItem{
		{OwnerModelReview: true, Backlog: true, Phase: "received", Event: fixtureEvent("1", "explicit", "room@chatroom", "incoming", "remote")},
		{Phase: "received", Event: fixtureEvent("2", "ordinary", "room@chatroom", "incoming", "remote")},
	}
	state := checkpoint{Version: 1, ServerID: "server-1", Account: "test", Cursor: "2"}
	if err := w.captureStartupUnread(t.Context(), filepath.Join(w.config.StateDir, "checkpoint.json"), &state); err != nil {
		t.Fatal(err)
	}
	if w.runtime.Inbox[0].Backlog || !w.runtime.Inbox[1].Backlog || !w.runtime.Inbox[0].OwnerModelReview {
		t.Fatal("explicit retry lost its independent trigger or changed ordinary recovery", w.runtime.Inbox)
	}
}

func (s *pagedUnreadSource) Events(ctx context.Context, after string) (linuxnative.EventPage, error) {
	n, _ := strconv.Atoi(after)
	page := linuxnative.EventPage{NextCursor: after}
	for _, e := range s.events {
		v, _ := strconv.Atoi(e.Cursor)
		if v > n {
			page.Items = append(page.Items, e)
			page.NextCursor = e.Cursor
			if len(page.Items) == 100 {
				break
			}
		}
	}
	return page, nil
}

func TestStartupUnreadGroupsAcrossPagesSpeakersAndPreservesTimes(t *testing.T) {
	var calls atomic.Int32
	var current struct {
		Unread []companion.ConversationEvent `json:"unread_messages"`
		Now    string                        `json:"current_time"`
		Zone   string                        `json:"timezone"`
		Sender string                        `json:"request_actor_id"`
	}
	w, c, _ := backgroundWorker(t, generationModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
		calls.Add(1)
		if err := json.Unmarshal([]byte(turns[len(turns)-1]["content"]), &current); err != nil {
			t.Error(err)
		}
		return `{"action":"silent"}`, nil
	}))
	c.Config.Permissions.ObserveGroups = true
	c.Config.Permissions.AutonomousGroups = true
	c.Config.MemoryMessages = 500
	w.config.GroupRules = map[string]GroupRule{"room@chatroom": {Mode: "social"}}
	source := &pagedUnreadSource{fakeSource: fakeSource{messages: map[string]linuxnative.Message{}}}
	base := time.Now().Add(-4 * time.Hour).Truncate(time.Second)
	for i := 1; i <= 104; i++ {
		id := fmt.Sprintf("offline-%d", i)
		m := fixtureMessage(id, "room@chatroom", fmt.Sprintf("第%d条原话", i), "incoming")
		m.SenderID = fmt.Sprintf("wxid_person_%d", i%2)
		m.CreatedAt = base.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		source.messages[id] = m
		source.events = append(source.events, fixtureEvent(strconv.Itoa(i), id, m.ThreadID, "incoming", "remote"))
	}
	w.source = source
	w.enableGenerations()
	defer w.closeGenerations()
	state := checkpoint{Version: 1, ServerID: "server-1", Account: "test", Cursor: "0"}
	if err := w.captureStartupUnread(context.Background(), filepath.Join(w.config.StateDir, "checkpoint.json"), &state); err != nil {
		t.Fatal(err)
	}
	if state.Cursor != "104" || len(w.runtime.Inbox) != 104 {
		t.Fatal("did not collect full unread tail")
	}
	if err := w.drainInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	pumpGeneration(t, w, func() bool { return w.generation.occupied() == 0 })
	if calls.Load() != 1 || len(current.Unread) != 104 || current.Now == "" || current.Zone == "" || current.Sender != "wxid_person_0" {
		t.Fatal("not one timestamped conversation turn", calls.Load(), len(current.Unread), current.Sender)
	}
	for _, index := range []int{0, 103} {
		actual, err := time.Parse(time.RFC3339, current.Unread[index].At)
		expected, expectedErr := time.Parse(time.RFC3339, source.messages[current.Unread[index].MessageID].CreatedAt)
		if err != nil || expectedErr != nil || !actual.Equal(expected) {
			t.Fatal("original time lost", current.Unread[index].At, expected)
		}
	}
	scope := c.Store.Context(companion.Input{Channel: "wechat", Account: "test", Thread: "room@chatroom"}, c.Config, time.Now())
	if len(scope.Messages) != 104 {
		t.Fatal("original messages duplicated/replaced with synthetic summary", len(scope.Messages))
	}
	for i, turn := range scope.Messages {
		if turn.Source == nil || turn.Source.Sequence != int64(i+1) || turn.Source.MessageID != turn.TraceID {
			t.Fatal("offline message lost its own native cursor", turn.TraceID, turn.Source)
		}
	}
	for _, item := range w.runtime.Inbox {
		if item.Phase != "silent" {
			t.Fatal("child message not settled", item.Phase)
		}
	}
	// Confirmed decisions do not become unread again on the next launch.
	if err := w.captureStartupUnread(context.Background(), filepath.Join(w.config.StateDir, "checkpoint.json"), &state); err != nil {
		t.Fatal(err)
	}
	w.drainInbox(context.Background())
	if calls.Load() != 1 {
		t.Fatal("already viewed messages replayed")
	}
}

func TestUnreadGroupReplyBindsDispatchToRealAnchor(t *testing.T) {
	const thread = "room@chatroom"
	w, c, n := backgroundWorker(t, generationModelFunc(func(context.Context, string, []map[string]string) (string, error) {
		return `{"action":"reply","messages":["好的。"]}`, nil
	}))
	c.Config.Permissions.ObserveGroups, c.Config.Permissions.AutonomousGroups = true, true
	w.config.GroupRules = map[string]GroupRule{thread: {Mode: "social"}}
	source := &fakeSource{messages: map[string]linuxnative.Message{}}
	for k, body := range []string{"群友的原话", "另一位群友的提问"} {
		id := fmt.Sprintf("unread-%d", k)
		m := fixtureMessage(id, thread, body, "incoming")
		m.SenderID = fmt.Sprintf("member%d", k)
		m.ServerID = strconv.Itoa(80 + k)
		m.CreatedAt = time.Now().Add(time.Duration(k-3) * time.Minute).UTC().Format(time.RFC3339)
		source.messages[id] = m
		w.runtime.Inbox = append(w.runtime.Inbox, inboxItem{Backlog: true, Phase: "received", Event: fixtureEvent(strconv.Itoa(k+1), id, thread, "incoming", "remote")})
	}
	w.source = source
	if err := w.processUnreadConversation(t.Context(), thread); err != nil {
		t.Fatal(err)
	}
	if len(w.runtime.Plans) != 1 {
		t.Fatalf("plans=%d", len(w.runtime.Plans))
	}
	p := w.runtime.Plans[0]
	ref := p.Input.Source
	if ref == nil || ref.EventID == "" || ref.MessageID != "unread-1" || ref.Sender != "member1" || ref.Revision < 1 {
		t.Fatal("missing canonical anchor", ref)
	}
	ev, err := c.Store.ReadEvent(t.Context(), []string{companion.ScopeID("wechat", "test", thread)}, ref.EventID)
	if err != nil || ev.Text != "另一位群友的提问" || p.Input.Text == ev.Text {
		t.Fatal("digest changed original body", err)
	}
	readyPlans(w)
	w.reconcileUtterancePlans(t.Context())
	if p.State != "accepted" || len(n.submissions) != 1 {
		t.Fatal("group digest did not pass real dispatch admission", p.State, p.StopReason, len(n.submissions))
	}
}

func TestGroupCollectionDeadlineIsFixedPerRoom(t *testing.T) {
	w := &Worker{runtime: &runtimeState{}}
	const room = "a@chatroom"
	const window = 20 * time.Second
	first := time.Now().UTC()
	end := w.collectionDeadline(room, first, window)
	w.runtime.Inbox = append(w.runtime.Inbox, inboxItem{Phase: "received", Event: fixtureEvent("1", "m", room, "incoming", "remote"), AggregateUntil: end})
	if !end.Equal(first.Add(window)) || !w.collectionDeadline(room, end.Add(-time.Nanosecond), window).Equal(end) {
		t.Fatal("later receipt shifted the group's original deadline")
	}
	if !w.collectionDeadline(room, end, window).Equal(end.Add(window)) || !w.collectionDeadline("b@chatroom", first.Add(time.Second), window).Equal(end.Add(time.Second)) {
		t.Fatal("adjacent windows or distinct rooms overlapped")
	}
	if !w.collectionDeadline(room, first, 0).IsZero() {
		t.Fatal("explicit zero delayed the decision")
	}
	w.config.MaxAge = 5 * time.Second
	w.runtime.Inbox[0].CollectedAt = first
	m := fixtureMessage("m", room, "hello", "incoming")
	m.CreatedAt = first.Format(time.RFC3339Nano)
	if !w.messageInTime(m, end) || w.messageInTime(m, end.Add(6*time.Second)) {
		t.Fatal("only deliberate collection time should be excluded from message age")
	}
	m.CreatedAt = first.Add(-6 * time.Second).Format(time.RFC3339Nano)
	if w.messageInTime(m, end) {
		t.Fatal("collection made an already expired message eligible")
	}
}

func TestLiveMixedGroupWindowReadsAllInputsBeforeDecision(t *testing.T) {
	const room = "room@chatroom"
	for _, permanent := range []bool{true, false} {
		t.Run(fmt.Sprintf("permanent_%t", permanent), func(t *testing.T) {
			var calls, videoReads, reports int
			var current struct {
				Unread []json.RawMessage `json:"unread_messages"`
			}
			w, c, n := backgroundWorker(t, generationModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
				calls++
				if err := json.Unmarshal([]byte(turns[len(turns)-1]["content"]), &current); err != nil {
					t.Error(err)
				}
				return `{"action":"silent"}`, nil
			}))
			c.Config.Permissions.ObserveGroups, c.Config.Permissions.AutonomousGroups = true, true
			w.config.GroupRules = map[string]GroupRule{room: {Mode: "social"}}
			w.config.Options.ReadImages, w.config.Options.ReadVideos = true, true
			w.config.Analyzer = sourceCheckingAnalyzer(func(_ context.Context, m linuxnative.Message) (string, error) {
				if m.Type == 43 {
					videoReads++
					if permanent {
						return "", errors.New("media_plugin_disabled")
					}
					if videoReads == 1 {
						return "", errors.New("plugin_cold_start_failed")
					}
				}
				return "verified-" + m.ID, nil
			})
			w.config.ObservationFailed = func(in companion.Input, reason string) error {
				reports++
				if in.Thread != room || !strings.Contains(reason, "media_plugin_disabled") {
					t.Error("incorrect private diagnostic", in.Thread, reason)
				}
				return nil
			}
			source := &fakeSource{messages: map[string]linuxnative.Message{}}
			now := time.Now()
			for i, kind := range []int{1, 3, 43} {
				id := fmt.Sprintf("window-%d", i)
				m := fixtureMessage(id, room, "本轮合成测试", "incoming")
				m.Type, m.SenderID, m.CreatedAt = kind, "member", now.Add(time.Duration(i-5)*time.Second).Format(time.RFC3339)
				if kind == 3 {
					m.SenderID = "another-member"
				}
				source.messages[id] = m
				w.runtime.Inbox = append(w.runtime.Inbox, inboxItem{Event: fixtureEvent(strconv.Itoa(i+1), id, room, "incoming", "remote"), Phase: "received", Updated: now.Add(-2 * time.Second), AggregateUntil: now.Add(-time.Second)})
			}
			w.source = source
			w.enableGenerations()
			defer w.closeGenerations()
			if err := w.drainInbox(t.Context()); err != nil {
				t.Fatal(err)
			}
			pumpGeneration(t, w, func() bool { return w.generation.occupied() == 0 })
			if permanent {
				if calls != 0 || reports != 1 || videoReads != 1 || len(n.submissions) != 0 {
					t.Fatal("failed input escaped to group or diagnostic missing", calls, reports, videoReads, len(n.submissions))
				}
				for _, it := range w.runtime.Inbox {
					if it.Phase != "stopped" {
						t.Fatal(it.Phase)
					}
				}
				if err := w.drainInbox(t.Context()); err != nil || reports != 1 {
					t.Fatal("failed turn replayed", err, reports)
				}
			} else if calls != 1 || reports != 0 || videoReads != 2 || len(current.Unread) != 3 || !strings.Contains(string(current.Unread[1]), "verified-window-1") || !strings.Contains(string(current.Unread[2]), "verified-window-2") {
				t.Fatalf("mixed window not recovered in source order: calls=%d reports=%d video=%d items=%+v", calls, reports, videoReads, current.Unread)
			}
		})
	}
}
