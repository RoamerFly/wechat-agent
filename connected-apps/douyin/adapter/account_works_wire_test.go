package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
)

type accountWorksRoundTripper func(*http.Request) (*http.Response, error)

func (f accountWorksRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testAccountWorksApp(t *testing.T, handler func(*http.Request) (*http.Response, error)) *SearchApp {
	t.Helper()
	app := New(t.TempDir())
	app.now = func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }
	app.client = &http.Client{Transport: accountWorksRoundTripper(handler), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return app
}

func testAccountWorksAuthorization() accountAuthorization {
	return accountAuthorization{OpenID: "open-id-1", AccessToken: "act.token", ReadWorks: true, ExpiresAt: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)}
}

func jsonAccountWorksResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestFetchAccountWorksUsesOfficialListContract(t *testing.T) {
	app := testAccountWorksApp(t, func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "open.douyin.com" || request.URL.Path != "/video/list/" {
			t.Fatalf("unexpected request: %s", request.URL.String())
		}
		query := request.URL.Query()
		if query.Get("open_id") != "open-id-1" || query.Get("cursor") != "7" || query.Get("count") != "2" {
			t.Fatalf("unexpected query: %q", request.URL.RawQuery)
		}
		if request.Header.Get("access-token") != "act.token" || request.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected credential headers: %#v", request.Header)
		}
		return jsonAccountWorksResponse(`{"data":{"error_code":0,"open_id":"open-id-1","has_more":true,"cursor":9,"list":[{"video_id":"123","item_id":"opaque-1","create_time":1,"title":"kept only in wire"}]},"extra":{"error_code":0}}`), nil
	})

	page, err := app.fetchAccountWorks(context.Background(), testAccountWorksAuthorization(), 7, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || page.Cursor != 9 || len(page.Items) != 1 || string(page.Items[0].VideoID) != `"123"` || page.Items[0].ItemID != "opaque-1" {
		t.Fatalf("unexpected page: %#v", page)
	}
}

func TestFetchAccountWorksRejectsUnsafeContinuationAndSource(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing_cursor_while_more", `{"data":{"error_code":0,"has_more":true,"list":[]},"extra":{"error_code":0}}`},
		{"mismatched_open_id", `{"data":{"error_code":0,"open_id":"another","has_more":false,"cursor":0,"list":[]},"extra":{"error_code":0}}`},
		{"duplicate_video", `{"data":{"error_code":0,"has_more":false,"cursor":0,"list":[{"video_id":"123","item_id":"one","create_time":1},{"video_id":123,"item_id":"two","create_time":2}]},"extra":{"error_code":0}}`},
		{"invalid_cursor", `{"data":{"error_code":0,"has_more":true,"cursor":"x","list":[{"video_id":"123","item_id":"one","create_time":1}]},"extra":{"error_code":0}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := testAccountWorksApp(t, func(*http.Request) (*http.Response, error) { return jsonAccountWorksResponse(test.body), nil })
			_, err := app.fetchAccountWorks(context.Background(), testAccountWorksAuthorization(), 0, 2)
			if !errors.Is(err, apphost.ErrObservationUpstreamUnavailable) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestFetchAccountWorksClassifiesProviderAndInputErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{"expired_token", `{"data":{"error_code":2190002},"extra":{"error_code":2190002}}`, apphost.ErrObservationCredentialExpired},
		{"missing_scope", `{"data":{"error_code":28001014},"extra":{"error_code":28001014}}`, apphost.ErrObservationCapabilityDenied},
		{"throttled", `{"data":{"error_code":28003017},"extra":{"error_code":28003017}}`, apphost.ErrObservationThrottled},
		{"unavailable", `{"data":{"error_code":2100004},"extra":{"error_code":2100004}}`, apphost.ErrObservationUpstreamUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := testAccountWorksApp(t, func(*http.Request) (*http.Response, error) { return jsonAccountWorksResponse(test.body), nil })
			_, err := app.fetchAccountWorks(context.Background(), testAccountWorksAuthorization(), 0, 1)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	app := testAccountWorksApp(t, func(*http.Request) (*http.Response, error) { t.Fatal("request should not be made"); return nil, nil })
	account := testAccountWorksAuthorization()
	account.ReadWorks = false
	if _, err := app.fetchAccountWorks(context.Background(), account, 0, 1); !errors.Is(err, apphost.ErrObservationCapabilityDenied) {
		t.Fatalf("scope error = %v", err)
	}
	if _, err := app.fetchAccountWorks(context.Background(), testAccountWorksAuthorization(), -1, 1); !errors.Is(err, apphost.ErrObservationInvalid) {
		t.Fatalf("cursor error = %v", err)
	}
}

func TestFetchAccountWorksReturnsResponseLimitError(t *testing.T) {
	app := testAccountWorksApp(t, func(*http.Request) (*http.Response, error) {
		return jsonAccountWorksResponse(strings.Repeat("x", maxResponseBytes+1)), nil
	})
	_, err := app.fetchAccountWorks(context.Background(), testAccountWorksAuthorization(), 0, 1)
	if !errors.Is(err, apphost.ErrObservationAccountWorksResponseSize) {
		t.Fatalf("error = %v", err)
	}
}
