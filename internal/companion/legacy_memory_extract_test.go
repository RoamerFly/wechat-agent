package companion

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadLegacyMemorySelectionKeepsOnlyBoundedSourcesAndFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(memorySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES('schema','2')`); err != nil {
		t.Fatal(err)
	}
	account, thread := "account-a", "thread-a"
	scope := Scope{ID: ScopeID("wechat", account, thread), Channel: "wechat", Account: account, Thread: thread}
	insertLegacyScope(t, db, scope.ID, scope)
	noteThread := "note-only@chatroom"
	noteScope := Scope{ID: ScopeID("wechat", account, noteThread), Channel: "wechat", Account: account, Thread: noteThread,
		Preferences: "合成备注", PreferencesRevision: 1}
	insertLegacyScope(t, db, noteScope.ID, noteScope)
	insertBoundLegacyEvent(t, db, "event-good", scope.ID, account, thread, 1, 1, "keep this source")
	insertBoundLegacyEvent(t, db, "event-revised", scope.ID, account, thread, 2, 2, "rewritten source")
	insertBoundLegacyEvent(t, db, "event-blocked", scope.ID, account, thread, 3, 1, "blocked source")
	if _, err := db.Exec(`INSERT INTO memory_blocks(scope,event_id) VALUES(?,?)`, scope.ID, "event-blocked"); err != nil {
		t.Fatal(err)
	}
	insertBoundLegacyFact(t, db, "fact-good", scope.ID, "event-good", "keep this", 1, false)
	insertBoundLegacyFact(t, db, "fact-revised", scope.ID, "event-revised", "rewritten", 1, false)
	insertBoundLegacyFact(t, db, "fact-blocked", scope.ID, "event-blocked", "blocked", 1, false)
	insertBoundLegacyFact(t, db, "fact-inference", scope.ID, "event-good", "keep this", 1, true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	selection, err := ReadLegacyMemorySelection(context.Background(), f, account, thread)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Events) != 1 || selection.Events[0].EventID != "event-good" || selection.Events[0].MessageID != "message-event-good" || selection.Events[0].Revision != 1 {
		t.Fatalf("events = %#v", selection.Events)
	}
	if len(selection.Memories) != 1 || selection.Memories[0].ID != "fact-good" || len(selection.Memories[0].Sources) != 1 || selection.Memories[0].Sources[0] != (LegacyMemoryFactSource{EventID: "event-good", Revision: 1}) {
		t.Fatalf("facts = %#v", selection.Memories)
	}
	note, err := ReadLegacyConversationPreference(context.Background(), f, account, noteThread)
	if err != nil || note != noteScope.Preferences {
		t.Fatalf("read preference without events: %q, %v", note, err)
	}
	if note, err := ReadLegacyConversationPreference(context.Background(), f, "account-b", noteThread); err == nil || note != "" {
		t.Fatalf("cross-account read returned %q, %v", note, err)
	}
}

func TestReadLegacyMemorySelectionRejectsHashAndScopeMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(memorySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES('schema','2')`); err != nil {
		t.Fatal(err)
	}
	account, thread := "account-a", "thread-a"
	scope := Scope{ID: ScopeID("wechat", account, thread), Channel: "wechat", Account: account, Thread: thread}
	insertLegacyScope(t, db, scope.ID, scope)
	insertBoundLegacyEvent(t, db, "event-hash", scope.ID, account, thread, 1, 1, "actual text")
	if _, err := db.Exec(`UPDATE events SET hash='not-the-body-hash' WHERE id='event-hash'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	selection, err := ReadLegacyMemorySelection(context.Background(), f, account, thread)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Events) != 0 || len(selection.Memories) != 0 {
		t.Fatalf("unexpected selection = %#v", selection)
	}
}

func insertBoundLegacyEvent(t *testing.T, db *sql.DB, id, scopeID, account, thread string, seq, revision int, text string) {
	t.Helper()
	at := memoryTime(time.Date(2026, time.January, 2, 3, 4, seq, 0, time.UTC))
	turn := Turn{
		EventID: id, TraceID: "message-" + id, Role: "user", Sender: "sender-a", Text: text, FullText: text, HasOriginal: true,
		Time: time.Date(2026, time.January, 2, 3, 4, seq, 0, time.UTC), UserTextState: "verified_original",
		Source: &SourceRef{InstanceID: "legacy-instance", EventID: id, MessageID: "message-" + id, ServerID: "server-" + id, Account: account, Thread: thread, Sender: "sender-a", MessageType: 1, OccurredAt: at, Revision: revision, BodyHash: sourceHash(text), Integrity: "observed"},
	}
	payload, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO events(id,scope,seq,trace,role,sender,at,text,hash,complete,revision,suppressed,active,payload) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, scopeID, seq, turn.TraceID, turn.Role, turn.Sender, at, text, sourceHash(text), 1, revision, 0, 1, payload); err != nil {
		t.Fatal(err)
	}
}

func insertBoundLegacyFact(t *testing.T, db *sql.DB, id, scopeID, eventID, quote string, version int, inference bool) {
	t.Helper()
	entry := MemoryEntry{ID: id, Scope: scopeID, Kind: "fact", Status: "active", Version: version, Detail: "safe fact " + id, Sources: []MemorySource{{EventID: eventID, Quote: quote}}, Inference: inference}
	payload, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories(id,scope,subject,kind,status,version,pinned,updated,payload) VALUES(?,?,?,?,?,?,?,?,?)`, id, scopeID, "subject", "fact", "active", version, 0, "2026", payload); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_sources(memory_id,event_id,quote) VALUES(?,?,?)`, id, eventID, quote); err != nil {
		t.Fatal(err)
	}
}
