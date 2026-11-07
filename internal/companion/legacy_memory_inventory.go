package companion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	_ "modernc.org/sqlite"
	"modernc.org/sqlite/vfs"
)

// ErrLegacyMemoryUnsupported means that a supplied snapshot is not a supported
// companion-memory SQLite database.  Callers should expose their own stable
// diagnostic instead of the underlying SQLite error, which can disclose host
// paths or snapshot details.
var ErrLegacyMemoryUnsupported = errors.New("legacy memory inventory unsupported")

// LegacyMemoryConversation is deliberately a metadata-only projection.  It
// never contains a message body, display name, summary text, or source quote.
//
// EventCount is the number of active, reliably-attributed source events that
// are eligible for recall in this scope at inspection time.  It is not a total
// history count: inactive, suppressed, blocked, conflicted, and non-source
// events are intentionally excluded.
//
// WithheldEvents counts reliably-attributed source events excluded because they
// are suppressed, blocked, or conflicted.  It does not claim to count every
// non-recallable historical event.
//
// MemoryCount is the number of active memories whose source set is nonempty,
// remains in this scope, and is entirely recall-eligible.  A zero-source memory
// is omitted because this inventory cannot establish its grounding.
type LegacyMemoryConversation struct {
	ThreadID       string `json:"thread_id"`
	EventCount     int    `json:"event_count"`
	WithheldEvents int    `json:"withheld_events"`
	MemoryCount    int    `json:"memory_count"`
	HasSummary     bool   `json:"has_summary"`
	HasPreference  bool   `json:"has_preference,omitempty"`
}

// LegacyMemoryInventoryResult contains only verified conversations for the
// requested WeChat account.  UnmappedScopes are same-account scope rows whose
// stored identity cannot be verified.  ForeignScopes are valid scope rows for
// another channel or account; their identities are never returned.
type LegacyMemoryInventoryResult struct {
	MatchedScopes  int                        `json:"matched_scopes"`
	UnmappedScopes int                        `json:"unmapped_scopes"`
	ForeignScopes  int                        `json:"foreign_scopes"`
	Conversations  []LegacyMemoryConversation `json:"conversations"`
}

// ValidateLegacyMemorySnapshot checks a pinned standalone database through
// the same read-only SQLite VFS as the migration reader.
func ValidateLegacyMemorySnapshot(ctx context.Context, file *os.File) error {
	db, closeSnapshot, err := openLegacyMemorySnapshot(ctx, file)
	if err != nil {
		return err
	}
	defer closeSnapshot()
	var status string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&status); err != nil || status != "ok" {
		return ErrLegacyMemoryUnsupported
	}
	return nil
}

// LegacyMemoryInventory examines an already-open, pinned, host-private
// snapshot.  The caller owns file and must keep it open for the call.  No path
// is accepted or retained here: the SQLite VFS serves exactly one read-only
// file named memory.sqlite, so SQLite cannot follow sidecar files or create a
// journal beside the host snapshot.
func LegacyMemoryInventory(ctx context.Context, file *os.File, account string) (LegacyMemoryInventoryResult, error) {
	if file == nil || strings.TrimSpace(account) == "" {
		return LegacyMemoryInventoryResult{}, ErrLegacyMemoryUnsupported
	}
	db, closeSnapshot, err := openLegacyMemorySnapshot(ctx, file)
	if err != nil {
		return LegacyMemoryInventoryResult{}, err
	}
	defer closeSnapshot()

	rows, err := db.QueryContext(ctx, `SELECT id,
		json_extract(payload,'$.id'),
		json_extract(payload,'$.channel'),
		json_extract(payload,'$.account'),
		json_extract(payload,'$.thread'),
		CASE WHEN json_type(payload,'$.summary') IS NULL THEN 0 ELSE 1 END,
		CASE WHEN json_type(payload,'$.preferences') = 'text'
			AND length(trim(json_extract(payload,'$.preferences'))) > 0 THEN 1 ELSE 0 END
		FROM scopes ORDER BY id`)
	if err != nil {
		return LegacyMemoryInventoryResult{}, legacyInventoryError(ctx, err)
	}
	result := LegacyMemoryInventoryResult{Conversations: make([]LegacyMemoryConversation, 0)}
	// database/sql reserves the sole connection until rows is closed.  Keep this
	// projection tiny, then run indexed aggregates after closing the cursor.
	type verifiedScope struct {
		id         string
		thread     string
		summary    bool
		preference bool
	}
	verified := make([]verifiedScope, 0)
	for rows.Next() {
		var rowID string
		var payloadID, channel, scopeAccount, thread sql.NullString
		var summary, preference int
		if err := rows.Scan(&rowID, &payloadID, &channel, &scopeAccount, &thread, &summary, &preference); err != nil {
			return LegacyMemoryInventoryResult{}, legacyInventoryError(ctx, err)
		}
		if !payloadID.Valid || !channel.Valid || !scopeAccount.Valid || !thread.Valid ||
			strings.TrimSpace(payloadID.String) == "" || strings.TrimSpace(channel.String) == "" ||
			strings.TrimSpace(scopeAccount.String) == "" || strings.TrimSpace(thread.String) == "" ||
			payloadID.String != rowID {
			result.UnmappedScopes++
			continue
		}
		if ScopeID(channel.String, scopeAccount.String, thread.String) != rowID {
			result.UnmappedScopes++
			continue
		}
		if channel.String != "wechat" || scopeAccount.String != account {
			result.ForeignScopes++
			continue
		}
		verified = append(verified, verifiedScope{id: rowID, thread: thread.String, summary: summary != 0, preference: preference != 0})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return LegacyMemoryInventoryResult{}, legacyInventoryError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return LegacyMemoryInventoryResult{}, legacyInventoryError(ctx, err)
	}

	for _, scope := range verified {
		events, withheld, err := legacyInventoryEvents(ctx, db, scope.id, account, scope.thread)
		if err != nil {
			return LegacyMemoryInventoryResult{}, legacyInventoryError(ctx, err)
		}
		memories, err := legacyInventoryMemories(ctx, db, scope.id, account, scope.thread)
		if err != nil {
			return LegacyMemoryInventoryResult{}, legacyInventoryError(ctx, err)
		}

		result.MatchedScopes++
		result.Conversations = append(result.Conversations, LegacyMemoryConversation{
			ThreadID:       scope.thread,
			EventCount:     events,
			WithheldEvents: withheld,
			MemoryCount:    memories,
			HasSummary:     scope.summary,
			HasPreference:  scope.preference,
		})
	}
	return result, nil
}

// openLegacyMemorySnapshot exposes exactly the supplied open file to SQLite.
// It deliberately has no path parameter, so a legacy database cannot follow
// journals, sidecars, or host-private paths. Callers must close the returned
// cleanup function before releasing file.
func openLegacyMemorySnapshot(ctx context.Context, file *os.File) (*sql.DB, func(), error) {
	if file == nil {
		return nil, nil, ErrLegacyMemoryUnsupported
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, nil, ErrLegacyMemoryUnsupported
	}
	vfsName, registered, err := vfs.New(legacyMemorySnapshotFS{file: file, info: info})
	if err != nil {
		return nil, nil, ErrLegacyMemoryUnsupported
	}
	db, err := sql.Open("sqlite", "file:memory.sqlite?mode=ro&immutable=1&vfs="+vfsName)
	if err != nil {
		registered.Close()
		return nil, nil, ErrLegacyMemoryUnsupported
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	cleanup := func() {
		_ = db.Close()
		_ = registered.Close()
	}
	if _, err := db.ExecContext(ctx, "PRAGMA temp_store=MEMORY"); err != nil {
		cleanup()
		return nil, nil, legacyInventoryError(ctx, err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA trusted_schema=OFF"); err != nil {
		cleanup()
		return nil, nil, legacyInventoryError(ctx, err)
	}
	var schema string
	if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='schema'").Scan(&schema); err != nil {
		cleanup()
		return nil, nil, legacyInventoryError(ctx, err)
	}
	if schema != "1" && schema != "2" {
		cleanup()
		return nil, nil, ErrLegacyMemoryUnsupported
	}
	return db, cleanup, nil
}

// A source payload produced by newer stores identifies its account/thread. Old
// rows may lack those fields; they remain candidates only after the enclosing
// verified scope has matched. A present mismatch always fails closed.
const legacySourceScopeMatches = `
	(COALESCE(json_extract(e.payload,'$.source.account'),'')='' OR json_extract(e.payload,'$.source.account')=?)
	AND (COALESCE(json_extract(e.payload,'$.source.thread'),'')='' OR json_extract(e.payload,'$.source.thread')=?)`

const legacySourceIdentityMatches = legacySourceScopeMatches + `
	AND COALESCE(json_extract(e.payload,'$.source.integrity'),'')!='conflicted'`

func legacyInventoryEvents(ctx context.Context, db *sql.DB, scope, account, thread string) (events, withheld int, err error) {
	err = db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN e.active=1 AND e.suppressed=0
			AND memory_user_text_reliable(e.role,e.text,e.payload)=1
			AND `+legacySourceIdentityMatches+`
			AND NOT EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id)
			AND NOT EXISTS(SELECT 1 FROM source_conflicts c WHERE c.scope=e.scope AND c.event_id=e.id)
			THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN memory_user_text_reliable(e.role,e.text,e.payload)=1
			AND `+legacySourceScopeMatches+`
			AND (e.suppressed!=0
				OR EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id)
				OR EXISTS(SELECT 1 FROM source_conflicts c WHERE c.scope=e.scope AND c.event_id=e.id)
				OR COALESCE(json_extract(e.payload,'$.source.integrity'),'')='conflicted')
			THEN 1 ELSE 0 END),0)
		FROM events e WHERE e.scope=?`, account, thread, account, thread, scope).Scan(&events, &withheld)
	return events, withheld, err
}

func legacyInventoryMemories(ctx context.Context, db *sql.DB, scope, account, thread string) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories m
		WHERE m.scope=? AND m.status='active'
		AND EXISTS(SELECT 1 FROM memory_sources ms WHERE ms.memory_id=m.id)
		-- Do not let a stale memory_sources index make an empty serialized
		-- source list look grounded.  Every serialized source must also have
		-- a matching index row and an eligible event in this scope.
		AND EXISTS(
			SELECT 1 FROM json_each(m.payload,'$.sources') src
			WHERE TRIM(COALESCE(json_extract(src.value,'$.event_id'),''))!=''
		)
		AND NOT EXISTS(
			SELECT 1 FROM json_each(m.payload,'$.sources') src
			LEFT JOIN events e ON e.id=json_extract(src.value,'$.event_id')
			WHERE TRIM(COALESCE(json_extract(src.value,'$.event_id'),''))=''
				OR NOT EXISTS(SELECT 1 FROM memory_sources ms
					WHERE ms.memory_id=m.id AND ms.event_id=e.id)
				OR e.id IS NULL OR e.scope!=m.scope OR e.active=0 OR e.suppressed!=0
				OR memory_user_text_reliable(e.role,e.text,e.payload)=0
				OR NOT (`+legacySourceIdentityMatches+`)
				OR EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id)
				OR EXISTS(SELECT 1 FROM source_conflicts c WHERE c.scope=e.scope AND c.event_id=e.id)
		)
		AND NOT EXISTS(
			SELECT 1 FROM memory_sources ms
			LEFT JOIN events e ON e.id=ms.event_id
			WHERE ms.memory_id=m.id
			AND (e.id IS NULL OR e.scope!=m.scope OR e.active=0 OR e.suppressed!=0
				OR memory_user_text_reliable(e.role,e.text,e.payload)=0
				OR NOT (`+legacySourceIdentityMatches+`)
				OR EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id)
				OR EXISTS(SELECT 1 FROM source_conflicts c WHERE c.scope=e.scope AND c.event_id=e.id))
		)`+reliableMemorySourcesSQL("m"), scope, account, thread, account, thread).Scan(&count)
	return count, err
}

func legacyInventoryError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: invalid snapshot", ErrLegacyMemoryUnsupported)
}

type legacyMemorySnapshotFS struct {
	file *os.File
	info fs.FileInfo
}

func (f legacyMemorySnapshotFS) Open(name string) (fs.File, error) {
	if name != "memory.sqlite" {
		return nil, fs.ErrNotExist
	}
	return legacyMemorySnapshotFile{
		SectionReader: io.NewSectionReader(f.file, 0, f.info.Size()),
		info:          f.info,
	}, nil
}

type legacyMemorySnapshotFile struct {
	*io.SectionReader
	info fs.FileInfo
}

func (f legacyMemorySnapshotFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (legacyMemorySnapshotFile) Close() error                 { return nil }
