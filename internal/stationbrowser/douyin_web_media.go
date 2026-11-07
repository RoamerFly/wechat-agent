package stationbrowser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrDouyinWebMediaUnavailable = errors.New("未取得可核对的完整视频媒体，网页音轨暂不可读取")
	ErrDouyinWebMediaTooLarge    = errors.New("网页视频媒体超过当前 32 MiB 的读取上限")
)

// DouyinWebMedia carries only a complete, bounded media body obtained from the
// exact video's observed detail. The signed URL and login credentials remain
// inside the browser host. Receiving bytes is not playback or transcription.
type DouyinWebMedia struct {
	Data        []byte
	ContentType string
}

func isDouyinWebMediaHost(host string) bool {
	host = strings.ToLower(host)
	return strings.HasSuffix(host, ".douyinvod.com") || strings.HasSuffix(host, ".ibytedtos.com")
}

func validDouyinWebMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 8192 && u.Scheme == "https" && u.User == nil && u.Port() == "" && u.Fragment == "" && u.Opaque == "" && playerProxyHostSyntax(u.Host) && isDouyinWebMediaHost(u.Host)
}

// This client never imports Edge cookies, HTTP credentials, an environment
// proxy, or a CookieJar. The CONNECT worker resolves approved hosts to public
// addresses and pins the actual dial. Redirects are followed explicitly below
// so every destination is checked before a request and gets fresh fixed headers.
func newDouyinWebMediaClient(ctx context.Context) (*http.Client, func(), error) {
	proxy, err := openPlayerProxy(ctx, isDouyinWebMediaHost)
	if err != nil {
		return nil, nil, ErrDouyinWebMediaUnavailable
	}
	proxyURL, err := url.Parse("http://" + proxy.address())
	if err != nil {
		_ = proxy.Close()
		return nil, nil, ErrDouyinWebMediaUnavailable
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableCompression: true, DisableKeepAlives: true, ResponseHeaderTimeout: 15 * time.Second, TLSHandshakeTimeout: 10 * time.Second, MaxResponseHeaderBytes: 64 << 10}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, func() { transport.CloseIdleConnections(); _ = proxy.Close() }, nil
}

func fetchDouyinWebMedia(ctx context.Context, raw string, client *http.Client, authorize func() error, allowURL func(string) bool) (DouyinWebMedia, error) {
	if client == nil || authorize == nil || allowURL == nil {
		return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
	}
	// A revoked grant must also interrupt a stalled request/body, rather than
	// waiting for another Read to return. The same callback checks the original
	// browser page and host permission without passing any credentials along.
	guardCtx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var revoked error
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-guardCtx.Done():
				return
			case <-tick.C:
				if err := authorize(); err != nil {
					mu.Lock()
					revoked = err
					mu.Unlock()
					cancel()
					return
				}
			}
		}
	}()
	media, err := fetchDouyinWebMediaGuarded(guardCtx, raw, client, authorize, allowURL)
	cancel()
	<-done
	mu.Lock()
	denied := revoked
	mu.Unlock()
	if denied != nil {
		return DouyinWebMedia{}, denied
	}
	return media, err
}

func fetchDouyinWebMediaGuarded(ctx context.Context, raw string, client *http.Client, authorize func() error, allowURL func(string) bool) (DouyinWebMedia, error) {
	// Do not let a fixture or a future caller accidentally enable automatic
	// redirects/cookie propagation inside this narrowly scoped downloader.
	localClient := *client
	localClient.Jar = nil
	localClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for hops := 0; hops <= 5; hops++ {
		if err := ctx.Err(); err != nil {
			return DouyinWebMedia{}, err
		}
		if err := authorize(); err != nil {
			return DouyinWebMedia{}, err
		}
		if !allowURL(raw) {
			return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
		}
		req.Header.Set("Referer", "https://www.douyin.com/")
		req.Header.Set("Accept", "video/*, audio/*, application/octet-stream")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := localClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return DouyinWebMedia{}, ctx.Err()
			}
			if err := authorize(); err != nil {
				return DouyinWebMedia{}, err
			}
			return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
		}
		if err := authorize(); err != nil {
			_ = resp.Body.Close()
			return DouyinWebMedia{}, err
		}
		if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusPermanentRedirect {
			next, err := resp.Location()
			_ = resp.Body.Close()
			if err != nil || !allowURL(next.String()) {
				return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
			}
			raw = next.String()
			continue
		}
		media, err := readDouyinWebMediaBody(ctx, resp, authorize)
		_ = resp.Body.Close()
		return media, err
	}
	return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
}

func readDouyinWebMediaBody(ctx context.Context, resp *http.Response, authorize func() error) (DouyinWebMedia, error) {
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Range") != "" || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
	}
	if resp.ContentLength > maxPlaybackMediaBytes {
		return DouyinWebMedia{}, ErrDouyinWebMediaTooLarge
	}
	declared := ""
	if value := resp.Header.Get("Content-Type"); value != "" {
		var err error
		declared, _, err = mime.ParseMediaType(value)
		if err != nil {
			return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
		}
		declared = strings.ToLower(declared)
		if declared != "application/octet-stream" && (!safeMediaType(declared) || strings.Contains(declared, "mpegurl")) {
			return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
		}
	}
	reader := io.LimitReader(resp.Body, maxPlaybackMediaBytes+1)
	var out bytes.Buffer
	buffer := make([]byte, 64<<10)
	lastCheck, unchecked := time.Now(), 0
	for {
		if err := ctx.Err(); err != nil {
			return DouyinWebMedia{}, err
		}
		if unchecked >= 1<<20 || time.Since(lastCheck) >= 100*time.Millisecond {
			if err := authorize(); err != nil {
				return DouyinWebMedia{}, err
			}
			lastCheck, unchecked = time.Now(), 0
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			out.Write(buffer[:n])
			unchecked += n
		}
		if out.Len() > maxPlaybackMediaBytes {
			return DouyinWebMedia{}, ErrDouyinWebMediaTooLarge
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return DouyinWebMedia{}, ctx.Err()
			}
			return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
		}
	}
	if err := authorize(); err != nil {
		return DouyinWebMedia{}, err
	}
	if out.Len() == 0 || resp.ContentLength >= 0 && int64(out.Len()) != resp.ContentLength {
		return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
	}
	data := out.Bytes()
	detected := douyinMediaContainerType(data)
	if detected == "" {
		return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
	}
	if declared == "" || declared == "application/octet-stream" {
		declared = detected
	} else if declared != detected {
		// No MIME aliases are assumed here. A valid media container with an
		// unrelated declaration must not be relabelled as that other format.
		return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
	}
	return DouyinWebMedia{Data: data, ContentType: declared}, nil
}

func douyinMediaContainerType(data []byte) string {
	switch {
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return "video/mp4"
	case len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "video/webm"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "audio/wav"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "AVI ":
		return "video/avi"
	case len(data) >= 4 && string(data[:4]) == "OggS":
		return "audio/ogg"
	case len(data) >= 4 && string(data[:4]) == "fLaC":
		return "audio/flac"
	case len(data) >= 3 && string(data[:3]) == "ID3":
		return "audio/mpeg"
	case len(data) >= 2 && data[0] == 0xff && data[1]&0xe0 == 0xe0:
		return "audio/mpeg"
	case len(data) > 376 && data[0] == 0x47 && data[188] == 0x47 && data[376] == 0x47:
		return "video/mp2t"
	}
	return ""
}
