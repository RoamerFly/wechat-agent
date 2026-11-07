//go:build windows

package station

import (
	"context"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/stationbrowser"
)

func TestLocalDouyinWebVisualBadMediaIsDecodeFailure(t *testing.T) {
	f, manager := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	view = setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	manager.visual = stationLocalVision{
		version: func(context.Context) (string, error) { return "synthetic-vision", nil },
		observe: func(context.Context, string, string, []deepseek.Image, int, func() error) (string, error) {
			t.Fatal("invalid media reached visual model")
			return "", nil
		},
	}
	code, view := enableSyntheticDouyinWebVisual(t, f, view, true)
	if code != 200 {
		t.Fatal(view)
	}
	id := syntheticDouyinWebPlaybackTarget(t, f, manager)
	manager.readMedia = syntheticInvalidVisualMedia
	in := syntheticDouyinWebPlaybackInput(id, 1)
	in["field"] = "visual"
	code, out := f.request("POST", "/api/local/apps/observation", in, true)
	if code != 422 || out["error"] != apphost.ErrObservationVisualDecode.Error() {
		t.Fatalf("bad media did not identify decoding failure: %d %v", code, out)
	}
}

func syntheticInvalidVisualMedia(_ context.Context, _, _ string, check func() error) (stationbrowser.DouyinWebMedia, error) {
	return stationbrowser.DouyinWebMedia{Data: []byte("synthetic-invalid-video"), ContentType: "video/mp4"}, check()
}
