package pluginhost

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func liveSessionFixture(id, key, fingerprint string) LiveSessionRecord {
	return LiveSessionRecord{ID: id, RequestKey: key, Fingerprint: fingerprint, Active: true, Payload: json.RawMessage(`{"stage":"starting"}`)}
}

func TestLiveSessionDurableIdempotencyAndTerminalHistory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(dir, "fish")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, created, err := store.CreateLiveSession(ctx, liveSessionFixture("live-one", "request-one", "input-v1"))
	if err != nil || !created || first.Revision != 1 || !first.Active {
		t.Fatalf("first create = %+v, %v, %v", first, created, err)
	}
	replay, created, err := store.CreateLiveSession(ctx, liveSessionFixture("other-id-is-ignored", "request-one", "input-v1"))
	if err != nil || created || replay.ID != first.ID || replay.Revision != first.Revision {
		t.Fatalf("replay = %+v, %v, %v", replay, created, err)
	}
	if _, _, err = store.CreateLiveSession(ctx, liveSessionFixture("live-conflict", "request-one", "input-v2")); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed request accepted: %v", err)
	}
	if _, _, err = store.CreateLiveSession(ctx, liveSessionFixture("live-two", "request-two", "input-v1")); !errors.Is(err, ErrLiveSessionBusy) {
		t.Fatalf("second active session accepted: %v", err)
	}
	updated, err := store.UpdateLiveSession(ctx, first.ID, first.Revision, true, json.RawMessage(`{"stage":"running"}`))
	if err != nil || updated.Revision != 2 || !updated.Active {
		t.Fatalf("active update = %+v, %v", updated, err)
	}
	if _, err = store.UpdateLiveSession(ctx, first.ID, first.Revision, true, json.RawMessage(`{"stage":"stale"}`)); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale update accepted: %v", err)
	}
	terminal, err := store.UpdateLiveSession(ctx, first.ID, updated.Revision, false, json.RawMessage(`{"stage":"finished"}`))
	if err != nil || terminal.Revision != 3 || terminal.Active {
		t.Fatalf("terminal update = %+v, %v", terminal, err)
	}
	if _, err = store.ActiveLiveSession(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("terminal session still active: %v", err)
	}
	if _, err = store.UpdateLiveSession(ctx, first.ID, terminal.Revision, true, json.RawMessage(`{"stage":"reopened"}`)); !errors.Is(err, ErrRevision) {
		t.Fatalf("terminal session reopened: %v", err)
	}
	byRequest, err := store.LiveSessionByRequest(ctx, "request-one")
	if err != nil || byRequest.ID != first.ID || byRequest.Active || byRequest.Revision != terminal.Revision {
		t.Fatalf("historical request lookup = %+v, %v", byRequest, err)
	}
	if _, err = store.LiveSessionByRequest(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing request = %v", err)
	}
	second, created, err := store.CreateLiveSession(ctx, liveSessionFixture("live-two", "request-two", "input-v1"))
	if err != nil || !created || !second.Active {
		t.Fatalf("next session = %+v, %v, %v", second, created, err)
	}
	if historical, err := store.LiveSession(ctx, first.ID); err != nil || historical.Active || string(historical.Payload) != `{"stage":"finished"}` {
		t.Fatalf("historical row changed = %+v, %v", historical, err)
	}
}

func TestLiveSessionConcurrentActiveReservation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	left, err := OpenStore(dir, "fish")
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := OpenStore(dir, "fish")
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	start := make(chan struct{})
	type outcome struct {
		created bool
		err     error
	}
	results := make(chan outcome, 2)
	var group sync.WaitGroup
	for _, input := range []struct {
		store  *Store
		record LiveSessionRecord
	}{
		{left, liveSessionFixture("live-left", "request-left", "left")},
		{right, liveSessionFixture("live-right", "request-right", "right")},
	} {
		group.Add(1)
		go func(store *Store, record LiveSessionRecord) {
			defer group.Done()
			<-start
			_, created, createErr := store.CreateLiveSession(ctx, record)
			results <- outcome{created, createErr}
		}(input.store, input.record)
	}
	close(start)
	group.Wait()
	close(results)
	createdCount, busyCount := 0, 0
	for result := range results {
		if result.err == nil && result.created {
			createdCount++
			continue
		}
		if errors.Is(result.err, ErrLiveSessionBusy) {
			busyCount++
			continue
		}
		t.Fatalf("unexpected concurrent result: %+v", result)
	}
	if createdCount != 1 || busyCount != 1 {
		t.Fatalf("created=%d busy=%d", createdCount, busyCount)
	}
	active, err := left.ActiveLiveSession(ctx)
	if err != nil || !active.Active {
		t.Fatalf("active record = %+v, %v", active, err)
	}
}
