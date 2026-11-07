package apphost

import (
	"errors"
	"testing"
)

func TestWebTextRefreshKeepsPaginationAndRechecksCachedGrant(t *testing.T) {
	f := &observationFixture{allowed: true, version: "web-v1", pages: map[string]ObservationPage{
		"": {Chunks: []ObservationChunk{{Text: "original"}}, Coverage: "visible text", Complete: true},
	}}
	o := NewObserver(observerFixtureRegistry(t, f), t.TempDir())
	in := ObservationInput{AppID: "offline", ConnectionID: "account", ContentID: "post", Field: ObservationPageText}
	if _, err := o.Read(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if result, err := o.Read(t.Context(), in); err != nil || !result.Cached {
		t.Fatalf("cache: %+v %v", result, err)
	}
	f.pages[""] = ObservationPage{Chunks: []ObservationChunk{{Text: "updated"}}, Coverage: "new visible text", Complete: true}
	in.Refresh = true
	if result, err := o.Read(t.Context(), in); err != nil || result.Cached || result.Chunks[0].Text != "updated" {
		t.Fatalf("refresh: %+v %v", result, err)
	}
	f.allowed = false
	in.Refresh = false
	if _, err := o.Read(t.Context(), in); !errors.Is(err, ErrObservationForbidden) {
		t.Fatalf("revoked cached read: %v", err)
	}
}
