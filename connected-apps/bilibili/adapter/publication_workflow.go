package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/strictjson"
)

type publicationTransport interface {
	Initialize(context.Context, string) (string, error)
	UploadPart(context.Context, string, int64, []byte) error
	Complete(context.Context, string) error
	Post(context.Context, string, ArchiveMetadata) (string, error)
	View(context.Context, string) (ArchiveStatus, error)
}

// The host persists one record per task in its private ledger. Neither the
// token nor the credential fingerprint appears in an application receipt.
type uploadCheckpoint struct {
	Stage        string `json:"stage"`
	PartBytes    int    `json:"part_bytes"`
	Total        int64  `json:"total"`
	Confirmed    int64  `json:"confirmed"`
	Token        string `json:"token,omitempty"`
	RemoteID     string `json:"remote_id,omitempty"`
	Credentials  string `json:"credentials"`
	ReadIdentity string `json:"read_identity,omitempty"`
}

var _ apphost.CheckpointedPublicationProvider = (*PublicApp)(nil)
var _ apphost.PublicationCheckpointProgressProvider = (*PublicApp)(nil)

// Concurrent calls in this station process must not send the same part or
// overtake the merge. The persisted CAS guards stale snapshots as well.
var publicationInFlight sync.Map

func (p *PublicApp) ValidatePublicationDraft(draft apphost.PublicationDraft) error {
	_, err := publicationMetadata(draft)
	return err
}

func publicationMetadata(draft apphost.PublicationDraft) (ArchiveMetadata, error) {
	if draft.Target.AppID != ID || draft.Target.ConnectionID != publicationConnectionID ||
		len(draft.Artifacts) != 1 || !safeUploadName(draft.Artifacts[0].Name) ||
		!strings.EqualFold(filepath.Ext(draft.Artifacts[0].Name), ".mp4") || draft.Artifacts[0].Bytes < 12 {
		return ArchiveMetadata{}, apphost.ErrPublicationInvalidDraft
	}
	var options struct {
		CategoryID int      `json:"category_id"`
		Tags       []string `json:"tags"`
		Copyright  int      `json:"copyright"`
		NoReprint  bool     `json:"no_reprint"`
		Source     string   `json:"source"`
	}
	if err := decodePublicationJSON(draft.Options, &options); err != nil {
		return ArchiveMetadata{}, apphost.ErrPublicationInvalidDraft
	}
	metadata := ArchiveMetadata{
		Title: draft.Title, Description: draft.Body, CategoryID: options.CategoryID,
		Tags: options.Tags, Copyright: options.Copyright, NoReprint: options.NoReprint, Source: options.Source,
	}
	if metadata.Validate() != nil {
		return ArchiveMetadata{}, apphost.ErrPublicationInvalidDraft
	}
	return metadata, nil
}

func decodePublicationJSON(raw json.RawMessage, into any) error {
	if _, err := strictjson.Decode(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

func loadUploadCheckpoint(raw json.RawMessage) (uploadCheckpoint, error) {
	var state uploadCheckpoint
	if len(raw) == 0 {
		return state, nil
	}
	if decodePublicationJSON(raw, &state) != nil || state.PartBytes < 1 ||
		state.PartBytes > maxPublicationPartBytes || state.Total < 1 || state.Confirmed < 0 ||
		state.Confirmed > state.Total || !isPublicationDigest(state.Credentials, 32) ||
		(state.ReadIdentity != "" && !isPublicationDigest(state.ReadIdentity, 32)) {
		return uploadCheckpoint{}, apphost.ErrPublicationConflict
	}
	valid := false
	switch state.Stage {
	case "initializing":
		valid = state.Confirmed == 0 && state.Token == "" && state.RemoteID == ""
	case "uploading":
		valid = safeUploadToken(state.Token) && state.RemoteID == ""
	case "merging", "merged", "posting":
		valid = state.Confirmed == state.Total && safeUploadToken(state.Token) && state.RemoteID == ""
	case "accepted":
		valid = state.Confirmed == state.Total && state.Token == "" && safeArchiveResourceID(state.RemoteID)
	}
	if !valid {
		return uploadCheckpoint{}, apphost.ErrPublicationConflict
	}
	return state, nil
}

// PublicationCheckpointProgress reduces Bilibili's private upload checkpoint
// to the sole safe fields that the local publication list may expose.
func (p *PublicApp) PublicationCheckpointProgress(raw json.RawMessage) (*apphost.PublicationProgress, error) {
	state, err := loadUploadCheckpoint(raw)
	if err != nil {
		return nil, err
	}
	if state.Stage == "" {
		return nil, nil
	}
	return &apphost.PublicationProgress{Stage: state.Stage, ConfirmedParts: state.Confirmed, TotalParts: state.Total}, nil
}

func (state uploadCheckpoint) receipt(ref apphost.PublicationReference) apphost.PublicationReceipt {
	receipt := apphost.PublicationReceipt{Reference: ref, State: "unknown"}
	if state.Stage != "" {
		receipt.Progress = &apphost.PublicationProgress{Stage: state.Stage, ConfirmedParts: state.Confirmed, TotalParts: state.Total}
	}
	if state.Stage == "accepted" {
		receipt.Reference.RemoteID = state.RemoteID
		receipt.State = "accepted"
	}
	return receipt
}

func publicationCredentialFingerprint(settings publicationSettings) string {
	data, _ := json.Marshal([]string{settings.AccountID, settings.ClientID, settings.AppSecret, settings.AccessToken})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (p *PublicApp) newPublicationTransport(settings publicationSettings) (publicationTransport, error) {
	credentials := OpenCredentials{ClientID: settings.ClientID, AppSecret: settings.AppSecret, AccessToken: settings.AccessToken}
	if p.publicationClient != nil {
		return p.publicationClient(credentials)
	}
	return NewOpenUploadClient(credentials)
}

func (p *PublicApp) ContinuePublication(ctx context.Context, req apphost.PublicationRequest) (apphost.PublicationReceipt, error) {
	state, err := loadUploadCheckpoint(req.Checkpoint)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	ref := req.Reference
	if req.CheckActive == nil || req.SaveCheckpoint == nil || req.OpenArtifact == nil || ref.Target != req.Draft.Target {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationInvalidDraft
	}
	if state.Stage == "initializing" || state.Stage == "merging" || state.Stage == "posting" || state.Stage == "accepted" {
		return p.ReconcilePublication(ctx, ref, req.Checkpoint, false)
	}
	key := filepath.Join(p.privateDirectory, ref.TaskID)
	if _, loaded := publicationInFlight.LoadOrStore(key, struct{}{}); loaded {
		return state.receipt(ref), nil
	}
	defer publicationInFlight.Delete(key)
	settings, grant, err := p.publicationAccess(ref.Target)
	if err != nil || !grant.Allowed || grant.Version != req.GrantVersion {
		return state.receipt(ref), nil
	}
	metadata, err := publicationMetadata(req.Draft)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	artifact := req.Draft.Artifacts[0]
	partBytes := int64(settings.PartBytes)
	if partBytes < 1 || partBytes > maxPublicationPartBytes {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationConflict
	}
	total := 1 + (artifact.Bytes-1)/partBytes
	credentials := publicationCredentialFingerprint(settings)
	if state.Stage != "" && (state.Credentials != credentials || state.PartBytes != settings.PartBytes || state.Total != total) {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationConflict
	}
	client, err := p.newPublicationTransport(settings)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	return runPublicationUpload(ctx, client, req, metadata, artifact, state, uploadCheckpoint{
		Stage: "initializing", PartBytes: settings.PartBytes, Total: total, Credentials: credentials,
		ReadIdentity: publicationReadIdentity(settings),
	})
}

func runPublicationUpload(ctx context.Context, client publicationTransport, req apphost.PublicationRequest, metadata ArchiveMetadata, artifact apphost.PublicationArtifact, state, initial uploadCheckpoint) (apphost.PublicationReceipt, error) {
	ref := req.Reference
	save := func(next uploadCheckpoint, afterResponse bool) error {
		checkpointCtx := ctx
		if afterResponse {
			// An observed response must survive a client timeout or a concurrent
			// cancel. This context grants no further network activity.
			var cancel context.CancelFunc
			checkpointCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
		}
		raw, err := json.Marshal(next)
		if err == nil {
			err = req.SaveCheckpoint(checkpointCtx, raw)
		}
		if err == nil {
			state = next
		}
		return err
	}
	claim := func(next uploadCheckpoint) error {
		if err := req.CheckActive(ctx); err != nil {
			return err
		}
		if err := save(next, false); err != nil {
			return err
		}
		return req.CheckActive(ctx)
	}

	if state.Stage != "merged" {
		file, err := req.OpenArtifact(ctx, artifact)
		if err != nil {
			return state.receipt(ref), nil
		}
		defer file.Close()
		// Inspect the initial MP4 box before any bytes leave the host.
		var head [12]byte
		if _, err := io.ReadFull(file, head[:]); err != nil || string(head[4:8]) != "ftyp" {
			return state.receipt(ref), nil
		}
		reader := io.MultiReader(bytes.NewReader(head[:]), file)
		if state.Stage == "" {
			if err := claim(initial); err != nil {
				return state.receipt(ref), nil
			}
			token, err := client.Initialize(ctx, artifact.Name)
			if err != nil || !safeUploadToken(token) {
				return state.receipt(ref), nil
			}
			next := state
			next.Stage, next.Token = "uploading", token
			if err := save(next, true); err != nil {
				return state.receipt(ref), nil
			}
		}
		if state.Confirmed > 0 && state.Confirmed < state.Total {
			if _, err := io.CopyN(io.Discard, reader, state.Confirmed*int64(state.PartBytes)); err != nil {
				return state.receipt(ref), nil
			}
		}
		buffer := make([]byte, min(int64(state.PartBytes), artifact.Bytes))
		for state.Confirmed < state.Total {
			remaining := artifact.Bytes - state.Confirmed*int64(state.PartBytes)
			part := buffer[:min(int64(len(buffer)), remaining)]
			if _, err := io.ReadFull(reader, part); err != nil || claim(state) != nil {
				return state.receipt(ref), nil
			}
			if err := client.UploadPart(ctx, state.Token, state.Confirmed+1, part); err != nil {
				return state.receipt(ref), nil
			}
			next := state
			next.Confirmed++
			if err := save(next, true); err != nil {
				return state.receipt(ref), nil
			}
		}
		next := state
		next.Stage = "merging"
		if err := claim(next); err != nil {
			return state.receipt(ref), nil
		}
		if err := client.Complete(ctx, state.Token); err != nil {
			return state.receipt(ref), nil
		}
		next.Stage = "merged"
		if err := save(next, true); err != nil {
			return state.receipt(ref), nil
		}
	}

	next := state
	next.Stage = "posting"
	if err := claim(next); err != nil {
		return state.receipt(ref), nil
	}
	remoteID, err := client.Post(ctx, state.Token, metadata)
	if err != nil || !safeArchiveResourceID(remoteID) {
		return state.receipt(ref), nil
	}
	next.Stage, next.RemoteID, next.Token = "accepted", remoteID, ""
	if err := save(next, true); err != nil {
		// If we observed a valid receipt, the host can still acknowledge it.
		// The durable posting claim prevents a second submit even if this fails.
		return next.receipt(ref), nil
	}
	return state.receipt(ref), nil
}

func (p *PublicApp) ReconcilePublication(ctx context.Context, ref apphost.PublicationReference, checkpoint json.RawMessage, cancel bool) (apphost.PublicationReceipt, error) {
	state, err := loadUploadCheckpoint(checkpoint)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	receipt := state.receipt(ref)
	if ref.RemoteID != "" {
		if !safeArchiveResourceID(ref.RemoteID) || (state.RemoteID != "" && state.RemoteID != ref.RemoteID) {
			return apphost.PublicationReceipt{}, apphost.ErrPublicationReceipt
		}
		receipt.State = "accepted"
	}
	if receipt.Reference.RemoteID == "" {
		// A staged upload is not a publication. Cancellation blocks all further
		// uploads even when one part is in flight. An uncertain post can only
		// remain unknown until the original remote resource can be verified.
		if cancel && state.Stage != "posting" {
			receipt.State = "cancelled"
		}
		return receipt, nil
	}
	settings, _, err := p.publicationAccess(ref.Target)
	if err != nil || !publicationRefMatches(ref.Target, settings) || !state.permitsRead(settings) {
		return receipt, nil
	}
	expiry, err := time.Parse(time.RFC3339, settings.ExpiresAt)
	if err != nil || !time.Now().Before(expiry) {
		return receipt, nil
	}
	client, err := p.newPublicationTransport(settings)
	if err != nil {
		return receipt, nil
	}
	status, err := client.View(ctx, receipt.Reference.RemoteID)
	if err == nil && status.Published {
		receipt.State = "published"
	}
	return receipt, nil
}

func (state uploadCheckpoint) permitsRead(settings publicationSettings) bool {
	if state.Credentials == publicationCredentialFingerprint(settings) {
		return true
	}
	if settings.SchemaVersion != 2 {
		return false
	}
	return (state.ReadIdentity != "" && state.ReadIdentity == publicationReadIdentity(settings)) ||
		(settings.LegacyCredentials != "" && state.Credentials == settings.LegacyCredentials)
}
