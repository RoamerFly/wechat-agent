package devicespec

import (
	"context"
	"errors"

	"wechat-bot/internal/identity"
)

// Repository stores only the durable device settings and task records. Media
// observations, recordings and their private ledger stay in the adapter's host
// private directory and are deliberately not copied during migration.
// Revision is an adapter-owned CAS value; zero means no active host state yet.
type RepositorySnapshot struct {
	Settings    Settings        `json:"settings"`
	Tasks       map[string]Task `json:"tasks"`
	Revision    int64           `json:"revision"`
	Initialized bool            `json:"initialized"`
}

// LegacyImport is validated by ReadLegacyImport before reaching the host adapter.
// SourcePath intentionally is not exposed: callers receive only the stable
// content hash and records required for an atomic one-time import.
type LegacyImport struct {
	Settings        Settings        `json:"settings"`
	Tasks           map[string]Task `json:"tasks"`
	SourceHash      string          `json:"source_hash"`
	ManifestVersion int             `json:"manifest_version"`
}

// Repository is implemented by the host-state adapter. Commit must apply the
// settings/task diff atomically with expectedRevision CAS and return the exact
// committed revision. It must not copy media files or silently merge conflicts.
type Repository interface {
	Load(context.Context) (RepositorySnapshot, error)
	Initialize(context.Context, LegacyImport) (RepositorySnapshot, error)
	Commit(context.Context, int64, RepositorySnapshot) (RepositorySnapshot, error)
}

var ErrRepositoryConflict = errors.New("device_repository_conflict")

func ValidateRepositorySnapshot(snapshot RepositorySnapshot) error {
	if snapshot.Tasks == nil || len(snapshot.Tasks) > 200 || snapshot.Settings.Validate() != nil {
		return errors.New("device_repository_invalid")
	}
	requestHashes := make(map[string]string, len(snapshot.Tasks))
	for id, task := range snapshot.Tasks {
		if id != task.ID || !identity.ValidLegacyDeviceID(id) || task.RequestHash == "" {
			return errors.New("device_migration_required")
		}
		if other, exists := requestHashes[task.RequestHash]; exists && other != id {
			return errors.New("device_migration_required")
		}
		requestHashes[task.RequestHash] = id
	}
	return nil
}
