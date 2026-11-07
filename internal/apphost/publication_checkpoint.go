package apphost

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"wechat-bot/internal/pluginhost"
)

const publicationCheckpointKey = "progress"

func publicationAdapter(provider Provider) (publicationAuthorizer, bool) {
	if workflow, ok := provider.(CheckpointedPublicationProvider); ok {
		return workflow, true
	}
	legacy, ok := provider.(PublicationProvider)
	return legacy, ok
}

func (p *Publisher) resume(ctx context.Context, job pluginhost.Job, record publicationRecord) (PublicationView, error) {
	if job.State != "submitting" || job.CancelRequested || job.RemoteID != "" {
		return viewFor(job, record), ErrPublicationConflict
	}
	_, provider, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return viewFor(job, record), err
	}
	if _, ok := provider.(CheckpointedPublicationProvider); !ok {
		return viewFor(job, record), ErrPublicationConflict
	}
	if err := p.checkPublicationActive(ctx, job.ID, record); err != nil {
		return viewFor(job, record), err
	}
	return p.continueSubmission(ctx, job, record, provider)
}

func (p *Publisher) checkPublicationActive(ctx context.Context, jobID string, record publicationRecord) error {
	job, err := p.store.Job(ctx, jobID)
	if err != nil || job.CancelRequested || job.State != "submitting" {
		return ErrPublicationDenied
	}
	entry, provider, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return err
	}
	grant, err := provider.PublicationGrant(ctx, record.Draft.Target)
	if err != nil || !grant.Allowed || grant.Version != job.ConfigRevision ||
		job.Version != entry.Registration || job.PackageDigest != publicationDigest(entry, record.Draft.Target) {
		return ErrPublicationDenied
	}
	if p.validate != nil && p.validate(ctx, job, record.Draft) != nil {
		return ErrPublicationInvalidDraft
	}
	job, err = p.store.Job(ctx, jobID)
	if err != nil || job.CancelRequested || job.State != "submitting" {
		return ErrPublicationDenied
	}
	return ctx.Err()
}

func (p *Publisher) publicationCheckpoint(ctx context.Context, job pluginhost.Job, record publicationRecord) (pluginhost.HostState, error) {
	state, err := p.store.ReadHostState(ctx, "application-"+record.Draft.Target.AppID, job.ID)
	if err != nil {
		return pluginhost.HostState{}, err
	}
	if state.Revision != 0 && (state.SourceHash != record.Manifest || !jsonObject(state.Records[publicationCheckpointKey])) {
		return pluginhost.HostState{}, ErrPublicationConflict
	}
	return state, nil
}

// listProgress asks the adapter to reduce its private checkpoint to safe
// operational progress. The host validates that reduction before exposing it.
func (p *Publisher) listProgress(ctx context.Context, job pluginhost.Job, record publicationRecord) (*PublicationProgress, error) {
	if isLocal(record.Draft.Target) || publicationState(job, record) == "published" {
		return nil, nil
	}
	state, err := p.publicationCheckpoint(ctx, job, record)
	if err != nil || len(state.Records[publicationCheckpointKey]) == 0 {
		return nil, err
	}
	entry, ok := p.registry.Lookup(record.Draft.Target.AppID)
	if !ok || entry.InstallationID != record.Draft.Target.InstallationID {
		return nil, ErrPublicationUnavailable
	}
	provider, ok := entry.Provider.(PublicationCheckpointProgressProvider)
	if !ok {
		return nil, nil
	}
	progress, err := provider.PublicationCheckpointProgress(state.Records[publicationCheckpointKey])
	if err != nil || !validPublicationProgress(progress) {
		return nil, ErrPublicationConflict
	}
	return &PublicationProgress{Stage: progress.Stage, ErrorCode: progress.ErrorCode, ConfirmedParts: progress.ConfirmedParts, TotalParts: progress.TotalParts}, nil
}

func (p *Publisher) runCheckpointed(ctx context.Context, job pluginhost.Job, record publicationRecord, active *atomic.Bool, workflow CheckpointedPublicationProvider, req PublicationRequest) (PublicationReceipt, error) {
	state, err := p.publicationCheckpoint(ctx, job, record)
	if err != nil {
		return PublicationReceipt{}, err
	}
	req.Checkpoint = state.Records[publicationCheckpointKey]
	req.CheckActive = func(ctx context.Context) error {
		if !active.Load() {
			return ErrPublicationDenied
		}
		return p.checkPublicationActive(ctx, job.ID, record)
	}
	revision := state.Revision
	req.SaveCheckpoint = func(ctx context.Context, payload json.RawMessage) error {
		if !active.Load() || !jsonObject(payload) {
			return ErrPublicationConflict
		}
		// A late response can still advance the checkpoint after cancellation.
		// Starting any subsequent side effect requires CheckActive again.
		data := map[string]json.RawMessage{publicationCheckpointKey: payload}
		plugin := "application-" + record.Draft.Target.AppID
		if revision == 0 {
			saved, err := p.store.InitializeHostState(ctx, plugin, job.ID, record.Manifest, data)
			if err == nil {
				revision = saved.Revision
			}
			return err
		}
		next, err := p.store.UpdateHostState(ctx, plugin, job.ID, revision, data, nil)
		if err == nil {
			revision = next
		}
		return err
	}
	return workflow.ContinuePublication(ctx, req)
}

func (p *Publisher) reconcile(ctx context.Context, job pluginhost.Job, record publicationRecord, provider publicationAuthorizer, cancel bool) (PublicationReceipt, error) {
	ref := referenceFor(job, record)
	if workflow, ok := provider.(CheckpointedPublicationProvider); ok {
		state, err := p.publicationCheckpoint(ctx, job, record)
		if err != nil {
			return PublicationReceipt{}, err
		}
		if recovery, ok := provider.(PublicationRecoveryProvider); ok && !cancel && !job.CancelRequested && ref.RemoteID == "" {
			draft := cloneDraft(record.Draft)
			draft.Origin, draft.Project = nil, nil
			return recovery.RecoverPublication(ctx, PublicationRecoveryRequest{Reference: ref, Draft: draft, Checkpoint: state.Records[publicationCheckpointKey], CreatedAt: job.Created})
		}
		return workflow.ReconcilePublication(ctx, ref, state.Records[publicationCheckpointKey], cancel)
	}
	legacy := provider.(PublicationProvider)
	if cancel {
		return legacy.CancelPublication(ctx, ref)
	}
	return legacy.QueryPublication(ctx, ref)
}
