package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func oauthSettings() OAuthSettings {
	return OAuthSettings{ClientKey: "awf-test", ClientSecret: "secret-value", RedirectURI: "https://station.example.test/oauth/douyin"}
}

func TestAuthorizationURLUsesDocumentedWebParameters(t *testing.T) {
	result, err := AuthorizationURL(oauthSettings(), []string{"user_info"}, "csrf-state")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(result)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "open.douyin.com" || parsed.Path != "/platform/oauth/connect" {
		t.Fatalf("unexpected authorization URL %q", result)
	}
	query := parsed.Query()
	if query.Get("client_key") != "awf-test" || query.Get("response_type") != "code" || query.Get("scope") != "user_info" || query.Get("redirect_uri") != oauthSettings().RedirectURI || query.Get("state") != "csrf-state" {
		t.Fatalf("unexpected query: %v", query)
	}
	for _, invalid := range []OAuthSettings{{}, {ClientKey: "x", ClientSecret: "y", RedirectURI: "http://station.example.test/callback"}, {ClientKey: "x", ClientSecret: "y", RedirectURI: "https://station.example.test/callback?a=1"}} {
		if _, err := AuthorizationURL(invalid, []string{"user_info"}, "state"); !errors.Is(err, ErrOAuthInvalid) {
			t.Fatalf("invalid settings error = %v", err)
		}
	}
}

func TestExchangeCodeUsesExactForm(t *testing.T) {
	app := New(t.TempDir())
	app.now = func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }
	app.client = &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != tokenEndpoint || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || request.Header.Get("access-token") != "" || request.Header.Get("Cookie") != "" {
			t.Fatalf("bad token request: %s %s headers=%v", request.Method, request.URL, request.Header)
		}
		raw, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(raw))
		if form.Get("client_key") != "awf-test" || form.Get("client_secret") != "secret-value" || form.Get("code") != "one-time-code" || form.Get("grant_type") != "authorization_code" {
			t.Fatalf("bad form %v", form)
		}
		return jsonResponse("{\"data\":{\"access_token\":\"act.token\",\"refresh_token\":\"rft.token\",\"open_id\":\"open-user\",\"scope\":\"user_info\",\"expires_in\":60,\"refresh_expires_in\":120,\"error_code\":0}}"), nil
	})}
	grant, err := app.ExchangeAuthorizationCode(context.Background(), oauthSettings(), "one-time-code")
	if err != nil || grant.OpenID != "open-user" || !grant.ExpiresAt.Equal(app.nowUTC().Add(time.Minute)) || grant.AccessToken != "act.token" {
		t.Fatalf("unexpected grant %#v, %v", grant, err)
	}
}

func TestReadProfileUsesHeaderAndBindsOpenID(t *testing.T) {
	app := New(t.TempDir())
	app.now = func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }
	app.client = &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/oauth/userinfo/" || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || request.Header.Get("access-token") != "" || request.Header.Get("Cookie") != "" {
			t.Fatalf("bad profile request %s %v", request.URL, request.Header)
		}
		raw, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(raw))
		if form.Get("open_id") != "open-user" || form.Get("access_token") != "act.token" {
			t.Fatalf("bad profile form %v", form)
		}
		return jsonResponse("{\"err_no\":0,\"data\":{\"open_id\":\"open-user\",\"nickname\":\" 大鱼 \",\"avatar\":\"https://avatar.example.test/a.png\",\"error_code\":\"0\"}}"), nil
	})}
	profile, err := app.ReadAuthorizedProfile(context.Background(), OAuthGrant{AccessToken: "act.token", OpenID: "open-user", ExpiresAt: app.nowUTC().Add(time.Hour)})
	if err != nil || profile != (AccountProfile{OpenID: "open-user", Nickname: "大鱼", Avatar: "https://avatar.example.test/a.png"}) {
		t.Fatalf("profile %#v, %v", profile, err)
	}
	app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return jsonResponse("{\"err_no\":0,\"data\":{\"open_id\":\"other\",\"error_code\":\"0\"}}"), nil
	})}
	if _, err := app.ReadAuthorizedProfile(context.Background(), OAuthGrant{AccessToken: "act.token", OpenID: "open-user", ExpiresAt: app.nowUTC().Add(time.Hour)}); !errors.Is(err, ErrOAuthRejected) {
		t.Fatalf("mismatch error = %v", err)
	}
}
