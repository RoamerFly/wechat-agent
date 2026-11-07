package station

import (
	"context"
	"errors"
	"testing"

	"wechat-bot/internal/deepseek"
)

func enableSyntheticDouyinWebVisual(t *testing.T, f *localHomeFixture, view map[string]any, allow bool) (int, map[string]any) {
	t.Helper()
	return f.request("POST", localDouyinWebRoute, map[string]any{
		"action": "set_read", "revision": view["revision"], "allow_read": true,
		"read_scope": "home_and_videos", "allow_visual": allow,
	}, true)
}

func TestLocalDouyinWebVisualPermissionNeedsReadyToolAndRevokesWithScope(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	m.visual = stationLocalVision{
		version: func(context.Context) (string, error) { return "synthetic-vision-v1", nil },
		observe: func(context.Context, string, string, []deepseek.Image, int, func() error) (string, error) {
			return "", nil
		},
	}
	view := enableSyntheticDouyinWebRead(t, f)
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{
		"action": "set_read", "revision": view["revision"], "allow_read": true, "allow_visual": true,
	}, true); code != 400 {
		t.Fatal("home-only read allowed visual analysis", code)
	}
	view = setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	code, view := enableSyntheticDouyinWebVisual(t, f, view, true)
	if code != 200 || view["visual_enabled"] != true || view["visual_ready"] != true {
		t.Fatalf("enable visual: %d %v", code, view)
	}
	view = setSyntheticDouyinWebScope(t, f, view, "home")
	if view["visual_enabled"] != false {
		t.Fatal("scope downgrade retained visual permission", view)
	}
	view = setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	if view["visual_enabled"] != false {
		t.Fatal("scope re-expansion restored visual permission", view)
	}
	_, view = enableSyntheticDouyinWebVisual(t, f, view, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{
		"action": "begin", "revision": view["revision"], "approve_profile": true,
	}, true)
	if view["visual_enabled"] != false {
		t.Fatal("relogin retained visual permission", view)
	}
}

func TestLocalDouyinWebVisualPermissionDoesNotPersistWhenToolIsUnavailable(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	m.visual = stationLocalVision{
		version: func(context.Context) (string, error) { return "", errors.New("synthetic visual unavailable") },
		observe: func(context.Context, string, string, []deepseek.Image, int, func() error) (string, error) {
			return "", nil
		},
	}
	view := enableSyntheticDouyinWebRead(t, f)
	view = setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	code, result := enableSyntheticDouyinWebVisual(t, f, view, true)
	if code != 503 {
		t.Fatalf("unavailable visual tool enabled: %d %v", code, result)
	}
	_, current := f.request("GET", localDouyinWebRoute, nil, false)
	if current["revision"] != view["revision"] || current["visual_enabled"] != false || current["visual_ready"] != false {
		t.Fatal("failed visual permission changed durable state", current)
	}
}
