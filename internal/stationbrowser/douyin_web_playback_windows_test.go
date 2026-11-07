//go:build windows

package stationbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/hostfile"
)

func TestDouyinWebPlaybackExactSourceRules(t *testing.T) {
	for _, raw := range []string{"https://www.douyin.com/video/123?other=1", "https://www.douyin.com/video/124", "https://evil.invalid/video/123", "http://127.0.0.1:1234/video/123"} {
		if validDouyinWebPlaybackTarget(raw, "123", false) {
			t.Fatal("unexpected production target accepted")
		}
	}
	for _, fixture := range []bool{false, true} {
		target := "https://www.douyin.com/video/123"
		media := "https://v26-web.douyinvod.com/synthetic/media.webm"
		if fixture {
			target = "http://127.0.0.1:1234/video/123"
			media = "http://127.0.0.1:1234/media.webm"
		}
		g := newDouyinWebDetailListener(target, "123", "s", fixture)
		origin := strings.TrimSuffix(target, "/video/123")
		if !g.detailURL(origin + douyinDetailPath + "?aweme_id=123&synthetic=yes") {
			t.Fatal("exact detail not accepted")
		}
		for _, suffix := range []string{"?aweme_id=124", "?aweme_id=123&aweme_id=123", "?aweme_id=123#fragment"} {
			if g.detailURL(origin + douyinDetailPath + suffix) {
				t.Fatal("mismatched detail accepted")
			}
		}
		body := func(id string, urls []string) []byte {
			b, _ := json.Marshal(map[string]any{"status_code": 0, "aweme_detail": map[string]any{"aweme_id": id, "video": map[string]any{"play_addr": map[string]any{"url_list": urls}}}})
			return b
		}
		urls, err := g.parseDetail(body("123", []string{media, media}))
		if err != nil || len(urls) != 1 || urls[0] != media {
			t.Fatalf("exact source mapping failed: %v", err)
		}
		for _, raw := range [][]byte{body("124", []string{media}), body("123", nil), body("123", []string{"https://evil.invalid/media"}), []byte(strings.Repeat(" ", maxDouyinWebDetailBytes+1))} {
			if _, err := g.parseDetail(raw); err == nil {
				t.Fatal("unverified detail accepted")
			}
		}
	}
}

type douyinWebPlaybackFixture struct {
	extra, actualPath, detailID, script     string
	redirect, noMedia, hiddenRecommendation bool
	onPlaying                               func()
}

func serveDouyinWebPlaybackFixture(t *testing.T, fixture douyinWebPlaybackFixture) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	media, err := os.ReadFile(filepath.Join("testdata", "playback.webm"))
	if err != nil {
		t.Fatal(err)
	}
	var origin string
	requests := &atomic.Int32{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/playing":
			if fixture.onPlaying != nil {
				fixture.onPlaying()
			}
			w.WriteHeader(http.StatusNoContent)
		case douyinDetailPath:
			w.Header().Set("Content-Type", "application/json")
			id := fixture.detailID
			if id == "" {
				id = "123"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 0, "aweme_detail": map[string]any{"aweme_id": id, "video": map[string]any{"play_addr": map[string]any{"url_list": []string{origin + "/media.webm"}}}}})
		case "/media.webm", "/other.webm":
			w.Header().Set("Content-Type", "video/webm")
			http.ServeContent(w, r, "fixture.webm", time.Time{}, strings.NewReader(string(media)))
		case "/video/123":
			if fixture.redirect {
				http.Redirect(w, r, "/video/124", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/html;charset=utf-8")
			video := `<video id="v" width="320" height="180" preload="auto" playsinline></video>`
			if fixture.noMedia {
				video = ""
			}
			if fixture.hiddenRecommendation {
				video += `<video style="display:none" src="/other.webm"></video>`
			}
			path := fixture.actualPath
			if path == "" {
				path = "/media.webm"
			}
			_, _ = fmt.Fprintf(w, `<title>synthetic video</title><p>synthetic detail</p>%s%s<script>fetch('%s?aweme_id=123').then(r=>r.json()).then(()=>{const v=document.getElementById('v');if(v)v.src=%q;%s})</script>`, video, fixture.extra, douyinDetailPath, path, fixture.script)
		default:
			_, _ = w.Write([]byte(`<title>another video</title><p>wrong video</p>`))
		}
	}))
	origin = s.URL
	t.Cleanup(s.Close)
	return s, requests
}

func TestLocalDouyinWebPlaybackSamplesOnlyBoundMedia(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s, _ := serveDouyinWebPlaybackFixture(t, douyinWebPlaybackFixture{hiddenRecommendation: true})
	for _, tc := range []struct {
		name    string
		seconds time.Duration
		ended   bool
	}{{"partial", time.Second, false}, {"ended", 6 * time.Second, true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			var calls atomic.Int32
			got, err := sampleDouyinWebVideo(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/video/123", "123", tc.seconds, func() error { calls.Add(1); return nil }, true)
			if err != nil {
				t.Fatalf("source bound playback failed: %v", err)
			}
			if !got.Ready || !got.Paused || got.Ended != tc.ended || got.DurationSeconds < 3.5 || len(got.Intervals) != 1 || got.Intervals[0].StartSeconds != 0 || got.Intervals[0].EndSeconds < 0.45 {
				t.Fatalf("invalid actual playback: %+v", got)
			}
			if tc.ended && got.Intervals[0].EndSeconds < got.DurationSeconds {
				t.Fatal("ended sample lost actual final range")
			}
			if calls.Load() < 4 {
				t.Fatal("authorization was not checked throughout playback")
			}
		})
	}
}

func TestLocalDouyinWebPlaybackRejectsUnboundAndChangedSources(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	for _, tc := range []struct {
		name    string
		fixture douyinWebPlaybackFixture
	}{
		{"wrong-metadata", douyinWebPlaybackFixture{detailID: "124"}},
		{"wrong-media", douyinWebPlaybackFixture{actualPath: "/other.webm"}},
		{"two-visible-videos", douyinWebPlaybackFixture{extra: `<video width=80 height=80 src="/other.webm"></video>`}},
		{"redirect", douyinWebPlaybackFixture{redirect: true}},
		{"source-replaced", douyinWebPlaybackFixture{script: `setTimeout(()=>v.src='/other.webm',500)`}},
		{"element-replaced", douyinWebPlaybackFixture{script: `setTimeout(()=>{const replacement=v.cloneNode();v.replaceWith(replacement)},500)`}},
		{"visible-login", douyinWebPlaybackFixture{extra: `<form action=/login><input type=password></form>`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := serveDouyinWebPlaybackFixture(t, tc.fixture)
			ctx, cancel := context.WithTimeout(t.Context(), 7*time.Second)
			defer cancel()
			got, err := sampleDouyinWebVideo(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/video/123", "123", time.Second, func() error { return nil }, true)
			if err == nil || got.Ready || len(got.Intervals) != 0 {
				t.Fatalf("unverified playback evidence escaped: sample=%+v error=%v", got, err)
			}
		})
	}
}

func TestLocalDouyinWebPlaybackMissingMediaAndCancellationReturnNoEvidence(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s, _ := serveDouyinWebPlaybackFixture(t, douyinWebPlaybackFixture{noMedia: true})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got, err := sampleDouyinWebVideo(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/video/123", "123", time.Second, func() error { return nil }, true)
	if err == nil || got.Ready || len(got.Intervals) != 0 {
		t.Fatal("missing media invented playback")
	}
	s2, _ := serveDouyinWebPlaybackFixture(t, douyinWebPlaybackFixture{})
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	var calls atomic.Int32
	got, err = sampleDouyinWebVideo(ctx2, filepath.Join(t.TempDir(), "profile2"), s2.URL+"/video/123", "123", 2*time.Second, func() error {
		if calls.Add(1) == 4 {
			cancel2()
		}
		return nil
	}, true)
	if err == nil || got.Ready || len(got.Intervals) != 0 {
		t.Fatal("cancelled playback returned evidence")
	}
}

func TestLocalDouyinWebPlaybackAuthorizationAndProfileLock(t *testing.T) {
	denied := errors.New("synthetic permission revoked")
	s, requests := serveDouyinWebPlaybackFixture(t, douyinWebPlaybackFixture{})
	profile := filepath.Join(t.TempDir(), "profile")
	if _, err := sampleDouyinWebVideo(t.Context(), profile, s.URL+"/video/123", "123", time.Second, func() error { return denied }, true); !errors.Is(err, denied) || requests.Load() != 0 {
		t.Fatal("unauthorized call reached a website")
	}
	lock, err := DouyinLoginProfileLockPath(profile)
	if err != nil {
		t.Fatal(err)
	}
	release, err := hostfile.TryLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := sampleDouyinWebVideo(t.Context(), profile, s.URL+"/video/123", "123", time.Second, func() error { return nil }, true); !errors.Is(err, ErrDouyinLoginActive) || requests.Load() != 0 {
		t.Fatal("active login profile was used")
	}
}

func TestLocalDouyinWebPlaybackRevocationAfterActualPlayReturnsNoEvidence(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	var played atomic.Bool
	s, _ := serveDouyinWebPlaybackFixture(t, douyinWebPlaybackFixture{script: `v.addEventListener('playing',()=>fetch('/playing'),{once:true})`, onPlaying: func() { played.Store(true) }})
	denied := errors.New("synthetic playback grant revoked")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	got, err := sampleDouyinWebVideo(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/video/123", "123", 2*time.Second, func() error {
		if played.Load() {
			return denied
		}
		return nil
	}, true)
	if !played.Load() || !errors.Is(err, denied) || got.Ready || len(got.Intervals) != 0 {
		t.Fatalf("revoked playback escaped: played=%v sample=%+v error=%v", played.Load(), got, err)
	}
}
