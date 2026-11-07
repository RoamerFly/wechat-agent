package mcptools

import (
	"strings"
	"testing"
)

func TestPublicBVMetadataUsesExactVideoAndSelectedPart(t *testing.T) {
	bvid := "BV1qP4y187Lf"
	page := []byte(`<script>window.__INITIAL_STATE__={"videoData":{"bvid":"BV1qP4y187Lf","cid":12,"title":"超兽武装","pages":[{"cid":12,"part":"一"},{"cid":13,"part":"二"}]}};(function(){})()</script>`)
	state, e := parseBiliPage(page, bvid, 2)
	if e != nil || state.Video.CID != 13 || !strings.Contains(state.Video.Title, "二") {
		t.Fatal(state, e)
	}
	if _, e = parseBiliPage(page, "BV1111111111", 1); e == nil {
		t.Fatal("video identity mismatch accepted")
	}
	if _, e = parseBiliPage(page, bvid, 3); e == nil {
		t.Fatal("missing part accepted")
	}
	for _, raw := range []string{"https://bilibili.com.evil.invalid/video/" + bvid, "https://www.bilibili.com/video/not-a-bv/", "https://www.bilibili.com/bangumi/play/ep1"} {
		if IsPublicVideoPage(raw) {
			t.Fatal("unsupported page accepted", raw)
		}
	}
}
func TestProgressiveSourceRejectsRestrictedTooLargeAndForeignHosts(t *testing.T) {
	good := `{"code":0,"data":{"timelength":100000,"durl":[{"url":"https://cdn.bilivideo.com/up/video.mp4?token=public-playback","size":1234}]}}`
	raw, duration, e := parseBiliProgressive([]byte(good), 2000, false)
	if e != nil || raw == "" || duration != 100000 {
		t.Fatal(raw, duration, e)
	}
	for _, bad := range []string{strings.Replace(good, `"code":0`, `"code":-10403`, 1), strings.Replace(good, `"timelength":100000`, `"timelength":100000,"is_preview":1`, 1), strings.Replace(good, "cdn.bilivideo.com", "cdn.bilivideo.com.evil.invalid", 1), strings.Replace(good, "100000", "900000", 1), strings.Replace(good, "1234", "4000", 1), strings.Replace(good, "https://cdn.bilivideo.com", "http://127.0.0.1", 1)} {
		if _, _, e = parseBiliProgressive([]byte(bad), 2000, false); e == nil {
			t.Fatal("unsafe source accepted")
		}
	}
}

func TestPlaybackSamplingRequiresDeclaredProgressiveMP4(t *testing.T) {
	const source = `{"code":0,"data":{"format":"mp4","timelength":900000,"durl":[{"url":"https://cdn.bilivideo.com/synthetic.mp4","size":1234}]}}`
	if _, duration, err := parseBiliProgressive([]byte(source), 2000, true); err != nil || duration != 900000 {
		t.Fatalf("small long media should remain eligible for a bounded sample: %d %v", duration, err)
	}
	for _, invalid := range []string{
		strings.Replace(source, `"code":0,`, "", 1),
		strings.Replace(source, `"mp4"`, `"flv"`, 1),
		strings.Replace(source, `"size":1234`, `"size":0`, 1),
		strings.Replace(source, `"format":"mp4"`, `"format":"mp4","is_preview":true`, 1),
		strings.Replace(source, `"format":"mp4"`, `"format":"mp4","is_preview":"1"`, 1),
	} {
		if _, _, err := parseBiliProgressive([]byte(invalid), 2000, true); err == nil {
			t.Fatal("unverified playback source accepted")
		}
	}
}
