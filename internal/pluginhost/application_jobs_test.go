package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func applicationJobFixture() Job {
	return Job{
		Version:        "adapter-v1",
		PackageDigest:  "authorization-v7",
		ConfigRevision: "connection-v3",
		Tool:           "publish_draft",
		Context: Invocation{
			Account: "fish-1", Thread: "local-owner", Requester: "local-owner",
			Purpose: "publish a verified local work", RequestKey: "draft-1",
			SourceVersions: map[string]int{"artifact": 4},
		},
		Arguments: json.RawMessage(`{"artifact_id":"work-synthetic","target":"draft"}`),
	}
}

func TestCreateApplicationJobUsesDurableHostLedgerWithoutPluginInstall(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir(), "fish-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	input := applicationJobFixture()
	first, created, err := store.CreateApplicationJob(ctx, "douyin", input)
	if err != nil || !created || first.Plugin != "application/douyin" || first.State != "pending" {
		t.Fatalf("first application job = %#v, created=%v, error=%v", first, created, err)
	}
	retry, created, err := store.CreateApplicationJob(ctx, "douyin", input)
	if err != nil || created || retry.ID != first.ID {
		t.Fatalf("idempotent application retry = %#v, created=%v, error=%v", retry, created, err)
	}
	input.ConfigRevision = "connection-v4"
	if _, _, err = store.CreateApplicationJob(ctx, "douyin", input); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed authorization version was accepted: %v", err)
	}
	input.ConfigRevision = "connection-v3"
	input.Context.Account = "other-fish"
	if _, _, err = store.CreateApplicationJob(ctx, "douyin", input); err == nil {
		t.Fatal("another fish account was accepted")
	}
	claimed, err := store.ClaimJob(ctx, first.ID, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	acknowledged, err := store.AcknowledgeJob(ctx, first.ID, claimed.Revision, "remote-draft-1")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.RequestJobCancel(ctx, first.ID, acknowledged.Revision)
	if err != nil || !cancelled.CancelRequested || cancelled.Recovery() != "query" {
		t.Fatalf("application cancellation was not retained: %#v, %v", cancelled, err)
	}
	page, err := store.Jobs(ctx, "application/douyin", 0, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first.ID {
		t.Fatalf("application page = %#v, %v", page, err)
	}
}

func TestJobsForToolFiltersBeforePagingAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(dir, "fish-1")
	if err != nil {
		t.Fatal(err)
	}
	create := func(key, tool string) Job {
		input := applicationJobFixture()
		input.Context.RequestKey, input.Tool = key, tool
		job, _, createErr := store.CreateApplicationJob(ctx, "douyin", input)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return job
	}
	// Deliberately interleave unrelated ledger records before and between the
	// target tool. A tool page must be full when sufficient target rows exist.
	create("publication-1", "publication")
	first := create("interaction-1", "interaction")
	create("publication-2", "publication")
	second := create("interaction-2", "interaction")
	create("publication-3", "publication")
	third := create("interaction-3", "interaction")

	page, err := store.JobsForTool(ctx, "application/douyin", "interaction", 0, 2)
	if err != nil || len(page.Items) != 2 || !page.HasMore || page.Items[0].ID != first.ID || page.Items[1].ID != second.ID {
		t.Fatalf("first filtered page = %#v, err=%v", page, err)
	}
	next, err := store.JobsForTool(ctx, "application/douyin", "interaction", page.Next, 2)
	if err != nil || len(next.Items) != 1 || next.HasMore || next.Items[0].ID != third.ID {
		t.Fatalf("continued filtered page = %#v, err=%v", next, err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir, "fish-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reopened, err := store.JobsForTool(ctx, "application/douyin", "interaction", 0, 10)
	if err != nil || len(reopened.Items) != 3 || reopened.Items[0].ID != first.ID || reopened.Items[2].ID != third.ID {
		t.Fatalf("reopened filtered page = %#v, err=%v", reopened, err)
	}
}

func TestApplicationJobsCannotUseProjectPluginEntryPoint(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir(), "fish-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	input := applicationJobFixture()
	input.Plugin = "application/douyin"
	if _, _, err = store.CreateJob(ctx, input); err == nil {
		t.Fatal("untrusted plugin path created an application job")
	}
	if _, _, err = store.CreateApplicationJob(ctx, "bad/id", applicationJobFixture()); err == nil {
		t.Fatal("invalid application id was accepted")
	}
	missing := applicationJobFixture()
	missing.ConfigRevision = ""
	if _, _, err = store.CreateApplicationJob(ctx, "douyin", missing); err == nil {
		t.Fatal("missing authorization version was accepted")
	}
	project := applicationJobFixture()
	project.Plugin, project.Version, project.PackageDigest, project.ConfigRevision = "speech", "1", "missing", "0"
	if _, _, err = store.CreateJob(ctx, project); err == nil {
		t.Fatal("ordinary plugin job bypassed installed package checks")
	}
}
