//go:build windows

package stationbrowser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type douyinMediaTransport func(*http.Request) (*http.Response, error)

func (f douyinMediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func syntheticDouyinMedia(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "playback.webm"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLocalDouyinWebMediaRetrievesExactSourceWithBlobPlayer(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s, _ := serveDouyinWebPlaybackFixture(t, douyinWebPlaybackFixture{script: `v.src=URL.createObjectURL(new Blob([],{type:'video/webm'}));`})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got, err := readDouyinWebMedia(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/video/123", "123", func() error { return nil }, true, nil)
	if err != nil || got.ContentType != "video/webm" || !bytes.Equal(got.Data, syntheticDouyinMedia(t)) {
		t.Fatalf("exact original media unavailable: type=%q bytes=%d error=%v", got.ContentType, len(got.Data), err)
	}
}

func TestLocalDouyinWebMediaRejectsWrongDetailAndChallenge(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	for _, tc := range []struct {
		name    string
		fixture douyinWebPlaybackFixture
	}{
		{"wrong-detail", douyinWebPlaybackFixture{detailID: "124"}},
		{"wrong-page", douyinWebPlaybackFixture{redirect: true}},
		{"challenge", douyinWebPlaybackFixture{extra: `<form action=/login><input type=password></form>`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := serveDouyinWebPlaybackFixture(t, tc.fixture)
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
			defer cancel()
			got, err := readDouyinWebMedia(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/video/123", "123", func() error { return nil }, true, nil)
			if err == nil || len(got.Data) != 0 {
				t.Fatal("unverified media returned")
			}
		})
	}
}

type cancelledDouyinMediaBody struct{ ctx context.Context }

func (b cancelledDouyinMediaBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (cancelledDouyinMediaBody) Close() error               { return nil }

func TestLocalDouyinWebMediaPageSwitchInterruptsDownload(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	var downloading atomic.Bool
	var origin string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video/123":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<title>synthetic video</title><p>source</p><script>fetch('%s?aweme_id=123').then(()=>setInterval(async()=>{if(await (await fetch('/download-started')).json())location.href='/video/124'},25))</script>`, douyinDetailPath)
		case douyinDetailPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status_code":0,"aweme_detail":{"aweme_id":"123","video":{"play_addr":{"url_list":[%q]}}}}`, origin+"/media.webm")
		case "/download-started":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, downloading.Load())
		default:
			_, _ = fmt.Fprint(w, "<title>another video</title>different source")
		}
	}))
	defer s.Close()
	origin = s.URL
	client := &http.Client{Transport: douyinMediaTransport(func(r *http.Request) (*http.Response, error) {
		downloading.Store(true)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"video/webm"}}, ContentLength: -1, Body: cancelledDouyinMediaBody{ctx: r.Context()}, Request: r}, nil
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := readDouyinWebMedia(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/video/123", "123", func() error { return nil }, true, client)
	if !downloading.Load() || !errors.Is(err, ErrDouyinWebMediaUnavailable) || len(got.Data) != 0 {
		t.Fatalf("changed page retained download: started=%v bytes=%d error=%v", downloading.Load(), len(got.Data), err)
	}
}

func TestDouyinWebMediaDownloadStripsCredentialsAcrossApprovedRedirects(t *testing.T) {
	data := syntheticDouyinMedia(t)
	var calls int
	client := &http.Client{Transport: douyinMediaTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Range") != "" || r.Referer() != "https://www.douyin.com/" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Fatal("unexpected request credentials or noncanonical headers")
		}
		if calls == 1 {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://v26-web.douyinvod.com/final.webm"}, "Set-Cookie": []string{"synthetic=not-forwarded"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"video/webm"}}, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data)), Request: r}, nil
	})}
	got, err := fetchDouyinWebMedia(t.Context(), "https://v3-web.douyinvod.com/start.webm", client, func() error { return nil }, validDouyinWebMediaURL)
	if err != nil || calls != 2 || !bytes.Equal(got.Data, data) {
		t.Fatalf("approved redirect: calls=%d bytes=%d error=%v", calls, len(got.Data), err)
	}
}

func TestDouyinWebMediaDownloadDeniesUnapprovedOrCredentialURLsBeforeRequest(t *testing.T) {
	for _, target := range []string{"http://v3-web.douyinvod.com/media", "https://user:pass@v3-web.douyinvod.com/media", "https://127.0.0.1/media", "https://v3-web.douyinvod.com.evil.invalid/media", "https://v3-web.douyinvod.com:444/media", "https://v3-web.douyinvod.com/media#fragment"} {
		calls := 0
		client := &http.Client{Transport: douyinMediaTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("must not request") })}
		got, err := fetchDouyinWebMedia(t.Context(), target, client, func() error { return nil }, validDouyinWebMediaURL)
		if err == nil || calls != 0 || len(got.Data) != 0 {
			t.Fatal("unapproved initial target reached network")
		}
	}
	for _, destination := range []string{"http://127.0.0.1/private", "https://other.invalid/media", "https://user:pass@v3-web.douyinvod.com/media"} {
		calls := 0
		client := &http.Client{Transport: douyinMediaTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{destination}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		})}
		got, err := fetchDouyinWebMedia(t.Context(), "https://v3-web.douyinvod.com/source", client, func() error { return nil }, validDouyinWebMediaURL)
		if err == nil || calls != 1 || len(got.Data) != 0 {
			t.Fatal("unapproved redirect reached network")
		}
	}
}

func TestDouyinWebMediaDownloadRejectsPartialPlaylistAndNonMedia(t *testing.T) {
	data := syntheticDouyinMedia(t)
	for _, tc := range []struct {
		name    string
		status  int
		headers http.Header
		body    []byte
		length  int64
	}{
		{"partial", 206, http.Header{"Content-Type": []string{"video/webm"}}, data, int64(len(data))},
		{"partial-header", 200, http.Header{"Content-Type": []string{"video/webm"}, "Content-Range": []string{"bytes 0-10/20"}}, data, int64(len(data))},
		{"playlist", 200, http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}}, []byte("#EXTM3U"), 7},
		{"html", 200, http.Header{"Content-Type": []string{"text/html"}}, data, int64(len(data))},
		{"fake-video", 200, http.Header{"Content-Type": []string{"video/mp4"}}, []byte("<html>not video</html>"), 21},
		{"valid-webm-declared-mp4", 200, http.Header{"Content-Type": []string{"video/mp4"}}, data, int64(len(data))},
		{"valid-webm-declared-wav", 200, http.Header{"Content-Type": []string{"audio/wav"}}, data, int64(len(data))},
		{"compressed", 200, http.Header{"Content-Type": []string{"video/webm"}, "Content-Encoding": []string{"gzip"}}, data, int64(len(data))},
		{"truncated", 200, http.Header{"Content-Type": []string{"video/webm"}}, data, int64(len(data) + 1)},
		{"empty", 200, http.Header{"Content-Type": []string{"video/webm"}}, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: douyinMediaTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: tc.headers, Body: io.NopCloser(bytes.NewReader(tc.body)), ContentLength: tc.length, Request: r}, nil
			})}
			got, err := fetchDouyinWebMedia(t.Context(), "https://v3-web.douyinvod.com/media", client, func() error { return nil }, validDouyinWebMediaURL)
			if err == nil || len(got.Data) != 0 {
				t.Fatal("non-complete media accepted")
			}
		})
	}
	client := &http.Client{Transport: douyinMediaTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: r}, nil
	})}
	got, err := fetchDouyinWebMedia(t.Context(), "https://v3-web.douyinvod.com/media", client, func() error { return nil }, validDouyinWebMediaURL)
	if err != nil || got.ContentType != "video/webm" {
		t.Fatal("valid container could not be identified")
	}
}

type zeroDouyinMediaReader struct{}

func (zeroDouyinMediaReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestDouyinWebMediaDownloadEnforcesKnownAndUnknownSizeBounds(t *testing.T) {
	for _, known := range []bool{true, false} {
		client := &http.Client{Transport: douyinMediaTransport(func(r *http.Request) (*http.Response, error) {
			length := int64(-1)
			if known {
				length = maxPlaybackMediaBytes + 1
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"video/webm"}}, ContentLength: length, Body: io.NopCloser(io.MultiReader(bytes.NewReader([]byte{0x1a, 0x45, 0xdf, 0xa3}), io.LimitReader(zeroDouyinMediaReader{}, maxPlaybackMediaBytes))), Request: r}, nil
		})}
		got, err := fetchDouyinWebMedia(t.Context(), "https://v3-web.douyinvod.com/media", client, func() error { return nil }, validDouyinWebMediaURL)
		if !errors.Is(err, ErrDouyinWebMediaTooLarge) || len(got.Data) != 0 {
			t.Fatalf("size boundary known=%v: %v", known, err)
		}
	}
}

func TestDouyinWebMediaDownloadRevocationAndCancellationInterruptStalledBody(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRequest), func(t *testing.T) {
			var revoked atomic.Bool
			denied := errors.New("synthetic media permission revoked")
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			stopped := make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(stopped)
				w.Header().Set("Content-Type", "video/webm")
				w.WriteHeader(200)
				_, _ = w.Write([]byte{0x1a, 0x45, 0xdf, 0xa3})
				w.(http.Flusher).Flush()
				time.AfterFunc(150*time.Millisecond, func() {
					if cancelRequest {
						cancel()
					} else {
						revoked.Store(true)
					}
				})
				<-r.Context().Done()
			}))
			defer s.Close()
			started := time.Now()
			got, err := fetchDouyinWebMedia(ctx, s.URL+"/media", s.Client(), func() error {
				if revoked.Load() {
					return denied
				}
				return nil
			}, func(raw string) bool { return validLocalDouyinMediaURL(raw, s.URL+"/video/123") })
			want := denied
			if cancelRequest {
				want = context.Canceled
			}
			if !errors.Is(err, want) || len(got.Data) != 0 || time.Since(started) > 2*time.Second {
				t.Fatalf("stalled read survived revocation/cancellation: elapsed=%v error=%v", time.Since(started), err)
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("HTTP request remained alive")
			}
		})
	}
}

func TestDouyinWebMediaClientUsesOnlyPrivateProxyAndSanitizedErrors(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://synthetic.invalid:1234")
	client, closeClient, err := newDouyinWebMediaClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	request, _ := http.NewRequest("GET", "https://v3-web.douyinvod.com/media", nil)
	transport := client.Transport.(*http.Transport)
	proxy, err := transport.Proxy(request)
	if err != nil || proxy.Hostname() != "127.0.0.1" || client.Jar != nil || !transport.DisableCompression || transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("client bypassed private proxy or imported unsafe state")
	}
	client = &http.Client{Transport: douyinMediaTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("synthetic signed URL must remain private")
	})}
	_, err = fetchDouyinWebMedia(t.Context(), request.URL.String(), client, func() error { return nil }, validDouyinWebMediaURL)
	if !errors.Is(err, ErrDouyinWebMediaUnavailable) || strings.Contains(err.Error(), "signed URL") {
		t.Fatal("upstream error leaked private request information")
	}
}
