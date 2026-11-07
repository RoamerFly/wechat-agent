package companion

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const LocalChannel = "local"

var (
	ErrLocalConversationIdentity = errors.New("local conversation identity required")
	ErrLocalRequestUnavailable   = errors.New("local request unavailable or forgotten")
	ErrDeliveryMemoryBlocked     = errors.New("delivery identity is forgotten or blocked")
)

type LocalConversationItem struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Text      string    `json:"text"`
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id,omitempty"`
	seq       int64
}
type LocalConversationPage struct {
	Items      []LocalConversationItem `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
	HasMore    bool                    `json:"has_more"`
}
type LocalConversationRequest struct {
	InputText  string   `json:"input_text"`
	OccurredAt string   `json:"occurred_at"`
	Messages   []string `json:"messages"`
	Completed  bool     `json:"completed"`
}
type localConversationCursor struct {
	Scope   string `json:"scope"`
	Before  int64  `json:"before"`
	Ceiling int64  `json:"ceiling"`
}

func localConversationScope(in Input) (string, error) {
	if in.Channel != LocalChannel || strings.TrimSpace(in.Account) == "" || strings.TrimSpace(in.Thread) == "" || strings.TrimSpace(in.Sender) == "" || in.Sender != in.Thread {
		return "", ErrLocalConversationIdentity
	}
	return ScopeID(LocalChannel, in.Account, in.Thread), nil
}

// LocalConversationPage returns the newest page first, with each page displayed
// in chronological order. It reads SQLite originals, rather than the bounded
// active context, and never returns forgotten/suppressed events.
func (s *Store) LocalConversationPage(ctx context.Context, in Input, cursor string, limit int) (LocalConversationPage, error) {
	scope, err := localConversationScope(in)
	if err != nil {
		return LocalConversationPage{}, err
	}
	if limit < 1 || limit > 200 {
		return LocalConversationPage{}, errors.New("local conversation page limit must be 1..200")
	}
	state := localConversationCursor{Scope: scope}
	if cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(b, &state) != nil || state.Scope != scope || state.Before < 0 || state.Ceiling < state.Before {
			return LocalConversationPage{}, errors.New("invalid local conversation cursor")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return LocalConversationPage{}, errors.New("memory database closed")
	}
	if cursor == "" {
		if err = s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM events WHERE scope=?", scope).Scan(&state.Ceiling); err != nil {
			return LocalConversationPage{}, err
		}
	}
	where, args := "e.scope=? AND e.seq<=?", []any{scope, state.Ceiling}
	if cursor != "" {
		where += " AND e.seq<?"
		args = append(args, state.Before)
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.trace,e.role,e.at,e.text,e.seq,e.payload FROM events e WHERE `+where+` AND e.suppressed=0 AND NOT EXISTS(SELECT 1 FROM source_conflicts sc WHERE sc.event_id=e.id) AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id) ORDER BY e.seq DESC LIMIT ?`, args...)
	if err != nil {
		return LocalConversationPage{}, err
	}
	defer rows.Close()
	reversed := []LocalConversationItem{}
	for rows.Next() {
		var item LocalConversationItem
		var at string
		var payload []byte
		if err = rows.Scan(&item.ID, &item.RequestID, &item.Role, &at, &item.Text, &item.seq, &payload); err != nil {
			return LocalConversationPage{}, err
		}
		if item.Time, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return LocalConversationPage{}, err
		}
		var turn Turn
		if item.Role == "assistant" && json.Unmarshal(payload, &turn) == nil {
			item.RequestID = turn.RequestTraceID
		}
		reversed = append(reversed, item)
	}
	if err = rows.Err(); err != nil {
		return LocalConversationPage{}, err
	}
	page := LocalConversationPage{Items: []LocalConversationItem{}}
	if len(reversed) > limit {
		page.HasMore = true
		reversed = reversed[:limit]
	}
	for i := len(reversed) - 1; i >= 0; i-- {
		page.Items = append(page.Items, reversed[i])
	}
	if page.HasMore && len(page.Items) > 0 {
		state.Before = page.Items[0].seq
		b, _ := json.Marshal(state)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}

// LocalConversationRequest is request-id deduplication over the same source
// event table. A deleted request returns ErrLocalRequestUnavailable, never a
// transcript that could be replayed.
func (s *Store) LocalConversationRequest(ctx context.Context, in Input, requestID string) (*LocalConversationRequest, error) {
	scope, err := localConversationScope(in)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(requestID) == "" {
		return nil, ErrLocalConversationIdentity
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, errors.New("memory database closed")
	}
	var text, occurred string
	err = s.db.QueryRowContext(ctx, `SELECT e.text,e.at FROM events e WHERE e.scope=? AND e.role='user' AND e.trace=? AND e.suppressed=0 AND NOT EXISTS(SELECT 1 FROM source_conflicts sc WHERE sc.event_id=e.id) AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id) LIMIT 2`, scope, requestID).Scan(&text, &occurred)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if e := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE scope=? AND role='user' AND trace=?)", scope, requestID).Scan(&exists); e != nil {
			return nil, e
		}
		if exists {
			return nil, ErrLocalRequestUnavailable
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var hiddenReply bool
	if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e WHERE e.scope=? AND e.role='assistant' AND json_extract(e.payload,'$.request_trace_id')=? AND (e.suppressed!=0 OR EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id) OR EXISTS(SELECT 1 FROM source_conflicts sc WHERE sc.event_id=e.id)))`, scope, requestID).Scan(&hiddenReply); err != nil {
		return nil, err
	}
	if hiddenReply {
		return nil, ErrLocalRequestUnavailable
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.text FROM events e WHERE e.scope=? AND e.role='assistant' AND e.suppressed=0 AND json_extract(e.payload,'$.request_trace_id')=? AND NOT EXISTS(SELECT 1 FROM source_conflicts sc WHERE sc.event_id=e.id) AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id) ORDER BY e.seq ASC`, scope, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &LocalConversationRequest{InputText: text, OccurredAt: occurred, Messages: []string{}}
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out.Completed = len(out.Messages) > 0
	return out, nil
}

// AcceptLocalConversation commits a complete response group exactly once. The
// request and prior output checks occur inside Store's existing synchronized
// scope transaction, so forgetting cannot race a delayed/retried delivery.
func (s *Store) AcceptLocalConversation(ctx context.Context, in Input, messages []string, c Config, now time.Time) error {
	return s.acceptLocalConversationGroup(ctx, in, "local:"+in.TraceID+":", "", messages, c, now)
}

func localContinuationPrefix(in Input, groupID string) (string, error) {
	scope, err := localConversationScope(in)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(groupID) == "" || len(groupID) > 512 || !utf8.ValidString(groupID) || strings.TrimSpace(in.TraceID) == "" {
		return "", ErrLocalConversationIdentity
	}
	return "local-continuation:" + localFollowupID(scope, groupID) + ":", nil
}

// AcceptLocalContinuation appends an independently idempotent assistant group
// to the original user request. It never manufactures another user event.
func (s *Store) AcceptLocalContinuation(ctx context.Context, in Input, groupID string, messages []string, c Config, now time.Time) error {
	prefix, err := localContinuationPrefix(in, groupID)
	if err != nil {
		return err
	}
	return s.acceptLocalConversationGroup(ctx, in, prefix, groupID, messages, c, now)
}

// LocalContinuationAccepted lets recovery reconcile an already committed
// group before it considers another model invocation.
func (s *Store) LocalContinuationAccepted(ctx context.Context, in Input, groupID string) (bool, error) {
	prefix, err := localContinuationPrefix(in, groupID)
	if err != nil {
		return false, err
	}
	scope, _ := localConversationScope(in)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return false, errors.New("memory database closed")
	}
	if in.Source == nil || !localFollowupSourceIdentity(in, *in.Source) {
		return false, ErrLocalRequestUnavailable
	}
	if err = s.localRequestSourceLocked(ctx, scope, in.TraceID, *in.Source); err != nil {
		return false, err
	}
	var count int
	var hidden bool
	var wrongRequest bool
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(e.suppressed!=0 OR EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id) OR EXISTS(SELECT 1 FROM source_conflicts sc WHERE sc.event_id=e.id)),0),COALESCE(MAX(COALESCE(json_extract(e.payload,'$.request_trace_id'),'')!=?),0) FROM events e WHERE e.scope=? AND e.role='assistant' AND substr(e.trace,1,?)=?`, in.TraceID, scope, len(prefix), prefix).Scan(&count, &hidden, &wrongRequest)
	if err != nil {
		return false, err
	}
	if hidden {
		return false, ErrDeliveryMemoryBlocked
	}
	if wrongRequest {
		return false, ErrDeliveryMemoryConflict
	}
	return count > 0, nil
}

func (s *Store) acceptLocalConversationGroup(ctx context.Context, in Input, prefix string, continuationID string, messages []string, c Config, now time.Time) error {
	scopeID, err := localConversationScope(in)
	if err != nil {
		return err
	}
	if strings.TrimSpace(in.TraceID) == "" || len(messages) == 0 {
		return ErrLocalConversationIdentity
	}
	canonical := make([]string, len(messages))
	for i, v := range messages {
		if strings.TrimSpace(v) == "" || !utf8.ValidString(v) {
			return ErrDeliveryMemoryConflict
		}
		canonical[i] = v
	}
	return s.scopeTransaction(scopeID, func(data *persisted) error {
		scope, ok := data.Scopes[scopeID]
		if !ok {
			return ErrLocalRequestUnavailable
		}
		if continuationID != "" {
			if in.Source == nil || !localFollowupSourceIdentity(in, *in.Source) {
				return ErrLocalRequestUnavailable
			}
			if err := s.localRequestSourceLocked(ctx, scopeID, in.TraceID, *in.Source); err != nil {
				return err
			}
		}
		var visible bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events e WHERE e.scope=? AND e.role='user' AND e.trace=? AND e.suppressed=0 AND NOT EXISTS(SELECT 1 FROM source_conflicts sc WHERE sc.event_id=e.id) AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id))`, scopeID, in.TraceID).Scan(&visible); err != nil {
			return err
		}
		if !visible {
			return ErrLocalRequestUnavailable
		}
		rows, err := s.db.QueryContext(ctx, `SELECT e.trace,e.text,e.suppressed, EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id) OR EXISTS(SELECT 1 FROM source_conflicts sc WHERE sc.event_id=e.id),COALESCE(json_extract(e.payload,'$.request_trace_id'),'') FROM events e WHERE e.scope=? AND e.role='assistant' AND substr(e.trace,1,?)=? AND (? OR json_extract(e.payload,'$.request_trace_id')=?) ORDER BY e.seq ASC`, scopeID, len(prefix), prefix, continuationID != "", in.TraceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		existing := map[string]string{}
		count := 0
		for rows.Next() {
			var key, text, request string
			var suppressed int
			var blocked bool
			if err = rows.Scan(&key, &text, &suppressed, &blocked, &request); err != nil {
				return err
			}
			if suppressed != 0 || blocked {
				return ErrDeliveryMemoryBlocked
			}
			if request != in.TraceID {
				return ErrDeliveryMemoryConflict
			}
			existing[key] = text
			count++
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if count > 0 {
			if count != len(canonical) {
				return ErrDeliveryMemoryConflict
			}
			for i, text := range canonical {
				if existing[prefix+strconv.Itoa(i)] != text {
					return ErrDeliveryMemoryConflict
				}
			}
			return nil
		}
		if continuationID != "" {
			// A cancellation and a late model reply serialize on Store.mu.
			// Existing groups above remain replayable after completion; a new
			// group may only cross the original task's generating boundary.
			var state, request, event, hash string
			var revision int
			err := s.db.QueryRowContext(ctx, `SELECT state,request_id,source_event_id,source_revision,source_hash FROM local_task_followups WHERE scope=? AND id=?`, scopeID, continuationID).Scan(&state, &request, &event, &revision, &hash)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && (state != "generating" || request != in.TraceID || event != in.Source.EventID || revision != in.Source.Revision || hash != in.Source.BodyHash) {
				return ErrLocalTaskFollowupConflict
			}
		}
		for i, text := range canonical {
			key := prefix + strconv.Itoa(i)
			scope.Messages = append(scope.Messages, Turn{ReplyContext: cloneReplyContext(in.ReplyContext), DeliveryID: key, RequestTraceID: in.TraceID, TraceID: key, Role: "assistant", Text: boundedText(text, 2048), FullText: text, SenderName: c.Name, SenderNameSource: "character", Time: now})
		}
		scope.LastInteraction = now
		recordAutonomyAccepted(&scope, in, c, now)
		clean, err := s.pruneStored(scope, c, now)
		if err != nil {
			return err
		}
		data.Scopes[scopeID] = clean
		return nil
	})
}
