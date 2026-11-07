package companion

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLocalConversationLongReplyRetainsOriginalAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Permissions.Remember = true
	now := time.Now().UTC()
	in := localTestInput("long-reply", "把完整长文留在记录", now)
	text := strings.Repeat("夜海里的共同经历。", 600)
	if err = store.Observe(in, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err = store.AcceptLocalConversation(context.Background(), in, []string{text}, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.LocalConversationRequest(context.Background(), in, in.TraceID)
	if err != nil || state == nil || len(state.Messages) != 1 || state.Messages[0] != text {
		t.Fatalf("long original lost: %v", err)
	}
	if err = store.AcceptLocalConversation(context.Background(), in, []string{text}, cfg, now); err != nil {
		t.Fatal("long reply retry conflicted", err)
	}
}

func localTestInput(id, text string, at time.Time) Input {
	original := text
	stamp := at.UTC().Format(time.RFC3339Nano)
	return Input{Channel: LocalChannel, Account: "fish-1", Thread: "owner-1", Sender: "owner-1", Text: text, OriginalText: &original, Mode: "direct", TraceID: id, MessageTime: stamp, Source: &SourceRef{MessageID: id, Account: "fish-1", Thread: "owner-1", Sender: "owner-1", OccurredAt: stamp, Revision: 1, Integrity: "observed"}}
}

func TestLocalConversationPersistsChronologicalHistoryAndAcceptedGroup(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := Defaults()
	cfg.Permissions.Remember = true
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	first := localTestInput("req-1", "第一句", now)
	if err = store.Observe(first, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err = store.AcceptLocalConversation(ctx, first, []string{"第一条回答", "第二条回答"}, cfg, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = store.AcceptLocalConversation(ctx, first, []string{"第一条回答", "第二条回答"}, cfg, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	state, err := store.LocalConversationRequest(ctx, first, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || !state.Completed || state.InputText != "第一句" || len(state.Messages) != 2 {
		t.Fatalf("unexpected request state: %#v", state)
	}
	if err = store.AcceptLocalConversation(ctx, first, []string{"已改写"}, cfg, now); !errors.Is(err, ErrDeliveryMemoryConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	second := localTestInput("req-2", "第二句", now.Add(3*time.Second))
	if err = store.Observe(second, cfg, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	page, err := store.LocalConversationPage(ctx, second, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || len(page.Items) != 2 || !page.Items[0].Time.Before(page.Items[1].Time) || page.Items[1].RequestID != "req-2" {
		t.Fatalf("unexpected newest chronological page: %#v", page)
	}
	older, err := store.LocalConversationPage(ctx, second, page.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Items) != 2 || older.Items[0].RequestID != "req-1" || older.Items[1].RequestID != "req-1" {
		t.Fatalf("unexpected older page: %#v", older)
	}
}

func TestLocalConversationRequestRejectsHiddenAcceptedReply(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := Defaults()
	cfg.Permissions.Remember = true
	now := time.Now().UTC()
	in := localTestInput("hidden-reply", "测试", now)
	if err = store.Observe(in, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err = store.AcceptLocalConversation(ctx, in, []string{"已发送"}, cfg, now); err != nil {
		t.Fatal(err)
	}
	scope := ScopeID(LocalChannel, in.Account, in.Thread)
	var id string
	if err = store.db.QueryRow(`SELECT id FROM events WHERE scope=? AND role='assistant' AND json_extract(payload,'$.request_trace_id')=?`, scope, in.TraceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`INSERT INTO memory_blocks(scope,event_id) VALUES(?,?)`, scope, id); err != nil {
		t.Fatal(err)
	}
	if _, err = store.LocalConversationRequest(ctx, in, in.TraceID); !errors.Is(err, ErrLocalRequestUnavailable) {
		t.Fatalf("want hidden reply to block replay, got %v", err)
	}
}
func TestAcceptLocalConversationRejectsForgottenRequest(t *testing.T) {
	ctx := context.Background()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := Defaults()
	cfg.Permissions.Remember = true
	now := time.Now().UTC()
	in := localTestInput("forgotten", "不要重放", now)
	if err = store.Observe(in, cfg, now); err != nil {
		t.Fatal(err)
	}
	scope := ScopeID(LocalChannel, in.Account, in.Thread)
	if err = store.Clear(scope); err != nil {
		t.Fatal(err)
	}
	if err = store.AcceptLocalConversation(ctx, in, []string{"旧回复"}, cfg, now); !errors.Is(err, ErrLocalRequestUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
}
