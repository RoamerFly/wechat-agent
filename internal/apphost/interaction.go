package apphost

// Durable, provider-neutral social interactions.  This deliberately uses the
// trusted application job ledger: an uncertain click is queried, never clicked
// again under a new task id.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wechat-bot/internal/pluginhost"
)

var (
	ErrInteractionDenied      = errors.New("interaction is not authorised")
	ErrInteractionUnavailable = errors.New("interaction application is unavailable")
	ErrInteractionConflict    = errors.New("interaction task is not ready for this action")
	ErrInteractionInvalid     = errors.New("invalid interaction draft")
	ErrInteractionReceipt     = errors.New("interaction receipt could not be verified")
	ErrInteractionOwnership   = errors.New("interaction task is outside this actor")
)

type InteractionDraft struct {
	Target    ConnectionRef `json:"target"`
	Kind      string        `json:"kind"`
	ContentID string        `json:"content_id"`
	CommentID string        `json:"comment_id,omitempty"`
	// CommentVersion is issued by the host after reading the comment.  It
	// prevents a reply approved against one comment revision from targeting a
	// changed one.
	CommentVersion string          `json:"comment_version,omitempty"`
	Text           string          `json:"text,omitempty"`
	Desired        bool            `json:"desired"`
	Origin         json.RawMessage `json:"origin"`
}

type InteractionReference struct {
	TaskID         string        `json:"task_id"`
	Target         ConnectionRef `json:"target"`
	ManifestSHA256 string        `json:"manifest_sha256"`
	RemoteID       string        `json:"remote_id,omitempty"`
}
type InteractionRequest struct {
	Reference      InteractionReference                         `json:"reference"`
	Draft          InteractionDraft                             `json:"draft"`
	Checkpoint     json.RawMessage                              `json:"-"`
	CheckActive    func(context.Context) error                  `json:"-"`
	SaveCheckpoint func(context.Context, json.RawMessage) error `json:"-"`
}
type InteractionQuery struct {
	Reference  InteractionReference `json:"reference"`
	Draft      InteractionDraft     `json:"draft"`
	Checkpoint json.RawMessage      `json:"-"`
	Cancel     bool                 `json:"cancel"`
}
type InteractionReceipt struct {
	Reference InteractionReference `json:"reference"`
	State     string               `json:"state"`
	Code      string               `json:"code,omitempty"`
}

// InteractionProvider is optional on an application adapter.  Origin never
// leaves the host; adapters receive a cloned draft with it cleared.
// Providers are trusted adapters. Before an external effect they must save a
// submitted checkpoint, abort on any save error, and re-check CheckActive at
// the effect boundary. A checkpoint that records submission permits Query
// only. These rules also apply to concurrent resume calls; checkpoint CAS
// admits only one preparation/submit sequence for a given stored revision.
type InteractionProvider interface {
	GrantInteraction(context.Context, InteractionDraft) (PublicationGrant, error)
	PerformInteraction(context.Context, InteractionRequest) (InteractionReceipt, error)
	QueryInteraction(context.Context, InteractionQuery) (InteractionReceipt, error)
}

type interactionRecord struct {
	Draft    InteractionDraft `json:"draft"`
	Manifest string           `json:"manifest_sha256"`
}
type InteractionView struct {
	JobID            string        `json:"job_id"`
	State            string        `json:"state"`
	CancelRequested  bool          `json:"cancel_requested"`
	Target           ConnectionRef `json:"target"`
	Kind             string        `json:"kind"`
	Desired          *bool         `json:"desired,omitempty"`
	ContentID        string        `json:"content_id"`
	CommentID        string        `json:"comment_id,omitempty"`
	RemoteID         string        `json:"remote_id,omitempty"`
	UpdatedAt        string        `json:"updated_at"`
	AvailableActions []string      `json:"available_actions"`
	Diagnostic       string        `json:"diagnostic,omitempty"`
}
type InteractionPage struct {
	Items   []InteractionView `json:"items"`
	Next    int64             `json:"next"`
	HasMore bool              `json:"has_more"`
	Limit   int               `json:"limit"`
}
type Interactor struct {
	registry Registry
	store    *pluginhost.Store
	validate func(context.Context, pluginhost.Job, InteractionDraft) error
}

func NewInteractor(registry Registry, store *pluginhost.Store, validate func(context.Context, pluginhost.Job, InteractionDraft) error) *Interactor {
	return &Interactor{registry: registry, store: store, validate: validate}
}

func (p *Interactor) ResolveTarget(ctx context.Context, appID, connectionID string) (ConnectionRef, error) {
	entry, ok := p.registry.Lookup(appID)
	if !ok {
		return ConnectionRef{}, ErrInteractionUnavailable
	}
	connections, err := entry.Provider.Connections(ctx)
	if err != nil {
		return ConnectionRef{}, ErrInteractionUnavailable
	}
	for _, c := range connections {
		if c.ID == connectionID {
			return ConnectionRef{AppID: entry.ID, InstallationID: entry.InstallationID, ConnectionID: c.ID, AccountID: c.AccountID}, nil
		}
	}
	return ConnectionRef{}, ErrNotFound
}
func (p *Interactor) Create(ctx context.Context, actor pluginhost.Invocation, draft InteractionDraft) (InteractionView, error) {
	if p == nil || p.store == nil || !validInteractionDraft(draft) {
		return InteractionView{}, ErrInteractionInvalid
	}
	entry, provider, err := p.providerFor(ctx, draft.Target)
	if err != nil {
		return InteractionView{}, err
	}
	resolved, err := p.ResolveTarget(ctx, draft.Target.AppID, draft.Target.ConnectionID)
	if err != nil || resolved != draft.Target {
		return InteractionView{}, ErrInteractionInvalid
	}
	grant, err := provider.GrantInteraction(ctx, providerInteractionDraft(draft))
	if err != nil || !grant.Allowed || grant.Version == "" {
		return InteractionView{}, ErrInteractionDenied
	}
	revision := grant.Version
	record := interactionRecord{Draft: cloneInteractionDraft(draft)}
	record.Manifest = interactionManifest(record.Draft)
	args, _ := json.Marshal(record)
	// Application jobs are keyed by plugin+thread+request key.  Keep the
	// interaction ledger disjoint from publication and other application work
	// originating from the same inbound message.
	jobActor := actor
	jobActor.RequestKey = "interaction/" + actor.RequestKey
	candidate := pluginhost.Job{Version: entry.Registration, PackageDigest: interactionDigest(entry, draft.Target), ConfigRevision: revision, Tool: "interaction", Context: actor, Arguments: args}
	candidate.Context = jobActor
	if p.validate != nil && p.validate(ctx, candidate, record.Draft) != nil {
		return InteractionView{}, ErrInteractionInvalid
	}
	job, _, err := p.store.CreateApplicationJob(ctx, entry.ID, candidate)
	if err != nil {
		return InteractionView{}, err
	}
	return p.decorate(ctx, p.view(job, record), job, record), nil
}
func (p *Interactor) Action(ctx context.Context, actor pluginhost.Invocation, jobID, action string) (InteractionView, error) {
	if p == nil || p.store == nil {
		return InteractionView{}, ErrInteractionUnavailable
	}
	job, err := p.store.Job(ctx, jobID)
	if err != nil {
		return InteractionView{}, ErrNotFound
	}
	record, err := interactionFromJob(job)
	if err != nil {
		return InteractionView{}, ErrInteractionConflict
	}
	if !sameActor(actor, job.Context) {
		return InteractionView{}, ErrInteractionOwnership
	}
	if job.Recovery() == "terminal" && (action == "status" || action == "cancel") {
		return p.decorate(ctx, p.view(job, record), job, record), nil
	}
	var view InteractionView
	switch action {
	case "submit":
		view, err = p.submit(ctx, job, record)
	case "resume":
		view, err = p.resume(ctx, job, record)
	case "status":
		view, err = p.query(ctx, job, record, false)
	case "cancel":
		view, err = p.cancel(ctx, job, record)
	default:
		return InteractionView{}, ErrInteractionConflict
	}
	return p.decorate(ctx, view, job, record), err
}
func (p *Interactor) List(ctx context.Context, actor pluginhost.Invocation, appID string, after int64, limit int) (InteractionPage, error) {
	if appID == "" {
		appID = "local"
	}
	page, err := p.store.JobsForTool(ctx, "application/"+appID, "interaction", after, limit)
	if err != nil {
		return InteractionPage{}, err
	}
	out := InteractionPage{Items: []InteractionView{}, Next: page.Next, HasMore: page.HasMore, Limit: page.Limit}
	for _, job := range page.Items {
		if !sameActor(actor, job.Context) {
			continue
		}
		record, e := interactionFromJob(job)
		if e != nil {
			continue
		}
		out.Items = append(out.Items, p.decorate(ctx, p.view(job, record), job, record))
	}
	return out, nil
}
func (p *Interactor) submit(ctx context.Context, job pluginhost.Job, record interactionRecord) (InteractionView, error) {
	if job.State != "pending" || job.CancelRequested {
		return p.view(job, record), ErrInteractionConflict
	}
	entry, provider, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return p.view(job, record), err
	}
	g, e := provider.GrantInteraction(ctx, providerInteractionDraft(record.Draft))
	if e != nil || !g.Allowed || g.Version == "" || job.Version != entry.Registration || job.PackageDigest != interactionDigest(entry, record.Draft.Target) || job.ConfigRevision != g.Version {
		return p.view(job, record), ErrInteractionDenied
	}
	if p.validate != nil && p.validate(ctx, job, record.Draft) != nil {
		return p.view(job, record), ErrInteractionInvalid
	}
	claimed, err := p.store.ClaimJobWithSubmission(ctx, job.ID, job.Revision, mustJSON(p.reference(job, record)))
	if err != nil {
		if fresh, e := p.store.Job(ctx, job.ID); e == nil {
			return p.view(fresh, record), nil
		}
		return p.view(job, record), ErrInteractionConflict
	}
	return p.perform(ctx, claimed, record, provider)
}
func (p *Interactor) resume(ctx context.Context, job pluginhost.Job, record interactionRecord) (InteractionView, error) {
	if job.State != "submitting" || job.CancelRequested || job.RemoteID != "" {
		return p.view(job, record), ErrInteractionConflict
	}
	_, provider, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return p.view(job, record), err
	}
	if err = p.checkActive(ctx, job.ID, record); err != nil {
		return p.view(job, record), err
	}
	return p.perform(ctx, job, record, provider)
}
func (p *Interactor) cancel(ctx context.Context, job pluginhost.Job, record interactionRecord) (InteractionView, error) {
	requested, err := p.store.RequestJobCancel(ctx, job.ID, job.Revision)
	if err != nil {
		// A concurrent cancel may have won. Re-read rather than claiming that
		// its persisted intent was lost.
		if fresh, readErr := p.store.Job(ctx, job.ID); readErr == nil && (fresh.CancelRequested || fresh.Recovery() == "terminal") {
			return p.view(fresh, record), nil
		}
		return p.view(job, record), ErrInteractionConflict
	}
	if requested.Recovery() == "terminal" {
		return p.view(requested, record), nil
	}
	return p.query(ctx, requested, record, true)
}
func (p *Interactor) query(ctx context.Context, job pluginhost.Job, record interactionRecord, cancel bool) (InteractionView, error) {
	_, provider, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return p.view(job, record), err
	}
	checkpoint, checkpointErr := p.checkpoint(ctx, job, record)
	if checkpointErr != nil {
		return p.view(job, record), checkpointErr
	}
	receipt, err := provider.QueryInteraction(ctx, InteractionQuery{Reference: p.reference(job, record), Draft: providerInteractionDraft(record.Draft), Checkpoint: checkpoint.Records[interactionCheckpointKey], Cancel: cancel})
	if err != nil {
		// A failed lookup is not a successful reconciliation. Preserve the
		// durable unknown task while letting callers explain the actual failure.
		return p.view(job, record), err
	}
	settled, err := p.settle(ctx, job, record, receipt)
	if err != nil {
		return p.view(job, record), err
	}
	return p.view(settled, record), nil
}

const (
	interactionCheckpointKey = "interaction"
	interactionDiagnosticKey = "diagnostic"
)

func (p *Interactor) perform(ctx context.Context, job pluginhost.Job, record interactionRecord, provider InteractionProvider) (InteractionView, error) {
	// Re-check at the durable submit boundary even if an adapter mistakenly
	// forgets to call CheckActive before its first click.
	if err := p.checkActive(ctx, job.ID, record); err != nil {
		return p.view(job, record), err
	}
	state, err := p.checkpoint(ctx, job, record)
	if err != nil {
		return p.view(job, record), err
	}
	rev := state.Revision
	var active atomic.Bool
	active.Store(true)
	defer active.Store(false)
	var checkpointMu sync.Mutex
	request := InteractionRequest{Reference: p.reference(job, record), Draft: providerInteractionDraft(record.Draft), Checkpoint: state.Records[interactionCheckpointKey]}
	request.CheckActive = func(c context.Context) error {
		if !active.Load() {
			return ErrInteractionDenied
		}
		return p.checkActive(c, job.ID, record)
	}
	request.SaveCheckpoint = func(c context.Context, raw json.RawMessage) error {
		if !active.Load() || !jsonObject(raw) {
			return ErrInteractionConflict
		}
		checkpointMu.Lock()
		defer checkpointMu.Unlock()
		if !active.Load() {
			return ErrInteractionConflict
		}
		puts := map[string]json.RawMessage{interactionCheckpointKey: raw}
		if rev == 0 {
			s, e := p.store.InitializeHostState(c, "application-"+record.Draft.Target.AppID, job.ID, record.Manifest, puts)
			if e == nil {
				rev = s.Revision
			}
			return e
		}
		next, e := p.store.UpdateHostState(c, "application-"+record.Draft.Target.AppID, job.ID, rev, puts, nil)
		if e == nil {
			rev = next
		}
		return e
	}
	receipt, err := provider.PerformInteraction(ctx, request)
	if err != nil {
		// The effect may already have begun. Report the failure without
		// converting this task to failed or creating a replacement submission.
		return p.view(job, record), err
	}
	settled, err := p.settle(context.WithoutCancel(ctx), job, record, receipt)
	if err != nil {
		return p.view(job, record), err
	}
	return p.view(settled, record), nil
}
func (p *Interactor) checkActive(ctx context.Context, id string, record interactionRecord) error {
	job, err := p.store.Job(ctx, id)
	if err != nil || job.State != "submitting" || job.CancelRequested {
		return ErrInteractionDenied
	}
	entry, provider, err := p.providerFor(ctx, record.Draft.Target)
	if err != nil {
		return err
	}
	g, err := provider.GrantInteraction(ctx, providerInteractionDraft(record.Draft))
	if err != nil || !g.Allowed || g.Version != job.ConfigRevision || job.Version != entry.Registration || job.PackageDigest != interactionDigest(entry, record.Draft.Target) {
		return ErrInteractionDenied
	}
	if p.validate != nil && p.validate(ctx, job, record.Draft) != nil {
		return ErrInteractionInvalid
	}
	return ctx.Err()
}
func (p *Interactor) checkpoint(ctx context.Context, job pluginhost.Job, record interactionRecord) (pluginhost.HostState, error) {
	state, err := p.store.ReadHostState(ctx, "application-"+record.Draft.Target.AppID, job.ID)
	if err != nil {
		return state, err
	}
	if state.Revision != 0 && (state.SourceHash != record.Manifest || (len(state.Records[interactionCheckpointKey]) != 0 && !jsonObject(state.Records[interactionCheckpointKey]))) {
		return pluginhost.HostState{}, ErrInteractionConflict
	}
	return state, nil
}
func (p *Interactor) settle(ctx context.Context, job pluginhost.Job, record interactionRecord, receipt InteractionReceipt) (pluginhost.Job, error) {
	if !validInteractionReceipt(job, record, receipt) {
		return job, ErrInteractionReceipt
	}
	if receipt.State == "unknown" {
		if err := p.storeDiagnostic(ctx, job, record, receipt.Code); err != nil {
			return job, err
		}
		return p.store.Job(ctx, job.ID)
	}
	if receipt.State == "applied" {
		if err := p.storeDiagnostic(ctx, job, record, ""); err != nil {
			return job, err
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		current, err := p.store.Job(ctx, job.ID)
		if err != nil {
			return job, err
		}
		if current.RemoteID != "" && current.RemoteID != receipt.Reference.RemoteID {
			return current, ErrInteractionReceipt
		}
		if current.Recovery() == "terminal" {
			return current, nil
		}
		if receipt.State == "cancelled" {
			if !current.CancelRequested || current.RemoteID != "" {
				return current, ErrInteractionReceipt
			}
			next, e := p.store.FinishJob(ctx, current.ID, current.Revision, "cancelled", nil)
			if errors.Is(e, pluginhost.ErrRevision) {
				continue
			}
			return next, e
		}
		if receipt.State == "failed" {
			if err := p.storeDiagnostic(ctx, current, record, receipt.Code); err != nil {
				return current, err
			}
			next, e := p.store.FailJob(ctx, current.ID, current.Revision, mustJSON(receipt))
			if errors.Is(e, pluginhost.ErrRevision) {
				continue
			}
			return next, e
		}
		if current.RemoteID == "" {
			next, e := p.store.AcknowledgeJob(ctx, current.ID, current.Revision, receipt.Reference.RemoteID)
			if errors.Is(e, pluginhost.ErrRevision) {
				continue
			}
			if e != nil {
				return current, e
			}
			current = next
		}
		next, e := p.store.CompleteJob(ctx, current.ID, current.Revision, nil, mustJSON(receipt))
		if errors.Is(e, pluginhost.ErrRevision) {
			continue
		}
		return next, e
	}
	return job, ErrInteractionConflict
}

// storeDiagnostic keeps only a provider-safe normalized code in the existing
// host state. It intentionally never records arbitrary provider error text.
func (p *Interactor) storeDiagnostic(ctx context.Context, job pluginhost.Job, record interactionRecord, code string) error {
	if !validInteractionCode(code) {
		return ErrInteractionReceipt
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	plugin := "application-" + record.Draft.Target.AppID
	for attempt := 0; attempt < 3; attempt++ {
		state, err := p.store.ReadHostState(ctx, plugin, job.ID)
		if err != nil {
			return err
		}
		if state.Revision != 0 && state.SourceHash != record.Manifest {
			return ErrInteractionConflict
		}
		if state.Revision == 0 {
			if code == "" {
				return nil
			}
			puts := map[string]json.RawMessage{}
			puts[interactionDiagnosticKey] = mustJSON(code)
			_, err = p.store.InitializeHostState(ctx, plugin, job.ID, record.Manifest, puts)
			if errors.Is(err, pluginhost.ErrRevision) {
				continue
			}
			return err
		}
		puts := map[string]json.RawMessage{}
		deletes := []string{}
		if code == "" {
			deletes = append(deletes, interactionDiagnosticKey)
		} else {
			puts[interactionDiagnosticKey] = mustJSON(code)
		}
		_, err = p.store.UpdateHostState(ctx, plugin, job.ID, state.Revision, puts, deletes)
		if errors.Is(err, pluginhost.ErrRevision) {
			continue
		}
		return err
	}
	return ErrInteractionConflict
}
func (p *Interactor) decorate(ctx context.Context, view InteractionView, job pluginhost.Job, record interactionRecord) InteractionView {
	if view.State != "unknown" && view.State != "failed" {
		return view
	}
	if view.State == "failed" && len(job.Result) != 0 {
		var receipt InteractionReceipt
		if json.Unmarshal(job.Result, &receipt) == nil && validInteractionCode(receipt.Code) {
			view.Diagnostic = receipt.Code
			return view
		}
	}
	state, err := p.checkpoint(ctx, job, record)
	if err != nil {
		return view
	}
	raw := state.Records[interactionDiagnosticKey]
	var code string
	if len(raw) != 0 && json.Unmarshal(raw, &code) == nil && validInteractionCode(code) {
		view.Diagnostic = code
	}
	return view
}
func (p *Interactor) providerFor(ctx context.Context, target ConnectionRef) (Registration, InteractionProvider, error) {
	entry, ok := p.registry.Lookup(target.AppID)
	if !ok || entry.InstallationID != target.InstallationID {
		return Registration{}, nil, ErrInteractionUnavailable
	}
	provider, ok := entry.Provider.(InteractionProvider)
	if !ok {
		return Registration{}, nil, ErrInteractionUnavailable
	}
	cs, err := entry.Provider.Connections(ctx)
	if err != nil {
		return Registration{}, nil, ErrInteractionUnavailable
	}
	for _, c := range cs {
		if c.ID == target.ConnectionID && c.AccountID == target.AccountID {
			return entry, provider, nil
		}
	}
	return Registration{}, nil, ErrInteractionUnavailable
}
func (p *Interactor) reference(job pluginhost.Job, record interactionRecord) InteractionReference {
	return InteractionReference{TaskID: job.ID, Target: record.Draft.Target, ManifestSHA256: record.Manifest, RemoteID: job.RemoteID}
}
func interactionDigest(entry Registration, target ConnectionRef) string {
	return publicationDigest(entry, target)
}
func interactionManifest(d InteractionDraft) string {
	raw, _ := json.Marshal(struct {
		Target                                           ConnectionRef `json:"target"`
		Kind, ContentID, CommentID, CommentVersion, Text string
		Desired                                          bool
		Origin                                           json.RawMessage
	}{d.Target, d.Kind, d.ContentID, d.CommentID, d.CommentVersion, d.Text, d.Desired, canonicalJSON(d.Origin)})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func interactionFromJob(job pluginhost.Job) (interactionRecord, error) {
	var r interactionRecord
	if job.Tool != "interaction" || json.Unmarshal(job.Arguments, &r) != nil || !validInteractionDraft(r.Draft) || r.Manifest == "" || r.Manifest != interactionManifest(r.Draft) || job.Plugin != "application/"+r.Draft.Target.AppID {
		return r, ErrInteractionConflict
	}
	return r, nil
}
func validInteractionDraft(d InteractionDraft) bool {
	if d.Target.AppID == "" || d.Target.InstallationID == "" || d.Target.ConnectionID == "" || d.Target.AccountID == "" || d.ContentID == "" || len(d.ContentID) > 1024 || len(d.Origin) == 0 || !jsonObject(d.Origin) || len(d.Text) > 64<<10 {
		return false
	}
	switch d.Kind {
	case "like":
		return d.CommentID == "" && d.CommentVersion == "" && d.Text == ""
	case "reply_comment":
		return d.CommentID != "" && d.CommentVersion != "" && strings.TrimSpace(d.Text) != ""
	default:
		return false
	}
}
func validInteractionReceipt(job pluginhost.Job, r interactionRecord, x InteractionReceipt) bool {
	if x.Reference.TaskID != job.ID || x.Reference.Target != r.Draft.Target || x.Reference.ManifestSHA256 != r.Manifest || !validRemoteID(x.Reference.RemoteID) || !validInteractionCode(x.Code) {
		return false
	}
	switch x.State {
	case "unknown":
		// An adapter which has already learned the provider identifier may keep
		// reporting unknown, but it may neither invent, change nor drop it.
		return x.Reference.RemoteID == job.RemoteID
	case "failed", "cancelled":
		return x.Reference.RemoteID == ""
	case "applied":
		return x.Reference.RemoteID != ""
	default:
		return false
	}
}
func validInteractionCode(code string) bool {
	if len(code) > 64 {
		return false
	}
	for _, r := range code {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}
func providerInteractionDraft(d InteractionDraft) InteractionDraft {
	d = cloneInteractionDraft(d)
	d.Origin = nil
	return d
}
func cloneInteractionDraft(d InteractionDraft) InteractionDraft {
	raw, _ := json.Marshal(d)
	var c InteractionDraft
	_ = json.Unmarshal(raw, &c)
	return c
}
func (p *Interactor) view(job pluginhost.Job, r interactionRecord) InteractionView {
	v := InteractionView{JobID: job.ID, State: interactionState(job), CancelRequested: job.CancelRequested, Target: r.Draft.Target, Kind: r.Draft.Kind, ContentID: r.Draft.ContentID, CommentID: r.Draft.CommentID, RemoteID: job.RemoteID, UpdatedAt: job.Updated, AvailableActions: []string{}}
	if r.Draft.Kind == "like" {
		desired := r.Draft.Desired
		v.Desired = &desired
	}
	if job.Diagnostic != nil {
		v.Diagnostic = job.Diagnostic.Code
	}
	switch v.State {
	case "pending":
		if !job.CancelRequested {
			v.AvailableActions = []string{"submit", "cancel"}
		}
	case "in_progress", "unknown":
		v.AvailableActions = []string{"status"}
		if !job.CancelRequested {
			v.AvailableActions = append(v.AvailableActions, "cancel")
		}
		if job.State == "submitting" && !job.CancelRequested && job.RemoteID == "" {
			v.AvailableActions = append(v.AvailableActions, "resume")
		}
	}
	return v
}
func interactionState(job pluginhost.Job) string {
	switch job.State {
	case "pending":
		return "pending"
	case "submitting", "needs_review":
		return "unknown"
	case "running":
		return "in_progress"
	case "completed":
		return "applied"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	default:
		return "unknown"
	}
}
