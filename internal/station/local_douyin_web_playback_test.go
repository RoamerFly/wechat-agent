package station

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

func syntheticDouyinWebPlaybackTarget(t *testing.T, f *localHomeFixture, m *localDouyinWebLogin) string {
	t.Helper()
	m.readPage = func(context.Context, string, func() error) (stationbrowser.DouyinWebPage, error) {
		return stationbrowser.DouyinWebPage{Text: "合成首页", Links: []stationbrowser.DouyinWebLink{{VideoID: "7000000000000000011", Text: "一段真实可见的合成链接"}}}, nil
	}
	code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebReadInput(2048), true)
	ids := localWebFollowUps(result)
	if code != 200 || len(ids) != 1 {
		t.Fatalf("home link: %d %v", code, result)
	}
	return ids[0]
}

func setSyntheticDouyinWebPlayback(t *testing.T, f *localHomeFixture, view map[string]any, allow bool) map[string]any {
	t.Helper()
	code, result := f.request("POST", localDouyinWebRoute, map[string]any{
		"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home_and_videos", "allow_playback": allow,
	}, true)
	if code != 200 || result["playback_enabled"] != allow {
		t.Fatalf("playback permission: %d %v", code, result)
	}
	return result
}

func syntheticDouyinWebPlaybackInput(id string, limit int) map[string]any {
	return map[string]any{"app_id": "douyin", "connection_id": douyinapp.WebReadConnectionID, "content_id": id, "field": "playback", "limit": limit}
}

func TestLocalDouyinWebPlaybackNeedsSeparatePermissionAndOriginalLink(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	if view["playback_enabled"] != false {
		t.Fatal("login or home read granted playback")
	}
	view = setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	plays := 0
	m.sampleVideo = func(ctx context.Context, profile, videoID string, seconds time.Duration, authorize func() error) (stationbrowser.PlaybackSample, error) {
		plays++
		if videoID != "7000000000000000011" || !strings.HasPrefix(profile, filepath.Join(f.dir, "apps", "douyin", "web-login")) || seconds < time.Second || seconds > 20*time.Second {
			t.Error("sample lost source, private scope or requested duration")
		}
		if err := authorize(); err != nil {
			return stationbrowser.PlaybackSample{}, err
		}
		return stationbrowser.PlaybackSample{Ready: true, Paused: true, DurationSeconds: 10, Intervals: []stationbrowser.PlaybackInterval{{StartSeconds: 0, EndSeconds: 1.5}}}, nil
	}
	input := syntheticDouyinWebPlaybackInput(id, 2)
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 || plays != 0 {
		t.Fatal("text permission granted media observation")
	}
	view = setSyntheticDouyinWebPlayback(t, f, view, true)
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 || plays != 0 {
		t.Fatal("new permission reused old source proof")
	}
	id = syntheticDouyinWebPlaybackTarget(t, f, m)
	input["content_id"] = id
	m.readVideo = func(context.Context, string, string, func() error) (stationbrowser.DouyinWebPage, error) {
		return stationbrowser.DouyinWebPage{Text: "这段视频的页面说明"}, nil
	}
	textInput := syntheticDouyinWebReadInput(2048)
	textInput["content_id"] = id
	code, text := f.request("POST", "/api/local/apps/observation", textInput, true)
	chunk := text["chunks"].([]any)[0].(map[string]any)
	if code != 200 || chunk["follow_up_content_id"] != id || chunk["follow_up_field"] != "playback" {
		t.Fatalf("missing authorized playback followup: %d %v", code, text)
	}
	code, result := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || plays != 1 || result["complete"] != true || result["cached"] != false {
		t.Fatalf("sample: %d %v plays=%d", code, result, plays)
	}
	proof := result["playback_proof"].(map[string]any)
	if proof["requested_seconds"] != float64(2) || proof["fully_played"] != false || proof["media_duration_seconds"] != float64(10) || len(result["playback"].([]any)) != 1 {
		t.Fatal("sample fabricated full watch or changed its range", result)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), f.dir) || strings.Contains(string(raw), "Cookies") || result["audio_proof"] != nil {
		t.Fatal("private profile leak or false audio evidence")
	}
	_, cached := f.request("POST", "/api/local/apps/observation", input, true)
	if cached["cached"] != true || plays != 1 {
		t.Fatal("same observation silently replayed media")
	}
	input["limit"] = 3
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 200 || plays != 2 {
		t.Fatal("explicit sample duration reused another duration cache")
	}
	input["content_id"] = "home"
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 || plays != 2 {
		t.Fatal("home became a playable content target")
	}
	input["content_id"] = id
	view = setSyntheticDouyinWebPlayback(t, f, view, false)
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 || plays != 2 {
		t.Fatal("revoked playback reused cache")
	}
	view = setSyntheticDouyinWebPlayback(t, f, view, true)
	view = setSyntheticDouyinWebScope(t, f, view, "home")
	if view["playback_enabled"] != false {
		t.Fatal("scope downgrade kept playback enabled")
	}
	view = setSyntheticDouyinWebPlayback(t, f, view, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": view["revision"], "approve_profile": true}, true)
	if view["playback_enabled"] != false {
		t.Fatal("relogin kept playback permission")
	}
}

func TestLocalDouyinWebPlaybackRevocationAfterSampleDiscardsEvidence(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := setSyntheticDouyinWebPlayback(t, f, enableSyntheticDouyinWebRead(t, f), true)
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	m.sampleVideo = func(ctx context.Context, _, _ string, _ time.Duration, authorize func() error) (stationbrowser.PlaybackSample, error) {
		if err := authorize(); err != nil {
			return stationbrowser.PlaybackSample{}, err
		}
		setSyntheticDouyinWebPlayback(t, f, view, false)
		return stationbrowser.PlaybackSample{Ready: true, Ended: true, Paused: true, DurationSeconds: 1, Intervals: []stationbrowser.PlaybackInterval{{StartSeconds: 0, EndSeconds: 1}}}, nil
	}
	code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebPlaybackInput(id, 1), true)
	if code != 403 || result["playback_proof"] != nil {
		t.Fatalf("revoked successful callback escaped: %d %v", code, result)
	}
}

func TestLocalDouyinWebPlaybackErrorsAndStalledPlayerStayDistinct(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	setSyntheticDouyinWebPlayback(t, f, enableSyntheticDouyinWebRead(t, f), true)
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	for _, test := range []struct {
		err  error
		code int
	}{
		{stationbrowser.ErrPlaybackUnavailable, 422}, {stationbrowser.ErrDouyinLoginActive, 409},
		{stationbrowser.ErrDouyinWebLoginRequired, 409}, {context.DeadlineExceeded, 504}, {errors.New("private-media-signature"), 503},
	} {
		m.sampleVideo = func(context.Context, string, string, time.Duration, func() error) (stationbrowser.PlaybackSample, error) {
			return stationbrowser.PlaybackSample{}, test.err
		}
		code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebPlaybackInput(id, 1), true)
		raw, _ := json.Marshal(result)
		if code != test.code || strings.Contains(string(raw), "private-media-signature") {
			t.Fatalf("wrong actionable error: %v -> %d %s", test.err, code, raw)
		}
	}
	plays := 0
	m.sampleVideo = func(context.Context, string, string, time.Duration, func() error) (stationbrowser.PlaybackSample, error) {
		plays++
		return stationbrowser.PlaybackSample{Ready: true, Paused: true, DurationSeconds: 10}, nil
	}
	for i := 0; i < 2; i++ {
		code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebPlaybackInput(id, 1), true)
		if code != 200 || result["cached"] != false || result["playback_proof"].(map[string]any)["fully_played"] != false || !strings.Contains(result["coverage"].(string), "未记录到实际播放区间") {
			t.Fatalf("stalled page: %d %v", code, result)
		}
	}
	if plays != 2 {
		t.Fatal("stalled sample prevented a later real attempt")
	}
	if code, _ := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebPlaybackInput(id, 21), true); code != 400 || plays != 2 {
		t.Fatal("invalid range silently clamped")
	}
}

func TestLocalDouyinWebPlaybackRejectsUnconfirmedOrHomeOnlyConsent(t *testing.T) {
	f, _ := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "allow_playback": true}, true)
	if code != 400 {
		t.Fatal("playback expanded text scope implicitly")
	}
	_, current := f.request("GET", localDouyinWebRoute, nil, false)
	if current["revision"] != view["revision"] || current["playback_enabled"] != false {
		t.Fatal("rejected consent changed saved grant")
	}
	// Previously granted text continues to work; no login/permission was lost.
	if localDouyinWebFieldAllowed(localDouyinWebRecord{ReadScope: "home_and_videos", PlaybackEnabled: true}, "home", apphost.ObservationPlayback) {
		t.Fatal("home got a media target")
	}
}

func TestLocalDouyinWebPlaybackConsentOmissionPreservesOnlyValidWideScope(t *testing.T) {
	f, _ := newLocalDouyinWebFixture(t)
	view := setSyntheticDouyinWebPlayback(t, f, enableSyntheticDouyinWebRead(t, f), true)
	code, next := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true}, true)
	if code != 200 || next["playback_enabled"] != true || next["read_scope"] != "home_and_videos" {
		t.Fatalf("omission lost explicit consent: %d %v", code, next)
	}
	code, next = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": next["revision"], "allow_read": false}, true)
	if code != 200 || next["playback_enabled"] != false || next["read_scope"] != "home" {
		t.Fatalf("stopping text kept implicit playback: %d %v", code, next)
	}
	code, next = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": next["revision"], "allow_read": true, "read_scope": "home_and_videos"}, true)
	if code != 200 || next["playback_enabled"] != false {
		t.Fatal("reenabling text silently reenabled playback", code, next)
	}
}

func TestLocalDouyinWebPlaybackCompletedSampleSurvivesRestartWithoutReplay(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	setSyntheticDouyinWebPlayback(t, f, enableSyntheticDouyinWebRead(t, f), true)
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	m.sampleVideo = func(context.Context, string, string, time.Duration, func() error) (stationbrowser.PlaybackSample, error) {
		return stationbrowser.PlaybackSample{Ready: true, Paused: true, DurationSeconds: 5, Intervals: []stationbrowser.PlaybackInterval{{StartSeconds: 0, EndSeconds: 1}}}, nil
	}
	input := syntheticDouyinWebPlaybackInput(id, 1)
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 200 {
		t.Fatal(code)
	}
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	registry, err := apphost.New(douyinapp.New(filepath.Join(f.dir, "apps", "douyin")).Registration())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLocalHomeHandlerWithApps(f.dir, f.root, "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	next := newLocalAppObservationFixtureWithHandler(t, handler, f.dir, f.root)
	next.login()
	handler.home.douyinWeb.available = func() bool { return true }
	handler.home.douyinWeb.sampleVideo = func(context.Context, string, string, time.Duration, func() error) (stationbrowser.PlaybackSample, error) {
		t.Error("restart replayed already recorded media")
		return stationbrowser.PlaybackSample{}, errors.New("should use original sample")
	}
	code, result := next.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || result["cached"] != true || result["playback_proof"].(map[string]any)["fully_played"] != false {
		t.Fatalf("restart: %d %v", code, result)
	}
}
