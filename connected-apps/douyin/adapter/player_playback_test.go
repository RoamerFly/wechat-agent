package adapter

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

func TestDouyinPlaybackRequiresOptInAndUsesFreshMeasuredEvidence(t *testing.T) {
	const videoID = "7000000000000000001"
	settings := validSettings()
	settings.FollowPublicVideos = true
	app, dir := configuredApp(t, settings)
	followup := func() string {
		t.Helper()
		policy, err := app.load()
		if err != nil {
			t.Fatal(err)
		}
		return playerFollowupID(policy, keywordContentID("亲子教育"), videoID)
	}
	var entries, samples int
	public := true
	app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		entries++
		if r.URL.Path != "/api/douyin/v1/video/get_iframe_by_video" || r.URL.Query().Get("video_id") != videoID || r.Header.Get("access-token") != "" {
			t.Fatal("player must use the anonymous endpoint and exact work")
		}
		if !public {
			return jsonResponse(`{"err_no":28003004}`), nil
		}
		return jsonResponse(`{"err_no":0,"data":{"video_title":"合成作品","iframe_code":"<iframe src=\"https://open.douyin.com/player/video?vid=7000000000000000001&autoplay=0\"></iframe>"}}`), nil
	})
	revokeDuringSample := false
	app.samplePlayer = func(_ context.Context, target string, duration time.Duration, authorize func() error) (stationbrowser.PlaybackSample, error) {
		samples++
		if target != "https://open.douyin.com/player/video?autoplay=0&vid="+videoID || duration != 2*time.Second {
			t.Fatalf("unexpected sampler input: %s, %v", target, duration)
		}
		if err := authorize(); err != nil {
			return stationbrowser.PlaybackSample{}, err
		}
		if revokeDuringSample {
			settings.SamplePublicPlayback = false
			writeSettings(t, dir, settings)
		}
		sample := stationbrowser.PlaybackSample{Ready: true, Paused: true, DurationSeconds: 14}
		if samples > 1 {
			sample.Intervals = []stationbrowser.PlaybackInterval{{StartSeconds: 0, EndSeconds: 1.8}}
		}
		return sample, nil
	}

	registry, err := apphost.New(app.Registration())
	if err != nil {
		t.Fatal(err)
	}
	observer := apphost.NewObserver(registry, filepath.Join(t.TempDir(), "observations"))
	request := apphost.ObservationInput{AppID: ID, ConnectionID: connectionID, ContentID: followup(), Field: apphost.ObservationPlayback, Limit: 2}
	if _, err := observer.Read(t.Context(), request); !errors.Is(err, apphost.ErrObservationForbidden) || entries != 0 || samples != 0 {
		t.Fatalf("playback without separate authorization: %v", err)
	}
	settings.SamplePublicPlayback = true
	writeSettings(t, dir, settings)
	request.ContentID = followup()
	entryRequest := request
	entryRequest.Field, entryRequest.Limit = apphost.ObservationPlayerEntry, 0
	entry, err := observer.Read(t.Context(), entryRequest)
	if err != nil || len(entry.Chunks) != 1 || entry.Chunks[0].FollowUpField != apphost.ObservationPlayback || entry.Chunks[0].FollowUpContentID != request.ContentID {
		t.Fatalf("authorized follow-up: %+v %v", entry, err)
	}

	stalled, err := observer.Read(t.Context(), request)
	if err != nil || stalled.PlaybackProof == nil || !stalled.PlaybackProof.Ready || stalled.PlaybackProof.FullyPlayed || len(stalled.Playback) != 0 {
		t.Fatalf("stalled sample fabricated progress: %+v %v", stalled, err)
	}
	measured, err := observer.Read(t.Context(), request)
	if err != nil || measured.Cached || measured.PlaybackProof == nil || measured.PlaybackProof.FullyPlayed || len(measured.Playback) != 1 || measured.Playback[0].EndSeconds != 1.8 || measured.PlaybackProof.RequestedSeconds != 2 {
		t.Fatalf("missing or overstated evidence: %+v %v", measured, err)
	}
	public = false
	if _, err := observer.Read(t.Context(), request); !errors.Is(err, apphost.ErrObservationContentUnavailable) || entries != 4 || samples != 2 {
		t.Fatalf("non-public result reused an old browser sample: %v", err)
	}
	public = true
	revokeDuringSample = true
	if _, err := observer.Read(t.Context(), request); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("late playback result after revocation: %v", err)
	}
}
