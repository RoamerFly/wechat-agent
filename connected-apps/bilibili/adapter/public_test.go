package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"wechat-bot/internal/apphost"
)

const fixtureBV = "BV1GJ411x7h7"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func newFixture(t *testing.T) *PublicApp {
	t.Helper()
	app := New(t.TempDir())
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected anonymous provider request: %s", request.URL)
		return nil, nil
	})
	return app
}

func saveAllowed(t *testing.T, app *PublicApp) Settings {
	t.Helper()
	settings, err := app.View()
	if err != nil {
		t.Fatal(err)
	}
	settings, err = app.Save(settings.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func observation(field apphost.ObservationField, content string) apphost.ObservationRequest {
	return apphost.ObservationRequest{ObservationAccess: apphost.ObservationAccess{
		ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: connectionID, AccountID: accountID},
		ContentID:     content,
		Field:         field,
	}}
}

func TestRegistrationAndOwnerPolicyAreLocal(t *testing.T) {
	app := newFixture(t)
	registration := app.Registration()
	if registration.ID != ID || registration.InstallationID != installationID || registration.Provider != app {
		t.Fatalf("unexpected registration: %#v", registration)
	}
	connections, err := app.Connections(t.Context())
	if err != nil || len(connections) != 1 || connections[0].ID != connectionID || connections[0].AccountID != accountID {
		t.Fatalf("unexpected public connection: %#v, %v", connections, err)
	}
	status, err := app.Inspect(t.Context(), connectionID)
	if err != nil || status.Login != "anonymous" || status.Worker != "disabled" || len(status.SupportedActions) != 0 {
		t.Fatalf("unexpected disabled status: %#v, %v", status, err)
	}
	targets, err := app.ObservationTargets(t.Context(), connectionID)
	if err != nil || len(targets) != 0 {
		t.Fatalf("disabled policy should be local and empty: %#v, %v", targets, err)
	}
}

func TestReadExactAllowedMetadataWithoutCookies(t *testing.T) {
	app := newFixture(t)
	settings := saveAllowed(t, app)
	var calls atomic.Int32
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "api.bilibili.com" || request.URL.Path != "/x/web-interface/view" || request.URL.Query().Get("bvid") != fixtureBV || len(request.URL.Query()) != 1 {
			t.Fatalf("unexpected exact endpoint: %s", request.URL)
		}
		if request.Header.Get("Cookie") != "" || app.client.Jar != nil || app.client.CheckRedirect == nil {
			t.Fatalf("anonymous request unexpectedly has cookie or redirect behavior")
		}
		return response(http.StatusOK, `{"code":0,"data":{"bvid":"BV1GJ411x7h7","title":"实际标题","desc":"投稿者的原始简介"}}`), nil
	})
	grant, err := app.ObservationGrant(t.Context(), observation(apphost.ObservationBody, fixtureBV).ObservationAccess)
	if err != nil || !grant.Allowed || grant.Version != "bilibili-public-2" || settings.Revision != 2 {
		t.Fatalf("unexpected exact grant: %#v / %#v / %v", grant, settings, err)
	}
	body, err := app.ReadObservation(t.Context(), observation(apphost.ObservationBody, fixtureBV))
	if err != nil || body.Coverage != "B站公开资料：投稿者简介原文" || !body.Complete || len(body.Chunks) != 1 || body.Chunks[0].Text != "投稿者的原始简介" {
		t.Fatalf("unexpected uploader-text evidence: %#v, %v", body, err)
	}
	title, err := app.ReadObservation(t.Context(), observation(apphost.ObservationTitle, fixtureBV))
	if err != nil || title.Coverage != "B站公开资料：视频标题原文" || len(title.Chunks) != 1 || title.Chunks[0].Text != "实际标题" {
		t.Fatalf("unexpected title evidence: %#v, %v", title, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("got %d provider calls, want exactly two permitted reads", calls.Load())
	}
}

func TestPolicyRevocationAndRestartStopFutureReads(t *testing.T) {
	directory := t.TempDir()
	app := New(directory)
	var calls atomic.Int32
	app.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusOK, `{"code":0,"data":{"bvid":"BV1GJ411x7h7","title":"title","desc":""}}`), nil
	})
	settings := saveAllowed(t, app)
	if _, err := app.ReadObservation(t.Context(), observation(apphost.ObservationTitle, fixtureBV)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("first permitted read did not run exactly once")
	}
	if _, err := app.Save(settings.Revision, Settings{}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ReadObservation(t.Context(), observation(apphost.ObservationTitle, fixtureBV)); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("revoked read error = %v, want forbidden", err)
	}
	restarted := New(directory)
	restarted.client.Transport = app.client.Transport
	if _, err := restarted.ReadObservation(t.Context(), observation(apphost.ObservationTitle, fixtureBV)); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("restart must retain revoked policy, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("revoked or restarted read contacted provider: %d", calls.Load())
	}
}

func TestSaveStrictCanonicalCASAndDuplicateRules(t *testing.T) {
	app := newFixture(t)
	initial, err := app.View()
	if err != nil || initial.Revision != 1 || initial.Enabled || len(initial.Videos) != 0 {
		t.Fatalf("unexpected initial settings: %#v, %v", initial, err)
	}
	for _, videos := range [][]string{{"bv1GJ411x7h7"}, {fixtureBV, fixtureBV}, {"BV1GJ411x7h7x"}, {"BV1GJ411x7h"}} {
		if _, err := app.Save(initial.Revision, Settings{Enabled: true, Videos: videos}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Save(%q) error = %v, want ErrInvalid", videos, err)
		}
	}
	if _, err := app.Save(initial.Revision, Settings{Enabled: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("enabled empty allowlist error = %v, want ErrInvalid", err)
	}
	next, err := app.Save(initial.Revision, Settings{})
	if err != nil || next.Revision != 2 || next.Videos == nil {
		t.Fatalf("unexpected disabled save: %#v, %v", next, err)
	}
	if current, err := app.Save(initial.Revision, Settings{}); !errors.Is(err, ErrConflict) || current.Revision != next.Revision {
		t.Fatalf("CAS conflict = %#v, %v", current, err)
	}
}

func TestReadRejectsAnythingOtherThanExactPermittedMetadata(t *testing.T) {
	app := newFixture(t)
	saveAllowed(t, app)
	for _, request := range []apphost.ObservationRequest{
		observation(apphost.ObservationComments, fixtureBV),
		observation(apphost.ObservationAudio, fixtureBV),
		observation(apphost.ObservationPlayback, fixtureBV),
		observation(apphost.ObservationTitle, "BV1GJ411x7h8"),
		observation(apphost.ObservationTitle, "BV1GJ411x7h7?x=1"),
		func() apphost.ObservationRequest {
			r := observation(apphost.ObservationTitle, fixtureBV)
			r.Cursor = "page-two"
			return r
		}(),
	} {
		if _, err := app.ReadObservation(t.Context(), request); !errors.Is(err, apphost.ErrObservationForbidden) && !errors.Is(err, apphost.ErrObservationInvalid) {
			t.Fatalf("request %#v error = %v, want local rejection", request, err)
		}
	}
	// A positive limit is accepted as a maximum; this one-record source is
	// complete in the same page instead of rejecting a caller's chosen limit.
	app.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"code":0,"data":{"bvid":"BV1GJ411x7h7","title":"title","desc":"desc"}}`), nil
	})
	limited := observation(apphost.ObservationTitle, fixtureBV)
	limited.Limit = 99
	if page, err := app.ReadObservation(t.Context(), limited); err != nil || !page.Complete || len(page.Chunks) != 1 {
		t.Fatalf("positive limit should not narrow or reject exact source: %#v, %v", page, err)
	}
}

func TestReadMapsProviderFailuresAndPreservesCancellation(t *testing.T) {
	app := newFixture(t)
	saveAllowed(t, app)
	app.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusPreconditionFailed, ``), nil
	})
	if _, err := app.ReadObservation(t.Context(), observation(apphost.ObservationTitle, fixtureBV)); !errors.Is(err, apphost.ErrObservationThrottled) {
		t.Fatalf("412 error = %v, want throttled", err)
	}
	app.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"code":0,"data":{"bvid":"BV1xx411x7h7","title":"title","desc":"desc"}}`), nil
	})
	if _, err := app.ReadObservation(t.Context(), observation(apphost.ObservationTitle, fixtureBV)); !errors.Is(err, apphost.ErrObservationContentUnavailable) {
		t.Fatalf("mismatched BVID error = %v, want content unavailable", err)
	}
	app.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := app.ReadObservation(ctx, observation(apphost.ObservationTitle, fixtureBV)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read error = %v, want context cancellation", err)
	}
}

func TestEmptyDescriptionIsExplicitAndDoesNotInventEvidence(t *testing.T) {
	app := newFixture(t)
	saveAllowed(t, app)
	app.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"code":0,"data":{"bvid":"BV1GJ411x7h7","title":"title","desc":"  "}}`), nil
	})
	page, err := app.ReadObservation(t.Context(), observation(apphost.ObservationBody, fixtureBV))
	if err != nil || !page.Complete || len(page.Chunks) != 0 || page.Coverage != "B站公开资料：投稿者简介原文为空" {
		t.Fatalf("unexpected empty description page: %#v, %v", page, err)
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
