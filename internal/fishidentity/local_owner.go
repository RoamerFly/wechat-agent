package fishidentity

import (
	"errors"

	"wechat-bot/internal/identity"
)

// GuardHome keeps an authorized mutation ordered with pause, resume and
// updates from another process. The callback must not call the registry.
func (r *Registry) GuardHome(fishID, ownerID string, revision uint64, mutate func(Record) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	release, err := r.acquire()
	if err != nil {
		return err
	}
	defer release()
	record, err := r.readHeld()
	if err != nil || record.FishID != fishID || record.LocalOwner == nil || !record.LocalOwner.HomeAccess ||
		record.LocalOwner.ID != ownerID || record.LocalOwner.Revision != revision {
		return ErrHomeRevisionConflict
	}
	return mutate(record)
}

// InitializeLocal explicitly creates the one local fish and its owner.
// It is deliberately idempotent: retrying initialization must not silently
// re-enable a home that its owner has paused.
func (r *Registry) InitializeLocal() (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	release, err := r.acquire()
	if err != nil {
		return Record{}, err
	}
	defer release()

	record, err := r.readHeld()
	if err != nil {
		return Record{}, err
	}
	changed := false
	if record.FishID == "" {
		record.FishID = "fish-" + identity.RandomToken()[:48]
		changed = true
	}
	if record.LocalOwner == nil {
		record.LocalOwner = &LocalOwner{
			ID:         fishLocalOwnerIDPrefix + identity.RandomToken()[:fishLocalOwnerTokenChars],
			Revision:   1,
			HomeAccess: true,
		}
		changed = true
	}
	if changed {
		// write normalizes on a value copy; make the returned record reflect the
		// persisted schema version as well.
		record.Version = fishIdentityCurrentVersion
		if err := r.write(record); err != nil {
			return Record{}, err
		}
	}
	return record, nil
}

// SetHomeAccess performs a compare-and-swap on the grant so an older
// settings page cannot reverse a newer pause or resume. Repeating a successful
// request with the current revision is intentionally a no-op.
func (r *Registry) SetHomeAccess(expected uint64, enabled bool) (Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	release, err := r.acquire()
	if err != nil {
		return Record{}, err
	}
	defer release()

	record, err := r.readHeld()
	if err != nil {
		return Record{}, err
	}
	if record.LocalOwner == nil {
		return Record{}, ErrHomeNotInitialized
	}
	if record.LocalOwner.Revision != expected {
		return Record{}, ErrHomeRevisionConflict
	}
	if record.LocalOwner.HomeAccess == enabled {
		return record, nil
	}
	record.LocalOwner.HomeAccess = enabled
	record.LocalOwner.Revision++
	if record.LocalOwner.Revision == 0 {
		return Record{}, errors.New("本机家目录授权版本已耗尽")
	}
	if err := r.write(record); err != nil {
		return Record{}, err
	}
	return record, nil
}
