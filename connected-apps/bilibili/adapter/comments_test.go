package adapter

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
)

const commentFixtureAID = "80433022"
const commentFixtureMetadata = `{"code":0,"data":{"bvid":"BV1GJ411x7h7","aid":80433022}}`
const commentFixtureFirst = `{"code":0,"data":{"cursor":{"is_end":false,"next":2},"replies":[
	{"rpid":9007199254740993,"oid":80433022,"type":1,"root":0,"parent":0,"ctime":1735689600,"member":{"mid":123,"uname":"synthetic"},"content":{"message":"first fixture"}},
	{"rpid":9007199254740994,"oid":80433022,"type":1,"root":0,"parent":0,"ctime":1735689600,"content":{"message":""}}
]}}`
const commentFixtureSecond = `{"code":0,"data":{"cursor":{"is_end":true},"replies":[
	{"rpid":9007199254740995,"oid":80433022,"type":1,"root":0,"parent":0,"ctime":1735689601,"content":{"message":"second fixture"}}
]}}`

func commentFixtureGrant(t *testing.T, app *PublicApp) Settings {
	t.Helper()
	settings, err := app.View()
	if err != nil {
		t.Fatal(err)
	}
	settings, err = app.Save(settings.Revision, Settings{Enabled: true, CommentsEnabled: true, Videos: []string{fixtureBV}})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestCommentsResumeWithExactSourceAfterRestart(t *testing.T) {
	dir := t.TempDir()
	app := New(filepath.Join(dir, "policy"))
	saved := commentFixtureGrant(t, app)
	var calls []string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Host != "api.bilibili.com" ||
			req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" {
			t.Fatalf("request escaped the anonymous provider: %v", req.URL)
		}
		q := req.URL.Query()
		switch req.URL.Path {
		case "/x/web-interface/view":
			if q.Get("bvid") != fixtureBV || len(q) != 1 {
				t.Fatalf("unexpected BV resolution: %s", req.URL)
			}
			calls = append(calls, "BV")
			return response(http.StatusOK, commentFixtureMetadata), nil
		case "/x/v2/reply/main":
			if q.Get("oid") != commentFixtureAID || q.Get("type") != "1" || q.Get("mode") != "3" || q.Get("ps") != "2" || len(q) != 5 {
				t.Fatalf("unexpected page request: %s", req.URL)
			}
			next := q.Get("next")
			calls = append(calls, next)
			switch next {
			case "0":
				return response(http.StatusOK, commentFixtureFirst), nil
			case "2":
				return response(http.StatusOK, commentFixtureSecond), nil
			}
		}
		t.Fatalf("unexpected request: %s", req.URL)
		return nil, nil
	})
	observerFor := func(app *PublicApp) *apphost.Observer {
		t.Helper()
		app.client.Transport = transport
		registry, err := apphost.New(app.Registration())
		if err != nil {
			t.Fatal(err)
		}
		return apphost.NewObserver(registry, filepath.Join(dir, "checkpoint"))
	}
	observer := observerFor(app)
	in := apphost.ObservationInput{AppID: ID, ConnectionID: connectionID, ContentID: fixtureBV, Field: apphost.ObservationComments, Limit: 2}
	first, err := observer.Read(t.Context(), in)
	if err != nil || first.Complete || first.Resumed || len(first.Chunks) != 1 ||
		!reflect.DeepEqual(first.Chunks[0], apphost.ObservationChunk{Position: 0, Text: "first fixture", SourceID: "9007199254740993", PublishedAt: "2025-01-01T00:00:00Z"}) ||
		!strings.Contains(first.Coverage, "1 条没有文字") {
		t.Fatalf("first page: %#v, %v", first, err)
	}

	app = New(filepath.Join(dir, "policy"))
	observer = observerFor(app)
	in.Limit = 3
	if _, err := observer.Read(t.Context(), in); !errors.Is(err, apphost.ErrObservationCommentPageSize) {
		t.Fatalf("changed page size could skip records: %v", err)
	}
	in.Limit = 0
	second, err := observer.Read(t.Context(), in)
	if err != nil || !second.Complete || !second.Resumed || len(second.Chunks) != 1 || second.Chunks[0].Position != 2 || second.Chunks[0].SourceID != "9007199254740995" {
		t.Fatalf("resumed page: %#v, %v", second, err)
	}
	if cached, err := observer.Read(t.Context(), in); err != nil || !cached.Cached || !cached.Complete || strings.Join(calls, ",") != "BV,0,2" {
		t.Fatalf("completed checkpoint: %#v, %v, calls %v", cached, err, calls)
	}
	if _, err := app.Save(saved.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}}); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Read(t.Context(), in); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("revoked cache remained available: %v", err)
	}
	if err := observer.Check(t.Context(), first); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("revoked evidence remained usable: %v", err)
	}
}

func TestCommentsRejectBadEvidenceAndProviderLimits(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		payload string
		want    error
	}{
		{"HTTP risk control", 412, "", apphost.ErrObservationThrottled},
		{"platform risk control", 200, `{"code":-352}`, apphost.ErrObservationThrottled},
		{"rate limited", 429, "", apphost.ErrObservationThrottled},
		{"closed", 200, `{"code":12002}`, apphost.ErrObservationContentUnavailable},
		{"missing end flag", 200, strings.Replace(commentFixtureFirst, `"is_end":false,`, "", 1), apphost.ErrObservationUpstreamUnavailable},
		{"wrong video", 200, strings.Replace(commentFixtureFirst, commentFixtureAID, "44", 1), apphost.ErrObservationUpstreamUnavailable},
		{"nested reply", 200, strings.Replace(commentFixtureFirst, `"root":0`, `"root":1`, 1), apphost.ErrObservationUpstreamUnavailable},
		{"duplicate id", 200, strings.Replace(commentFixtureFirst, "9007199254740994", "9007199254740993", 1), apphost.ErrObservationUpstreamUnavailable},
		{"stalled cursor", 200, strings.Replace(commentFixtureFirst, `"next":2`, `"next":0`, 1), apphost.ErrObservationUpstreamUnavailable},
		{"no replies", 200, `{"code":0,"data":{"cursor":{"is_end":true},"replies":null}}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := newFixture(t)
			commentFixtureGrant(t, app)
			app.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/x/web-interface/view" {
					return response(http.StatusOK, commentFixtureMetadata), nil
				}
				return response(tc.status, tc.payload), nil
			})
			request := observation(apphost.ObservationComments, fixtureBV)
			page, err := app.ReadObservation(t.Context(), request)
			if !errors.Is(err, tc.want) || (tc.want == nil && (!page.Complete || len(page.Chunks) != 0)) {
				t.Fatalf("page = %#v, error = %v, want %v", page, err, tc.want)
			}
		})
	}
}

func TestCommentsRevocationCancelsInFlightRequest(t *testing.T) {
	app := newFixture(t)
	saved := commentFixtureGrant(t, app)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	app.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/x/web-interface/view" {
			return response(http.StatusOK, commentFixtureMetadata), nil
		}
		close(started)
		<-req.Context().Done()
		close(cancelled)
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := app.ReadObservation(ctx, observation(apphost.ObservationComments, fixtureBV))
		result <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("comment request never started")
	}
	if _, err := app.Save(saved.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("revocation did not cancel the HTTP request")
	}
	if err := <-result; !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("revoked in-flight request returned %v", err)
	}
}
