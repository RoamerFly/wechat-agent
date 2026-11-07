package adapter

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

type uploadRoundTripper func(*http.Request) (*http.Response, error)

func (f uploadRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func uploadResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func uploadFixture(t *testing.T, run uploadRoundTripper) *OpenUploadClient {
	t.Helper()
	c, err := NewOpenUploadClient(OpenCredentials{ClientID: "synthetic-client", AppSecret: "synthetic-secret", AccessToken: "synthetic-oauth"})
	if err != nil {
		t.Fatal(err)
	}
	c.client.Transport = run
	c.now = func() time.Time { return time.Unix(1700000000, 0) }
	return c
}

// The transport observes the wire contract rather than replacing any of the
// upload methods. No external host is contacted and all credentials are synthetic.
func TestOpenUploadSequenceAndCredentialBoundary(t *testing.T) {
	const token = "opaque+/=?#&-token"
	var calls int
	var nonces []string
	parts := [][]byte{{0, 255, 1, 2}, {3, 4}}
	c := uploadFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.Header.Get("Accept") != "application/json" || r.ContentLength != int64(len(body)) {
			t.Fatalf("request shape: %s %s length %d/%d", r.Method, r.URL, r.ContentLength, len(body))
		}
		switch calls {
		case 1, 4:
			if r.URL.Host != "member.bilibili.com" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Access-Token") != "synthetic-oauth" {
				t.Fatal("signed endpoint or headers changed")
			}
			assertUploadSignature(t, r, body)
			nonces = append(nonces, r.Header.Get("X-Bili-Signature-Nonce"))
			if calls == 1 {
				if r.URL.Path != "/arcopen/fn/archive/video/init" || r.URL.RawQuery != "" || string(body) != `{"name":"合成视频.mp4","utype":"0"}` {
					t.Fatal("initialization wire shape changed")
				}
				return uploadResponse(200, `{"code":0,"data":{"upload_token":"`+token+`"}}`), nil
			}
			if r.URL.Path != "/arcopen/fn/archive/video/complete" || r.URL.Query().Get("upload_token") != token || len(body) != 0 {
				t.Fatal("completion wire shape changed")
			}
		case 2, 3:
			if r.URL.Host != "openupos.bilivideo.com" || r.URL.Path != "/video/v2/part/upload" || r.URL.Query().Get("upload_token") != token || r.URL.Query().Get("part_number") != fmt.Sprint(calls-1) {
				t.Fatal("part target changed")
			}
			if r.Header.Get("Content-Type") != "application/octet-stream" || string(body) != string(parts[calls-2]) {
				t.Fatal("part is not exact raw bytes")
			}
			for key := range r.Header {
				if strings.HasPrefix(strings.ToLower(key), "x-bili-") || strings.EqualFold(key, "Access-Token") || strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Cookie") {
					t.Fatalf("credential header reached part host: %s", key)
				}
			}
		default:
			t.Fatalf("unexpected repeated request %d", calls)
		}
		return uploadResponse(200, `{"code":0}`), nil
	})
	got, err := c.Initialize(t.Context(), "合成视频.mp4")
	if err != nil || got != token {
		t.Fatalf("initialize: %q / %v", got, err)
	}
	for i, part := range parts {
		if err := c.UploadPart(t.Context(), got, int64(i+1), part); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Complete(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || len(nonces) != 2 || nonces[0] == nonces[1] {
		t.Fatalf("requests/nonces: %d / %v", calls, nonces)
	}
}

func assertUploadSignature(t *testing.T, r *http.Request, body []byte) {
	t.Helper()
	keys := []string{"x-bili-timestamp", "x-bili-signature-version", "x-bili-signature-nonce", "x-bili-signature-method", "x-bili-content-md5", "x-bili-accesskeyid"}
	sort.Strings(keys)
	var lines []string
	for _, key := range keys {
		lines = append(lines, key+":"+r.Header.Get(key))
	}
	digest := md5.Sum(body)
	if r.Header.Get("X-Bili-Content-Md5") != hex.EncodeToString(digest[:]) || r.Header.Get("X-Bili-Timestamp") != "1700000000" || r.Header.Get("X-Bili-Signature-Method") != "HMAC-SHA256" || r.Header.Get("X-Bili-Signature-Version") != "2.0" {
		t.Fatal("signing inputs differ from exact request body or protocol")
	}
	mac := hmac.New(sha256.New, []byte("synthetic-secret"))
	_, _ = mac.Write([]byte(strings.Join(lines, "\n")))
	if r.Header.Get("Authorization") != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("signature does not match canonical sorted headers")
	}
}

func TestOpenUploadProtocolFailuresNeverRetryOrExposeProviderText(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		want       error
		kind       string
	}{
		{"missing-code", `{}`, 200, ErrOpenUploadResponse, ""},
		{"duplicate-code", `{"code":127001,"code":0}`, 200, ErrOpenUploadResponse, ""},
		{"null-code", `{"code":null}`, 200, ErrOpenUploadResponse, ""},
		{"string-code", `{"code":"0"}`, 200, ErrOpenUploadResponse, ""},
		{"malformed", `{`, 200, ErrOpenUploadResponse, ""},
		{"trailing-data", `{"code":0}{}`, 200, ErrOpenUploadResponse, ""},
		{"oversized", strings.Repeat(" ", openUploadResponseLimit+1), 200, ErrOpenUploadResponse, ""},
		{"http-failure", "synthetic-oauth", 503, ErrOpenUploadUnavailable, ""},
		{"http-rate", "synthetic-oauth", 429, nil, "限流"},
		{"http-auth", "synthetic-oauth", 401, nil, "鉴权"},
		{"http-permission", "synthetic-oauth", 403, nil, "权限"},
		{"invalid-token", `{"code":127001,"message":"synthetic-oauth"}`, 200, nil, "鉴权"},
		{"user-permission", `{"code":127011}`, 200, nil, "权限"},
		{"signature", `{"code":4002}`, 200, nil, "参数"},
		{"rate", `{"code":127306}`, 200, nil, "限流"},
		{"processing", `{"code":123028}`, 200, nil, "处理中"},
		{"provider-service", `{"code":4011}`, 200, nil, "服务暂不可用"},
		{"unknown-code", `{"code":199999}`, 200, nil, "请求被拒绝"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := uploadFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				return uploadResponse(tc.status, tc.body), nil
			})
			err := c.UploadPart(t.Context(), "synthetic-upload-token", 1, []byte{1})
			if err == nil || calls != 1 || strings.Contains(err.Error(), "synthetic-") {
				t.Fatalf("failure/calls/redaction: %v / %d", err, calls)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if tc.kind != "" {
				var apiErr *OpenAPIError
				if !errors.As(err, &apiErr) || apiErr.Kind != tc.kind {
					t.Fatalf("want %s, got %v", tc.kind, err)
				}
			}
		})
	}
}

func TestOpenUploadInvalidInputsAndTokens(t *testing.T) {
	for _, value := range []string{"", "line\r\ninjection", "space token", "中文", strings.Repeat("x", 4097)} {
		if _, err := NewOpenUploadClient(OpenCredentials{ClientID: "id", AppSecret: "secret", AccessToken: value}); !errors.Is(err, ErrOpenUploadInvalid) {
			t.Fatalf("accepted invalid credentials: %v", err)
		}
	}
	calls := 0
	c := uploadFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return uploadResponse(200, `{"code":0}`), nil
	})
	for _, name := range []string{"", ".", "..", "../video.mp4", `C:\video.mp4`, "name\x00.mp4"} {
		if _, err := c.Initialize(t.Context(), name); !errors.Is(err, ErrOpenUploadInvalid) {
			t.Fatalf("accepted unsafe basename %q: %v", name, err)
		}
	}
	for _, token := range []string{"", "\r\n", "a b", "中文"} {
		if err := c.Complete(t.Context(), token); !errors.Is(err, ErrOpenUploadInvalid) {
			t.Fatalf("accepted unsafe token: %v", err)
		}
	}
	if err := c.UploadPart(t.Context(), "token", 0, []byte{1}); !errors.Is(err, ErrOpenUploadInvalid) {
		t.Fatal(err)
	}
	if err := c.UploadPart(t.Context(), "token", 1, nil); !errors.Is(err, ErrOpenUploadInvalid) || calls != 0 {
		t.Fatalf("invalid input reached transport: %v / %d", err, calls)
	}
	for _, body := range []string{`{"code":0}`, `{"code":0,"data":{"upload_token":""}}`, `{"code":0,"data":{"upload_token":"a b"}}`} {
		c.client.Transport = uploadRoundTripper(func(*http.Request) (*http.Response, error) { return uploadResponse(200, body), nil })
		if _, err := c.Initialize(t.Context(), "video.mp4"); !errors.Is(err, ErrOpenUploadResponse) {
			t.Fatalf("invalid initialization accepted: %v", err)
		}
	}
}

func TestOpenUploadCancellationAndTransportFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := uploadFixture(t, func(r *http.Request) (*http.Response, error) {
		cancel()
		return nil, errors.New("synthetic-oauth and secret transport details")
	})
	if err := c.Complete(ctx, "token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if err := c.Complete(t.Context(), "token"); !errors.Is(err, ErrOpenUploadUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe transport error: %v", err)
	}
	c.nonce = func() (string, error) { return "", errors.New("entropy failure") }
	if _, err := c.Initialize(t.Context(), "video.mp4"); !errors.Is(err, ErrOpenUploadUnavailable) {
		t.Fatal(err)
	}
}

func TestOpenUploadTransportDoesNotFollowRedirects(t *testing.T) {
	var reached bool
	var calls int
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer destination.Close()
	c := uploadFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("unexpected redirect request")
		}
		response := uploadResponse(http.StatusTemporaryRedirect, "")
		response.Header.Set("Location", destination.URL)
		return response, nil
	})
	if _, err := c.Initialize(t.Context(), "video.mp4"); !errors.Is(err, ErrOpenUploadUnavailable) || reached || calls != 1 {
		t.Fatalf("redirect was accepted/followed: %v / %v / %d", err, reached, calls)
	}
	production, err := NewOpenUploadClient(OpenCredentials{ClientID: "id", AppSecret: "secret", AccessToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := production.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || production.client.Jar != nil || production.client.Timeout <= 0 || transport.TLSHandshakeTimeout <= 0 {
		t.Fatal("production transport lost direct connection, timeout, or cookie isolation")
	}
}

func TestOpenUploadTimeoutIsConfigurable(t *testing.T) {
	credentials := OpenCredentials{ClientID: "id", AppSecret: "secret", AccessToken: "token"}
	c, err := NewOpenUploadClientWithOptions(credentials, OpenUploadOptions{Timeout: 15 * time.Minute})
	if err != nil || c.client.Timeout != 15*time.Minute {
		t.Fatalf("explicit timeout was clamped: %v", err)
	}
	if _, err := NewOpenUploadClientWithOptions(credentials, OpenUploadOptions{Timeout: -time.Second}); !errors.Is(err, ErrOpenUploadInvalid) {
		t.Fatalf("negative timeout accepted: %v", err)
	}
}
