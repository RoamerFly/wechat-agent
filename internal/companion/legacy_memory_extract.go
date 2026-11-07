package companion

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// LegacyMemorySelection is a read-only, provenance-checked projection of one
// legacy WeChat conversation. It is intended solely as import input; callers
// must not expose message text or hashes in inventory and diagnostic views.
type LegacyMemorySelection struct {
	Events   []LegacyMemoryEvent `json:"events"`
	Memories []LegacyMemoryFact  `json:"memories"`
}

// LegacyMemoryEvent retains only the original-message fields needed to create
// a new, independently verifiable event in the destination store.
type LegacyMemoryEvent struct {
	InstanceID string `json:"instance_id"`
	EventID    string `json:"event_id"`
	MessageID  string `json:"message_id"`
	ServerID   string `json:"server_id,omitempty"`
	Sender     string `json:"sender"`
	OccurredAt string `json:"occurred_at"`
	Revision   int    `json:"revision"`
	Text       string `json:"text"`
	Hash       string `json:"hash"`
}

// LegacyMemoryFactSource records the exact source version used by a fact.
// Legacy memory_sources has no revision column, so facts are accepted only
// when every referenced event remains at revision one.
type LegacyMemoryFactSource struct {
	EventID  string `json:"event_id"`
	Revision int    `json:"revision"`
}

// LegacyMemoryFact is an active, fully grounded legacy fact. Text is the
// legacy detail when present, otherwise its summary; source quotes remain
// verified internally and are deliberately not exported as another copy.
type LegacyMemoryFact struct {
	ID      string                   `json:"id"`
	Version int                      `json:"version"`
	Kind    string                   `json:"kind"`
	Text    string                   `json:"text"`
	Sources []LegacyMemoryFactSource `json:"sources"`
}

// ReadLegacyMemorySelection reads one selected conversation from an already
// open pinned legacy snapshot. It fails closed: a malformed scope, source, or
// fact is omitted; malformed snapshots return ErrLegacyMemoryUnsupported.
// No host path is accepted or included in returned errors.
func ReadLegacyMemorySelection(ctx context.Context, file *os.File, account, thread string) (LegacyMemorySelection, error) {
	if file == nil || strings.TrimSpace(account) == "" || strings.TrimSpace(thread) == "" {
		return LegacyMemorySelection{}, ErrLegacyMemoryUnsupported
	}
	db, closeSnapshot, err := openLegacyMemorySnapshot(ctx, file)
	if err != nil {
		return LegacyMemorySelection{}, err
	}
	defer closeSnapshot()

	scopeID := ScopeID("wechat", account, thread)
	if _, err := legacySelectionScope(ctx, db, scopeID, account, thread); err != nil {
		return LegacyMemorySelection{}, legacyInventoryError(ctx, err)
	}
	events, err := legacySelectionEvents(ctx, db, scopeID, account, thread)
	if err != nil {
		return LegacyMemorySelection{}, legacyInventoryError(ctx, err)
	}
	facts, err := legacySelectionFacts(ctx, db, scopeID, events)
	if err != nil {
		return LegacyMemorySelection{}, legacyInventoryError(ctx, err)
	}
	return LegacyMemorySelection{Events: events, Memories: facts}, nil
}

// ReadLegacyConversationPreference reads the owner's text setting for an exact
// verified scope. It is independent of event history and memory eligibility.
func ReadLegacyConversationPreference(ctx context.Context, file *os.File, account, thread string) (string, error) {
	if strings.TrimSpace(account) == "" || strings.TrimSpace(thread) == "" {
		return "", ErrLegacyMemoryUnsupported
	}
	db, closeSnapshot, err := openLegacyMemorySnapshot(ctx, file)
	if err != nil {
		return "", err
	}
	defer closeSnapshot()
	scope, err := legacySelectionScope(ctx, db, ScopeID("wechat", account, thread), account, thread)
	if err != nil {
		return "", legacyInventoryError(ctx, err)
	}
	if scope.PreferencesRevision < 0 || len(scope.Preferences) > 4000 || !utf8.ValidString(scope.Preferences) {
		return "", ErrLegacyMemoryUnsupported
	}
	return strings.TrimSpace(scope.Preferences), nil
}

func legacySelectionScope(ctx context.Context, db *sql.DB, scopeID, account, thread string) (Scope, error) {
	var payload []byte
	if err := db.QueryRowContext(ctx, `SELECT payload FROM scopes WHERE id=?`, scopeID).Scan(&payload); err != nil {
		return Scope{}, err
	}
	var scope Scope
	if err := json.Unmarshal(payload, &scope); err != nil || scope.ID != scopeID ||
		scope.Channel != "wechat" || scope.Account != account || scope.Thread != thread ||
		ScopeID(scope.Channel, scope.Account, scope.Thread) != scopeID {
		return Scope{}, ErrLegacyMemoryUnsupported
	}
	return scope, nil
}

func legacySelectionEvents(ctx context.Context, db *sql.DB, scopeID, account, thread string) ([]LegacyMemoryEvent, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,trace,role,sender,at,text,hash,complete,revision,payload
		FROM events e WHERE scope=? AND e.active=1 AND e.suppressed=0
		AND memory_user_text_reliable(e.role,e.text,e.payload)=1
		AND COALESCE(json_extract(e.payload,'$.source.integrity'),'') IN ('observed','verified','verified_import')
		AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id)
		AND NOT EXISTS(SELECT 1 FROM source_conflicts c WHERE c.scope=e.scope AND c.event_id=e.id)
		ORDER BY seq, id`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]LegacyMemoryEvent, 0)
	for rows.Next() {
		var id, trace, role, sender, at, text, hash string
		var complete, revision int
		var payload []byte
		if err := rows.Scan(&id, &trace, &role, &sender, &at, &text, &hash, &complete, &revision, &payload); err != nil {
			return nil, err
		}
		// Legacy memory_sources records only an event ID, never the source
		// revision. A rewritten event can therefore not be tied to the text
		// that an old fact quoted. Keep this import projection bounded to the
		// sole revision that legacy facts can prove, rather than exporting a
		// source which a later import could accidentally treat as grounded.
		if complete != 1 || revision != 1 {
			continue
		}
		var turn Turn
		if err := json.Unmarshal(payload, &turn); err != nil || !legacySelectionEventMatches(turn, id, trace, role, sender, at, text, hash, revision, account, thread) {
			continue
		}
		events = append(events, LegacyMemoryEvent{
			InstanceID: turn.Source.InstanceID, EventID: id, MessageID: trace, ServerID: turn.Source.ServerID, Sender: sender,
			OccurredAt: at, Revision: revision, Text: text, Hash: hash,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func legacySelectionEventMatches(turn Turn, id, trace, role, sender, at, text, hash string, revision int, account, thread string) bool {
	r := turn.Source
	occurredAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || memoryTime(occurredAt) != at || r == nil || strings.TrimSpace(r.InstanceID) == "" || strings.TrimSpace(r.MessageID) == "" {
		return false
	}
	return turn.EventID == id && turn.TraceID == trace && turn.Role == role && turn.Sender == sender &&
		r.EventID == id && r.MessageID == trace && r.Account == account && r.Thread == thread && r.Sender == sender &&
		r.MessageType == 1 && (r.Integrity == "observed" || r.Integrity == "verified" || r.Integrity == "verified_import") &&
		memoryTime(turn.Time) == at && r.OccurredAt == at && r.Revision == revision && r.BodyHash == hash && sourceHash(text) == hash
}

func legacySelectionFacts(ctx context.Context, db *sql.DB, scopeID string, events []LegacyMemoryEvent) ([]LegacyMemoryFact, error) {
	byID := make(map[string]LegacyMemoryEvent, len(events))
	for _, event := range events {
		byID[event.EventID] = event
	}
	rows, err := db.QueryContext(ctx, `SELECT id,kind,status,version,payload FROM memories WHERE scope=? AND status='active' ORDER BY updated,id`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type candidate struct {
		id, kind, status string
		version          int
		payload          []byte
	}
	candidates := make([]candidate, 0)
	for rows.Next() {
		var id, kind, status string
		var version int
		var payload []byte
		if err := rows.Scan(&id, &kind, &status, &version, &payload); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate{id: id, kind: kind, status: status, version: version, payload: append([]byte(nil), payload...)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	facts := make([]LegacyMemoryFact, 0, len(candidates))
	for _, candidate := range candidates {
		var entry MemoryEntry
		if err := json.Unmarshal(candidate.payload, &entry); err != nil || !legacySelectionFactMatches(entry, candidate.id, scopeID, candidate.kind, candidate.status, candidate.version) {
			continue
		}
		sources, ok := legacySelectionFactSources(ctx, db, candidate.id, entry.Sources, byID)
		if !ok {
			continue
		}
		text := strings.TrimSpace(entry.Detail)
		if text == "" {
			text = strings.TrimSpace(entry.Summary)
		}
		if text == "" {
			continue
		}
		facts = append(facts, LegacyMemoryFact{ID: candidate.id, Version: candidate.version, Kind: candidate.kind, Text: text, Sources: sources})
	}
	return facts, nil
}

func legacySelectionFactMatches(entry MemoryEntry, id, scope, kind, status string, version int) bool {
	return entry.ID == id && entry.Scope == scope && entry.Kind == kind && entry.Status == status && entry.Version == version &&
		!entry.Inference && !entry.Continuity && strings.TrimSpace(entry.CommitmentState) == ""
}

func legacySelectionFactSources(ctx context.Context, db *sql.DB, memoryID string, sources []MemorySource, events map[string]LegacyMemoryEvent) ([]LegacyMemoryFactSource, bool) {
	if len(sources) == 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, len(sources))
	result := make([]LegacyMemoryFactSource, 0, len(sources))
	for _, source := range sources {
		if strings.TrimSpace(source.EventID) == "" || strings.TrimSpace(source.Quote) == "" {
			return nil, false
		}
		event, ok := events[source.EventID]
		// Legacy memory_sources does not retain a source revision. Refuse a
		// rewritten source rather than guessing which revision the old fact used.
		if !ok || event.Revision != 1 || !strings.Contains(event.Text, source.Quote) {
			return nil, false
		}
		if _, duplicate := seen[source.EventID]; duplicate {
			return nil, false
		}
		seen[source.EventID] = struct{}{}
		var quote string
		if err := db.QueryRowContext(ctx, `SELECT quote FROM memory_sources WHERE memory_id=? AND event_id=?`, memoryID, source.EventID).Scan(&quote); err != nil || quote != source.Quote {
			return nil, false
		}
		result = append(result, LegacyMemoryFactSource{EventID: source.EventID, Revision: event.Revision})
	}
	var indexed int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_sources WHERE memory_id=?`, memoryID).Scan(&indexed); err != nil || indexed != len(result) {
		return nil, false
	}
	sort.Slice(result, func(i, j int) bool { return result[i].EventID < result[j].EventID })
	return result, true
}
