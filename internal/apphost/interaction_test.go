package apphost

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"wechat-bot/internal/pluginhost"
)

type interactionFixtureProvider struct {
	mu             sync.Mutex
	grant          PublicationGrant
	calls, queries int
	receipt        InteractionReceipt
	checkpoint     bool
	lateSave       func(context.Context, json.RawMessage) error
	lateActive     func(context.Context) error
	grants         []InteractionDraft
	performed      []InteractionDraft
	queried        []InteractionDraft
	performErr     error
	queryErr       error
}

func (p *interactionFixtureProvider) Connections(context.Context) ([]Connection, error) {
	return []Connection{{ID: "account-1", AccountID: "owner"}}, nil
}
func (p *interactionFixtureProvider) Inspect(context.Context, string) (ConnectionStatus, error) {
	return ConnectionStatus{}, nil
}
func (p *interactionFixtureProvider) GrantInteraction(_ context.Context, draft InteractionDraft) (PublicationGrant, error) {
	p.mu.Lock()
	p.grants = append(p.grants, draft)
	p.mu.Unlock()
	return p.grant, nil
}
func (p *interactionFixtureProvider) PerformInteraction(ctx context.Context, r InteractionRequest) (InteractionReceipt, error) {
	p.mu.Lock()
	p.calls++
	p.performed = append(p.performed, r.Draft)
	p.mu.Unlock()
	p.lateSave, p.lateActive = r.SaveCheckpoint, r.CheckActive
	if p.checkpoint {
		if err := r.SaveCheckpoint(ctx, json.RawMessage(`{"submitted":true}`)); err != nil {
			return InteractionReceipt{}, err
		}
	}
	if p.performErr != nil {
		return InteractionReceipt{}, p.performErr
	}
	if p.receipt.State != "" {
		receipt := p.receipt
		if receipt.Reference.TaskID == "" {
			receipt.Reference = r.Reference
		}
		return receipt, nil
	}
	return InteractionReceipt{Reference: r.Reference, State: "unknown"}, nil
}
func (p *interactionFixtureProvider) QueryInteraction(_ context.Context, q InteractionQuery) (InteractionReceipt, error) {
	p.mu.Lock()
	p.queries++
	p.queried = append(p.queried, q.Draft)
	p.mu.Unlock()
	if p.queryErr != nil {
		return InteractionReceipt{}, p.queryErr
	}
	if p.receipt.State != "" {
		r := p.receipt
		r.Reference = q.Reference
		if r.State == "applied" {
			r.Reference.RemoteID = "remote-1"
		}
		return r, nil
	}
	return InteractionReceipt{Reference: q.Reference, State: "unknown"}, nil
}

func TestInteractionProviderNeverReceivesOriginButHostValidationDoes(t *testing.T) {
	provider := &interactionFixtureProvider{grant: PublicationGrant{Allowed: true, Version: "g1"}, checkpoint: true}
	reg, err := New(Registration{Application: Application{ID: "social", Name: "social", InstallationID: "social-install", Registration: "v1"}, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	store, err := pluginhost.OpenStore(t.TempDir(), "fish")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seenOrigin := false
	p := NewInteractor(reg, store, func(_ context.Context, _ pluginhost.Job, draft InteractionDraft) error {
		seenOrigin = len(draft.Origin) != 0
		return nil
	})
	target := ConnectionRef{AppID: "social", InstallationID: "social-install", ConnectionID: "account-1", AccountID: "owner"}
	actor := interactionActor("origin-private")
	v, err := p.Create(t.Context(), actor, interactionDraft(target))
	if err != nil {
		t.Fatal(err)
	}
	if !seenOrigin {
		t.Fatal("host validation lost origin")
	}
	if _, err = p.Action(t.Context(), actor, v.JobID, "submit"); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Action(t.Context(), actor, v.JobID, "status"); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	for _, drafts := range [][]InteractionDraft{provider.grants, provider.performed, provider.queried} {
		if len(drafts) == 0 {
			t.Fatal("expected provider path was not reached")
		}
		for _, draft := range drafts {
			if len(draft.Origin) != 0 {
				t.Fatalf("provider received origin: %s", draft.Origin)
			}
		}
	}
}

func interactionFixture(t *testing.T) (*Interactor, *interactionFixtureProvider, *pluginhost.Store, ConnectionRef) {
	t.Helper()
	provider := &interactionFixtureProvider{grant: PublicationGrant{Allowed: true, Version: "g1"}, checkpoint: true}
	reg, err := New(Registration{Application: Application{ID: "social", Name: "social", InstallationID: "social-install", Registration: "v1"}, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	store, err := pluginhost.OpenStore(t.TempDir(), "fish")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return NewInteractor(reg, store, nil), provider, store, ConnectionRef{AppID: "social", InstallationID: "social-install", ConnectionID: "account-1", AccountID: "owner"}
}
func interactionActor(key string) pluginhost.Invocation {
	return pluginhost.Invocation{Account: "fish", Thread: "thread", Requester: "owner", Purpose: "interact", RequestKey: key, SourceVersions: map[string]int{"m": 1}}
}
func interactionDraft(t ConnectionRef) InteractionDraft {
	return InteractionDraft{Target: t, Kind: "like", ContentID: "video", Desired: true, Origin: json.RawMessage(`{"message":"m"}`)}
}

func TestInteractionDeniedCreateDoesNotPoisonRequestKey(t *testing.T) {
	p, provider, store, target := interactionFixture(t)
	actor := interactionActor("enable-after-denial")
	provider.grant = PublicationGrant{Allowed: false}
	if _, err := p.Create(t.Context(), actor, interactionDraft(target)); err != ErrInteractionDenied {
		t.Fatal("unauthorized task created", err)
	}
	page, err := store.Jobs(t.Context(), "application/social", 0, 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("unusable task was retained", err)
	}
	provider.grant = PublicationGrant{Allowed: true, Version: "g1"}
	if _, err := p.Create(t.Context(), actor, interactionDraft(target)); err != nil {
		t.Fatal("request could not recover after enable", err)
	}
}

func TestInteractionUnknownNeverDoubleSubmitsAndStatusOnlyQueries(t *testing.T) {
	p, provider, store, target := interactionFixture(t)
	actor := interactionActor("one")
	v, err := p.Create(t.Context(), actor, interactionDraft(target))
	if err != nil {
		t.Fatal(err)
	}
	v, err = p.Action(t.Context(), actor, v.JobID, "submit")
	if err != nil || v.State != "unknown" {
		t.Fatalf("submit=%+v %v", v, err)
	}
	if _, err = p.Action(t.Context(), actor, v.JobID, "status"); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || provider.queries != 1 {
		t.Fatalf("calls=%d queries=%d", provider.calls, provider.queries)
	}
	if _, err = p.Action(t.Context(), actor, v.JobID, "submit"); err == nil {
		t.Fatal("second submit accepted")
	}
	job, _ := store.Job(t.Context(), v.JobID)
	if job.State != "submitting" {
		t.Fatal(job.State)
	}
}
func TestInteractionCancelPersistsBeforeQueryAndLateAppliedWins(t *testing.T) {
	p, provider, store, target := interactionFixture(t)
	actor := interactionActor("two")
	v, _ := p.Create(t.Context(), actor, interactionDraft(target))
	v, _ = p.Action(t.Context(), actor, v.JobID, "submit")
	provider.receipt = InteractionReceipt{State: "applied"}
	v, err := p.Action(t.Context(), actor, v.JobID, "cancel")
	if err != nil || v.State != "applied" || v.RemoteID != "remote-1" {
		t.Fatalf("cancel=%+v %v", v, err)
	}
	job, _ := store.Job(t.Context(), v.JobID)
	if !job.CancelRequested {
		t.Fatal("cancel intent lost")
	}
}
func TestInteractionRejectsOwnershipReceiptAndRebinding(t *testing.T) {
	p, provider, _, target := interactionFixture(t)
	actor := interactionActor("three")
	v, _ := p.Create(t.Context(), actor, interactionDraft(target))
	other := interactionActor("other")
	other.Requester = "another-owner"
	if _, err := p.Action(t.Context(), other, v.JobID, "submit"); err != ErrInteractionOwnership {
		t.Fatalf("ownership %v", err)
	}
	provider.receipt = InteractionReceipt{State: "applied", Reference: InteractionReference{TaskID: "forged", Target: target, ManifestSHA256: "bad", RemoteID: "remote"}}
	_, err := p.Action(t.Context(), actor, v.JobID, "submit")
	if err != ErrInteractionReceipt {
		t.Fatalf("receipt %v", err)
	}
}

func TestInteractionQueryRejectsCorruptCheckpoint(t *testing.T) {
	p, provider, store, target := interactionFixture(t)
	actor := interactionActor("corrupt")
	v, err := p.Create(t.Context(), actor, interactionDraft(target))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.InitializeHostState(t.Context(), "application-social", v.JobID, strings.Repeat("a", 64), map[string]json.RawMessage{interactionCheckpointKey: json.RawMessage(`{"submitted":true}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Action(t.Context(), actor, v.JobID, "status"); err != ErrInteractionConflict {
		t.Fatalf("status err=%v", err)
	}
	if provider.queries != 0 {
		t.Fatal("provider queried with corrupt checkpoint")
	}
}

func TestInteractionCallbacksExpireAfterPerform(t *testing.T) {
	p, provider, _, target := interactionFixture(t)
	actor := interactionActor("late-callback")
	v, err := p.Create(t.Context(), actor, interactionDraft(target))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Action(t.Context(), actor, v.JobID, "submit"); err != nil {
		t.Fatal(err)
	}
	if provider.lateSave == nil || provider.lateActive == nil {
		t.Fatal("callbacks missing")
	}
	if err = provider.lateSave(t.Context(), json.RawMessage(`{"submitted":true,"late":true}`)); err != ErrInteractionConflict {
		t.Fatalf("late save=%v", err)
	}
	if err = provider.lateActive(t.Context()); err != ErrInteractionDenied {
		t.Fatalf("late active=%v", err)
	}
}

func TestInteractionRepeatedCancelAndKnownRemoteUnknown(t *testing.T) {
	p, provider, store, target := interactionFixture(t)
	actor := interactionActor("cancel-twice")
	v, _ := p.Create(t.Context(), actor, interactionDraft(target))
	if _, err := p.Action(t.Context(), actor, v.JobID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Action(t.Context(), actor, v.JobID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if provider.queries != 0 {
		t.Fatal("terminal cancel queried provider")
	}

	v, _ = p.Create(t.Context(), interactionActor("known-unknown"), interactionDraft(target))
	job, _ := store.Job(t.Context(), v.JobID)
	job, _ = store.ClaimJobWithSubmission(t.Context(), job.ID, job.Revision, json.RawMessage(`{}`))
	job, _ = store.AcknowledgeJob(t.Context(), job.ID, job.Revision, "remote-known")
	provider.receipt = InteractionReceipt{State: "unknown"}
	result, err := p.Action(t.Context(), interactionActor("known-unknown"), v.JobID, "status")
	if err != nil || result.RemoteID != "remote-known" || result.State != "in_progress" {
		t.Fatalf("known unknown=%+v %v", result, err)
	}
}

func TestInteractionDiagnosticPersistsWithoutProviderText(t *testing.T) {
	p, provider, _, target := interactionFixture(t)
	actor := interactionActor("diagnostic")
	v, _ := p.Create(t.Context(), actor, interactionDraft(target))
	provider.receipt = InteractionReceipt{State: "unknown", Code: "rate_limited"}
	v, err := p.Action(t.Context(), actor, v.JobID, "submit")
	if err != nil || v.Diagnostic != "rate_limited" {
		t.Fatalf("diagnostic=%+v %v", v, err)
	}
	page, err := p.List(t.Context(), actor, "social", 0, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Diagnostic != "rate_limited" {
		t.Fatalf("list=%+v %v", page, err)
	}
}

func TestInteractionProviderFailureRemainsUnknownAndIsReported(t *testing.T) {
	for _, action := range []string{"status", "cancel"} {
		t.Run(action, func(t *testing.T) {
			p, provider, store, target := interactionFixture(t)
			actor := interactionActor("provider-failure-" + action)
			v, err := p.Create(t.Context(), actor, interactionDraft(target))
			if err != nil {
				t.Fatal(err)
			}
			provider.performErr = context.DeadlineExceeded
			v, err = p.Action(t.Context(), actor, v.JobID, "submit")
			if err != context.DeadlineExceeded || v.State != "unknown" {
				t.Fatalf("submit: %+v %v", v, err)
			}
			provider.queryErr = ErrInteractionDenied
			v, err = p.Action(t.Context(), actor, v.JobID, action)
			if err != ErrInteractionDenied || v.State != "unknown" {
				t.Fatalf("query: %+v %v", v, err)
			}
			job, err := store.Job(t.Context(), v.JobID)
			if err != nil || job.State != "submitting" || job.CancelRequested != (action == "cancel") {
				t.Fatalf("failure lost original state: %+v %v", job, err)
			}
			if _, err := p.Action(t.Context(), actor, job.ID, "submit"); err != ErrInteractionConflict {
				t.Fatal("uncertain effect resubmitted", err)
			}
			provider.queryErr = nil
			provider.receipt = InteractionReceipt{State: "applied"}
			v, err = p.Action(t.Context(), actor, job.ID, "status")
			if err != nil || v.State != "applied" || v.RemoteID != "remote-1" || provider.calls != 1 {
				t.Fatalf("late receipt: %+v %v, calls %d", v, err, provider.calls)
			}
		})
	}
}
