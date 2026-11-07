package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strconv"

	"wechat-bot/internal/pluginhost"
)

// newBoundMediaTranscriptionPolicy is the only non-incoming constructor. Its
// caller must already hold a durable, purpose-specific proof for the bytes it
// supplies. It cannot be reached from an Agent argument: the origin callback
// is host-only and is rechecked before every service/request/result boundary.
func (m *Manager) newBoundMediaTranscriptionPolicy(ctx context.Context, wav []byte, inv pluginhost.Invocation, revision int, check func(context.Context) error) (*mediaTranscriptionPolicy, error) {
	if m == nil || check == nil || revision < 1 || inv.Account == "" || inv.Account != m.native.Account() || inv.Thread == "" || inv.Requester == "" || inv.Purpose == "" || inv.RequestKey == "" || inv.Authorization == "" || len(inv.SourceVersions) == 0 {
		return nil, errors.New("media_transcription_origin_required")
	}
	for id, version := range inv.SourceVersions {
		if id == "" || version < 1 {
			return nil, errors.New("media_transcription_origin_required")
		}
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	store, err := m.packageStore()
	if err != nil {
		return nil, err
	}
	p, err := m.newMediaTranscriptionPolicyForOrigin(ctx, store, revision, wav)
	if err != nil {
		return nil, err
	}
	// Include the exact audio binding in the outer job identity. The plugin
	// still receives only the opaque reference; bytes never leave the host
	// except through the already-bound model/local service callback.
	ref := p.Reference()
	inv.RequestKey = "bound-media-transcription-" + agentID(inv.RequestKey, ref.SHA, strconv.Itoa(ref.Version), strconv.Itoa(p.ConfigRevision()))
	p.invocationValue = inv
	p.originCheck = func(ctx context.Context, _ pluginhost.Job, _ pluginhost.Capability) error {
		if m.native.Account() != inv.Account {
			return errors.New("media_transcription_origin_changed")
		}
		return check(ctx)
	}
	return p, nil
}

// newAdminMediaTranscriptionPolicy binds a generated test WAV to the exact
// completed administrative speech job. It intentionally has no native message
// or conversation sender and therefore cannot be confused with chat evidence.
func (m *Manager) newAdminMediaTranscriptionPolicy(ctx context.Context, speech *speechPluginPolicy, wav []byte) (*mediaTranscriptionPolicy, error) {
	if speech == nil || speech.hostManager() != m || speech.admin == nil {
		return nil, errors.New("media_transcription_admin_origin_required")
	}
	if err := speech.checkCompleted(ctx); err != nil {
		return nil, err
	}
	completed := speech.completedJob
	if completed.ID == "" || completed.State != "completed" || completed.Context.Purpose != "admin speech synthesis test" {
		return nil, errors.New("media_transcription_admin_origin_required")
	}
	revision := completed.Revision
	if revision < 1 {
		revision = 1
	}
	seed := sha256.Sum256([]byte(completed.ID + "\x00" + completed.Context.Authorization + "\x00" + speech.admin.Session + "\x00" + speech.admin.RequestID))
	inv := pluginhost.Invocation{
		Account:        speech.admin.Account,
		Thread:         "host:admin-speech-test",
		Requester:      "administrator",
		Purpose:        "admin speech ASR test",
		RequestKey:     "admin-speech-asr-" + hex.EncodeToString(seed[:]),
		Authorization:  agentID(completed.Context.Authorization, completed.ID, speech.admin.Session, speech.admin.RequestID),
		SourceVersions: map[string]int{"speech-job:" + completed.ID: revision},
	}
	return m.newBoundMediaTranscriptionPolicy(ctx, wav, inv, revision, func(ctx context.Context) error {
		if err := speech.checkCompleted(ctx); err != nil {
			return err
		}
		current := speech.completedJob
		if current.ID != completed.ID || current.Revision != completed.Revision || !reflect.DeepEqual(current.Context, completed.Context) {
			return errors.New("media_transcription_admin_origin_changed")
		}
		return nil
	})
}
