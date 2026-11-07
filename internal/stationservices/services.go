// Package stationservices describes the host capabilities available to a
// trusted application adapter. It does not expose stores, paths, or secrets.
package stationservices

import (
	"context"
	"io"

	"wechat-bot/internal/companion"
)

type Owner struct {
	FishID string
	ID     string
}

type OwnerReader func() (Owner, error)

type Model struct {
	// Persona returns the configured persona only when the host model is ready.
	Persona func() (string, error)
	Client  companion.LanguageModel
}

type Artifacts interface {
	HomeRevision() (uint64, error)
	ReadArtifact(id string) (Artifact, error)
}

// Artifact identifies the immutable bytes. OpenVerified checks the digest and
// calls the app's stillAllowed fence throughout the check. The caller owns the
// returned reader and must check its grant before submitting a delivery.
type Artifact struct {
	ID, Name, SHA256             string
	Bytes                        int64
	InputArtifactID, InputSHA256 string
	OpenVerified                 func(context.Context, func() bool) (io.ReadCloser, error)
}

func (a Artifact) View() map[string]any {
	view := map[string]any{"artifact_id": a.ID, "filename": a.Name, "sha256": a.SHA256, "bytes": a.Bytes, "status": "registered"}
	if a.InputArtifactID != "" {
		view["input_artifact_id"], view["input_sha256"] = a.InputArtifactID, a.InputSHA256
	}
	return view
}
