package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/strictjson"
)

const (
	douyinSingleUploadBytes = 50_000_000
	douyinMaxVideoBytes     = 4_000_000_000
	douyinTargetPartBytes   = 20_000_000
)

var (
	_                         apphost.CheckpointedPublicationProvider       = (*SearchApp)(nil)
	_                         apphost.PublicationCheckpointProgressProvider = (*SearchApp)(nil)
	douyinPublicationInFlight sync.Map
)

type douyinPublicationDraft struct {
	text       string
	visibility int
}

func (p *SearchApp) ValidatePublicationDraft(draft apphost.PublicationDraft) error {
	_, err := douyinDraft(draft)
	return err
}

func douyinDraft(draft apphost.PublicationDraft) (douyinPublicationDraft, error) {
	invalid := apphost.ErrPublicationInvalidDraft
	if draft.Target.AppID != ID || draft.Target.InstallationID != installationID || draft.Target.ConnectionID != accountConnectionID || len(draft.Artifacts) != 1 {
		return douyinPublicationDraft{}, invalid
	}
	artifact := draft.Artifacts[0]
	extension := strings.ToLower(filepath.Ext(artifact.Name))
	if !safePublicationFileName(artifact.Name) || artifact.Bytes < 12 || artifact.Bytes > douyinMaxVideoBytes || (extension != ".mp4" && extension != ".webm") {
		return douyinPublicationDraft{}, invalid
	}
	var options struct {
		Visibility string `json:"visibility"`
	}
	if decodeDouyinPublication(draft.Options, &options) != nil {
		return douyinPublicationDraft{}, invalid
	}
	var visibility int
	switch options.Visibility {
	case "public":
		visibility = 0
	case "private":
		visibility = 1
	case "friends":
		visibility = 2
	default:
		return douyinPublicationDraft{}, invalid
	}
	text := strings.TrimSpace(draft.Title)
	if body := strings.TrimSpace(draft.Body); body != "" {
		if text != "" {
			text += "\n"
		}
		text += body
	}
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 1000 || strings.ContainsRune(text, 0) {
		return douyinPublicationDraft{}, invalid
	}
	return douyinPublicationDraft{text: text, visibility: visibility}, nil
}

func decodeDouyinPublication(raw json.RawMessage, target any) error {
	if _, err := strictjson.Decode(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func (p *SearchApp) PublicationGrant(ctx context.Context, ref apphost.ConnectionRef) (apphost.PublicationGrant, error) {
	if err := ctx.Err(); err != nil {
		return apphost.PublicationGrant{}, err
	}
	account, err := p.publicationAccount(ref)
	if err != nil {
		return apphost.PublicationGrant{}, err
	}
	return apphost.PublicationGrant{Allowed: true, Version: account.grantRevision()}, nil
}

func (p *SearchApp) publicationAccount(ref apphost.ConnectionRef) (accountAuthorization, error) {
	account, err := p.currentAccount()
	if err != nil || !account.PublishVideo || account.AccessToken == "" || !p.nowUTC().Before(account.ExpiresAt) ||
		ref.AppID != ID || ref.InstallationID != installationID || ref.ConnectionID != accountConnectionID || ref.AccountID != accountWorksAccountID(account) {
		return accountAuthorization{}, apphost.ErrPublicationDenied
	}
	return account, nil
}

// A checkpoint contains remote upload or creation identifiers, never a login token.
type douyinPublicationCheckpoint struct {
	Stage         string `json:"stage"`
	PartBytes     int64  `json:"part_bytes"`
	Total         int64  `json:"total"`
	Confirmed     int64  `json:"confirmed"`
	Visibility    int    `json:"visibility"`
	UploadID      string `json:"upload_id,omitempty"`
	UploadVideoID string `json:"upload_video_id,omitempty"`
	VideoID       string `json:"video_id,omitempty"`
	ItemID        string `json:"item_id,omitempty"`
}

func loadDouyinPublication(raw json.RawMessage) (douyinPublicationCheckpoint, error) {
	var state douyinPublicationCheckpoint
	if len(raw) == 0 {
		return state, nil
	}
	if decodeDouyinPublication(raw, &state) != nil || state.PartBytes < 12 || state.PartBytes > douyinSingleUploadBytes ||
		state.Total < 1 || state.Total > 1+douyinMaxVideoBytes/douyinTargetPartBytes || state.Confirmed < 0 || state.Confirmed > state.Total ||
		state.Visibility < 0 || state.Visibility > 2 {
		return douyinPublicationCheckpoint{}, apphost.ErrPublicationConflict
	}
	upload := validDouyinCheckpointID(state.UploadID)
	video := validDouyinCheckpointID(state.UploadVideoID)
	remote := (decimalID.MatchString(state.VideoID) && state.ItemID == "") || (state.VideoID == "" && validDouyinCheckpointID(state.ItemID))
	noUpload := state.UploadID == "" && state.UploadVideoID == ""
	noRemote := state.VideoID == "" && state.ItemID == ""
	valid := false
	switch state.Stage {
	case "initializing":
		valid = state.Confirmed == 0 && noUpload && noRemote
	case "uploading", "part_pending":
		valid = state.Total > 1 && upload && state.UploadVideoID == "" && noRemote &&
			(state.Stage != "part_pending" || state.Confirmed < state.Total)
	case "merging":
		valid = state.Total > 1 && state.Confirmed == state.Total && upload && state.UploadVideoID == "" && noRemote
	case "merged", "posting":
		valid = state.Confirmed == state.Total && state.UploadID == "" && video && noRemote
	case "accepted":
		valid = state.Confirmed == state.Total && noUpload && remote
	case "rejected":
		valid = noUpload && noRemote
	}
	if !valid {
		return douyinPublicationCheckpoint{}, apphost.ErrPublicationConflict
	}
	return state, nil
}

func validDouyinCheckpointID(value string) bool {
	if len(value) < 1 || len(value) > 512 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func (state douyinPublicationCheckpoint) progress() *apphost.PublicationProgress {
	if state.Stage == "" {
		return nil
	}
	return &apphost.PublicationProgress{Stage: state.Stage, ConfirmedParts: state.Confirmed, TotalParts: state.Total}
}

func (p *SearchApp) PublicationCheckpointProgress(raw json.RawMessage) (*apphost.PublicationProgress, error) {
	state, err := loadDouyinPublication(raw)
	return state.progress(), err
}

func douyinRemoteID(video, item string) string {
	if video != "" {
		return video
	}
	if item == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(item))
	return "dy-item-" + hex.EncodeToString(hash[:])
}

func (state douyinPublicationCheckpoint) receipt(ref apphost.PublicationReference) apphost.PublicationReceipt {
	receipt := apphost.PublicationReceipt{Reference: ref, State: "unknown", Progress: state.progress()}
	if state.Stage == "accepted" {
		receipt.Reference.RemoteID = douyinRemoteID(state.VideoID, state.ItemID)
		receipt.State = "accepted"
	} else if state.Stage == "rejected" {
		receipt.State = "failed"
	}
	return receipt
}

func (p *SearchApp) ContinuePublication(ctx context.Context, req apphost.PublicationRequest) (apphost.PublicationReceipt, error) {
	state, err := loadDouyinPublication(req.Checkpoint)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	ref := req.Reference
	if ref.Target != req.Draft.Target || req.CheckActive == nil || req.SaveCheckpoint == nil || req.OpenArtifact == nil {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationInvalidDraft
	}
	if state.Stage != "" && state.Stage != "uploading" && state.Stage != "merged" {
		return p.ReconcilePublication(ctx, ref, req.Checkpoint, false)
	}
	key := filepath.Join(p.privateDirectory, ref.TaskID)
	if _, busy := douyinPublicationInFlight.LoadOrStore(key, struct{}{}); busy {
		return state.receipt(ref), nil
	}
	defer douyinPublicationInFlight.Delete(key)
	account, err := p.publicationAccount(ref.Target)
	if err != nil || account.grantRevision() != req.GrantVersion {
		return state.receipt(ref), nil
	}
	draft, err := douyinDraft(req.Draft)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	artifact := req.Draft.Artifacts[0]
	total, partBytes := int64(1), artifact.Bytes
	if artifact.Bytes > douyinSingleUploadBytes {
		total = 1 + (artifact.Bytes-1)/douyinTargetPartBytes
		partBytes = 1 + (artifact.Bytes-1)/total // Balance the final part above the 5 MB floor.
	}
	if state.Stage != "" && (state.Total != total || state.PartBytes != partBytes || state.Visibility != draft.visibility) {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationConflict
	}
	return p.runDouyinPublication(ctx, req, draft, artifact, state, douyinPublicationCheckpoint{
		Stage: "initializing", PartBytes: partBytes, Total: total, Visibility: draft.visibility,
	})
}

func (p *SearchApp) runDouyinPublication(ctx context.Context, req apphost.PublicationRequest,
	draft douyinPublicationDraft, artifact apphost.PublicationArtifact, state, initial douyinPublicationCheckpoint) (apphost.PublicationReceipt, error) {
	ref := req.Reference
	save := func(next douyinPublicationCheckpoint, afterResponse bool) error {
		storeCtx := ctx
		if afterResponse {
			var cancel context.CancelFunc
			storeCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
		}
		raw, err := json.Marshal(next)
		if err == nil {
			err = req.SaveCheckpoint(storeCtx, raw)
		}
		if err == nil {
			state = next
		}
		return err
	}
	claim := func(next douyinPublicationCheckpoint) (accountAuthorization, error) {
		if err := req.CheckActive(ctx); err != nil {
			return accountAuthorization{}, err
		}
		previous := state
		if err := save(next, false); err != nil {
			return accountAuthorization{}, err
		}
		err := req.CheckActive(ctx)
		var account accountAuthorization
		if err == nil {
			account, err = p.publicationAccount(ref.Target)
			if err == nil && account.grantRevision() != req.GrantVersion {
				err = apphost.ErrPublicationDenied
			}
		}
		if err != nil && previous.Stage != "" {
			// No provider call was made for this claim.
			_ = save(previous, true)
		}
		return account, err
	}
	stop := func(cause error) (apphost.PublicationReceipt, error) {
		if errors.Is(cause, errDouyinPublishRejected) || errors.Is(cause, errDouyinPublishInvalid) || errors.Is(cause, errDouyinPublishAuthorization) {
			next := state
			if next.Stage == "" {
				next = initial
			}
			next.Stage, next.UploadID, next.UploadVideoID = "rejected", "", ""
			_ = save(next, true)
			return next.receipt(ref), nil
		}
		return state.receipt(ref), nil
	}

	if state.Stage != "merged" {
		file, err := req.OpenArtifact(ctx, artifact)
		if err != nil {
			return state.receipt(ref), nil
		}
		defer file.Close()
		var head [12]byte
		if _, err := io.ReadFull(file, head[:]); err != nil || !douyinVideoHeader(artifact.Name, head) {
			return stop(errDouyinPublishRejected)
		}
		reader := io.MultiReader(bytes.NewReader(head[:]), file)
		if initial.Total == 1 {
			account, err := claim(initial)
			if err != nil {
				return state.receipt(ref), nil
			}
			videoID, err := p.uploadPublicationVideo(ctx, account, artifact.Name, reader, artifact.Bytes)
			if err != nil {
				return stop(err)
			}
			next := state
			next.Stage, next.Confirmed, next.UploadVideoID = "merged", 1, videoID
			if !validDouyinCheckpointID(videoID) || save(next, true) != nil {
				return state.receipt(ref), nil
			}
		} else {
			if state.Stage == "" {
				account, err := claim(initial)
				if err != nil {
					return state.receipt(ref), nil
				}
				id, err := p.initializePublicationUpload(ctx, account)
				if err != nil {
					return stop(err)
				}
				next := state
				next.Stage, next.UploadID = "uploading", id
				if !validDouyinCheckpointID(id) || save(next, true) != nil {
					return state.receipt(ref), nil
				}
			}
			complete, err := p.uploadDouyinParts(ctx, artifact, reader, state, claim, save)
			if err != nil {
				return stop(err)
			}
			if !complete || !douyinPublicationTimeLeft(ctx, 5*time.Second) {
				return state.receipt(ref), nil
			}
			next := state
			next.Stage = "merging"
			account, err := claim(next)
			if err != nil {
				return state.receipt(ref), nil
			}
			videoID, err := p.completePublicationUpload(ctx, account, state.UploadID)
			if err != nil {
				return stop(err)
			}
			next.Stage, next.UploadID, next.UploadVideoID = "merged", "", videoID
			if !validDouyinCheckpointID(videoID) || save(next, true) != nil {
				return state.receipt(ref), nil
			}
		}
	}
	if !douyinPublicationTimeLeft(ctx, 5*time.Second) {
		return state.receipt(ref), nil
	}
	next := state
	next.Stage = "posting"
	account, err := claim(next)
	if err != nil {
		return state.receipt(ref), nil
	}
	created, err := p.createPublicationVideo(ctx, account, state.UploadVideoID, draft.text, draft.visibility)
	if err != nil {
		return stop(err)
	}
	next.Stage, next.UploadVideoID = "accepted", ""
	if decimalID.MatchString(created.VideoID) {
		next.VideoID = created.VideoID
	} else if validDouyinCheckpointID(created.ItemID) {
		next.ItemID = created.ItemID
	} else {
		return state.receipt(ref), nil
	}
	_ = save(next, true)
	return next.receipt(ref), nil
}

func (p *SearchApp) uploadDouyinParts(ctx context.Context, artifact apphost.PublicationArtifact,
	reader io.Reader, state douyinPublicationCheckpoint, claim func(douyinPublicationCheckpoint) (accountAuthorization, error),
	save func(douyinPublicationCheckpoint, bool) error) (bool, error) {
	if state.Confirmed == state.Total {
		return true, nil
	}
	if state.Confirmed > 0 {
		if _, err := io.CopyN(io.Discard, reader, state.Confirmed*state.PartBytes); err != nil {
			return false, err
		}
	}
	buffer := make([]byte, state.PartBytes)
	var lastPart time.Duration
	for state.Confirmed < state.Total {
		if !douyinPublicationTimeLeft(ctx, lastPart+5*time.Second) {
			// Yield between acknowledged parts while the caller can still resume.
			return false, nil
		}
		started := time.Now()
		remaining := artifact.Bytes - state.Confirmed*state.PartBytes
		part := buffer[:min(state.PartBytes, remaining)]
		if _, err := io.ReadFull(reader, part); err != nil {
			return false, err
		}
		pending := state
		pending.Stage = "part_pending"
		account, err := claim(pending)
		if err != nil {
			return false, err
		}
		if err := p.uploadPublicationPart(ctx, account, state.UploadID, state.Confirmed+1, part); err != nil {
			return false, err
		}
		state.Confirmed++
		if err := save(state, true); err != nil {
			return false, err
		}
		lastPart = time.Since(started)
	}
	return true, nil
}

func douyinPublicationTimeLeft(ctx context.Context, minimum time.Duration) bool {
	deadline, hasDeadline := ctx.Deadline()
	return ctx.Err() == nil && (!hasDeadline || time.Until(deadline) >= minimum)
}

func douyinVideoHeader(name string, head [12]byte) bool {
	if strings.EqualFold(filepath.Ext(name), ".mp4") {
		return string(head[4:8]) == "ftyp"
	}
	return bytes.Equal(head[:4], []byte{0x1a, 0x45, 0xdf, 0xa3})
}

func (p *SearchApp) ReconcilePublication(ctx context.Context, ref apphost.PublicationReference, checkpoint json.RawMessage, cancel bool) (apphost.PublicationReceipt, error) {
	state, err := loadDouyinPublication(checkpoint)
	if err != nil {
		return apphost.PublicationReceipt{}, err
	}
	receipt := state.receipt(ref)
	if ref.RemoteID != "" && (state.Stage != "accepted" && state.Stage != "posting" || ref.RemoteID != receipt.Reference.RemoteID) {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationReceipt
	}
	if receipt.Reference.RemoteID == "" {
		// Only creation can publish. Pending uploads can be stopped locally.
		if cancel && state.Stage != "posting" && state.Stage != "rejected" {
			receipt.State = "cancelled"
		}
		return receipt, nil
	}
	receipt.State = "accepted"
	account, err := p.publicationAccount(ref.Target)
	if err != nil {
		return receipt, nil
	}
	settings := loadedOwnVideoSettings{ownVideoSettings: ownVideoSettings{OpenID: account.OpenID, AccessToken: account.AccessToken}}
	var status ownVideoInfo
	if decimalID.MatchString(receipt.Reference.RemoteID) {
		status, err = p.fetchOwnVideo(ctx, settings, receipt.Reference.RemoteID)
	} else if state.Stage == "accepted" && state.ItemID != "" {
		status, err = p.fetchVideoStatus(ctx, settings, state.ItemID, true)
	} else {
		return receipt, nil
	}
	// The status API proves public visibility only. Private or friends-only
	// creations remain accepted because this query cannot distinguish them.
	current, authErr := p.publicationAccount(ref.Target)
	if err == nil && authErr == nil && current.grantRevision() == account.grantRevision() && state.Visibility == 0 && status.Status != nil && *status.Status == 5 {
		receipt.State = "published"
	}
	return receipt, nil
}
