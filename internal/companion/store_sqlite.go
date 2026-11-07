package companion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	_ "modernc.org/sqlite"
)

const memorySchema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS scopes(id TEXT PRIMARY KEY,payload BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS events(
 id TEXT PRIMARY KEY,scope TEXT NOT NULL,seq INTEGER NOT NULL,trace TEXT NOT NULL,
 role TEXT NOT NULL,sender TEXT NOT NULL,at TEXT NOT NULL,text TEXT NOT NULL,
 hash TEXT NOT NULL,complete INTEGER NOT NULL,revision INTEGER NOT NULL DEFAULT 1,
 suppressed INTEGER NOT NULL DEFAULT 0,active INTEGER NOT NULL DEFAULT 1,payload BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS events_scope_seq ON events(scope,seq);
CREATE INDEX IF NOT EXISTS events_scope_time ON events(scope,at,id);
CREATE INDEX IF NOT EXISTS events_scope_trace ON events(scope,trace);
CREATE TABLE IF NOT EXISTS local_tool_checkpoints(
 event_id TEXT NOT NULL,revision INTEGER NOT NULL,call_key TEXT NOT NULL,
 tool TEXT NOT NULL,grant_key TEXT NOT NULL,target TEXT NOT NULL,result TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(event_id,revision,call_key));
CREATE TABLE IF NOT EXISTS local_task_followups(
 id TEXT PRIMARY KEY,scope TEXT NOT NULL,request_id TEXT NOT NULL,process_id TEXT NOT NULL,
 home_revision INTEGER NOT NULL,source_event_id TEXT NOT NULL,source_revision INTEGER NOT NULL,
 source_hash TEXT NOT NULL,source BLOB NOT NULL,state TEXT NOT NULL,next_at TEXT NOT NULL,
 created_at TEXT NOT NULL,updated_at TEXT NOT NULL,reason TEXT NOT NULL,revision INTEGER NOT NULL,
 result_tool TEXT NOT NULL DEFAULT '',job_id TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS local_task_followups_due ON local_task_followups(scope,state,next_at,id);
CREATE INDEX IF NOT EXISTS local_task_followups_page ON local_task_followups(scope,id);
CREATE INDEX IF NOT EXISTS events_reflection_observable_v2 ON events(scope,seq DESC,id DESC) WHERE role='user' AND suppressed=0 AND instr(CAST(payload AS TEXT),'"observation_excerpt":')>0;
CREATE TABLE IF NOT EXISTS observations(event_id TEXT,hash TEXT,text TEXT,PRIMARY KEY(event_id,hash));
CREATE TABLE IF NOT EXISTS event_revisions(event_id TEXT,revision INTEGER,text TEXT,hash TEXT,PRIMARY KEY(event_id,revision));
CREATE TABLE IF NOT EXISTS event_terms(term TEXT,event_id TEXT,scope TEXT,PRIMARY KEY(term,event_id));
CREATE INDEX IF NOT EXISTS event_terms_scope ON event_terms(scope,term,event_id);
CREATE INDEX IF NOT EXISTS event_terms_event ON event_terms(event_id);
CREATE TABLE IF NOT EXISTS jobs(scope TEXT PRIMARY KEY,processed_seq INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'pending',error TEXT NOT NULL DEFAULT '',updated TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS memories(id TEXT PRIMARY KEY,scope TEXT NOT NULL,subject TEXT NOT NULL,kind TEXT NOT NULL,status TEXT NOT NULL,version INTEGER NOT NULL,pinned INTEGER NOT NULL DEFAULT 0,updated TEXT NOT NULL,payload BLOB NOT NULL,statement_at TEXT NOT NULL DEFAULT '',event_at TEXT NOT NULL DEFAULT '',event_year INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS memories_scope ON memories(scope,status,kind,updated,id);
CREATE TABLE IF NOT EXISTS memory_sources(memory_id TEXT,event_id TEXT,quote TEXT,PRIMARY KEY(memory_id,event_id));
CREATE INDEX IF NOT EXISTS memory_sources_event ON memory_sources(event_id,memory_id);
CREATE TABLE IF NOT EXISTS memory_terms(term TEXT,memory_id TEXT,scope TEXT,PRIMARY KEY(term,memory_id));
CREATE INDEX IF NOT EXISTS memory_terms_scope ON memory_terms(scope,term,memory_id);
CREATE INDEX IF NOT EXISTS memory_terms_memory ON memory_terms(memory_id);
CREATE TABLE IF NOT EXISTS memory_revisions(memory_id TEXT,version INTEGER,payload BLOB,PRIMARY KEY(memory_id,version));
CREATE TABLE IF NOT EXISTS embeddings(memory_id TEXT PRIMARY KEY,space TEXT NOT NULL,version INTEGER NOT NULL,vector BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS embeddings_space ON embeddings(space,memory_id);
CREATE TABLE IF NOT EXISTS maintenance_usage(day TEXT,purpose TEXT,reported INTEGER,tokens INTEGER,requests INTEGER,PRIMARY KEY(day,purpose,reported));
CREATE TABLE IF NOT EXISTS memory_blocks(scope TEXT,event_id TEXT,PRIMARY KEY(scope,event_id));
CREATE TABLE IF NOT EXISTS rebuild_requests(scope TEXT PRIMARY KEY,requested TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS search_documents(namespace TEXT,id TEXT,scope TEXT,kind TEXT,at TEXT,text TEXT,payload BLOB,hash TEXT,PRIMARY KEY(namespace,id));
CREATE INDEX IF NOT EXISTS search_documents_scope_time ON search_documents(scope,at,id);
CREATE TABLE IF NOT EXISTS source_conflicts(scope TEXT,event_id TEXT,other_event_id TEXT,reason TEXT,PRIMARY KEY(scope,event_id,other_event_id));
CREATE TABLE IF NOT EXISTS context_metrics(id INTEGER PRIMARY KEY,at TEXT,total INTEGER,bytes INTEGER,components TEXT,method TEXT);
CREATE TABLE IF NOT EXISTS experience_reflections(
 id TEXT PRIMARY KEY,scope TEXT NOT NULL,source_event_id TEXT NOT NULL,source_event_revision INTEGER NOT NULL,
 source_observation_version INTEGER NOT NULL,source_observation_hash TEXT NOT NULL,
 parent_experience_id TEXT NOT NULL DEFAULT '',parent_experience_version INTEGER NOT NULL DEFAULT 0,
 status TEXT NOT NULL,revision INTEGER NOT NULL,created TEXT NOT NULL,updated TEXT NOT NULL,payload BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS experience_reflections_scope_updated ON experience_reflections(scope,status,updated,id);
CREATE INDEX IF NOT EXISTS experience_reflections_source ON experience_reflections(scope,source_event_id);
CREATE INDEX IF NOT EXISTS experience_reflections_recent_feeling ON experience_reflections(scope,created DESC,id DESC) WHERE status='active' AND instr(CAST(payload AS TEXT),'"feeling":')>0;
CREATE TABLE IF NOT EXISTS experience_reflection_revisions(reflection_id TEXT,revision INTEGER,payload BLOB NOT NULL,PRIMARY KEY(reflection_id,revision));
`

// Existing ledgers predate tool targets. Inspect columns before altering so
// opening a current store or retrying after interruption is safe and preserves rows.
func upgradeLocalTaskFollowupTargets(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("PRAGMA table_info(local_task_followups)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, name := range []string{"result_tool", "job_id"} {
		if !columns[name] {
			if _, err = tx.Exec("ALTER TABLE local_task_followups ADD COLUMN " + name + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Fixed-width UTC timestamps sort chronologically in SQLite TEXT indexes,
// including whole seconds and fractional seconds in the same result set.
func memoryTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// Migration is one SQLite transaction. An interrupted import has no completion
// marker and is retried from the unchanged legacy file, never from partial rows.
func openSQLiteStore(dir string) (*Store, error) { return openStoreWithLease(dir, false) }

func openSQLiteStoreUnlocked(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "memory.sqlite")
	if _, err := os.Lstat(path); err == nil {
		return openSQLiteAt(dir, path, false)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.CreateTemp(dir, ".memory-migration-*.sqlite")
	if err != nil {
		return nil, err
	}
	stage := file.Name()
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	file.Close()
	s, err := openSQLiteAt(dir, stage, true)
	if err != nil {
		return nil, err
	}
	if err = s.Verify(context.Background()); err == nil {
		err = s.verifyProjection(context.Background())
	}
	if err == nil {
		_, err = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	}
	closeErr := s.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	// Same-volume publication cannot replace another process's database.
	if err = os.Link(stage, path); err != nil {
		return nil, fmt.Errorf("memory migration publication failed; staged database preserved: %w", err)
	}
	_ = os.Remove(stage)
	return openSQLiteAt(dir, path, false)
}

func openSQLiteAt(dir, path string, allowImport bool) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil && (!st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
		return nil, errors.New("unsafe memory database")
	}
	if !allowImport {
		probe, e := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
		if e != nil {
			return nil, e
		}
		var version string
		e = probe.QueryRow("SELECT value FROM meta WHERE key='schema'").Scan(&version)
		closeErr := probe.Close()
		if e != nil || version != "1" && version != "2" {
			return nil, errors.New("unsupported or incomplete memory database; preserved for inspection")
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(e error) (*Store, error) { db.Close(); return nil, e }
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000", memorySchema} {
		if _, err = db.Exec(q); err != nil {
			return fail(err)
		}
	}
	if err = upgradeLocalTaskFollowupTargets(db); err != nil {
		return fail(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		return fail(err)
	}
	var version string
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema'").Scan(&version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fail(err)
	}
	s := &Store{db: db, path: path, data: persisted{Version: 1, Scopes: map[string]Scope{}}}
	if errors.Is(err, sql.ErrNoRows) {
		if !allowImport {
			return fail(errors.New("memory database has no migration marker; preserved for inspection"))
		}
		legacy, e := readLegacyStore(dir)
		if e != nil {
			return fail(e)
		}
		s.data = legacy.data
		tx, e := db.Begin()
		if e != nil {
			return fail(e)
		}
		for id, scope := range s.data.Scopes {
			if e = s.writeScope(tx, id, Scope{}, scope); e != nil {
				tx.Rollback()
				return fail(e)
			}
		}
		state := s.data
		state.Scopes = nil
		b, _ := json.Marshal(state)
		if _, e = tx.Exec("INSERT INTO meta(key,value) VALUES('schema','2'),('state',?)", string(b)); e != nil {
			tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
	} else {
		if version == "1" {
			if err = upgradeMemoryTimes(db); err != nil {
				return fail(err)
			}
			version = "2"
		}
		if version != "2" {
			return fail(errors.New("unsupported memory schema; database preserved"))
		}
		var raw string
		if err = db.QueryRow("SELECT value FROM meta WHERE key='state'").Scan(&raw); err != nil {
			return fail(err)
		}
		if err = json.Unmarshal([]byte(raw), &s.data); err != nil {
			return fail(err)
		}
		s.data.Scopes = map[string]Scope{}
		rows, e := db.Query("SELECT id,payload FROM scopes")
		if e != nil {
			return fail(e)
		}
		for rows.Next() {
			var id string
			var b []byte
			var scope Scope
			if e = rows.Scan(&id, &b); e != nil {
				break
			}
			if e = json.Unmarshal(b, &scope); e != nil {
				break
			}
			if !validScopeID(id) || scope.ID != id || ScopeID(scope.Channel, scope.Account, scope.Thread) != id {
				e = errors.New("invalid persisted scope")
				break
			}
			s.data.Scopes[id] = scope
		}
		err = rows.Err()
		rows.Close()
		if e != nil {
			return fail(e)
		}
		if err != nil {
			return fail(err)
		}
	}
	if err := s.initializeSourceInstance(); err != nil {
		return fail(err)
	}
	return s, nil
}

func upgradeMemoryTimes(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{"ALTER TABLE memories ADD COLUMN statement_at TEXT NOT NULL DEFAULT ''", "ALTER TABLE memories ADD COLUMN event_at TEXT NOT NULL DEFAULT ''", "ALTER TABLE memories ADD COLUMN event_year INTEGER NOT NULL DEFAULT 0"} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	rows, err := tx.Query("SELECT id,at FROM events")
	if err != nil {
		return err
	}
	type stampRow struct{ id, at string }
	stamps := []stampRow{}
	for rows.Next() {
		var r stampRow
		if err = rows.Scan(&r.id, &r.at); err != nil {
			break
		}
		stamps = append(stamps, r)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	for _, r := range stamps {
		stamp, e := time.Parse(time.RFC3339Nano, r.at)
		if e != nil {
			return e
		}
		if _, err = tx.Exec("UPDATE events SET at=? WHERE id=?", memoryTime(stamp), r.id); err != nil {
			return err
		}
	}
	rows, err = tx.Query("SELECT id,payload FROM memories")
	if err != nil {
		return err
	}
	type entryRow struct {
		id string
		m  MemoryEntry
	}
	entries := []entryRow{}
	for rows.Next() {
		var r entryRow
		var raw []byte
		if err = rows.Scan(&r.id, &raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &r.m); err != nil {
			break
		}
		entries = append(entries, r)
	}
	rowErr = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	for _, r := range entries {
		statement := r.m.LastMention
		if statement.IsZero() {
			statement = r.m.Created
		}
		if _, err = tx.Exec("UPDATE memories SET statement_at=?,updated=? WHERE id=?", memoryTime(statement), memoryTime(r.m.Updated), r.id); err != nil {
			return err
		}
	}
	_, err = tx.Exec("UPDATE meta SET value='2' WHERE key='schema'")
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	e := s.db.Close()
	s.db = nil
	if s.accessLease != nil {
		e = errors.Join(e, s.accessLease.Close())
		s.accessLease = nil
	}
	return e
}

func (s *Store) scopeTransaction(id string, change func(*persisted) error) error {
	return s.transact([]string{id}, change)
}

// The writer mutex provides a bounded, cancellation-free commit boundary for
// existing synchronous claims. Only the named scope is copied/serialized on the
// hot path; unrelated conversations do not amplify one message write.
func (s *Store) transact(ids []string, change func(*persisted) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errors.New("memory database closed")
	}
	before := s.data
	before.Scopes = map[string]Scope{}
	if ids == nil {
		for id := range s.data.Scopes {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if v, ok := s.data.Scopes[id]; ok {
			before.Scopes[id] = copyScope(v)
		}
	}
	rollback := func() {
		for _, id := range ids {
			if v, ok := before.Scopes[id]; ok {
				s.data.Scopes[id] = v
			} else {
				delete(s.data.Scopes, id)
			}
		}
		scopes := s.data.Scopes
		s.data = before
		s.data.Scopes = scopes
	}
	if e := change(&s.data); e != nil {
		rollback()
		return e
	}
	tx, e := s.db.Begin()
	if e != nil {
		rollback()
		return e
	}
	defer tx.Rollback()
	for _, id := range ids {
		if v, ok := s.data.Scopes[id]; ok {
			if e = s.writeScope(tx, id, before.Scopes[id], v); e != nil {
				rollback()
				return e
			}
		}
	}
	state := s.data
	state.Scopes = nil
	b, e := json.Marshal(state)
	if e == nil {
		_, e = tx.Exec("INSERT INTO meta(key,value) VALUES('state',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(b))
	}
	if e == nil {
		e = bumpMemoryEpoch(tx)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		rollback()
	}
	return e
}

func bumpMemoryEpoch(tx *sql.Tx) error {
	_, e := tx.Exec("INSERT INTO meta(key,value) VALUES('data_epoch','1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1")
	return e
}

func eventID(scope string, t Turn, index int) string {
	if t.EventID != "" {
		return t.EventID
	}
	if t.Source != nil && t.Source.InstanceID != "" {
		return sourceTurnID(scope, t)
	}
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", scope, t.Role, t.TraceID, t.Time.Format(time.RFC3339Nano), index)
	if t.TraceID != "" {
		key = scope + "\x00" + t.Role + "\x00" + t.TraceID
		if t.Media != nil {
			key += "\x00media:" + t.Media.Key
		}
	}
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func (s *Store) writeScope(tx *sql.Tx, id string, old, next Scope) error {
	if old.Preferences != next.Preferences {
		if err := bumpRecallEpoch(tx); err != nil {
			return err
		}
	}
	revisedSource := false
	conflictedSource := false
	if reflect.DeepEqual(old, next) {
		return nil
	}
	recallChanged := old.RecallGeneration != next.RecallGeneration || old.Summary != next.Summary || !slices.Equal(old.Notes, next.Notes)
	// Clear creates a durable barrier. Read-only archive browsing remains possible,
	// but indexing, extraction and every model-facing query exclude this prefix.
	if old.RecallGeneration != next.RecallGeneration {
		if _, e := tx.Exec("UPDATE events SET suppressed=1 WHERE scope=?", id); e != nil {
			return e
		}
		if _, e := tx.Exec("UPDATE memories SET status='deleted',version=version+1 WHERE scope=? AND status!='deleted'", id); e != nil {
			return e
		}
	}
	previous := map[string]Turn{}
	for i, t := range old.Messages {
		previous[eventID(id, t, i)] = t
	}
	retained := map[string]bool{}
	for i, t := range next.Messages {
		retained[eventID(id, t, i)] = true
	}
	for event := range previous {
		if !retained[event] {
			if _, e := tx.Exec("UPDATE events SET active=0 WHERE id=?", event); e != nil {
				return e
			}
		}
	}
	for i := range next.Messages {
		t := &next.Messages[i]
		t.EventID = eventID(id, *t, i)
		if t.Source != nil {
			t.Source = cloneSource(t.Source)
			t.Source.EventID = t.EventID
		}
		if t.Source != nil {
			for j := range t.IncomingMedia {
				m := &t.IncomingMedia[j]
				if m.Source.MessageID == "" || m.Source.MessageID == t.Source.MessageID {
					m.Source = *t.Source
				}
			}
		}
		if prev, ok := previous[t.EventID]; ok && reflect.DeepEqual(prev, *t) {
			continue
		}
		if prev, ok := previous[t.EventID]; ok && t.Role == "user" &&
			(prev.Text != t.Text || prev.UserTextState != t.UserTextState || prev.FullObservation != t.FullObservation || !reflect.DeepEqual(prev.Source, t.Source) ||
				!reflect.DeepEqual(prev.QuotedSource, t.QuotedSource) || !reflect.DeepEqual(prev.Addressees, t.Addressees) ||
				!reflect.DeepEqual(prev.IncomingMedia, t.IncomingMedia)) {
			// Changes to an existing user's evidence include refreshed media
			// observations and attribution, even when the original body is intact.
			recallChanged = true
		}
		text, complete := t.Text, 0
		if t.FullText != "" || t.HasOriginal {
			text = t.FullText
			complete = 1
		}
		h := sha256.Sum256([]byte(text))
		hash := hex.EncodeToString(h[:])
		payload, _ := json.Marshal(t)
		if t.FullObservation != "" {
			oh := sha256.Sum256([]byte(t.FullObservation))
			if _, e := tx.Exec("INSERT OR IGNORE INTO observations VALUES(?,?,?)", t.EventID, hex.EncodeToString(oh[:]), t.FullObservation); e != nil {
				return e
			}
		}
		var oldHash string
		var revision int
		err := tx.QueryRow("SELECT hash,revision FROM events WHERE id=?", t.EventID).Scan(&oldHash, &revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if t.Source != nil && t.Source.Integrity == "conflicted" {
			conflictedSource = true
			if _, err = tx.Exec("INSERT OR IGNORE INTO source_conflicts(scope,event_id,other_event_id,reason) VALUES(?,?,?,?)", id, t.EventID, "", "source_identity_conflict"); err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE memories SET status='conflicted',version=version+1 WHERE id IN(SELECT memory_id FROM memory_sources WHERE event_id=?) AND status='active'", t.EventID); err != nil {
				return err
			}
			if _, err = tx.Exec("DELETE FROM embeddings WHERE memory_id IN(SELECT memory_id FROM memory_sources WHERE event_id=?)", t.EventID); err != nil {
				return err
			}
			next.Summary = ""
			next.SummaryBoundary = 0
			next.CompressedCount = 0
		}
		if oldHash == hash {
			if t.Source != nil {
				t.Source.Revision = revision
				payload, _ = json.Marshal(t)
			}
			if _, err = tx.Exec("UPDATE events SET payload=? WHERE id=?", payload, t.EventID); err != nil {
				return err
			}
			continue
		}
		if oldHash != "" {
			if t.FullText == "" && !t.HasOriginal {
				var complete int
				if e := tx.QueryRow("SELECT complete FROM events WHERE id=?", t.EventID).Scan(&complete); e != nil {
					return e
				}
				if complete != 0 {
					continue
				}
			}
			// Nickname/status refresh must not downgrade a retained full original.
			if t.FullText == "" {
				var oldText string
				if e := tx.QueryRow("SELECT text FROM events WHERE id=?", t.EventID).Scan(&oldText); e != nil {
					return e
				}
				if boundedInputMemory(oldText) == t.Text {
					continue
				}
			}
			if _, err = tx.Exec("INSERT OR IGNORE INTO event_revisions SELECT id,revision,text,hash FROM events WHERE id=?", t.EventID); err != nil {
				return err
			}
			revisedSource = true
			if _, err = tx.Exec("UPDATE memories SET status='conflicted',version=version+1 WHERE id IN(SELECT memory_id FROM memory_sources WHERE event_id=?) AND status='active'", t.EventID); err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE jobs SET processed_seq=MIN(processed_seq,(SELECT seq-1 FROM events WHERE id=?)),state='pending' WHERE scope=?", t.EventID, id); err != nil {
				return err
			}
		}
		writeRevision := max(1, revision)
		if oldHash != "" {
			writeRevision = revision + 1
		}
		if t.Source != nil {
			writeRevision = max(writeRevision, t.Source.Revision)
			t.Source.Revision = writeRevision
			payload, _ = json.Marshal(t)
		}
		inheritedBlock := 0
		if t.Role == "user" && t.Source != nil {
			if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM events e WHERE e.scope=? AND (e.trace=? OR json_extract(e.payload,'$.source.message_id')=?) AND e.sender=? AND e.at=? AND (e.suppressed!=0 OR EXISTS(SELECT 1 FROM memory_blocks b WHERE b.scope=e.scope AND b.event_id=e.id)))", id, t.TraceID, t.Source.MessageID, t.Sender, memoryTime(t.Time)).Scan(&inheritedBlock); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO events(id,scope,seq,trace,role,sender,at,text,hash,complete,revision,payload,suppressed)
VALUES(?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE scope=?),?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET text=excluded.text,hash=excluded.hash,revision=excluded.revision,payload=excluded.payload,complete=excluded.complete`, t.EventID, id, id, t.TraceID, t.Role, t.Sender, memoryTime(t.Time), text, hash, complete, writeRevision, payload, inheritedBlock)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM event_terms WHERE event_id=?", t.EventID); err != nil {
			return err
		}
		for _, term := range memoryTerms(text) {
			if _, err = tx.Exec("INSERT OR IGNORE INTO event_terms VALUES(?,?,?)", term, t.EventID, id); err != nil {
				return err
			}
		}
	}
	if revisedSource {
		next.Summary = ""
		next.Notes = nil
		next.CompressedCount = 0
		next.SummaryBoundary = 0
		if old.ID == id {
			s.data.Scopes[id] = next
		}
	}
	if conflictedSource {
		next.Summary = ""
		next.CompressedCount = 0
		next.SummaryBoundary = 0
		if old.ID == id {
			s.data.Scopes[id] = next
		}
	}
	if recallChanged || revisedSource || conflictedSource {
		if err := bumpRecallEpoch(tx); err != nil {
			return err
		}
	}
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO scopes VALUES(?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload", id, b); e != nil {
		return e
	}
	_, e = tx.Exec("INSERT OR IGNORE INTO jobs(scope) VALUES(?)", id)
	return e
}

// Backup uses SQLite's consistent online snapshot, including committed WAL data.
func (s *Store) Backup(ctx context.Context, path string) error {
	if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
		return errors.New("backup target already exists or is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e := s.db.ExecContext(ctx, "VACUUM INTO ?", path)
	if e == nil {
		e = os.Chmod(path, 0600)
	}
	return e
}

func (s *Store) Verify(ctx context.Context) error {
	var result string
	e := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result)
	if e != nil {
		return e
	}
	if result != "ok" {
		return errors.New("memory database integrity check failed")
	}
	return nil
}
