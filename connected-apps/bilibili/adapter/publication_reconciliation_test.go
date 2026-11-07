package adapter

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
)

func (f *fixturePublicationTransport) ListArchives(_ context.Context, in ArchiveListRequest) (ArchiveList, error) {
	if in.Page != 2 || in.PageSize != 50 || in.Status != "all" {
		f.t.Fatalf("candidate page changed: %+v", in)
	}
	created := int64(1730000000)
	return ArchiveList{Page: 2, PageSize: 50, NextPage: 3, Items: []ArchiveCandidate{{
		ResourceID: fixturePublicationResource, Title: "候选稿件", CreatedAt: &created,
	}}}, nil
}

func verifiedReconciliationFixture(t *testing.T) *biliPublicationFixture {
	t.Helper()
	f := newBiliPublicationFixture(t)
	f.app.verifyPublicationAccount = func(context.Context, OpenCredentials) (string, error) {
		return enrollmentOpenID, nil
	}
	start, _ := f.app.PublicationAuthorization()
	view, err := f.app.SavePublicationAuthorization(t.Context(), enrollmentInput(start.Revision, enrollmentCredentials(f.settings)))
	if err != nil || !view.Verified || !view.Enabled {
		t.Fatalf("verify fixture: %+v %v", view, err)
	}
	current, err := f.app.loadPublicationSettings()
	if err != nil {
		t.Fatal(err)
	}
	f.settings = current.publicationSettings
	return f
}

func TestPublicationOwnerReconciliationRemainsSeparateAfterRestart(t *testing.T) {
	f := verifiedReconciliationFixture(t)
	jobID := f.save()
	query := apphost.PublicationReconciliationQuery{Action: "verify", BVID: fixturePublicationResource}
	if _, err := f.publisher.ReconcileForOwner(t.Context(), f.actor, jobID, query); !errors.Is(err, apphost.ErrPublicationConflict) {
		t.Fatalf("draft unexpectedly admitted: %v", err)
	}
	f.provider.failPost = true
	f.action(jobID, "submit", "awaiting_receipt")
	f.action(jobID, "cancel", "awaiting_receipt")
	foreign := f.actor
	foreign.Requester = "other-owner"
	if _, err := f.publisher.ReconcileForOwner(t.Context(), foreign, jobID, query); !errors.Is(err, apphost.ErrPublicationOwnership) || f.provider.queries != 0 {
		t.Fatalf("foreign owner queried the archive: %v", err)
	}
	if _, err := f.publisher.ReconcileForOwner(t.Context(), f.actor, jobID, apphost.PublicationReconciliationQuery{Action: "verify", BVID: "https://example.test"}); !errors.Is(err, apphost.ErrPublicationReconciliationInput) || f.provider.queries != 0 {
		t.Fatalf("invalid BV reached the provider: %v", err)
	}
	candidates, err := f.publisher.ReconcileForOwner(t.Context(), f.actor, jobID, apphost.PublicationReconciliationQuery{Action: "candidates", Page: 2, PageSize: 50})
	if err != nil || candidates.JobID != jobID || candidates.Page != 2 || candidates.PageSize != 50 || !candidates.HasMore || candidates.NextPage != 3 ||
		len(candidates.Items) != 1 || candidates.Items[0].BVID != fixturePublicationResource || candidates.Items[0].CreatedAt != time.Unix(1730000000, 0).UTC().Format(time.RFC3339) ||
		candidates.Item != nil {
		t.Fatalf("invalid or implicitly verified candidate page: %+v %v", candidates, err)
	}
	f.provider.published = true
	out, err := f.publisher.ReconcileForOwner(t.Context(), f.actor, jobID, query)
	if err != nil || out.JobID != jobID || out.Item == nil || !out.Item.Published || out.Item.CheckedAt == "" {
		t.Fatalf("exact candidate was not checked: %+v %v", out, err)
	}
	f.restart()
	view := f.action(jobID, "status", "awaiting_receipt")
	if view.RemoteID != "" || !view.CancelRequested || f.provider.posts != 1 || f.provider.queries != 1 {
		t.Fatalf("candidate changed authoritative status: %+v", view)
	}
	ownerPage, err := f.publisher.ListForOwner(t.Context(), f.actor, ID, 0, 10)
	if err != nil || len(ownerPage.Items) != 1 || !slices.Contains(ownerPage.Items[0].AvailableActions, "reconcile") ||
		ownerPage.Items[0].Reconciliation == nil || *ownerPage.Items[0].Reconciliation != *out.Item {
		t.Fatalf("owner observation did not survive: %+v %v", ownerPage, err)
	}
	toolPage, err := f.publisher.List(t.Context(), f.actor, ID, 0, 10)
	if err != nil || len(toolPage.Items) != 1 || slices.Contains(toolPage.Items[0].AvailableActions, "reconcile") ||
		toolPage.Items[0].Reconciliation != nil {
		t.Fatalf("owner-only result exposed to the tool: %+v %v", toolPage, err)
	}
}

func TestPublicationOwnerReconciliationDoesNotRaceOriginalReceipt(t *testing.T) {
	f := verifiedReconciliationFixture(t)
	jobID := f.save()
	f.provider.onPost = func() {
		out, err := f.publisher.ReconcileForOwner(t.Context(), f.actor, jobID, apphost.PublicationReconciliationQuery{Action: "verify", BVID: fixturePublicationResource})
		if err != nil || out.Item == nil {
			t.Fatalf("read in-flight candidate: %+v %v", out, err)
		}
	}
	view := f.action(jobID, "submit", "accepted")
	if view.RemoteID != fixturePublicationResource || f.provider.posts != 1 || f.provider.queries != 1 {
		t.Fatalf("original receipt was lost: %+v", view)
	}
	page, err := f.publisher.ListForOwner(t.Context(), f.actor, ID, 0, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Progress == nil || page.Items[0].Progress.Stage != "accepted" ||
		page.Items[0].Reconciliation != nil || slices.Contains(page.Items[0].AvailableActions, "reconcile") {
		t.Fatalf("authoritative checkpoint was not persisted or candidate remained active: %+v %v", page, err)
	}
}

func TestPublicationOwnerReconciliationRechecksAuthorizationAfterRead(t *testing.T) {
	f := verifiedReconciliationFixture(t)
	jobID := f.save()
	f.provider.failPost = true
	f.action(jobID, "submit", "awaiting_receipt")
	f.provider.onView = func() {
		view, _ := f.app.PublicationAuthorization()
		_, err := f.app.SavePublicationAuthorization(t.Context(), PublicationAuthorizationInput{Action: "disable", Revision: view.Revision})
		if err != nil {
			t.Fatal(err)
		}
	}
	query := apphost.PublicationReconciliationQuery{Action: "verify", BVID: fixturePublicationResource}
	if _, err := f.publisher.ReconcileForOwner(t.Context(), f.actor, jobID, query); !errors.Is(err, apphost.ErrPublicationDenied) {
		t.Fatalf("read survived revocation: %v", err)
	}
	if _, err := f.publisher.ReconcileForOwner(t.Context(), f.actor, jobID, query); !errors.Is(err, apphost.ErrPublicationDenied) || f.provider.queries != 1 {
		t.Fatalf("revoked read contacted provider: %v", err)
	}
	page, err := f.publisher.ListForOwner(t.Context(), f.actor, ID, 0, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Reconciliation != nil {
		t.Fatalf("revoked result was stored: %+v %v", page, err)
	}
}
