package station

// This file is the ordinary-web publication boundary.  It owns the login
// profile and all staging paths; the application adapter sees only a verified
// artifact reader from apphost and never a user-controlled file name/path.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/stationbrowser"
)

const localDouyinPublicationCheckpointVersion = 1

type localDouyinPublicationCheckpoint struct {
	Version        int    `json:"version"`
	TaskID         string `json:"task_id"`
	ManifestSHA256 string `json:"manifest_sha256"`
	MediaSHA256    string `json:"media_sha256"`
	Bytes          int64  `json:"bytes"`
	Submitted      bool   `json:"submitted"`
	RemoteID       string `json:"remote_id,omitempty"`
	Rejected       bool   `json:"rejected,omitempty"`
	RejectionCode  int    `json:"rejection_code,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	BrowserStage   string `json:"browser_stage,omitempty"`
}

// localDouyinWebPublication deliberately reads the same saved profile record
// as observation. It does not require read/playback permission: publishing is
// a distinct opt-in grant on the same owner-confirmed browser profile.
type localDouyinWebPublication struct {
	home         *localHomeService
	installation string
}

func (p *localDouyinWebPublication) access(ctx context.Context) (localDouyinWebAccess, error) {
	if p == nil || p.home == nil {
		return localDouyinWebAccess{}, apphost.ErrPublicationUnavailable
	}
	identity, err := p.home.identity.Read()
	if err != nil || identity.LocalOwner == nil || !fishidentity.ValidFishID(identity.FishID) {
		return localDouyinWebAccess{}, apphost.ErrPublicationDenied
	}
	// observation.state is intentionally also the one place that derives the
	// host-owned connection/account reference from current login metadata.
	return (&localDouyinWebObservation{home: p.home, installation: p.installation}).state(ctx)
}

func (p *localDouyinWebPublication) PublicationGrant(ctx context.Context, target apphost.ConnectionRef) (apphost.PublicationGrant, error) {
	a, err := p.access(ctx)
	if err != nil {
		return apphost.PublicationGrant{Allowed: false}, err
	}
	ref := apphost.ConnectionRef{AppID: douyinapp.ID, InstallationID: p.installation, ConnectionID: a.connection.ID, AccountID: a.connection.AccountID}
	if target != ref || !a.record.PublishEnabled || a.record.Profile == "" || a.record.ConfirmedAt == "" || !p.profileAvailable(a.record) {
		return apphost.PublicationGrant{Allowed: false}, nil
	}
	// a.version hashes the durable login/grant record. Toggling publish or
	// re-login changes it, so old drafts cannot become new external effects.
	return apphost.PublicationGrant{Allowed: true, Version: a.version}, nil
}

func (p *localDouyinWebPublication) ContinuePublication(ctx context.Context, req apphost.PublicationRequest) (apphost.PublicationReceipt, error) {
	if req.CheckActive == nil || req.SaveCheckpoint == nil || req.OpenArtifact == nil || len(req.Draft.Artifacts) != 1 {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationInvalidDraft
	}
	if err := req.CheckActive(ctx); err != nil {
		return apphost.PublicationReceipt{}, err
	}
	a, err := p.access(ctx)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	ref := apphost.ConnectionRef{AppID: douyinapp.ID, InstallationID: p.installation, ConnectionID: a.connection.ID, AccountID: a.connection.AccountID}
	if req.Reference.Target != ref {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationDenied
	}
	grant, err := p.PublicationGrant(ctx, ref)
	if err != nil || !grant.Allowed || grant.Version != req.GrantVersion {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationDenied
	}
	checkpoint, err := localDouyinPublicationCheckpointFor(req)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	// Submitted is an irreversible boundary. Never stage/reupload/click again;
	// only ask the creator page for the original task's result.
	if checkpoint.Submitted {
		return p.reconcile(ctx, a, req.Reference, checkpoint)
	}
	// A durable pre-submit checkpoint exposes safe resume even if materializing
	// the input or opening the creator page fails before any publish attempt.
	checkpoint.MediaSHA256, checkpoint.Bytes = req.Draft.Artifacts[0].SHA256, req.Draft.Artifacts[0].Bytes
	checkpoint.ErrorCode = ""
	if err := req.SaveCheckpoint(ctx, mustLocalDouyinJSON(checkpoint)); err != nil {
		return apphost.PublicationReceipt{}, err
	}
	media, err := p.stage(ctx, a, req, checkpoint)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	visibility, err := localDouyinPublicationVisibility(req.Draft.Options)
	if err != nil {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationInvalidDraft
	}
	result, err := stationbrowser.ContinueDouyinPublication(ctx, a.profile, stationbrowser.CreatorPublicationRequest{
		TaskID: req.Reference.TaskID, ManifestSHA256: req.Reference.ManifestSHA256,
		Title: req.Draft.Title, Body: req.Draft.Body, Visibility: visibility, MediaPath: media,
		Progress: func(progressCtx context.Context, stage string) error {
			checkpoint.BrowserStage = stage
			return req.SaveCheckpoint(progressCtx, mustLocalDouyinJSON(checkpoint))
		},
		BeforeSubmit: func(checkpointCtx context.Context) error {
			if err := req.CheckActive(checkpointCtx); err != nil {
				return err
			}
			checkpoint.Submitted = true
			return req.SaveCheckpoint(checkpointCtx, mustLocalDouyinJSON(checkpoint))
		},
	}, func() error { return req.CheckActive(ctx) })
	if result.RemoteID != "" {
		checkpoint.RemoteID = result.RemoteID
		if err := req.SaveCheckpoint(ctx, mustLocalDouyinJSON(checkpoint)); err != nil {
			return apphost.PublicationReceipt{}, err
		}
	}
	// A creator-center rejection is a verified terminal outcome, even though
	// the browser transport reports it as an error. Preserve the submitted
	// checkpoint so neither a restart nor a retry can upload it again.
	if result.Stage == "rejected" {
		checkpoint.Rejected, checkpoint.RejectionCode = true, result.RejectionCode
		checkpoint.ErrorCode = "creator_rejected"
		if err := req.SaveCheckpoint(ctx, mustLocalDouyinJSON(checkpoint)); err != nil {
			return apphost.PublicationReceipt{}, err
		}
		return localDouyinPublicationReceipt(req.Reference, result), nil
	}
	if err != nil {
		checkpoint.ErrorCode = localDouyinCreatorErrorCode(err)
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if saveErr := req.SaveCheckpoint(saveCtx, mustLocalDouyinJSON(checkpoint)); saveErr != nil {
			return apphost.PublicationReceipt{}, saveErr
		}
		progress, _ := p.PublicationCheckpointProgress(mustLocalDouyinJSON(checkpoint))
		return apphost.PublicationReceipt{Reference: req.Reference, State: "unknown", Progress: progress}, nil
	}
	return localDouyinPublicationReceipt(req.Reference, result), nil
}

func (p *localDouyinWebPublication) ReconcilePublication(ctx context.Context, reference apphost.PublicationReference, raw json.RawMessage, cancel bool) (apphost.PublicationReceipt, error) {
	checkpoint, err := localDouyinPublicationCheckpointFrom(raw, reference)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	if !checkpoint.Submitted {
		state := "unknown"
		if cancel {
			state = "cancelled"
		}
		return apphost.PublicationReceipt{Reference: reference, State: state, Progress: &apphost.PublicationProgress{Stage: "uploading", ErrorCode: checkpoint.ErrorCode}}, nil
	}
	if checkpoint.Rejected {
		return apphost.PublicationReceipt{Reference: reference, State: "failed", Progress: &apphost.PublicationProgress{Stage: "rejected"}}, nil
	}
	a, err := p.access(ctx)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	ref := apphost.ConnectionRef{AppID: douyinapp.ID, InstallationID: p.installation, ConnectionID: a.connection.ID, AccountID: a.connection.AccountID}
	if reference.Target != ref {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationDenied
	}
	return p.reconcile(ctx, a, reference, checkpoint)
}

func (p *localDouyinWebPublication) reconcile(ctx context.Context, access localDouyinWebAccess, reference apphost.PublicationReference, checkpoint localDouyinPublicationCheckpoint) (apphost.PublicationReceipt, error) {
	if checkpoint.Rejected {
		return apphost.PublicationReceipt{Reference: reference, State: "failed", Progress: &apphost.PublicationProgress{Stage: "rejected"}}, nil
	}
	// A recovered ID is durably acknowledged by apphost. It need not rewrite
	// the adapter's older checkpoint, but conflicting identities are invalid.
	if reference.RemoteID != "" {
		if checkpoint.RemoteID != "" && checkpoint.RemoteID != reference.RemoteID {
			return apphost.PublicationReceipt{}, apphost.ErrPublicationConflict
		}
		checkpoint.RemoteID = reference.RemoteID
	}
	if checkpoint.RemoteID == "" {
		return apphost.PublicationReceipt{Reference: reference, State: "unknown", Progress: &apphost.PublicationProgress{Stage: "posting", ErrorCode: checkpoint.ErrorCode}}, nil
	}
	// Reconciliation intentionally does not require the current publish grant:
	// an owner who revokes it must still be able to account for an already
	// submitted work. It does, however, re-check the exact account/profile so a
	// relogin cannot send an old receipt lookup through a different account.
	result, err := stationbrowser.QueryDouyinPublication(ctx, access.profile, checkpoint.RemoteID, p.queryAuthorization(ctx, access, reference))
	if err != nil {
		return apphost.PublicationReceipt{Reference: reference, State: "unknown", Progress: &apphost.PublicationProgress{Stage: "posting", ErrorCode: localDouyinCreatorErrorCode(err)}}, nil
	}
	result.RemoteID = checkpoint.RemoteID
	return localDouyinPublicationReceipt(reference, result), nil
}

func (p *localDouyinWebPublication) queryAuthorization(ctx context.Context, access localDouyinWebAccess, reference apphost.PublicationReference) func() error {
	return func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := p.access(ctx)
		if err != nil {
			return apphost.ErrPublicationDenied
		}
		currentRef := apphost.ConnectionRef{AppID: douyinapp.ID, InstallationID: p.installation, ConnectionID: current.connection.ID, AccountID: current.connection.AccountID}
		if currentRef != reference.Target || current.profile != access.profile || !p.profileAvailable(current.record) {
			return apphost.ErrPublicationDenied
		}
		return nil
	}
}

// RecoverPublication has no artifact opener, checkpoint writer or submit
// callback. It can only identify an already submitted work in the same saved
// account, using the host's immutable draft rather than new model parameters.
func (p *localDouyinWebPublication) RecoverPublication(ctx context.Context, req apphost.PublicationRecoveryRequest) (apphost.PublicationReceipt, error) {
	checkpoint, err := localDouyinPublicationCheckpointFrom(req.Checkpoint, req.Reference)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	if !checkpoint.Submitted || checkpoint.Rejected || checkpoint.RemoteID != "" || req.Reference.RemoteID != "" {
		return p.ReconcilePublication(ctx, req.Reference, req.Checkpoint, false)
	}
	if req.Draft.Target != req.Reference.Target || len(req.Draft.Artifacts) != 1 ||
		(checkpoint.MediaSHA256 != "" && (checkpoint.MediaSHA256 != req.Draft.Artifacts[0].SHA256 || checkpoint.Bytes != req.Draft.Artifacts[0].Bytes)) {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationConflict
	}
	visibility, err := localDouyinPublicationVisibility(req.Draft.Options)
	if err != nil {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationInvalidDraft
	}
	if _, err = time.Parse(time.RFC3339Nano, req.CreatedAt); err != nil {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationConflict
	}
	a, err := p.access(ctx)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	authorize := p.queryAuthorization(ctx, a, req.Reference)
	if err := authorize(); err != nil {
		return apphost.PublicationReceipt{}, err
	}
	result, err := stationbrowser.FindDouyinPublication(ctx, a.profile, stationbrowser.CreatorPublicationQuery{
		Title: req.Draft.Title, Body: req.Draft.Body, Visibility: visibility, CreatedAt: req.CreatedAt,
	}, authorize)
	if err != nil {
		return apphost.PublicationReceipt{Reference: req.Reference, State: "unknown", Progress: &apphost.PublicationProgress{Stage: "posting", ErrorCode: localDouyinCreatorErrorCode(err)}}, nil
	}
	return localDouyinPublicationReceipt(req.Reference, result), nil
}

func (p *localDouyinWebPublication) stage(ctx context.Context, access localDouyinWebAccess, req apphost.PublicationRequest, checkpoint localDouyinPublicationCheckpoint) (string, error) {
	if err := req.CheckActive(ctx); err != nil {
		return "", err
	}
	artifact := req.Draft.Artifacts[0]
	if checkpoint.MediaSHA256 != "" && (checkpoint.MediaSHA256 != artifact.SHA256 || checkpoint.Bytes != artifact.Bytes) {
		return "", apphost.ErrPublicationConflict
	}
	name := filepath.Base(artifact.Name)
	if name != artifact.Name || name == "." || name == "" || !localDouyinPublicationHex(req.Reference.TaskID) || !localDouyinPublicationHex(req.Reference.ManifestSHA256) {
		return "", apphost.ErrPublicationInvalidDraft
	}
	// Chromium's native file picker cannot reliably open Windows paths beyond
	// MAX_PATH, even though Go and FFmpeg can. Bind the same identity in a hash
	// instead of nesting three 64-character IDs into the upload path.
	key := sha256.Sum256([]byte(access.record.FishID + "\x00" + req.Reference.TaskID + "\x00" + req.Reference.ManifestSHA256))
	dir := filepath.Join(p.home.dataDir, "apps", "douyin", "publication-staging")
	path := filepath.Join(dir, hex.EncodeToString(key[:])+strings.ToLower(filepath.Ext(name)))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", apphost.ErrPublicationUnavailable
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() == artifact.Bytes {
		file, openErr := os.Open(path)
		if openErr == nil {
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr == nil && closeErr == nil && hex.EncodeToString(hash.Sum(nil)) == artifact.SHA256 {
				return path, nil
			}
		}
	}
	if err := req.CheckActive(ctx); err != nil {
		return "", err
	}
	in, err := req.OpenArtifact(ctx, artifact)
	if err != nil {
		return "", apphost.ErrPublicationInvalidDraft
	}
	defer in.Close()
	temporary, err := os.CreateTemp(dir, ".stage-*")
	if err != nil {
		return "", apphost.ErrPublicationUnavailable
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return "", apphost.ErrPublicationUnavailable
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(in, artifact.Bytes+1))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil || written != artifact.Bytes || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 || req.CheckActive(ctx) != nil {
		return "", apphost.ErrPublicationInvalidDraft
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return "", apphost.ErrPublicationUnavailable
	}
	return path, nil
}

func localDouyinPublicationCheckpointFor(req apphost.PublicationRequest) (localDouyinPublicationCheckpoint, error) {
	if len(req.Checkpoint) == 0 {
		return localDouyinPublicationCheckpoint{Version: localDouyinPublicationCheckpointVersion, TaskID: req.Reference.TaskID, ManifestSHA256: req.Reference.ManifestSHA256}, nil
	}
	return localDouyinPublicationCheckpointFrom(req.Checkpoint, req.Reference)
}

func localDouyinPublicationCheckpointFrom(raw json.RawMessage, reference apphost.PublicationReference) (localDouyinPublicationCheckpoint, error) {
	var checkpoint localDouyinPublicationCheckpoint
	if len(raw) == 0 || json.Unmarshal(raw, &checkpoint) != nil || checkpoint.Version != localDouyinPublicationCheckpointVersion ||
		checkpoint.TaskID != reference.TaskID || checkpoint.ManifestSHA256 != reference.ManifestSHA256 ||
		(checkpoint.MediaSHA256 != "" && !localDouyinPublicationHex(checkpoint.MediaSHA256)) || checkpoint.Bytes < 0 || strings.ContainsAny(checkpoint.RemoteID, "\x00\r\n") ||
		(checkpoint.Rejected && (!checkpoint.Submitted || checkpoint.RejectionCode == 0)) {
		return localDouyinPublicationCheckpoint{}, apphost.ErrPublicationConflict
	}
	return checkpoint, nil
}

func localDouyinPublicationVisibility(raw json.RawMessage) (string, error) {
	var value struct {
		Visibility string `json:"visibility"`
	}
	if json.Unmarshal(raw, &value) != nil || (value.Visibility != "public" && value.Visibility != "private" && value.Visibility != "friends") {
		return "", errors.New("visibility invalid")
	}
	return value.Visibility, nil
}

func localDouyinPublicationReceipt(reference apphost.PublicationReference, result stationbrowser.CreatorPublicationResult) apphost.PublicationReceipt {
	if result.RemoteID != "" {
		reference.RemoteID = result.RemoteID
	}
	if result.Stage == "rejected" {
		// A verified platform rejection is terminal even when it does not expose
		// a work ID. The durable submitted checkpoint still prevents re-upload.
		return apphost.PublicationReceipt{Reference: reference, State: "failed", Progress: &apphost.PublicationProgress{Stage: "rejected"}}
	}
	if result.Published && reference.RemoteID != "" {
		return apphost.PublicationReceipt{Reference: reference, State: "published"}
	}
	if result.Submitted && reference.RemoteID != "" {
		return apphost.PublicationReceipt{Reference: reference, State: "accepted"}
	}
	return apphost.PublicationReceipt{Reference: reference, State: "unknown"}
}

func mustLocalDouyinJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

func (p *localDouyinWebPublication) PublicationCheckpointProgress(raw json.RawMessage) (*apphost.PublicationProgress, error) {
	var checkpoint localDouyinPublicationCheckpoint
	if json.Unmarshal(raw, &checkpoint) != nil || checkpoint.Version != localDouyinPublicationCheckpointVersion {
		return nil, apphost.ErrPublicationConflict
	}
	stage := "uploading"
	if checkpoint.BrowserStage != "" {
		stage = checkpoint.BrowserStage
	}
	if checkpoint.Submitted {
		stage = "posting"
	}
	if checkpoint.RemoteID != "" {
		stage = "accepted"
	}
	if checkpoint.Rejected {
		stage = "rejected"
	}
	return &apphost.PublicationProgress{Stage: stage, ErrorCode: checkpoint.ErrorCode}, nil
}

// Only host-defined codes reach the model/UI. Raw browser failures, response
// bodies and private profile paths are never copied into task diagnostics.
func localDouyinCreatorErrorCode(err error) string {
	switch {
	case errors.Is(err, stationbrowser.ErrDouyinWebLoginRequired):
		return "creator_login_required"
	case errors.Is(err, stationbrowser.ErrDouyinLoginActive):
		return "creator_profile_busy"
	case errors.Is(err, stationbrowser.ErrCreatorPublicationControls):
		return "creator_controls_unavailable"
	case errors.Is(err, stationbrowser.ErrCreatorPublicationUpload):
		return "creator_upload_incomplete"
	case errors.Is(err, stationbrowser.ErrCreatorPublicationVisibility):
		return "creator_visibility_unconfirmed"
	case errors.Is(err, stationbrowser.ErrCreatorPublicationCaption):
		return "creator_caption_unconfirmed"
	case errors.Is(err, stationbrowser.ErrCreatorPublicationUnknown):
		return "creator_result_unknown"
	case errors.Is(err, stationbrowser.ErrBrowserUnavailable):
		return "creator_browser_unavailable"
	case errors.Is(err, context.Canceled):
		return "creator_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "creator_timeout"
	default:
		return "creator_failed"
	}
}

func localDouyinPublicationHex(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (p *localDouyinWebPublication) profileAvailable(record localDouyinWebRecord) bool {
	if p == nil || p.home == nil || p.home.douyinWeb == nil {
		return false
	}
	m := p.home.douyinWeb
	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.view(record)
	return view.State == "saved" && !view.Active && view.AttemptID == "" && view.Available
}
