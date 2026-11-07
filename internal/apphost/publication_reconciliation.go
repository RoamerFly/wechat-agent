package apphost

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"wechat-bot/internal/pluginhost"
)

const publicationObservationKey = "verified-candidate"

var ErrPublicationReconciliationInput = errors.New("invalid publication reconciliation request")
var ErrPublicationReconciliationRead = errors.New("publication reconciliation read failed")

type PublicationReconciliationQuery struct {
	Action   string `json:"action"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
	BVID     string `json:"bvid,omitempty"`
}

type PublicationCandidate struct {
	BVID      string `json:"bvid"`
	Title     string `json:"title,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type PublicationCandidateObservation struct {
	BVID      string `json:"bvid"`
	Published bool   `json:"published"`
	CheckedAt string `json:"checked_at"`
}

type PublicationReconciliation struct {
	JobID    string                           `json:"job_id"`
	Items    []PublicationCandidate           `json:"items"`
	Page     int                              `json:"page,omitempty"`
	PageSize int                              `json:"page_size,omitempty"`
	HasMore  bool                             `json:"has_more"`
	NextPage int                              `json:"next_page,omitempty"`
	Item     *PublicationCandidateObservation `json:"item,omitempty"`
}

// PublicationCandidateProvider observes account archives. Its response is not
// a PublicationReceipt and cannot settle a job or continue an upload.
type PublicationCandidateProvider interface {
	ReadPublicationCandidates(context.Context, PublicationReference, json.RawMessage, PublicationReconciliationQuery) (PublicationReconciliation, error)
}

func (p *Publisher) ReconcileForOwner(ctx context.Context, actor pluginhost.Invocation, jobID string, query PublicationReconciliationQuery) (PublicationReconciliation, error) {
	if p == nil || p.store == nil {
		return PublicationReconciliation{}, ErrPublicationUnavailable
	}
	if query.Action != "candidates" && query.Action != "verify" {
		return PublicationReconciliation{}, ErrPublicationReconciliationInput
	}
	job, err := p.store.Job(ctx, jobID)
	if err != nil {
		return PublicationReconciliation{}, ErrNotFound
	}
	if !sameActor(actor, job.Context) {
		return PublicationReconciliation{}, ErrPublicationOwnership
	}
	record, err := publicationFromJob(job)
	if err != nil || publicationState(job, record) != "awaiting_receipt" || job.RemoteID != "" {
		return PublicationReconciliation{}, ErrPublicationConflict
	}
	entry, _, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return PublicationReconciliation{}, err
	}
	provider, ok := entry.Provider.(PublicationCandidateProvider)
	if !ok {
		return PublicationReconciliation{}, ErrPublicationUnavailable
	}
	checkpoint, err := p.publicationCheckpoint(ctx, job, record)
	if err != nil {
		return PublicationReconciliation{}, err
	}
	var observations pluginhost.HostState
	if query.Action == "verify" {
		observations, err = p.publicationObservations(ctx, job, record)
		if err != nil {
			return PublicationReconciliation{}, err
		}
	}
	out, err := provider.ReadPublicationCandidates(ctx, referenceFor(job, record), checkpoint.Records[publicationCheckpointKey], query)
	if err != nil {
		return PublicationReconciliation{}, err
	}
	current, err := p.store.Job(ctx, jobID)
	if err != nil || !sameActor(actor, current.Context) || current.RemoteID != "" || publicationState(current, record) != "awaiting_receipt" {
		return PublicationReconciliation{}, ErrPublicationConflict
	}
	latest, err := p.publicationCheckpoint(ctx, current, record)
	if err != nil || latest.Revision != checkpoint.Revision {
		return PublicationReconciliation{}, ErrPublicationConflict
	}
	if query.Action == "candidates" {
		out.JobID, out.Item = jobID, nil
		if out.Items == nil {
			out.Items = []PublicationCandidate{}
		}
		return out, nil
	}
	if out.Item == nil || query.BVID == "" || out.Item.BVID != query.BVID {
		return PublicationReconciliation{}, ErrPublicationReconciliationRead
	}
	item := PublicationCandidateObservation{BVID: out.Item.BVID, Published: out.Item.Published, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	data, err := json.Marshal(item)
	if err != nil {
		return PublicationReconciliation{}, err
	}
	puts := map[string]json.RawMessage{publicationObservationKey: data}
	plugin, namespace := "application-"+record.Draft.Target.AppID, job.ID+"-observations"
	if observations.Revision == 0 {
		_, err = p.store.InitializeHostState(ctx, plugin, namespace, record.Manifest, puts)
	} else {
		_, err = p.store.UpdateHostState(ctx, plugin, namespace, observations.Revision, puts, nil)
	}
	if err != nil {
		return PublicationReconciliation{}, err
	}
	return PublicationReconciliation{JobID: jobID, Item: &item}, nil
}

// Observations use a separate namespace so they cannot race an authoritative
// posting checkpoint write. Only the owner management page reads them.
func (p *Publisher) publicationObservations(ctx context.Context, job pluginhost.Job, record publicationRecord) (pluginhost.HostState, error) {
	state, err := p.store.ReadHostState(ctx, "application-"+record.Draft.Target.AppID, job.ID+"-observations")
	if err == nil && state.Revision != 0 && state.SourceHash != record.Manifest {
		err = ErrPublicationConflict
	}
	return state, err
}

func (p *Publisher) addPublicationReconciliation(ctx context.Context, job pluginhost.Job, record publicationRecord, view *PublicationView) {
	if view.State != "awaiting_receipt" || view.RemoteID != "" || view.Progress == nil || view.Progress.Stage != "posting" {
		return
	}
	entry, ok := p.registry.Lookup(record.Draft.Target.AppID)
	if !ok || entry.InstallationID != record.Draft.Target.InstallationID {
		return
	}
	if _, ok := entry.Provider.(PublicationCandidateProvider); !ok {
		return
	}
	view.AvailableActions = append(view.AvailableActions, "reconcile")
	state, err := p.publicationObservations(ctx, job, record)
	var item PublicationCandidateObservation
	if err == nil && json.Unmarshal(state.Records[publicationObservationKey], &item) == nil && item.BVID != "" && item.CheckedAt != "" {
		view.Reconciliation = &item
	}
}
