package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func oauthFixture(t *testing.T, run uploadRoundTripper) *publicationOAuthClient {
	t.Helper()
	client, ok := newPublicationOAuthClient().(*publicationOAuthClient)
	if !ok {
		t.Fatal("unexpected OAuth transport")
	}
	client.client.Transport = run
	client.now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	return client
}

// These tests inspect the actual HTTP request emitted by Exchange and Refresh;
// all data is synthetic and no network endpoint or credential is used.
func TestPublicationOAuthWireContract(t *testing.T) {
	client := oauthFixture(t, func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Host != "api.bilibili.com" || len(body) != 0 || request.ContentLength != 0 || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("unexpected request shape: %s %s body=%q length=%d", request.Method, request.URL, body, request.ContentLength)
		}
		query := request.URL.Query()
		if query.Get("client_id") != "synthetic-client" || query.Get("client_secret") != "synthetic-secret" || request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "" {
			t.Fatal("credentials did not remain in the documented query-only form")
		}
		switch request.URL.Path {
		case "/x/account-oauth2/v1/token":
			if query.Get("grant_type") != "authorization_code" || query.Get("code") != "synthetic-code" || len(query) != 4 {
				t.Fatalf("exchange query: %v", query)
			}
			return uploadResponse(200, `{"code":0,"data":{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":1700003600,"scopes":["video.publish"]}}`), nil
		case "/x/account-oauth2/v1/refresh_token":
			if query.Get("grant_type") != "refresh_token" || query.Get("refresh_token") != "synthetic-refresh" || len(query) != 4 {
				t.Fatalf("refresh query: %v", query)
			}
			return uploadResponse(200, `{"code":0,"data":{"access_token":"new-access","refresh_token":"new-refresh","expires_in":1700007200}}`), nil
		default:
			t.Fatalf("unexpected OAuth path: %s", request.URL.Path)
			return nil, nil
		}
	})
	credentials := OpenCredentials{ClientID: "synthetic-client", AppSecret: "synthetic-secret"}
	exchanged, err := client.Exchange(t.Context(), credentials, "synthetic-code")
	if err != nil || exchanged.AccessToken != "synthetic-access" || exchanged.RefreshToken != "synthetic-refresh" || !exchanged.ExpiresAt.Equal(time.Unix(1700003600, 0).UTC()) || len(exchanged.Scopes) != 1 || exchanged.Scopes[0] != "video.publish" {
		t.Fatalf("exchange result: %+v / %v", exchanged, err)
	}
	refreshed, err := client.Refresh(t.Context(), credentials, "synthetic-refresh")
	if err != nil || refreshed.AccessToken != "new-access" || refreshed.Scopes != nil || !refreshed.ExpiresAt.Equal(time.Unix(1700007200, 0).UTC()) {
		t.Fatalf("refresh result: %+v / %v", refreshed, err)
	}
}

func TestPublicationOAuthRejectsBadResponsesWithoutLeakingOrRetrying(t *testing.T) {
	cases := []string{
		`{}`,
		`{"code":0,"code":1,"data":{}}`,
		`{"code":0,"data":{"access_token":"synthetic-secret","refresh_token":"r","expires_in":1700003600}}`,
		`{"code":0,"data":{"access_token":"a","refresh_token":"r","expires_in":1700000000,"scopes":[]}}`,
		`{"code":0,"data":{"access_token":"a","refresh_token":"r","expires_in":1700003600,"scopes":null}}`,
		`{"code":1,"message":"synthetic-provider-detail"}`,
	}
	for _, response := range cases {
		t.Run(response[:min(len(response), 12)], func(t *testing.T) {
			calls := 0
			client := oauthFixture(t, func(*http.Request) (*http.Response, error) {
				calls++
				return uploadResponse(200, response), nil
			})
			_, err := client.Exchange(context.Background(), OpenCredentials{ClientID: "id", AppSecret: "secret"}, "code")
			if err == nil || calls != 1 || strings.Contains(err.Error(), "synthetic-") {
				t.Fatalf("error/calls/redaction: %v / %d", err, calls)
			}
		})
	}
}

func TestPublicationOAuthInputAndHTTPFailures(t *testing.T) {
	client := oauthFixture(t, func(*http.Request) (*http.Response, error) {
		return uploadResponse(http.StatusServiceUnavailable, "synthetic-provider-detail"), nil
	})
	if _, err := client.Exchange(t.Context(), OpenCredentials{ClientID: "id", AppSecret: "secret"}, "bad\r\ncode"); !errors.Is(err, ErrOpenUploadInvalid) {
		t.Fatalf("bad code: %v", err)
	}
	_, err := client.Refresh(t.Context(), OpenCredentials{ClientID: "id", AppSecret: "secret"}, "refresh")
	if !errors.Is(err, ErrOpenUploadUnavailable) || strings.Contains(err.Error(), "synthetic-") {
		t.Fatalf("HTTP response leaked or classified incorrectly: %v", err)
	}
}
