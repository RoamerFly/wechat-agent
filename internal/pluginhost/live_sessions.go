package pluginhost

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const maxLiveSessionPayloadBytes = 1 << 20

var ErrLiveSessionBusy = errors.New("another live session is already active")

// LiveSessionRecord is the host-owned, durable coordination state for a live
// activity. Payload is deliberately opaque to this storage layer: callers own
// its schema, while the Store owns identity, revisions, and the active slot.
type LiveSessionRecord struct {
	ID          string          `json:"id"`
	RequestKey  string          `json:"request_key"`
	Fingerprint string          `json:"fingerprint"`
	Revision    int64           `json:"revision"`
	Active      bool            `json:"active"`
	Payload     json.RawMessage `json:"payload"`
}

func validLiveSessionText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}

func validLiveSessionPayload(payload json.RawMessage) bool {
	return len(payload) > 0 && len(payload) <= maxLiveSessionPayloadBytes && json.Valid(payload)
}

func validateLiveSessionRecord(record LiveSessionRecord, creating bool) error {
	if !validLiveSessionText(record.ID, 256) || !validLiveSessionText(record.RequestKey, 512) || !validLiveSessionText(record.Fingerprint, 256) || !validLiveSessionPayload(record.Payload) {
		return errors.New("invalid live session record")
	}
	if creating && !record.Active {
		return errors.New("live session must start active")
	}
	return nil
}

func cloneLiveSession(record LiveSessionRecord) LiveSessionRecord {
	record.Payload = append(json.RawMessage(nil), record.Payload...)
	return record
}

func scanLiveSession(scanner interface{ Scan(...any) error }) (LiveSessionRecord, error) {
	var record LiveSessionRecord
	var active int
	if err := scanner.Scan(&record.ID, &record.RequestKey, &record.Fingerprint, &record.Revision, &active, &record.Payload); err != nil {
		return LiveSessionRecord{}, err
	}
	if record.Revision < 1 || (active != 0 && active != 1) || !validLiveSessionText(record.ID, 256) || !validLiveSessionText(record.RequestKey, 512) || !validLiveSessionText(record.Fingerprint, 256) || !validLiveSessionPayload(record.Payload) {
		return LiveSessionRecord{}, errors.New("invalid live session record in store")
	}
	record.Active = active == 1
	return cloneLiveSession(record), nil
}

const liveSessionColumns = `id,request_key,fingerprint,revision,active,payload`

// LiveSession reads a session by its durable ID. sql.ErrNoRows is preserved so
// a coordinator can distinguish absence from a failed store.
func (s *Store) LiveSession(ctx context.Context, id string) (LiveSessionRecord, error) {
	if !validLiveSessionText(id, 256) {
		return LiveSessionRecord{}, errors.New("invalid live session id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return scanLiveSession(s.db.QueryRowContext(ctx, `SELECT `+liveSessionColumns+` FROM live_sessions WHERE id=?`, id))
}

// LiveSessionByRequest reads the durable idempotency binding, including a
// terminal historical row. Callers use it before attempting to create a new
// session after a previous grant or controller process has expired.
func (s *Store) LiveSessionByRequest(ctx context.Context, requestKey string) (LiveSessionRecord, error) {
	if !validLiveSessionText(requestKey, 512) {
		return LiveSessionRecord{}, errors.New("invalid live session request key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return scanLiveSession(s.db.QueryRowContext(ctx, `SELECT `+liveSessionColumns+` FROM live_sessions WHERE request_key=?`, requestKey))
}

// ActiveLiveSession returns this fish's single active live session. Historical
// terminal rows remain addressable through LiveSession and LiveSessionByRequest.
func (s *Store) ActiveLiveSession(ctx context.Context) (LiveSessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return LiveSessionRecord{}, err
	}
	// Finish this single indexed local read once admitted. The SQLite driver
	// can discard an already-created row handle when cancelled during query
	// setup, leaving the Windows file open after the database has closed.
	return scanLiveSession(s.db.QueryRowContext(context.WithoutCancel(ctx), `SELECT `+liveSessionColumns+` FROM live_sessions WHERE active=1`))
}

// CreateLiveSession atomically reserves the per-fish active slot. A replay of
// the same request key and fingerprint returns the original row; a changed
// invocation never silently reuses it.
func (s *Store) CreateLiveSession(ctx context.Context, record LiveSessionRecord) (LiveSessionRecord, bool, error) {
	if err := validateLiveSessionRecord(record, true); err != nil {
		return LiveSessionRecord{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if previous, err := scanLiveSession(s.db.QueryRowContext(ctx, `SELECT `+liveSessionColumns+` FROM live_sessions WHERE request_key=?`, record.RequestKey)); err == nil {
		if previous.Fingerprint != record.Fingerprint {
			return LiveSessionRecord{}, false, ErrRequestConflict
		}
		return previous, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return LiveSessionRecord{}, false, err
	}

	record.Revision = 1
	record.Active = true
	record = cloneLiveSession(record)
	_, err := s.db.ExecContext(ctx, `INSERT INTO live_sessions(id,request_key,fingerprint,revision,active,payload) VALUES(?,?,?,?,1,?)`, record.ID, record.RequestKey, record.Fingerprint, record.Revision, []byte(record.Payload))
	if err == nil {
		return record, true, nil
	}
	// A concurrent controller can win either unique race. Re-read the request
	// key first so an identical replay remains idempotent instead of looking
	// like an active-slot collision.
	if previous, lookupErr := scanLiveSession(s.db.QueryRowContext(ctx, `SELECT `+liveSessionColumns+` FROM live_sessions WHERE request_key=?`, record.RequestKey)); lookupErr == nil {
		if previous.Fingerprint == record.Fingerprint {
			return previous, false, nil
		}
		return LiveSessionRecord{}, false, ErrRequestConflict
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return LiveSessionRecord{}, false, lookupErr
	}
	if _, activeErr := scanLiveSession(s.db.QueryRowContext(ctx, `SELECT `+liveSessionColumns+` FROM live_sessions WHERE active=1`)); activeErr == nil {
		return LiveSessionRecord{}, false, ErrLiveSessionBusy
	} else if !errors.Is(activeErr, sql.ErrNoRows) {
		return LiveSessionRecord{}, false, activeErr
	}
	return LiveSessionRecord{}, false, err
}

// UpdateLiveSession performs an optimistic CAS update. Only active sessions
// are mutable: transitioning active to false is terminal, so a historical row
// can never be reopened by a late controller or retry.
func (s *Store) UpdateLiveSession(ctx context.Context, id string, revision int64, active bool, payload json.RawMessage) (LiveSessionRecord, error) {
	if !validLiveSessionText(id, 256) || revision < 1 || !validLiveSessionPayload(payload) {
		return LiveSessionRecord{}, errors.New("invalid live session update")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := revision + 1
	result, err := s.db.ExecContext(ctx, `UPDATE live_sessions SET revision=?,active=?,payload=? WHERE id=? AND revision=? AND active=1`, next, boolToSQLite(active), []byte(payload), id, revision)
	if err != nil {
		return LiveSessionRecord{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return LiveSessionRecord{}, err
	}
	if count != 1 {
		current, readErr := scanLiveSession(s.db.QueryRowContext(ctx, `SELECT `+liveSessionColumns+` FROM live_sessions WHERE id=?`, id))
		if readErr != nil {
			return LiveSessionRecord{}, readErr
		}
		return current, ErrRevision
	}
	return scanLiveSession(s.db.QueryRowContext(ctx, `SELECT `+liveSessionColumns+` FROM live_sessions WHERE id=?`, id))
}

func boolToSQLite(value bool) int {
	if value {
		return 1
	}
	return 0
}
