package companion

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func localFollowupFixture(t *testing.T) (*Store, Config, Input, time.Time) {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := Defaults()
	c.Permissions.Remember = true
	now := time.Date(2026, 11, 7, 10, 0, 0, 123, time.UTC)
	in := localTestInput("request-one", "等原程序运行结束后告诉我结果", now)
	if err = s.Observe(in, c, now); err != nil {
		t.Fatal(err)
	}
	ref, ok := s.SourceForInput(in)
	if !ok {
		t.Fatal("canonical source missing")
	}
	in.Source = ref
	return s, c, in, now
}

func TestLocalTaskFollowupStableRequestIdentityAndPrivateListing(t *testing.T) {
	s, c, in, now := localFollowupFixture(t)
	ctx := context.Background()
	first, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "process-one", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "process-one", 2, now.Add(time.Second))
	if err != nil || first.ID != retry.ID || retry.Revision != 1 || !retry.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("not idempotent: %#v %v", retry, err)
	}
	other := localTestInput("request-two", in.Text, now.Add(time.Second))
	if err = s.Observe(other, c, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	other.Source, _ = s.SourceForInput(other)
	second, err := s.ScheduleLocalTaskFollowup(ctx, other, *other.Source, "process-one", 2, now)
	if err != nil || second.ID == first.ID {
		t.Fatalf("same body merged requests: %#v %v", second, err)
	}
	third, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "process-one", 3, now)
	if err != nil || third.ID == first.ID {
		t.Fatalf("home revision not bound: %#v %v", third, err)
	}
	page, err := s.ListLocalTaskFollowups(ctx, in, "", 1)
	if err != nil || !page.HasMore || page.Next == "" || len(page.Items) != 1 {
		t.Fatalf("first page: %#v %v", page, err)
	}
	if page.Items[0].Source.EventID != "" {
		t.Fatal("list returned private source")
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), in.Text) || strings.Contains(string(raw), "event_id") || strings.Contains(string(raw), in.Source.BodyHash) {
		t.Fatalf("listing leaks private evidence: %s", raw)
	}
	seen := map[string]bool{page.Items[0].ID: true}
	for page.HasMore {
		page, err = s.ListLocalTaskFollowups(ctx, in, page.Next, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate page item")
			}
			seen[item.ID] = true
		}
	}
	if len(seen) != 3 {
		t.Fatalf("lost pages: %#v", seen)
	}
	wrong := in
	wrong.Account = "different-fish"
	if _, err = s.GetLocalTaskFollowup(ctx, wrong, first.ID); !errors.Is(err, ErrLocalTaskFollowupUnavailable) {
		t.Fatal("cross scope read", err)
	}
	initial, _ := s.ListLocalTaskFollowups(ctx, in, "", 1)
	if _, err = s.ListLocalTaskFollowups(ctx, wrong, initial.Next, 1); err == nil {
		t.Fatal("cross scope cursor accepted")
	}
	if _, err = s.UpdateLocalTaskFollowup(ctx, wrong, first.ID, 1, "ready", now, ""); !errors.Is(err, ErrLocalTaskFollowupUnavailable) {
		t.Fatal("cross scope update", err)
	}
}

func TestLocalTaskFollowupMigratesProgramRowsAndPersistsToolTargets(t *testing.T) {
	s, _, in, now := localFollowupFixture(t)
	ctx := t.Context()
	program, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "process-original", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	legacyID := "local-task-" + localFollowupID(ScopeID(in.Channel, in.Account, in.Thread), in.Source.EventID,
		in.Source.Revision, in.Source.BodyHash, "process-original", uint64(2))
	if program.ID != legacyID {
		t.Fatal("existing program identity changed", program.ID, legacyID)
	}
	// Remove only the new columns from this isolated fixture to reproduce
	// the table shape already present in users' previous installations.
	for _, column := range []string{"result_tool", "job_id"} {
		if _, err = s.db.ExecContext(ctx, "ALTER TABLE local_task_followups DROP COLUMN "+column); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Dir(s.path)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	got, err := migrated.GetLocalTaskFollowup(ctx, in, program.ID)
	if err != nil || got != program {
		t.Fatal("migration changed the existing row", got, err)
	}
	jobID := strings.Repeat("a", 64)
	job, err := migrated.ScheduleLocalToolFollowup(ctx, in, *in.Source, "project_comfy_job", jobID, 2, now)
	if err != nil || job.JobID != jobID || job.ProcessID != "" || job.ID == program.ID {
		t.Fatal("invalid project target", job, err)
	}
	retry, err := migrated.ScheduleLocalToolFollowup(ctx, in, *in.Source, job.ResultTool, jobID, 2, now.Add(time.Minute))
	if err != nil || retry != job {
		t.Fatal("repeated registration changed project target", retry, err)
	}
	if _, err := migrated.ScheduleLocalToolFollowup(ctx, in, *in.Source, "unknown route", jobID, 2, now); err == nil {
		t.Fatal("invalid result tool accepted")
	}
	if _, err := migrated.ScheduleLocalToolFollowup(ctx, in, *in.Source, job.ResultTool, "", 2, now); err == nil {
		t.Fatal("incomplete project target accepted")
	}
	if err = migrated.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.GetLocalTaskFollowup(ctx, in, job.ID)
	if err != nil || recovered != job {
		t.Fatal("project target lost on reopen", recovered, err)
	}
}

func TestLocalTaskFollowupDueCASAndTerminalCancellation(t *testing.T) {
	s, _, in, now := localFollowupFixture(t)
	ctx := context.Background()
	job, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "process-due", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "waiting", now.Add(time.Hour), "process_running")
	if err != nil {
		t.Fatal(err)
	}
	due, err := s.DueLocalTaskFollowups(ctx, in, now, 3)
	if err != nil || len(due) != 0 {
		t.Fatal("future job due", due, err)
	}
	due, err = s.DueLocalTaskFollowups(ctx, in, now.Add(time.Hour), 3)
	if err != nil || len(due) != 1 || due[0].Source.EventID != in.Source.EventID {
		t.Fatal("due source missing", due, err)
	}
	if _, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision-1, "ready", now, ""); !errors.Is(err, ErrLocalTaskFollowupConflict) {
		t.Fatal("stale CAS accepted", err)
	}
	job, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "ready", now, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "generating", now, "")
			outcomes <- e
		}()
	}
	wg.Wait()
	close(outcomes)
	ok, conflict := 0, 0
	for e := range outcomes {
		if e == nil {
			ok++
		} else if errors.Is(e, ErrLocalTaskFollowupConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("CAS duplicated claim %d/%d", ok, conflict)
	}
	job, err = s.GetLocalTaskFollowup(ctx, in, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	due, err = s.DueLocalTaskFollowups(ctx, in, now, 1)
	if err != nil || len(due) != 1 || due[0].State != "generating" {
		t.Fatal("lost generating recovery", due, err)
	}
	job, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "completed", now, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateLocalTaskFollowup(ctx, in, job.ID, job.Revision, "waiting", now, ""); !errors.Is(err, ErrLocalTaskFollowupConflict) {
		t.Fatal("terminal task revived", err)
	}
	due, err = s.DueLocalTaskFollowups(ctx, in, now.Add(time.Hour), 1)
	if err != nil || len(due) != 0 {
		t.Fatal("completed task due", due, err)
	}
	other, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "process-cancel", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	other, err = s.UpdateLocalTaskFollowup(ctx, in, other.ID, other.Revision, "cancelled", now, "owner_cancelled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateLocalTaskFollowup(ctx, in, other.ID, other.Revision, "ready", now, ""); !errors.Is(err, ErrLocalTaskFollowupConflict) {
		t.Fatal("cancelled task revived", err)
	}
}

func TestLocalTaskFollowupInvalidSourceAutomaticallyCancels(t *testing.T) {
	for _, kind := range []string{"forgotten", "suppressed", "conflict", "revision", "hash", "deleted"} {
		t.Run(kind, func(t *testing.T) {
			s, _, in, now := localFollowupFixture(t)
			ctx := context.Background()
			job, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "process-source", 2, now)
			if err != nil {
				t.Fatal(err)
			}
			scope, _ := localConversationScope(in)
			switch kind {
			case "forgotten":
				_, err = s.db.Exec(`INSERT INTO memory_blocks(scope,event_id) VALUES(?,?)`, scope, in.Source.EventID)
			case "suppressed":
				_, err = s.db.Exec(`UPDATE events SET suppressed=1 WHERE id=?`, in.Source.EventID)
			case "conflict":
				_, err = s.db.Exec(`INSERT INTO source_conflicts(scope,event_id,other_event_id,reason) VALUES(?,?,?,'synthetic')`, scope, in.Source.EventID, "other")
			case "revision":
				_, err = s.db.Exec(`UPDATE events SET revision=revision+1 WHERE id=?`, in.Source.EventID)
			case "hash":
				_, err = s.db.Exec(`UPDATE events SET hash='changed' WHERE id=?`, in.Source.EventID)
			case "deleted":
				_, err = s.db.Exec(`DELETE FROM events WHERE id=?`, in.Source.EventID)
			}
			if err != nil {
				t.Fatal(err)
			}
			due, err := s.DueLocalTaskFollowups(ctx, in, now, 20)
			if err != nil || len(due) != 0 {
				t.Fatal("obsolete source still polls", due, err)
			}
			cancelled, err := s.GetLocalTaskFollowup(ctx, in, job.ID)
			if err != nil || cancelled.State != "cancelled" || cancelled.Reason != "source_unavailable" || cancelled.Revision != 2 {
				t.Fatalf("not terminal: %#v %v", cancelled, err)
			}
			if _, err = s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "process-source", 2, now); !errors.Is(err, ErrLocalRequestUnavailable) {
				t.Fatal("invalid source rescheduled", err)
			}
		})
	}
}

func TestLocalTaskFollowupRejectsForgedSourceAndPersistsRestart(t *testing.T) {
	s, _, in, now := localFollowupFixture(t)
	ctx := context.Background()
	for _, field := range []string{"event", "revision", "hash", "message", "instance", "account", "thread", "sender"} {
		ref := *in.Source
		switch field {
		case "event":
			ref.EventID = "other"
		case "revision":
			ref.Revision++
		case "hash":
			ref.BodyHash = "other"
		case "message":
			ref.MessageID = "other"
		case "instance":
			ref.InstanceID = "other"
		case "account":
			ref.Account = "other"
		case "thread":
			ref.Thread = "other"
		case "sender":
			ref.Sender = "other"
		}
		if _, err := s.ScheduleLocalTaskFollowup(ctx, in, ref, "process-source", 2, now); !errors.Is(err, ErrLocalRequestUnavailable) {
			t.Fatalf("forged %s accepted: %v", field, err)
		}
	}
	job, err := s.ScheduleLocalTaskFollowup(ctx, in, *in.Source, "process-persistent", 2, now)
	if err != nil {
		t.Fatal(err)
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
	got, err := reopened.GetLocalTaskFollowup(ctx, in, job.ID)
	if err != nil || got.State != "waiting" || got.Source.EventID != in.Source.EventID || got.Revision != 1 {
		t.Fatal("restart lost task", got, err)
	}
	plan, err := reopened.db.Query(`EXPLAIN QUERY PLAN SELECT id FROM local_task_followups WHERE scope=? AND state IN('waiting','ready','generating') AND next_at<=? ORDER BY next_at,id LIMIT 5`, "scope", memoryTime(now))
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	indexUsed := false
	for plan.Next() {
		var a, b, c int
		var detail string
		if err = plan.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		indexUsed = indexUsed || strings.Contains(detail, "local_task_followups_due")
	}
	if !indexUsed {
		t.Fatal("due query does not use task index")
	}
}
