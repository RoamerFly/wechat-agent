package stationbrowser

import (
	"net/url"
	"testing"
)

const fixtureDouyinPlayer = "https://open.douyin.com/player/video?autoplay=0&vid=7356906132482428172"

func TestCanonicalDouyinPlayer(t *testing.T) {
	got, ok := canonicalDouyinPlayer(fixtureDouyinPlayer)
	if !ok || got != "7356906132482428172" {
		t.Fatalf("canonicalDouyinPlayer() = %q, %v", got, ok)
	}
	for _, target := range []string{
		"http://open.douyin.com/player/video?autoplay=0&vid=7356906132482428172",
		"https://open.douyin.com/player/video?vid=7356906132482428172&autoplay=0",
		"https://open.douyin.com/player/video?autoplay=1&vid=7356906132482428172",
		"https://open.douyin.com/player/video?autoplay=0&vid=abc",
		"https://open.douyin.com/player/video?autoplay=0&vid=7356906132482428172&x=1",
		"https://www.douyin.com/player/video?autoplay=0&vid=7356906132482428172",
		"https://open.douyin.com/player/video?autoplay=0&vid=7356906132482428172#x",
	} {
		if _, ok := canonicalDouyinPlayer(target); ok {
			t.Fatalf("canonicalDouyinPlayer(%q) unexpectedly allowed", target)
		}
	}
}

func TestDouyinPlaybackGateAllowsOnlyVerifiedWire(t *testing.T) {
	gate := newDouyinPlaybackGate(fixtureDouyinPlayer, "7356906132482428172", "session", func() error { return nil })
	allowed, embed, detail, media := gate.allowRequest("Document", fixtureDouyinPlayer, "GET", "", "root")
	if !allowed || embed || detail || media {
		t.Fatalf("document gate = %v,%v,%v,%v", allowed, embed, detail, media)
	}
	for _, tc := range []struct {
		name, typ, rawURL, method, body string
		want                            [4]bool
	}{
		{"static js", "Script", "https://lf-developer-cdn.open-douyin.com/obj/open-platform-scm/open_douyin_sdk_demo/static/js/main.js", "GET", "", [4]bool{true, false, false, false}},
		{"static css", "Stylesheet", "https://lf-developer-cdn.open-douyin.com/obj/open-platform-scm/open_douyin_sdk_demo/static/css/main.css", "GET", "", [4]bool{true, false, false, false}},
		{"security", "Script", "https://lf-c-flwb.bytetos.com/obj/rc-client-security/c-webmssdk/1.0/webmssdk.js", "GET", "", [4]bool{true, false, false, false}},
		{"embed", "XHR", "https://open.douyin.com/api/douyin/v1/open/auth/embed_video/", "POST", `{"item_id":"7356906132482428172"}`, [4]bool{true, true, false, false}},
		{"detail", "XHR", "https://www.douyin.com/aweme/v1/web/aweme/detail/?aid=6383&aweme_id=7356906132482428172&msToken=x&X-Bogus=y&_signature=z", "GET", "", [4]bool{true, false, true, false}},
		{"media", "Media", "https://v26-web.douyinvod.com/a/b/video/tos/cn/tos-cn-ve-15/c?signature=opaque", "GET", "", [4]bool{true, false, false, true}},
		{"telemetry", "XHR", "https://mcs.zijieapi.com/list", "POST", "{}", [4]bool{}},
		{"cross video", "XHR", "https://www.douyin.com/aweme/v1/web/aweme/detail/?aid=6383&aweme_id=999&msToken=x&X-Bogus=y&_signature=z", "GET", "", [4]bool{}},
		{"embed extras", "XHR", "https://open.douyin.com/api/douyin/v1/open/auth/embed_video/", "POST", `{"item_id":"7356906132482428172","x":1}`, [4]bool{}},
		{"download", "Document", "https://open.douyin.com/download", "GET", "", [4]bool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := [4]bool{}
			got[0], got[1], got[2], got[3] = gate.allowRequest(tc.typ, tc.rawURL, tc.method, tc.body, "root")
			if got != tc.want {
				t.Fatalf("allowRequest(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestExactDouyinProtocolDataRejectsAmbiguity(t *testing.T) {
	videoID := "7356906132482428172"
	for _, body := range []string{"", `[]`, `{"item_id":7356906132482428172}`, `{"item_id":"other"}`, `{"item_id":"7356906132482428172","extra":true}`} {
		if exactEmbedItemID(body, videoID) {
			t.Fatalf("exactEmbedItemID(%s) unexpectedly allowed", body)
		}
	}
	if !exactEmbedItemID(`{"item_id":"7356906132482428172"}`, videoID) {
		t.Fatal("exact embed body rejected")
	}
	if !exactDetailQuery(url.Values{"aid": {"6383"}, "aweme_id": {videoID}, "msToken": {""}, "X-Bogus": {"y"}, "_signature": {"z"}}, videoID) {
		t.Fatal("fresh-profile detail query with empty msToken rejected")
	}
	for _, q := range []url.Values{
		{"aid": {"6383"}, "aweme_id": {videoID}, "msToken": {"x"}, "X-Bogus": {"y"}, "_signature": {"z"}, "extra": {"bad"}},
		{"aid": {"6383"}, "aweme_id": {"other"}, "msToken": {"x"}, "X-Bogus": {"y"}, "_signature": {"z"}},
		{"aid": {"6383", "more"}, "aweme_id": {videoID}, "msToken": {"x"}, "X-Bogus": {"y"}, "_signature": {"z"}},
	} {
		if exactDetailQuery(q, videoID) {
			t.Fatalf("exactDetailQuery(%v) unexpectedly allowed", q)
		}
	}
}

func TestDouyinGateRejectsLateFrameChangeAfterLoad(t *testing.T) {
	gate := newDouyinPlaybackGate(fixtureDouyinPlayer, "7356906132482428172", "session", func() error { return nil })
	if allowed, _, _, _ := gate.allowRequest("Document", fixtureDouyinPlayer, "GET", "", "root"); !allowed {
		t.Fatal("fixture document rejected")
	}
	gate.mu.Lock()
	gate.loaded, gate.status, gate.embedVerified, gate.detailVerified, gate.mediaSeen = true, 200, true, true, true
	gate.mu.Unlock()
	gate.finish(nil)
	gate.finish(ErrPlaybackUnavailable)
	if err := gate.validate(); err == nil {
		t.Fatal("late terminal error was suppressed after document readiness")
	}
	if err := gate.waitDocument(t.Context()); err == nil {
		t.Fatal("waitDocument lost late terminal error")
	}
	if allowed, _, _, _ := gate.allowRequest("Script", "https://lf-developer-cdn.open-douyin.com/obj/open-platform-scm/open_douyin_sdk_demo/static/js/main.js", "GET", "", "child"); allowed {
		t.Fatal("child-frame request was allowed")
	}
}

func TestDouyinPlaybackHosts(t *testing.T) {
	for _, host := range []string{douyinPlayerHost, douyinDetailHost, douyinStaticHost, douyinSecurityHost, douyinMediaHost} {
		if !isDouyinPlaybackHost(host) {
			t.Fatalf("required host %q was denied", host)
		}
	}
	if isDouyinPlaybackHost("mcs.zijieapi.com") || isDouyinPlaybackHost("open.douyin.com.evil.example") {
		t.Fatal("unapproved host allowed")
	}
}
