package companion

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLocalContinuationGroupsIndependentlyPersistWithoutUserEvents(t *testing.T) {
	s, c, in, now := localFollowupFixture(t)
	ctx := context.Background()
	if err := s.AcceptLocalConversation(ctx, in, []string{"程序已开始"}, c, now); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"finish-one", "finish-two"} {
		accepted, err := s.LocalContinuationAccepted(ctx, in, group)
		if err != nil || accepted {
			t.Fatal("new group already accepted", accepted, err)
		}
		if err = s.AcceptLocalContinuation(ctx, in, group, []string{"原程序完成：" + group, "结果已保存"}, c, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err = s.AcceptLocalContinuation(ctx, in, group, []string{"原程序完成：" + group, "结果已保存"}, c, now.Add(2*time.Second)); err != nil {
			t.Fatal("idempotent group conflicted", err)
		}
		if err = s.AcceptLocalContinuation(ctx, in, group, []string{"changed"}, c, now); !errors.Is(err, ErrDeliveryMemoryConflict) {
			t.Fatal("changed group accepted", err)
		}
	}
	if err := s.AcceptLocalConversation(ctx, in, []string{"程序已开始"}, c, now); err != nil {
		t.Fatal("continuation invalidated original response", err)
	}
	other := localTestInput("other-request", "另一任务", now.Add(time.Second))
	if err := s.Observe(other, c, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	other.Source, _ = s.SourceForInput(other)
	if err := s.AcceptLocalContinuation(ctx, other, "finish-one", []string{"原程序完成：finish-one", "结果已保存"}, c, now); !errors.Is(err, ErrDeliveryMemoryConflict) {
		t.Fatal("group reassigned to another request", err)
	}
	if _, err := s.LocalContinuationAccepted(ctx, other, "finish-one"); !errors.Is(err, ErrDeliveryMemoryConflict) {
		t.Fatal("group lookup crossed request", err)
	}
	var users int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE role='user'`).Scan(&users); err != nil || users != 2 {
		t.Fatal("manufactured user event", users, err)
	}
	state, err := s.LocalConversationRequest(ctx, in, in.TraceID)
	if err != nil || !state.Completed || len(state.Messages) != 5 {
		t.Fatal("request lost independent groups", state, err)
	}
	dir := filepath.Dir(s.path)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	accepted, err := reopened.LocalContinuationAccepted(ctx, in, "finish-two")
	if err != nil || !accepted {
		t.Fatal("restart lost accepted group", accepted, err)
	}
	if err = reopened.AcceptLocalContinuation(ctx, in, "finish-one", []string{"原程序完成：finish-one", "结果已保存"}, c, now); err != nil {
		t.Fatal(err)
	}
	wrong := in
	wrong.Account = "other-fish"
	if _, err = reopened.LocalContinuationAccepted(ctx, wrong, "finish-one"); !errors.Is(err, ErrLocalRequestUnavailable) {
		t.Fatal("wrong scope accepted lookup", err)
	}
	if err = reopened.AcceptLocalContinuation(ctx, wrong, "finish-one", []string{"wrong"}, c, now); !errors.Is(err, ErrLocalRequestUnavailable) {
		t.Fatal("wrong scope accepted reply", err)
	}
}

func TestLocalContinuationCancellationAndAcceptanceShareAtomicBoundary(t *testing.T) {
	for _, replyFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel_first", true: "reply_first"}[replyFirst], func(t *testing.T) {
			s, c, in, now := localFollowupFixture(t)
			ctx := context.Background()
			job, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "original-process", 2, now)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.AcceptLocalContinuation(ctx, in, job.ID, []string{"too early"}, c, now); !errors.Is(err, ErrLocalTaskFollowupConflict) {
				t.Fatal("waiting task delivered", err)
			}
			job, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "ready", now, "")
			if err != nil {
				t.Fatal(err)
			}
			job, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "generating", now, "")
			if err != nil {
				t.Fatal(err)
			}
			if replyFirst {
				if err = s.AcceptLocalContinuation(ctx, in, job.ID, []string{"原程序已完成"}, c, now); err != nil {
					t.Fatal(err)
				}
			}
			job, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "cancelled", now, "owner_cancelled")
			if err != nil {
				t.Fatal(err)
			}
			if replyFirst {
				if job.State != "completed" {
					t.Fatal("accepted reply reported cancelled", job)
				}
				if err = s.AcceptLocalContinuation(ctx, in, job.ID, []string{"原程序已完成"}, c, now); err != nil {
					t.Fatal("completed group replay rejected", err)
				}
			} else {
				if job.State != "cancelled" {
					t.Fatal("cancellation not retained", job)
				}
				if err = s.AcceptLocalContinuation(ctx, in, job.ID, []string{"late reply"}, c, now); !errors.Is(err, ErrLocalTaskFollowupConflict) {
					t.Fatal("cancelled task delivered late reply", err)
				}
				accepted, e := s.LocalContinuationAccepted(ctx, in, job.ID)
				if e != nil || accepted {
					t.Fatal("late result committed", accepted, e)
				}
			}
		})
	}
}

func TestLocalContinuationRejectsForgottenOrChangedSourceAndHiddenGroup(t *testing.T) {
	for _, kind := range []string{"forgotten", "revision", "hidden_group"} {
		t.Run(kind, func(t *testing.T) {
			s, c, in, now := localFollowupFixture(t)
			ctx := context.Background()
			if err := s.AcceptLocalContinuation(ctx, in, "group", []string{"saved"}, c, now); err != nil {
				t.Fatal(err)
			}
			scope, _ := localConversationScope(in)
			switch kind {
			case "forgotten":
				if _, err := s.db.Exec(`INSERT INTO memory_blocks(scope,event_id) VALUES(?,?)`, scope, in.Source.EventID); err != nil {
					t.Fatal(err)
				}
			case "revision":
				if _, err := s.db.Exec(`UPDATE events SET revision=revision+1 WHERE id=?`, in.Source.EventID); err != nil {
					t.Fatal(err)
				}
			case "hidden_group":
				if _, err := s.db.Exec(`UPDATE events SET suppressed=1 WHERE scope=? AND role='assistant'`, scope); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.LocalContinuationAccepted(ctx, in, "group"); err == nil {
				t.Fatal("hidden/stale continuation accepted")
			}
			if err := s.AcceptLocalContinuation(ctx, in, "group", []string{"saved"}, c, now); err == nil {
				t.Fatal("hidden/stale continuation replayed")
			}
		})
	}
}

func TestLocalContinuationConcurrentCancelHasOneVisibleOutcome(t *testing.T) {
	s, c, in, now := localFollowupFixture(t)
	ctx := context.Background()
	job, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "racing-process", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "ready", now, "")
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "generating", now, "")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var replyErr, cancelErr error
	var cancelled LocalTaskFollowup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		replyErr = s.AcceptLocalContinuation(ctx, in, job.ID, []string{"原进程完成"}, c, now)
	}()
	go func() {
		defer wg.Done()
		<-start
		cancelled, cancelErr = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "cancelled", now, "owner_cancelled")
	}()
	close(start)
	wg.Wait()
	if cancelErr != nil {
		t.Fatal(cancelErr)
	}
	accepted, err := s.LocalContinuationAccepted(ctx, in, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		if replyErr != nil || cancelled.State != "completed" {
			t.Fatal("saved reply disagrees with terminal task", replyErr, cancelled)
		}
	} else if !errors.Is(replyErr, ErrLocalTaskFollowupConflict) || cancelled.State != "cancelled" {
		t.Fatal("cancelled task received reply", replyErr, cancelled)
	}
}

func TestLocalTaskFollowupPendingIncludesFutureWorkAfterTerminalHistory(t *testing.T) {
	s, _, in, now := localFollowupFixture(t)
	ctx := context.Background()
	first, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "old-process", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateLocalTaskFollowup(ctx, in, first.ID, first.Revision, "cancelled", now, ""); err != nil {
		t.Fatal(err)
	}
	pending, err := s.HasPendingLocalTaskFollowups(ctx, in)
	if err != nil || pending {
		t.Fatal("terminal history pending", pending, err)
	}
	second, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "future-process", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateLocalTaskFollowup(ctx, in, second.ID, second.Revision, "waiting", now.Add(24*time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	pending, err = s.HasPendingLocalTaskFollowups(ctx, in)
	if err != nil || !pending {
		t.Fatal("future task missed", pending, err)
	}
	wrong := in
	wrong.Account = "other-fish"
	pending, err = s.HasPendingLocalTaskFollowups(ctx, wrong)
	if err != nil || pending {
		t.Fatal("other fish saw task", pending, err)
	}
	if _, err = s.db.Exec(`UPDATE events SET suppressed=1 WHERE id=?`, in.Source.EventID); err != nil {
		t.Fatal(err)
	}
	pending, err = s.HasPendingLocalTaskFollowups(ctx, in)
	if err != nil || pending {
		t.Fatal("obsolete future task pending", pending, err)
	}
}
