package companion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyMemoryInventoryMetadataOnlyProjection(t *testing.T) {
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
	scope := Scope{ID: ScopeID("wechat", account, thread), Channel: "wechat", Account: account, Thread: thread, Summary: "never return this"}
	insertLegacyScope(t, db, scope.ID, scope)
	insertLegacyEvent(t, db, "event-good", scope.ID, account, thread, 1, false)
	insertLegacyEvent(t, db, "event-blocked", scope.ID, account, thread, 2, false)
	insertLegacyEvent(t, db, "event-conflicted", scope.ID, account, thread, 3, false)
	if _, err := db.Exec(`INSERT INTO memory_blocks(scope,event_id) VALUES(?,?)`, scope.ID, "event-blocked"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO source_conflicts(scope,event_id,other_event_id,reason) VALUES(?,?,?,?)`, scope.ID, "event-conflicted", "other", "synthetic"); err != nil {
		t.Fatal(err)
	}
	insertLegacyMemory(t, db, "memory-good", scope.ID, "event-good")
	insertLegacyMemory(t, db, "memory-blocked", scope.ID, "event-blocked")
	if _, err := db.Exec(`INSERT INTO memories(id,scope,subject,kind,status,version,pinned,updated,payload) VALUES(?,?,?,?,?,?,?,?,?)`, "memory-zero", scope.ID, "subject", "fact", "active", 1, 0, "2026", `{}`); err != nil {
		t.Fatal(err)
	}
	// An old memory_sources index alone is not enough: the serialized source
	// list is the authoritative basis for a safe migration preview.
	if _, err := db.Exec(`INSERT INTO memories(id,scope,subject,kind,status,version,pinned,updated,payload) VALUES(?,?,?,?,?,?,?,?,?)`, "memory-index-only", scope.ID, "subject", "fact", "active", 1, 0, "2026", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_sources(memory_id,event_id,quote) VALUES(?,?,?)`, "memory-index-only", "event-good", "safe"); err != nil {
		t.Fatal(err)
	}

	foreign := Scope{ID: ScopeID("wechat", "another-account", "thread-z"), Channel: "wechat", Account: "another-account", Thread: "thread-z"}
	insertLegacyScope(t, db, foreign.ID, foreign)
	unmapped := Scope{ID: "not-a-scope-id", Channel: "wechat", Account: account, Thread: "thread-unmapped"}
	insertLegacyScope(t, db, unmapped.ID, unmapped)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := LegacyMemoryInventory(context.Background(), f, account)
	if err != nil {
		t.Fatal(err)
	}
	if got.MatchedScopes != 1 || got.ForeignScopes != 1 || got.UnmappedScopes != 1 {
		t.Fatalf("scope projection = %+v", got)
	}
	if len(got.Conversations) != 1 {
		t.Fatalf("conversations = %+v", got.Conversations)
	}
	conversation := got.Conversations[0]
	if conversation.ThreadID != thread || conversation.EventCount != 1 || conversation.WithheldEvents != 2 || conversation.MemoryCount != 1 || !conversation.HasSummary {
		t.Fatalf("conversation = %+v", conversation)
	}
}

func TestLegacyMemoryInventoryRejectsUnsupportedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(memorySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES('schema','99')`); err != nil {
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
	_, err = LegacyMemoryInventory(context.Background(), f, "account-a")
	if !errors.Is(err, ErrLegacyMemoryUnsupported) {
		t.Fatalf("error = %v, want ErrLegacyMemoryUnsupported", err)
	}
}

func insertLegacyScope(t *testing.T, db *sql.DB, id string, scope Scope) {
	t.Helper()
	payload, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO scopes(id,payload) VALUES(?,?)`, id, payload); err != nil {
		t.Fatal(err)
	}
}

func insertLegacyEvent(t *testing.T, db *sql.DB, id, scopeID, account, thread string, seq int, suppressed bool) {
	t.Helper()
	payload, err := json.Marshal(Turn{Role: "user", Text: "safe synthetic source", UserTextState: "verified_original", Source: &SourceRef{Account: account, Thread: thread}})
	if err != nil {
		t.Fatal(err)
	}
	suppressedValue := 0
	if suppressed {
		suppressedValue = 1
	}
	if _, err := db.Exec(`INSERT INTO events(id,scope,seq,trace,role,sender,at,text,hash,complete,revision,suppressed,active,payload)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, scopeID, seq, "trace", "user", "sender", "2026-01-01T00:00:00.000000000Z", "safe synthetic source", "hash", 1, 1, suppressedValue, 1, payload); err != nil {
		t.Fatal(err)
	}
}

func insertLegacyMemory(t *testing.T, db *sql.DB, id, scopeID, eventID string) {
	t.Helper()
	payload := `{"sources":[{"event_id":"` + eventID + `","quote":"safe"}]}`
	if _, err := db.Exec(`INSERT INTO memories(id,scope,subject,kind,status,version,pinned,updated,payload) VALUES(?,?,?,?,?,?,?,?,?)`, id, scopeID, "subject", "fact", "active", 1, 0, "2026", payload); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memory_sources(memory_id,event_id,quote) VALUES(?,?,?)`, id, eventID, "safe"); err != nil {
		t.Fatal(err)
	}
}
