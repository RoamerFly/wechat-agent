package apphost

// The publication host keeps a creation request in the same durable ledger as
// other host work.  It deliberately does not treat an adapter acknowledgement
// as publication: only a receipt bound to this task, connection and manifest
// can advance a task to published.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"wechat-bot/internal/pluginhost"
)

var (
	ErrPublicationDenied       = errors.New("publication is not authorised")
	ErrPublicationUnavailable  = errors.New("publication application is unavailable")
	ErrPublicationInvalidDraft = errors.New("invalid publication draft")
	ErrPublicationOwnership    = errors.New("publication task is outside this actor")
	ErrPublicationReceipt      = errors.New("publication receipt could not be verified")
	ErrPublicationConflict     = errors.New("publication task is not ready for this action")
)

// PublicationArtifact is a host-verified immutable artifact reference.  The
// application receives it only through OpenArtifact, never an arbitrary path.
type PublicationArtifact struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
}

// PublicationDraft keeps editable metadata locally. Origin is issued by the
// host, not accepted from a tool argument, and is intentionally absent from a
// PublicationView.
type PublicationDraft struct {
	Target    ConnectionRef         `json:"target"`
	Title     string                `json:"title"`
	Body      string                `json:"body"`
	Project   *PublicationArtifact  `json:"project,omitempty"`
	Artifacts []PublicationArtifact `json:"artifacts,omitempty"`
	Options   json.RawMessage       `json:"platform_options,omitempty"`
	Origin    json.RawMessage       `json:"origin"`
}

type PublicationGrant struct {
	Allowed bool   `json:"allowed"`
	Version string `json:"version"`
}

// PublicationReference is the complete provider-side idempotency identity.
// A provider must echo it exactly in every receipt.
type PublicationReference struct {
	TaskID         string        `json:"task_id"`
	Target         ConnectionRef `json:"target"`
	ManifestSHA256 string        `json:"manifest_sha256"`
	RemoteID       string        `json:"remote_id,omitempty"`
}

type PublicationRequest struct {
	Reference      PublicationReference                                              `json:"reference"`
	Draft          PublicationDraft                                                  `json:"draft"`
	OpenArtifact   func(context.Context, PublicationArtifact) (io.ReadCloser, error) `json:"-"`
	CheckActive    func(context.Context) error                                       `json:"-"`
	Checkpoint     json.RawMessage                                                   `json:"-"`
	GrantVersion   string                                                            `json:"-"`
	SaveCheckpoint func(context.Context, json.RawMessage) error                      `json:"-"`
}

type PublicationReceipt struct {
	Reference PublicationReference `json:"reference"`
	// State is one of unknown, accepted, published, failed, cancelled.
	State    string               `json:"state"`
	Progress *PublicationProgress `json:"progress,omitempty"`
}

type PublicationProgress struct {
	Stage          string `json:"stage"`
	ErrorCode      string `json:"error_code,omitempty"`
	ConfirmedParts int64  `json:"confirmed_parts"`
	TotalParts     int64  `json:"total_parts"`
}

// PublicationRecoveryProvider is a read-only account lookup for an original
// submitted task whose response ID was lost. It receives the immutable draft
// and host creation time, never authority to submit another copy.
type PublicationRecoveryRequest struct {
	Reference  PublicationReference
	Draft      PublicationDraft
	Checkpoint json.RawMessage
	CreatedAt  string
}
type PublicationRecoveryProvider interface {
	RecoverPublication(context.Context, PublicationRecoveryRequest) (PublicationReceipt, error)
}

// PublicationProvider is optional on an application adapter.  Grant is
// checked only before a new submit. Query and cancel remain possible after a
// later revocation so the host can account for an earlier submission safely.
type PublicationProvider interface {
	publicationAuthorizer
	SubmitPublication(context.Context, PublicationRequest) (PublicationReceipt, error)
	QueryPublication(context.Context, PublicationReference) (PublicationReceipt, error)
	CancelPublication(context.Context, PublicationReference) (PublicationReceipt, error)
}

type publicationAuthorizer interface {
	PublicationGrant(context.Context, ConnectionRef) (PublicationGrant, error)
}

// CheckpointedPublicationProvider can resume only the original workflow. Status
// and cancellation may reconcile receipts, but must never upload or post.
type CheckpointedPublicationProvider interface {
	publicationAuthorizer
	ValidatePublicationDraft(PublicationDraft) error
	ContinuePublication(context.Context, PublicationRequest) (PublicationReceipt, error)
	ReconcilePublication(context.Context, PublicationReference, json.RawMessage, bool) (PublicationReceipt, error)
}

// PublicationCheckpointProgressProvider optionally reduces an adapter-private
// checkpoint into safe, user-visible operational progress. The host validates
// the returned progress but never interprets provider checkpoint JSON itself.
type PublicationCheckpointProgressProvider interface {
	PublicationCheckpointProgress(json.RawMessage) (*PublicationProgress, error)
}

type publicationRecord struct {
	Draft    PublicationDraft `json:"draft"`
	Manifest string           `json:"manifest_sha256"`
}

// PublicationView contains only safe operational metadata. In particular it
// omits publication text, provenance and artifact names/hashes so a status
// query cannot re-inject forgotten content into a later conversation.
type PublicationView struct {
	JobID            string                           `json:"job_id"`
	State            string                           `json:"state"`
	CancelRequested  bool                             `json:"cancel_requested"`
	Target           ConnectionRef                    `json:"target"`
	ProjectID        string                           `json:"project_id,omitempty"`
	ArtifactIDs      []string                         `json:"artifact_ids,omitempty"`
	ManifestSHA256   string                           `json:"manifest_sha256"`
	RemoteID         string                           `json:"remote_id,omitempty"`
	Diagnostic       string                           `json:"diagnostic,omitempty"`
	Progress         *PublicationProgress             `json:"progress,omitempty"`
	Reconciliation   *PublicationCandidateObservation `json:"reconciliation,omitempty"`
	UpdatedAt        string                           `json:"updated_at"`
	AvailableActions []string                         `json:"available_actions"`
}

type PublicationPage struct {
	Items   []PublicationView `json:"items"`
	Next    int64             `json:"next"`
	HasMore bool              `json:"has_more"`
	Limit   int               `json:"limit"`
}

type Publisher struct {
	registry Registry
	store    *pluginhost.Store
	validate func(context.Context, pluginhost.Job, PublicationDraft) error
	open     func(context.Context, PublicationArtifact) (io.ReadCloser, error)
}

func NewPublisher(registry Registry, store *pluginhost.Store, validate func(context.Context, pluginhost.Job, PublicationDraft) error, open func(context.Context, PublicationArtifact) (io.ReadCloser, error)) *Publisher {
	return &Publisher{registry: registry, store: store, validate: validate, open: open}
}

// ResolveTarget converts a user-facing application/connection pair into the
// exact host registration and account reference persisted in a task.
func (p *Publisher) ResolveTarget(ctx context.Context, appID, connectionID string) (ConnectionRef, error) {
	entry, ok := p.registry.Lookup(appID)
	if !ok {
		return ConnectionRef{}, ErrPublicationUnavailable
	}
	connections, err := entry.Provider.Connections(ctx)
	if err != nil {
		return ConnectionRef{}, ErrPublicationUnavailable
	}
	for _, connection := range connections {
		if connection.ID == connectionID {
			return ConnectionRef{AppID: entry.ID, InstallationID: entry.InstallationID, ConnectionID: connection.ID, AccountID: connection.AccountID}, nil
		}
	}
	return ConnectionRef{}, ErrNotFound
}

// Discover lists configured connections supported by a publication adapter.
// It is discovery only; it never proves that the current home grant allows a
// new publication.
func (p *Publisher) Discover(ctx context.Context) []ConnectionRef {
	found := make([]ConnectionRef, 0)
	for _, app := range p.registry.Applications() {
		entry, _ := p.registry.Lookup(app.ID)
		provider, ok := publicationAdapter(entry.Provider)
		if !ok {
			continue
		}
		connections, err := entry.Provider.Connections(ctx)
		if err != nil {
			continue
		}
		for _, connection := range connections {
			ref := ConnectionRef{AppID: app.ID, InstallationID: app.InstallationID, ConnectionID: connection.ID, AccountID: connection.AccountID}
			grant, err := provider.PublicationGrant(ctx, ref)
			if err == nil && grant.Allowed && grant.Version != "" {
				found = append(found, ref)
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		return found[i].AppID+"\x00"+found[i].ConnectionID < found[j].AppID+"\x00"+found[j].ConnectionID
	})
	return found
}

func (p *Publisher) Draft(ctx context.Context, actor pluginhost.Invocation, draft PublicationDraft) (PublicationView, error) {
	if p == nil || p.store == nil || !validDraft(draft) {
		return PublicationView{}, ErrPublicationInvalidDraft
	}
	appID, version, digest, revision, err := p.draftContract(ctx, draft.Target)
	if err != nil {
		return PublicationView{}, err
	}
	if entry, ok := p.registry.Lookup(draft.Target.AppID); ok {
		if workflow, ok := entry.Provider.(CheckpointedPublicationProvider); ok {
			if err := workflow.ValidatePublicationDraft(draft); err != nil {
				return PublicationView{}, err
			}
		}
	}
	record := publicationRecord{Draft: cloneDraft(draft)}
	record.Manifest = publicationManifest(record.Draft)
	arguments, _ := json.Marshal(record)
	candidate := pluginhost.Job{Version: version, PackageDigest: digest, ConfigRevision: revision, Tool: "publication", Context: actor, Arguments: arguments}
	if p.validate != nil {
		if err := p.validate(ctx, candidate, record.Draft); err != nil {
			return PublicationView{}, ErrPublicationInvalidDraft
		}
	}
	job, _, err := p.store.CreateApplicationJob(ctx, appID, candidate)
	if err != nil {
		return PublicationView{}, err
	}
	return viewFor(job, record), nil
}

// Action applies submit, status, or cancel to one durable task. It never
// re-submits an uncertain request: status/cancel reconcile the same task key.
func (p *Publisher) Action(ctx context.Context, actor pluginhost.Invocation, jobID, action string) (PublicationView, error) {
	if p == nil || p.store == nil {
		return PublicationView{}, ErrPublicationUnavailable
	}
	job, err := p.store.Job(ctx, jobID)
	if err != nil {
		return PublicationView{}, ErrNotFound
	}
	record, err := publicationFromJob(job)
	if err != nil || !sameActor(actor, job.Context) {
		if err != nil {
			return PublicationView{}, ErrPublicationConflict
		}
		return PublicationView{}, ErrPublicationOwnership
	}
	switch action {
	case "status":
		return p.status(ctx, job, record)
	case "submit":
		return p.submit(ctx, job, record)
	case "resume":
		return p.resume(ctx, job, record)
	case "cancel":
		return p.cancel(ctx, job, record)
	default:
		return PublicationView{}, ErrPublicationConflict
	}
}

func (p *Publisher) List(ctx context.Context, actor pluginhost.Invocation, appID string, after int64, limit int) (PublicationPage, error) {
	return p.list(ctx, actor, appID, after, limit, false)
}

func (p *Publisher) ListForOwner(ctx context.Context, actor pluginhost.Invocation, appID string, after int64, limit int) (PublicationPage, error) {
	return p.list(ctx, actor, appID, after, limit, true)
}

func (p *Publisher) list(ctx context.Context, actor pluginhost.Invocation, appID string, after int64, limit int, ownerEvidence bool) (PublicationPage, error) {
	if appID == "" {
		appID = "local"
	}
	page, err := p.store.JobsForTool(ctx, "application/"+appID, "publication", after, limit)
	if err != nil {
		return PublicationPage{}, err
	}
	result := PublicationPage{Items: make([]PublicationView, 0, len(page.Items)), Next: page.Next, HasMore: page.HasMore, Limit: page.Limit}
	for _, job := range page.Items {
		if !sameActor(actor, job.Context) {
			continue
		}
		record, err := publicationFromJob(job)
		if err != nil {
			continue
		}
		view := viewFor(job, record)
		// Listing is intentionally ledger-only: a browser refresh must never
		// query a provider or cause a workflow transition. Checkpoint state is
		// host-owned and is reduced to the safe progress triple below.
		if progress, progressErr := p.listProgress(ctx, job, record); progressErr == nil {
			view.Progress = progress
		}
		view.AvailableActions = p.listActions(ctx, job, record, view.Progress)
		if ownerEvidence {
			p.addPublicationReconciliation(ctx, job, record, &view)
		}
		result.Items = append(result.Items, view)
	}
	return result, nil
}

// listActions determines affordances, not authority. Action still performs
// all source, ownership and authorization checks at its side-effect boundary.
func (p *Publisher) listActions(ctx context.Context, job pluginhost.Job, record publicationRecord, progress *PublicationProgress) []string {
	state := publicationState(job, record)
	if state == "published" || state == "failed" || state == "cancelled" {
		return []string{}
	}
	if state == "draft" || state == "local_draft" {
		actions := []string{"cancel"}
		if state == "draft" && p.listExactGrant(ctx, job, record) {
			actions = append(actions, "submit")
		}
		return actions
	}
	if state == "accepted" {
		return []string{"status"}
	}
	actions := []string{"status"}
	if job.CancelRequested {
		return actions
	}
	actions = append(actions, "cancel")
	// Only an interrupted upload or already merged upload can safely continue.
	// Initialization, merge and post are durable uncertainty boundaries: they
	// may be queried or cancelled, but must never be replayed from a list UI.
	douyinPrepared := record.Draft.Target.AppID == "douyin" && progress != nil && (progress.Stage == "opening_creator" || progress.Stage == "selecting_file" || progress.Stage == "editing" || progress.Stage == "verifying_form")
	if state == "awaiting_receipt" && progress != nil && (progress.Stage == "uploading" || progress.Stage == "merged" || douyinPrepared) && p.listExactGrant(ctx, job, record) {
		actions = append(actions, "resume")
	}
	return actions
}

func (p *Publisher) listExactGrant(ctx context.Context, job pluginhost.Job, record publicationRecord) bool {
	entry, ok := p.registry.Lookup(record.Draft.Target.AppID)
	if !ok || entry.InstallationID != record.Draft.Target.InstallationID || job.Version != entry.Registration || job.PackageDigest != publicationDigest(entry, record.Draft.Target) {
		return false
	}
	provider, ok := publicationAdapter(entry.Provider)
	if !ok {
		return false
	}
	grant, err := provider.PublicationGrant(ctx, record.Draft.Target)
	return err == nil && grant.Allowed && grant.Version != "" && grant.Version == job.ConfigRevision
}

func (p *Publisher) submit(ctx context.Context, job pluginhost.Job, record publicationRecord) (PublicationView, error) {
	if isLocal(record.Draft.Target) {
		return viewFor(job, record), ErrPublicationDenied
	}
	entry, provider, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return viewFor(job, record), err
	}
	grant, err := provider.PublicationGrant(ctx, record.Draft.Target)
	if err != nil || !grant.Allowed || grant.Version == "" {
		return viewFor(job, record), ErrPublicationDenied
	}
	if job.Version != entry.Registration || job.PackageDigest != publicationDigest(entry, record.Draft.Target) || job.ConfigRevision != grant.Version {
		return viewFor(job, record), ErrPublicationDenied
	}
	if p.validate != nil {
		if err := p.validate(ctx, job, record.Draft); err != nil {
			return viewFor(job, record), ErrPublicationInvalidDraft
		}
	}
	// The claim is the ordering point with cancellation. Once it wins, a later
	// cancel can only stop subsequent provider work; it cannot make this submit
	// appear never to have been attempted.
	claimed, err := p.store.ClaimJobWithSubmission(ctx, job.ID, job.Revision, mustJSON(PublicationReference{TaskID: job.ID, Target: record.Draft.Target, ManifestSHA256: record.Manifest}))
	if err != nil {
		fresh, getErr := p.store.Job(ctx, job.ID)
		if getErr == nil {
			return viewFor(fresh, record), nil
		}
		return viewFor(job, record), ErrPublicationConflict
	}
	// Recheck immediately before the first external side effect. A revoke or
	// cancellation after the durable claim means an unknown/awaiting task, not
	// a new submission. Cancellation is never held behind this network call.
	fresh, getErr := p.store.Job(ctx, claimed.ID)
	if getErr != nil {
		return viewFor(claimed, record), nil
	}
	if fresh.CancelRequested {
		if stopped, stopErr := p.store.FinishJob(ctx, fresh.ID, fresh.Revision, "cancelled", nil); stopErr == nil {
			fresh = stopped
		}
		return viewFor(fresh, record), nil
	}
	grant, grantErr := provider.PublicationGrant(ctx, record.Draft.Target)
	if grantErr != nil || !grant.Allowed || grant.Version != claimed.ConfigRevision {
		if failed, failErr := p.store.FinishJob(ctx, fresh.ID, fresh.Revision, "failed", nil); failErr == nil {
			fresh = failed
		}
		return viewFor(fresh, record), ErrPublicationDenied
	}
	if p.validate != nil && p.validate(ctx, fresh, record.Draft) != nil {
		if failed, failErr := p.store.FinishJob(ctx, fresh.ID, fresh.Revision, "failed", nil); failErr == nil {
			fresh = failed
		}
		return viewFor(fresh, record), ErrPublicationInvalidDraft
	}
	return p.continueSubmission(ctx, claimed, record, provider)
}

func (p *Publisher) continueSubmission(ctx context.Context, claimed pluginhost.Job, record publicationRecord, provider publicationAuthorizer) (PublicationView, error) {
	providerDraft := cloneDraft(record.Draft)
	providerDraft.Origin, providerDraft.Project = nil, nil
	var active atomic.Bool
	active.Store(true)
	defer active.Store(false)
	request := PublicationRequest{Reference: referenceFor(claimed, record), Draft: providerDraft, GrantVersion: claimed.ConfigRevision, OpenArtifact: p.openAllowed(ctx, &active, claimed.ID, record)}
	var receipt PublicationReceipt
	var err error
	if workflow, ok := provider.(CheckpointedPublicationProvider); ok {
		receipt, err = p.runCheckpointed(ctx, claimed, record, &active, workflow, request)
	} else {
		receipt, err = provider.(PublicationProvider).SubmitPublication(ctx, request)
	}
	if err != nil {
		// A transport/provider failure is an unknown result. Do not expose its
		// body and never turn it into another Submit automatically.
		if current, readErr := p.store.Job(ctx, claimed.ID); readErr == nil {
			claimed = current
		}
		return viewFor(claimed, record), nil
	}
	// Receipt accounting is local and may finish after the request deadline.
	// Never label a valid provider result as a mismatched receipt solely because
	// the page operation consumed its context. This does not start new work.
	settleCtx, settleCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer settleCancel()
	settled, err := p.settle(settleCtx, claimed, record, receipt)
	if err != nil {
		return viewFor(claimed, record), ErrPublicationReceipt
	}
	return viewWithProgress(settled, record, receipt), nil
}

func (p *Publisher) status(ctx context.Context, job pluginhost.Job, record publicationRecord) (PublicationView, error) {
	if isLocal(record.Draft.Target) || job.Recovery() == "claim" || job.Recovery() == "terminal" {
		return viewFor(job, record), nil
	}
	_, provider, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return viewFor(job, record), ErrPublicationUnavailable
	}
	receipt, err := p.reconcile(ctx, job, record, provider, false)
	if err != nil {
		return viewFor(job, record), nil
	}
	settled, err := p.settle(ctx, job, record, receipt)
	if err != nil {
		return viewFor(job, record), ErrPublicationReceipt
	}
	return viewWithProgress(settled, record, receipt), nil
}

func (p *Publisher) cancel(ctx context.Context, job pluginhost.Job, record publicationRecord) (PublicationView, error) {
	requested := job
	for attempts := 0; !requested.CancelRequested; attempts++ {
		if requested.Recovery() == "terminal" {
			return viewFor(requested, record), nil
		}
		if attempts == 3 {
			return viewFor(requested, record), ErrPublicationConflict
		}
		next, err := p.store.RequestJobCancel(ctx, requested.ID, requested.Revision)
		if err == nil {
			requested = next
			break
		}
		if !errors.Is(err, pluginhost.ErrRevision) {
			return viewFor(requested, record), ErrPublicationConflict
		}
		requested, err = p.store.Job(ctx, job.ID)
		if err != nil {
			return viewFor(job, record), ErrPublicationConflict
		}
	}
	if requested.Recovery() == "terminal" {
		return viewFor(requested, record), nil
	}
	_, provider, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return viewFor(requested, record), ErrPublicationUnavailable
	}
	// Query first so an already published work never receives a cancel call.
	if receipt, queryErr := p.reconcile(ctx, requested, record, provider, false); queryErr == nil {
		known, err := p.settle(ctx, requested, record, receipt)
		if err != nil {
			return viewFor(requested, record), ErrPublicationReceipt
		}
		requested = known
		if requested.Recovery() == "terminal" {
			return viewFor(requested, record), nil
		}
	}
	// Cancellation is for pending work only. A late receipt can still prove
	// that the platform published it before cancellation took effect.
	receipt, callErr := p.reconcile(ctx, requested, record, provider, true)
	if callErr == nil {
		if settled, settleErr := p.settle(ctx, requested, record, receipt); settleErr == nil {
			requested = settled
		}
	}
	if requested.Recovery() == "terminal" {
		return viewFor(requested, record), nil
	}
	receipt, err = p.reconcile(ctx, requested, record, provider, false)
	if err != nil {
		return viewFor(requested, record), nil
	}
	settled, err := p.settle(ctx, requested, record, receipt)
	if err != nil {
		return viewFor(requested, record), ErrPublicationReceipt
	}
	return viewFor(settled, record), nil
}

func (p *Publisher) settle(ctx context.Context, job pluginhost.Job, record publicationRecord, receipt PublicationReceipt) (pluginhost.Job, error) {
	if !validReceipt(job, record, receipt) {
		return job, ErrPublicationReceipt
	}
	if receipt.State == "unknown" {
		return p.store.Job(ctx, job.ID)
	}
	// A known receipt may race cancellation. Re-read and apply it with the
	// newest revision; this never issues another provider Submit. A late
	// published receipt wins over cancellation because it proves the platform
	// already accepted the work.
	for attempts := 0; attempts < 3; attempts++ {
		current, err := p.store.Job(ctx, job.ID)
		if err != nil {
			return job, err
		}
		if current.RemoteID != "" && current.RemoteID != receipt.Reference.RemoteID {
			return current, ErrPublicationReceipt
		}
		if current.Recovery() == "terminal" {
			return current, nil
		}
		if receipt.State == "cancelled" && receipt.Reference.RemoteID == "" {
			if !current.CancelRequested || current.RemoteID != "" {
				return current, ErrPublicationReceipt
			}
			stopped, stopErr := p.store.FinishJob(ctx, current.ID, current.Revision, "cancelled", nil)
			if errors.Is(stopErr, pluginhost.ErrRevision) {
				continue
			}
			return stopped, stopErr
		}
		if receipt.State == "failed" && receipt.Reference.RemoteID == "" {
			// An adapter can prove a rejection before a remote resource exists.
			// A network timeout must still be returned as unknown.
			failed, failErr := p.store.FailJob(ctx, current.ID, current.Revision, mustJSON(receipt))
			if errors.Is(failErr, pluginhost.ErrRevision) {
				continue
			}
			return failed, failErr
		}
		if current.RemoteID == "" {
			ack, err := p.store.AcknowledgeJob(ctx, current.ID, current.Revision, receipt.Reference.RemoteID)
			if errors.Is(err, pluginhost.ErrRevision) {
				continue
			}
			if err != nil {
				return current, err
			}
			current = ack
		}
		if receipt.State == "accepted" {
			return current, nil
		}
		var settled pluginhost.Job
		switch receipt.State {
		case "published":
			settled, err = p.store.CompleteJob(ctx, current.ID, current.Revision, nil, mustJSON(receipt))
		case "failed":
			settled, err = p.store.FailJob(ctx, current.ID, current.Revision, mustJSON(receipt))
		case "cancelled":
			settled, err = p.store.FinishJob(ctx, current.ID, current.Revision, "cancelled", nil)
		}
		if errors.Is(err, pluginhost.ErrRevision) {
			continue
		}
		if err != nil {
			return current, err
		}
		return settled, nil
	}
	return job, ErrPublicationConflict
}

func (p *Publisher) providerFor(ctx context.Context, target ConnectionRef) (Registration, publicationAuthorizer, error) {
	entry, ok := p.registry.Lookup(target.AppID)
	if !ok || entry.InstallationID != target.InstallationID {
		return Registration{}, nil, ErrPublicationUnavailable
	}
	provider, ok := publicationAdapter(entry.Provider)
	if !ok {
		return Registration{}, nil, ErrPublicationUnavailable
	}
	// A persisted target must still resolve to the same app account. This
	// prevents a reconfigured adapter from using a new account's credentials to
	// query or publish an old task. The grant itself is deliberately not read
	// here: revoked grants must still permit reconciliation.
	connections, err := entry.Provider.Connections(ctx)
	if err != nil {
		return Registration{}, nil, ErrPublicationUnavailable
	}
	for _, connection := range connections {
		if connection.ID == target.ConnectionID && connection.AccountID == target.AccountID {
			return entry, provider, nil
		}
	}
	return Registration{}, nil, ErrPublicationUnavailable
}

func (p *Publisher) draftContract(ctx context.Context, target ConnectionRef) (string, string, string, string, error) {
	if isLocal(target) {
		return "local", "1", "local-draft-v1", "local", nil
	}
	entry, provider, err := p.providerFor(ctx, target)
	if err != nil {
		return "", "", "", "", err
	}
	// Resolve before persisting so an input cannot forge AccountID or
	// InstallationID. Drafts may exist before grant is enabled.
	resolved, err := p.ResolveTarget(ctx, target.AppID, target.ConnectionID)
	if err != nil || resolved != target {
		return "", "", "", "", ErrPublicationInvalidDraft
	}
	grant, err := provider.PublicationGrant(ctx, target)
	revision := "unauthorised"
	if err == nil && grant.Allowed && grant.Version != "" {
		revision = grant.Version
	}
	return entry.ID, entry.Registration, publicationDigest(entry, target), revision, nil
}

func (p *Publisher) openAllowed(submitCtx context.Context, active *atomic.Bool, jobID string, record publicationRecord) func(context.Context, PublicationArtifact) (io.ReadCloser, error) {
	draft := record.Draft
	allowed := make(map[string]PublicationArtifact, len(draft.Artifacts))
	for _, artifact := range draft.Artifacts {
		allowed[artifact.ID] = artifact
	}
	return func(ctx context.Context, requested PublicationArtifact) (io.ReadCloser, error) {
		if active == nil || !active.Load() || submitCtx.Err() != nil {
			return nil, ErrPublicationDenied
		}
		artifact, ok := allowed[requested.ID]
		if !ok || artifact != requested || p.open == nil {
			return nil, ErrPublicationInvalidDraft
		}
		// Adapters may defer opening an artifact. Re-check the durable task and
		// the exact grant at that point so cancellation/revocation prevents a
		// slow provider from starting a later upload.
		job, err := p.store.Job(ctx, jobID)
		if err != nil || job.CancelRequested || job.State != "submitting" {
			return nil, ErrPublicationDenied
		}
		_, provider, err := p.providerFor(ctx, draft.Target)
		if err != nil {
			return nil, ErrPublicationUnavailable
		}
		grant, err := provider.PublicationGrant(ctx, draft.Target)
		if err != nil || !grant.Allowed || grant.Version != job.ConfigRevision {
			return nil, ErrPublicationDenied
		}
		if p.validate != nil && p.validate(ctx, job, draft) != nil {
			return nil, ErrPublicationInvalidDraft
		}
		return p.open(ctx, artifact)
	}
}

func publicationDigest(entry Registration, target ConnectionRef) string {
	payload, _ := json.Marshal(struct {
		Registration string        `json:"registration"`
		Target       ConnectionRef `json:"target"`
	}{entry.Registration, target})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func referenceFor(job pluginhost.Job, record publicationRecord) PublicationReference {
	return PublicationReference{TaskID: job.ID, Target: record.Draft.Target, ManifestSHA256: record.Manifest, RemoteID: job.RemoteID}
}

func publicationManifest(draft PublicationDraft) string {
	origin := canonicalJSON(draft.Origin)
	payload, _ := json.Marshal(struct {
		Target    ConnectionRef         `json:"target"`
		Title     string                `json:"title"`
		Body      string                `json:"body"`
		Project   *PublicationArtifact  `json:"project,omitempty"`
		Artifacts []PublicationArtifact `json:"artifacts,omitempty"`
		Options   json.RawMessage       `json:"platform_options,omitempty"`
		Origin    json.RawMessage       `json:"origin"`
	}{draft.Target, draft.Title, draft.Body, draft.Project, draft.Artifacts, canonicalJSON(draft.Options), origin})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func publicationFromJob(job pluginhost.Job) (publicationRecord, error) {
	var record publicationRecord
	if job.Tool != "publication" || !json.Valid(job.Arguments) || json.Unmarshal(job.Arguments, &record) != nil || !validDraft(record.Draft) {
		return publicationRecord{}, ErrPublicationConflict
	}
	expectedPlugin := "application/" + record.Draft.Target.AppID
	if isLocal(record.Draft.Target) {
		expectedPlugin = "application/local"
	}
	if job.Plugin != expectedPlugin || record.Manifest == "" || record.Manifest != publicationManifest(record.Draft) {
		return publicationRecord{}, ErrPublicationConflict
	}
	return record, nil
}

func validDraft(draft PublicationDraft) bool {
	if len(draft.Title) > 16<<10 || len(draft.Body) > 128<<10 || len(draft.Origin) == 0 || !json.Valid(draft.Origin) || !jsonObject(draft.Origin) {
		return false
	}
	if len(draft.Options) != 0 && (isLocal(draft.Target) || len(draft.Options) > 16<<10 || !jsonObject(draft.Options)) {
		return false
	}
	if !isLocal(draft.Target) && (draft.Target.AppID == "" || draft.Target.ConnectionID == "" || draft.Target.InstallationID == "" || draft.Target.AccountID == "") {
		return false
	}
	seen := map[string]bool{}
	for _, artifact := range draft.Artifacts {
		if !validArtifact(artifact) || seen[artifact.ID] {
			return false
		}
		seen[artifact.ID] = true
	}
	if draft.Project != nil && !validArtifact(*draft.Project) {
		return false
	}
	return draft.Title != "" || draft.Body != "" || draft.Project != nil || len(draft.Artifacts) > 0
}

func validArtifact(artifact PublicationArtifact) bool {
	if strings.TrimSpace(artifact.ID) == "" || len(artifact.ID) > 512 || strings.TrimSpace(artifact.Name) == "" || len(artifact.Name) > 1024 || artifact.Bytes < 0 || len(artifact.SHA256) != 64 {
		return false
	}
	for _, r := range artifact.SHA256 {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validReceipt(job pluginhost.Job, record publicationRecord, receipt PublicationReceipt) bool {
	if receipt.Reference.TaskID != job.ID || receipt.Reference.Target != record.Draft.Target || receipt.Reference.ManifestSHA256 != record.Manifest || !validReceiptState(receipt.State) || !validRemoteID(receipt.Reference.RemoteID) {
		return false
	}
	if (receipt.State == "accepted" || receipt.State == "published") && receipt.Reference.RemoteID == "" {
		return false
	}
	return receipt.Progress == nil || validPublicationProgress(receipt.Progress)
}

func validPublicationProgress(progress *PublicationProgress) bool {
	if progress == nil || progress.ConfirmedParts < 0 || progress.TotalParts < progress.ConfirmedParts {
		return false
	}
	if len(progress.ErrorCode) > 64 {
		return false
	}
	for _, ch := range progress.ErrorCode {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_') {
			return false
		}
	}
	switch progress.Stage {
	case "initializing", "uploading", "part_pending", "merging", "merged", "posting", "accepted", "rejected":
		return true
	case "opening_creator", "selecting_file", "editing", "verifying_form":
		return true
	default:
		return false
	}
}

func viewWithProgress(job pluginhost.Job, record publicationRecord, receipt PublicationReceipt) PublicationView {
	view := viewFor(job, record)
	view.Progress = receipt.Progress
	return view
}

func validReceiptState(state string) bool {
	return state == "unknown" || state == "accepted" || state == "published" || state == "failed" || state == "cancelled"
}

func validRemoteID(id string) bool {
	if len(id) > 1024 {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func viewFor(job pluginhost.Job, record publicationRecord) PublicationView {
	view := PublicationView{JobID: job.ID, State: publicationState(job, record), CancelRequested: job.CancelRequested, Target: record.Draft.Target, ManifestSHA256: record.Manifest, RemoteID: job.RemoteID, ArtifactIDs: make([]string, 0, len(record.Draft.Artifacts)), UpdatedAt: job.Updated, AvailableActions: []string{}}
	if record.Draft.Project != nil {
		view.ProjectID = record.Draft.Project.ID
	}
	for _, artifact := range record.Draft.Artifacts {
		view.ArtifactIDs = append(view.ArtifactIDs, artifact.ID)
	}
	if job.Diagnostic != nil {
		view.Diagnostic = job.Diagnostic.Code
	}
	return view
}

func publicationState(job pluginhost.Job, record publicationRecord) string {
	switch job.State {
	case "pending":
		if isLocal(record.Draft.Target) {
			return "local_draft"
		}
		return "draft"
	case "submitting", "needs_review":
		return "awaiting_receipt"
	case "running":
		return "accepted"
	case "completed":
		return "published"
	case "cancelled":
		return "cancelled"
	case "failed":
		return "failed"
	default:
		return "awaiting_receipt"
	}
}

func isLocal(target ConnectionRef) bool { return target == (ConnectionRef{}) }
func sameActor(a, b pluginhost.Invocation) bool {
	return a.Account == b.Account && a.Thread == b.Thread && a.Requester == b.Requester
}
func jsonObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}
func cloneDraft(draft PublicationDraft) PublicationDraft {
	raw, _ := json.Marshal(draft)
	var clone PublicationDraft
	_ = json.Unmarshal(raw, &clone)
	return clone
}
func mustJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

func canonicalJSON(raw json.RawMessage) json.RawMessage {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	canonical, _ := json.Marshal(value)
	return canonical
}
