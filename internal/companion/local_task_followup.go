package companion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrLocalTaskFollowupUnavailable = errors.New("local task followup unavailable")
	ErrLocalTaskFollowupConflict    = errors.New("local task followup revision or state conflict")
)

// Source stays in the private ledger. Public task listings contain neither
// source references nor a second copy of the owner's original request.
type LocalTaskFollowup struct {
	ID           string    `json:"id"`
	RequestID    string    `json:"request_id"`
	ProcessID    string    `json:"process_id"`
	ResultTool   string    `json:"result_tool,omitempty"`
	JobID        string    `json:"job_id,omitempty"`
	HomeRevision uint64    `json:"home_revision"`
	Source       SourceRef `json:"-"`
	State        string    `json:"state"`
	NextAt       time.Time `json:"next_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Reason       string    `json:"reason,omitempty"`
	Revision     int64     `json:"revision"`
}

type LocalTaskFollowupPage struct {
	Items   []LocalTaskFollowup `json:"items"`
	Next    string              `json:"next,omitempty"`
	HasMore bool                `json:"has_more"`
}

const localTaskFollowupColumns = `id,request_id,process_id,home_revision,source,state,next_at,created_at,updated_at,reason,revision,result_tool,job_id`

func localFollowupID(parts ...any) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// localRequestSourceLocked resolves only the exact current user event. Unlike
// active-context lookup this continues to work after the request is pruned.
func (s *Store) localRequestSourceLocked(ctx context.Context, scope, request string, source SourceRef) error {
	if request == "" || source.EventID == "" || source.Revision < 1 || source.BodyHash == "" || source.Integrity == "conflicted" {
		return ErrLocalRequestUnavailable
	}
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT e.payload FROM events e WHERE e.id=? AND e.scope=? AND e.trace=? AND e.role='user' AND e.sender=? AND e.revision=? AND e.hash=? AND `+visibleLocalToolEvent, source.EventID, scope, request, source.Sender, source.Revision, source.BodyHash).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLocalRequestUnavailable
	}
	if err != nil {
		return err
	}
	var turn Turn
	if json.Unmarshal(payload, &turn) != nil || turn.Source == nil {
		return ErrLocalRequestUnavailable
	}
	if source != *turn.Source {
		return ErrLocalRequestUnavailable
	}
	return nil
}

func localFollowupSourceIdentity(in Input, source SourceRef) bool {
	return source.Account == in.Account && source.Thread == in.Thread && source.Sender == in.Sender && source.MessageID == in.TraceID
}

func scanLocalTaskFollowup(row interface{ Scan(...any) error }) (LocalTaskFollowup, error) {
	var out LocalTaskFollowup
	var source []byte
	var next, created, updated string
	err := row.Scan(&out.ID, &out.RequestID, &out.ProcessID, &out.HomeRevision, &source, &out.State, &next, &created, &updated, &out.Reason, &out.Revision, &out.ResultTool, &out.JobID)
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(source, &out.Source); err != nil {
		return out, err
	}
	for _, item := range []struct {
		raw    string
		target *time.Time
	}{{next, &out.NextAt}, {created, &out.CreatedAt}, {updated, &out.UpdatedAt}} {
		if *item.target, err = time.Parse(time.RFC3339Nano, item.raw); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *Store) ScheduleLocalTaskFollowup(ctx context.Context, in Input, source SourceRef, processID string, homeRevision uint64, now time.Time) (LocalTaskFollowup, error) {
	return s.scheduleLocalFollowup(ctx, in, source, processID, "", "", homeRevision, now)
}

// ScheduleLocalToolFollowup tracks an asynchronous tool result using the same
// source authorization, idempotency and terminal-state rules as process tasks.
func (s *Store) ScheduleLocalToolFollowup(ctx context.Context, in Input, source SourceRef, resultTool, jobID string, homeRevision uint64, now time.Time) (LocalTaskFollowup, error) {
	return s.scheduleLocalFollowup(ctx, in, source, "", resultTool, jobID, homeRevision, now)
}

func validLocalFollowupTarget(processID, resultTool, jobID string) bool {
	if processID != "" {
		return resultTool == "" && jobID == "" && strings.TrimSpace(processID) != "" && utf8.ValidString(processID) && len(processID) <= 512
	}
	if resultTool == "" || len(jobID) != 64 {
		return false
	}
	for _, c := range resultTool {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	for _, c := range jobID {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Store) scheduleLocalFollowup(ctx context.Context, in Input, source SourceRef, processID, resultTool, jobID string, homeRevision uint64, now time.Time) (LocalTaskFollowup, error) {
	scope, err := localConversationScope(in)
	if err != nil {
		return LocalTaskFollowup{}, err
	}
	if !localFollowupSourceIdentity(in, source) || !validLocalFollowupTarget(processID, resultTool, jobID) || homeRevision == 0 || now.IsZero() {
		return LocalTaskFollowup{}, ErrLocalRequestUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return LocalTaskFollowup{}, errors.New("memory database closed")
	}
	if err = s.localRequestSourceLocked(ctx, scope, in.TraceID, source); err != nil {
		return LocalTaskFollowup{}, err
	}
	// Keep the original process hash byte-for-byte stable. Tool targets have
	// a distinct domain and include both provider identity and job identity.
	id := "local-task-" + localFollowupID(scope, source.EventID, source.Revision, source.BodyHash, processID, homeRevision)
	if resultTool != "" {
		id = "local-task-" + localFollowupID(scope, source.EventID, source.Revision, source.BodyHash, "tool", resultTool, jobID, homeRevision)
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return LocalTaskFollowup{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO local_task_followups(id,scope,request_id,process_id,home_revision,source_event_id,source_revision,source_hash,source,state,next_at,created_at,updated_at,reason,revision,result_tool,job_id) VALUES(?,?,?,?,?,?,?,?,?,'waiting',?,?,?,'',1,?,?) ON CONFLICT(id) DO NOTHING`, id, scope, in.TraceID, processID, homeRevision, source.EventID, source.Revision, source.BodyHash, raw, memoryTime(now), memoryTime(now), memoryTime(now), resultTool, jobID)
	if err != nil {
		return LocalTaskFollowup{}, err
	}
	return scanLocalTaskFollowup(s.db.QueryRowContext(ctx, `SELECT `+localTaskFollowupColumns+` FROM local_task_followups WHERE scope=? AND id=?`, scope, id))
}

// Invalidated sources become terminal even if the process is still running.
// The indexed scope/state prefix keeps polling independent of other owners.
func (s *Store) cancelStaleLocalFollowupsLocked(ctx context.Context, scope string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE local_task_followups SET state='cancelled',reason='source_unavailable',updated_at=?,revision=revision+1 WHERE scope=? AND state IN('waiting','ready','generating') AND NOT EXISTS(SELECT 1 FROM events e WHERE e.id=local_task_followups.source_event_id AND e.scope=local_task_followups.scope AND e.role='user' AND e.trace=local_task_followups.request_id AND e.revision=local_task_followups.source_revision AND e.hash=local_task_followups.source_hash AND `+visibleLocalToolEvent+`)`, memoryTime(now), scope)
	return err
}

func (s *Store) GetLocalTaskFollowup(ctx context.Context, in Input, id string) (LocalTaskFollowup, error) {
	scope, err := localConversationScope(in)
	if err != nil {
		return LocalTaskFollowup{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return LocalTaskFollowup{}, errors.New("memory database closed")
	}
	if err = s.cancelStaleLocalFollowupsLocked(ctx, scope, time.Now()); err != nil {
		return LocalTaskFollowup{}, err
	}
	out, err := scanLocalTaskFollowup(s.db.QueryRowContext(ctx, `SELECT `+localTaskFollowupColumns+` FROM local_task_followups WHERE scope=? AND id=?`, scope, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrLocalTaskFollowupUnavailable
	}
	return out, err
}

func (s *Store) ListLocalTaskFollowups(ctx context.Context, in Input, after string, limit int) (LocalTaskFollowupPage, error) {
	scope, err := localConversationScope(in)
	if err != nil {
		return LocalTaskFollowupPage{}, err
	}
	if limit < 1 || limit == int(^uint(0)>>1) {
		return LocalTaskFollowupPage{}, errors.New("local task followup limit must be positive and allow lookahead")
	}
	last := ""
	if after != "" {
		var cursor struct{ Scope, ID string }
		b, e := base64.RawURLEncoding.DecodeString(after)
		if e != nil || json.Unmarshal(b, &cursor) != nil || cursor.Scope != scope || cursor.ID == "" {
			return LocalTaskFollowupPage{}, errors.New("invalid local task followup cursor")
		}
		last = cursor.ID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return LocalTaskFollowupPage{}, errors.New("memory database closed")
	}
	if err = s.cancelStaleLocalFollowupsLocked(ctx, scope, time.Now()); err != nil {
		return LocalTaskFollowupPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+localTaskFollowupColumns+` FROM local_task_followups WHERE scope=? AND id>? ORDER BY id LIMIT ?`, scope, last, limit+1)
	if err != nil {
		return LocalTaskFollowupPage{}, err
	}
	defer rows.Close()
	page := LocalTaskFollowupPage{Items: []LocalTaskFollowup{}}
	for rows.Next() {
		item, e := scanLocalTaskFollowup(rows)
		if e != nil {
			return LocalTaskFollowupPage{}, e
		}
		item.Source = SourceRef{}
		page.Items = append(page.Items, item)
	}
	if err = rows.Err(); err != nil {
		return LocalTaskFollowupPage{}, err
	}
	if len(page.Items) > limit {
		page.HasMore = true
		page.Items = page.Items[:limit]
		b, _ := json.Marshal(struct{ Scope, ID string }{scope, page.Items[len(page.Items)-1].ID})
		page.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}

func (s *Store) DueLocalTaskFollowups(ctx context.Context, in Input, now time.Time, limit int) ([]LocalTaskFollowup, error) {
	scope, err := localConversationScope(in)
	if err != nil {
		return nil, err
	}
	if limit < 1 || now.IsZero() {
		return nil, errors.New("local task followup requires a positive limit and time")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, errors.New("memory database closed")
	}
	if err = s.cancelStaleLocalFollowupsLocked(ctx, scope, now); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+localTaskFollowupColumns+` FROM local_task_followups WHERE scope=? AND state IN('waiting','ready','generating') AND next_at<=? ORDER BY next_at,id LIMIT ?`, scope, memoryTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LocalTaskFollowup{}
	for rows.Next() {
		item, e := scanLocalTaskFollowup(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// HasPendingLocalTaskFollowups includes future tasks, so a paged history view
// does not stop polling merely because its current page contains old results.
func (s *Store) HasPendingLocalTaskFollowups(ctx context.Context, in Input) (bool, error) {
	scope, err := localConversationScope(in)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return false, errors.New("memory database closed")
	}
	if err = s.cancelStaleLocalFollowupsLocked(ctx, scope, time.Now()); err != nil {
		return false, err
	}
	var pending bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM local_task_followups WHERE scope=? AND state IN('waiting','ready','generating'))`, scope).Scan(&pending)
	return pending, err
}

func localFollowupState(state string) bool {
	switch state {
	case "waiting", "ready", "generating", "completed", "cancelled", "failed":
		return true
	}
	return false
}

func localFollowupTransition(from, to string) bool {
	if from == "completed" || from == "cancelled" || from == "failed" {
		return false
	}
	if to == "cancelled" || to == "failed" {
		return true
	}
	switch from {
	case "waiting":
		return to == "waiting" || to == "ready"
	case "ready":
		return to == "ready" || to == "generating"
	case "generating":
		return to == "generating" || to == "completed"
	}
	return false
}

func (s *Store) UpdateLocalTaskFollowup(ctx context.Context, in Input, id string, revision int64, state string, next time.Time, reason string) (LocalTaskFollowup, error) {
	scope, err := localConversationScope(in)
	if err != nil {
		return LocalTaskFollowup{}, err
	}
	if !localFollowupState(state) || revision < 1 || next.IsZero() || !utf8.ValidString(reason) || len(reason) > 1024 {
		return LocalTaskFollowup{}, ErrLocalTaskFollowupConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return LocalTaskFollowup{}, errors.New("memory database closed")
	}
	if err = s.cancelStaleLocalFollowupsLocked(ctx, scope, time.Now()); err != nil {
		return LocalTaskFollowup{}, err
	}
	current, err := scanLocalTaskFollowup(s.db.QueryRowContext(ctx, `SELECT `+localTaskFollowupColumns+` FROM local_task_followups WHERE scope=? AND id=?`, scope, id))
	if errors.Is(err, sql.ErrNoRows) {
		return LocalTaskFollowup{}, ErrLocalTaskFollowupUnavailable
	}
	if err != nil {
		return LocalTaskFollowup{}, err
	}
	if current.Revision != revision {
		return current, ErrLocalTaskFollowupConflict
	}
	if state == "cancelled" {
		prefix, _ := localContinuationPrefix(Input{Channel: in.Channel, Account: in.Account, Thread: in.Thread, Sender: in.Sender, TraceID: current.RequestID}, current.ID)
		var accepted bool
		if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e WHERE e.scope=? AND e.role='assistant' AND json_extract(e.payload,'$.request_trace_id')=? AND substr(e.trace,1,?)=? AND `+visibleLocalToolEvent+`)`, scope, current.RequestID, len(prefix), prefix).Scan(&accepted); err != nil {
			return LocalTaskFollowup{}, err
		}
		if accepted {
			if current.State == "completed" {
				return current, nil
			}
			state, reason = "completed", "continuation_already_accepted"
		}
	}
	if !localFollowupTransition(current.State, state) {
		return current, ErrLocalTaskFollowupConflict
	}
	stamp := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE local_task_followups SET state=?,next_at=?,updated_at=?,reason=?,revision=revision+1 WHERE scope=? AND id=? AND revision=?`, state, memoryTime(next), memoryTime(stamp), reason, scope, id, revision)
	if err != nil {
		return LocalTaskFollowup{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return LocalTaskFollowup{}, err
	}
	if n != 1 {
		return current, ErrLocalTaskFollowupConflict
	}
	current.State, current.NextAt, current.UpdatedAt, current.Reason, current.Revision = state, next.UTC(), stamp, reason, revision+1
	return current, nil
}
