package adapter

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/stationbrowser"
)

func enablePlayback(t *testing.T, app *PublicApp) Settings {
	t.Helper()
	app.browserAvailable = func() bool { return true }
	settings, err := app.View()
	if err != nil {
		t.Fatal(err)
	}
	settings, err = app.Save(settings.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}, BrowserEnabled: true, PlaybackEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestPlaybackRequiresItsOwnOwnerGrantAndAdvertisesOnlyWhenReady(t *testing.T) {
	app := newFixture(t)
	app.browserAvailable = func() bool { return true }
	initial, err := app.View()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.Save(initial.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}, BrowserEnabled: true}); err != nil {
		t.Fatal(err)
	}
	request := observation(apphost.ObservationPlayback, fixtureBV)
	if _, err := app.ReadObservation(t.Context(), request); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("playback without its separate grant = %v", err)
	}
	current, err := app.View()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.Save(current.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}, BrowserEnabled: true, PlaybackEnabled: true}); err != nil {
		t.Fatal(err)
	}
	targets, err := app.ObservationTargets(t.Context(), connectionID)
	if err != nil || len(targets) != 1 || !hasField(targets[0].Fields, apphost.ObservationPlayback) {
		t.Fatalf("playback target = %#v, %v", targets, err)
	}
	status, err := app.Inspect(t.Context(), connectionID)
	if err != nil || !hasAction(status.SupportedActions, "playback.observe") {
		t.Fatalf("playback action = %#v, %v", status, err)
	}
}

func TestPlaybackUsesBoundedRequestedSampleAndReturnsOnlyMeasuredEvidence(t *testing.T) {
	app := newFixture(t)
	enablePlayback(t, app)
	var resolverLimit int64
	app.resolveMedia = func(ctx context.Context, bvid string, limit int64) (mcptools.PublicVideo, error) {
		if err := ctx.Err(); err != nil {
			return mcptools.PublicVideo{}, err
		}
		if bvid != fixtureBV {
			t.Fatalf("resolver BVID = %q", bvid)
		}
		resolverLimit = limit
		return mcptools.PublicVideo{Data: []byte("synthetic mp4 bytes"), ContentType: "video/mp4", DurationMS: 42_000}, nil
	}
	app.samplePlayback = func(ctx context.Context, media []byte, contentType string, sample time.Duration, authorize func() error) (stationbrowser.PlaybackSample, error) {
		if err := authorize(); err != nil {
			return stationbrowser.PlaybackSample{}, err
		}
		if string(media) != "synthetic mp4 bytes" || contentType != "video/mp4" || sample != 6*time.Second {
			t.Fatalf("sample input %q %q %s", media, contentType, sample)
		}
		return stationbrowser.PlaybackSample{Ready: true, Paused: true, DurationSeconds: 42, Intervals: []stationbrowser.PlaybackInterval{{StartSeconds: 0, EndSeconds: 5.85}}}, nil
	}
	page, err := app.ReadObservation(t.Context(), observation(apphost.ObservationPlayback, fixtureBV))
	if err != nil || !page.Complete || page.PlaybackProof == nil || page.PlaybackProof.RequestedSeconds != 6 || !page.PlaybackProof.Ready || !page.PlaybackProof.Paused || len(page.Playback) != 1 || resolverLimit != maxMediaBytes {
		t.Fatalf("unexpected bounded playback evidence %#v %v", page, err)
	}
	for _, requested := range []int{1, 20} {
		request := observation(apphost.ObservationPlayback, fixtureBV)
		request.Limit = requested
		app.samplePlayback = func(_ context.Context, _ []byte, _ string, sample time.Duration, authorize func() error) (stationbrowser.PlaybackSample, error) {
			if err := authorize(); err != nil {
				return stationbrowser.PlaybackSample{}, err
			}
			if sample != time.Duration(requested)*time.Second {
				t.Fatalf("explicit sample = %s, want %d seconds", sample, requested)
			}
			return stationbrowser.PlaybackSample{Ready: true, Paused: true, DurationSeconds: 42}, nil
		}
		if _, err := app.ReadObservation(t.Context(), request); err != nil {
			t.Fatalf("sample %d: %v", requested, err)
		}
	}
	for _, requested := range []int{-1, 21, 1 << 30} {
		request := observation(apphost.ObservationPlayback, fixtureBV)
		request.Limit = requested
		if _, err := app.ReadObservation(t.Context(), request); !errors.Is(err, apphost.ErrObservationPlaybackLimit) && !errors.Is(err, apphost.ErrObservationInvalid) {
			t.Fatalf("sample %d error = %v", requested, err)
		}
	}
}

func TestPlaybackRevocationCancelsBeforeLateSampleIsReturned(t *testing.T) {
	app := newFixture(t)
	saved := enablePlayback(t, app)
	app.resolveMedia = func(context.Context, string, int64) (mcptools.PublicVideo, error) {
		return mcptools.PublicVideo{Data: []byte("synthetic mp4 bytes"), ContentType: "video/mp4", DurationMS: 42_000}, nil
	}
	started := make(chan struct{})
	app.samplePlayback = func(ctx context.Context, _ []byte, _ string, _ time.Duration, authorize func() error) (stationbrowser.PlaybackSample, error) {
		if err := authorize(); err != nil {
			return stationbrowser.PlaybackSample{}, err
		}
		close(started)
		<-ctx.Done()
		return stationbrowser.PlaybackSample{Ready: true, Paused: true, DurationSeconds: 42, Intervals: []stationbrowser.PlaybackInterval{{StartSeconds: 0, EndSeconds: 2}}}, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		_, err := app.ReadObservation(ctx, observation(apphost.ObservationPlayback, fixtureBV))
		completed <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("playback sample did not start")
	}
	if _, err := app.Save(saved.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}, BrowserEnabled: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if !errors.Is(err, apphost.ErrObservationForbidden) {
			t.Fatalf("late playback error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("playback revocation did not cancel")
	}
}

func TestPlaybackFailureClassificationDoesNotMislabelNetworkFaultsAsThrottle(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"rate limit", &mcptools.WebError{Code: "http_status", Status: http.StatusTooManyRequests}, apphost.ErrObservationThrottled},
		{"too large", &mcptools.WebError{Code: "response_too_large"}, apphost.ErrObservationPlaybackSize},
		{"provider fault", &mcptools.WebError{Code: "http_status", Status: http.StatusServiceUnavailable}, apphost.ErrObservationUpstreamUnavailable},
		{"network timeout", &mcptools.WebError{Code: "network_timeout"}, apphost.ErrObservationUpstreamUnavailable},
		{"unsupported", &mcptools.WebError{Code: "video_progressive_unavailable"}, apphost.ErrObservationPlaybackUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyPlaybackError(test.err); !errors.Is(got, test.want) {
				t.Fatalf("classification = %v, want %v", got, test.want)
			}
		})
	}
}

func hasField(fields []apphost.ObservationField, wanted apphost.ObservationField) bool {
	for _, field := range fields {
		if field == wanted {
			return true
		}
	}
	return false
}

func hasAction(actions []string, wanted string) bool {
	for _, action := range actions {
		if action == wanted {
			return true
		}
	}
	return false
}
