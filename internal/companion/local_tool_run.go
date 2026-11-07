package companion

import (
	"context"
	"database/sql"
	"errors"
)

// LocalToolCheckpoint contains no arguments, file contents or process output.
// An empty result means dispatch was reserved but the outcome is unverified.
type LocalToolCheckpoint struct {
	Key, Tool, Grant, Target, Result string
}

type LocalToolRun struct {
	store      *Store
	eventID    string
	revision   int
	Tracked    bool
	Operations []LocalToolCheckpoint
}

const visibleLocalToolEvent = `e.suppressed=0
 AND NOT EXISTS(SELECT 1 FROM source_conflicts sc WHERE sc.event_id=e.id)
 AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id)`

// OpenLocalToolRun attaches the journal to the exact visible source revision.
// An empty-key row identifies a turn admitted under the checkpoint protocol.
// An older unfinished turn without that row may have already acted.
func (s *Store) OpenLocalToolRun(ctx context.Context, in Input, fresh bool) (*LocalToolRun, error) {
	scope, err := localConversationScope(in)
	if err != nil || in.TraceID == "" || in.OriginalText == nil {
		return nil, ErrLocalConversationIdentity
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, errors.New("memory database closed")
	}
	run := &LocalToolRun{store: s}
	err = s.db.QueryRowContext(ctx, `SELECT e.id,e.revision FROM events e
 WHERE e.scope=? AND e.trace=? AND e.role='user' AND e.text=? AND `+visibleLocalToolEvent, scope, in.TraceID, *in.OriginalText).Scan(&run.eventID, &run.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLocalRequestUnavailable
	}
	if err != nil {
		return nil, err
	}
	if fresh {
		_, err = s.db.ExecContext(ctx, `INSERT INTO local_tool_checkpoints(event_id,revision,call_key,tool,grant_key,target)
 VALUES(?,?,'','','','') ON CONFLICT DO NOTHING`, run.eventID, run.revision)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT call_key,tool,grant_key,target,result FROM local_tool_checkpoints
 WHERE event_id=? AND revision=? ORDER BY rowid`, run.eventID, run.revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var op LocalToolCheckpoint
		if err := rows.Scan(&op.Key, &op.Tool, &op.Grant, &op.Target, &op.Result); err != nil {
			return nil, err
		}
		if op.Key == "" {
			run.Tracked = true
		} else {
			run.Operations = append(run.Operations, op)
		}
	}
	return run, rows.Err()
}

// Reserve must succeed durably before a mutating tool is dispatched. Existing
// checkpoints and obsolete sources cannot be dispatched through this method.
func (r *LocalToolRun) Reserve(ctx context.Context, op LocalToolCheckpoint) error {
	if !r.Tracked || op.Key == "" || op.Tool == "" {
		return ErrLocalRequestUnavailable
	}
	return r.update(ctx, `INSERT INTO local_tool_checkpoints(event_id,revision,call_key,tool,grant_key,target)
 SELECT e.id,e.revision,?,?,?,? FROM events e WHERE e.id=? AND e.revision=? AND `+visibleLocalToolEvent+
		` ON CONFLICT DO NOTHING`, op.Key, op.Tool, op.Grant, op.Target, r.eventID, r.revision)
}

func (r *LocalToolRun) RecordResult(ctx context.Context, key, result string) error {
	if key == "" || result == "" {
		return ErrLocalRequestUnavailable
	}
	return r.update(ctx, `UPDATE local_tool_checkpoints SET result=? WHERE event_id=? AND revision=?
 AND call_key=? AND result='' AND EXISTS(SELECT 1 FROM events e
 WHERE e.id=local_tool_checkpoints.event_id AND e.revision=local_tool_checkpoints.revision AND `+visibleLocalToolEvent+`)`,
		result, r.eventID, r.revision, key)
}

func (r *LocalToolRun) CheckSource(ctx context.Context) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.store.db == nil {
		return errors.New("memory database closed")
	}
	var visible bool
	err := r.store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e
 WHERE e.id=? AND e.revision=? AND `+visibleLocalToolEvent+`)`, r.eventID, r.revision).Scan(&visible)
	if err != nil {
		return err
	}
	if !visible {
		return ErrLocalRequestUnavailable
	}
	return nil
}

func (r *LocalToolRun) update(ctx context.Context, statement string, args ...any) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.store.db == nil {
		return errors.New("memory database closed")
	}
	result, err := r.store.db.ExecContext(ctx, statement, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLocalRequestUnavailable
	}
	return nil
}
