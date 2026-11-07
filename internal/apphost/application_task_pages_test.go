package apphost

import (
	"fmt"
	"testing"

	"wechat-bot/internal/pluginhost"
)

func TestApplicationTaskPagesSelectKindBeforeLimit(t *testing.T) {
	publisher, provider, store, target := newPublicationFixture(t)
	provider.query = func(PublicationReference) PublicationReceipt {
		t.Fatal("listing queried platform")
		return PublicationReceipt{}
	}
	interactor := NewInteractor(publisher.registry, store, nil)
	for i := 0; i < 3; i++ {
		actor := publicationActor(fmt.Sprintf("interaction-%d", i))
		draft := interactionDraft(target)
		draft.Desired = false
		record := interactionRecord{Draft: draft, Manifest: interactionManifest(draft)}
		if _, _, err := store.CreateApplicationJob(t.Context(), "social", pluginhost.Job{
			Version: "adapter-v1", PackageDigest: "synthetic", ConfigRevision: "home-v1", Tool: "interaction", Context: actor, Arguments: mustJSON(record),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := publisher.Draft(t.Context(), publicationActor(fmt.Sprintf("publication-%d", i)), publicationDraft(target)); err != nil {
			t.Fatal(err)
		}
	}
	actor := publicationActor("list")
	var publicationCursor, interactionCursor int64
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		publications, err := publisher.List(t.Context(), actor, "social", publicationCursor, 1)
		if err != nil || len(publications.Items) != 1 || publications.HasMore != (i < 2) || publications.Next <= publicationCursor {
			t.Fatalf("publication page %d: %+v %v", i, publications, err)
		}
		interactions, err := interactor.List(t.Context(), actor, "social", interactionCursor, 1)
		if err != nil || len(interactions.Items) != 1 || interactions.HasMore != (i < 2) || interactions.Next <= interactionCursor {
			t.Fatalf("interaction page %d: %+v %v", i, interactions, err)
		}
		for _, id := range []string{publications.Items[0].JobID, interactions.Items[0].JobID} {
			if seen[id] {
				t.Fatal("cross-kind or repeated task", id)
			}
			seen[id] = true
		}
		if interactions.Items[0].Desired == nil || *interactions.Items[0].Desired {
			t.Fatal("cancel-like intent was lost from metadata view")
		}
		publicationCursor, interactionCursor = publications.Next, interactions.Next
	}
	if provider.submits != 0 {
		t.Fatal("listing invoked platform")
	}
}
