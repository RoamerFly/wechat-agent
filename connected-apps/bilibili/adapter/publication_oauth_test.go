package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

type publicationOAuthFake struct {
	exchange func(OpenCredentials, string) (publicationOAuthToken, error)
	refresh  func(OpenCredentials, string) (publicationOAuthToken, error)
}

func (f publicationOAuthFake) Exchange(_ context.Context, creds OpenCredentials, code string) (publicationOAuthToken, error) {
	return f.exchange(creds, code)
}

func (f publicationOAuthFake) Refresh(_ context.Context, creds OpenCredentials, token string) (publicationOAuthToken, error) {
	return f.refresh(creds, token)
}

const oauthOwner = "owner-session-fixture"
const oauthRedirect = "https://callbacks.example.test/bilibili"

func beginOAuthFixture(t *testing.T, app *PublicApp) string {
	t.Helper()
	view, err := app.PublicationAuthorization()
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	result, err := app.PublicationOAuth(t.Context(), oauthOwner, PublicationOAuthInput{
		Action: "begin", Revision: view.Revision, RedirectURI: oauthRedirect,
		ClientID: "fixture-app", AppSecret: "fixture-app-secret", Enabled: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := result.(PublicationOAuthStart)
	uri, err := url.Parse(start.AuthorizeURL)
	if err != nil || uri.Scheme != "https" || uri.Host != "account.bilibili.com" || uri.Path != "/pc/account-pc/auth/oauth" ||
		uri.Query().Get("client_id") != "fixture-app" || uri.Query().Get("gourl") != oauthRedirect ||
		uri.Query().Get("state") == "" || strings.Contains(start.AuthorizeURL, "fixture-app-secret") {
		t.Fatalf("invalid authorization URL: %+v %v", start, err)
	}
	return oauthRedirect + "?" + url.Values{"state": {uri.Query().Get("state")}, "code": {"fixture-code"}}.Encode()
}

func oauthTokenFixture(access, refresh string) publicationOAuthToken {
	return publicationOAuthToken{
		AccessToken: access, RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour),
		Scopes: []string{"USER_INFO", "ARC_BASE"},
	}
}

func completeOAuthFixture(t *testing.T, app *PublicApp, callback string) PublicationAuthorizationView {
	t.Helper()
	value, err := app.PublicationOAuth(t.Context(), oauthOwner, PublicationOAuthInput{Action: "complete", CallbackURL: callback})
	if err != nil {
		t.Fatal(err)
	}
	return value.(PublicationAuthorizationView)
}

func TestPublicationOAuthBindsCallbackScopeAndSession(t *testing.T) {
	app := New(t.TempDir())
	exchanges := 0
	app.publicationOAuth = publicationOAuthFake{exchange: func(creds OpenCredentials, code string) (publicationOAuthToken, error) {
		exchanges++
		if creds.ClientID != "fixture-app" || creds.AppSecret != "fixture-app-secret" || code != "fixture-code" || creds.AccessToken != "" {
			t.Fatal("exchange was not bound to original app")
		}
		token := oauthTokenFixture("fixture-access", "fixture-refresh")
		if exchanges == 1 {
			token.Scopes = []string{"USER_INFO"}
		}
		return token, nil
	}}
	app.verifyPublicationAccount = func(_ context.Context, got OpenCredentials) (string, error) {
		if got.AccessToken != "fixture-access" {
			t.Fatal("signed identity query did not use exchanged token")
		}
		return enrollmentOpenID, nil
	}

	callback := beginOAuthFixture(t, app)
	for _, bad := range []struct{ owner, callback string }{
		{"another-session", callback},
		{oauthOwner, strings.Replace(callback, "callbacks.example.test", "another.example.test", 1)},
		{oauthOwner, strings.Replace(callback, "/bilibili?", "/different?", 1)},
		{oauthOwner, oauthRedirect + "?code=fixture-code&state=wrong"},
		{oauthOwner, callback + "&code=other"},
	} {
		if _, err := app.PublicationOAuth(t.Context(), bad.owner, PublicationOAuthInput{Action: "complete", CallbackURL: bad.callback}); !errors.Is(err, ErrPublicationOAuthCallback) {
			t.Fatalf("invalid callback accepted: %v", err)
		}
	}
	if exchanges != 0 {
		t.Fatal("invalid callback reached the provider")
	}
	for _, want := range []error{ErrPublicationOAuthScope, ErrPublicationOAuthCallback} {
		if _, err := app.PublicationOAuth(t.Context(), oauthOwner, PublicationOAuthInput{Action: "complete", CallbackURL: callback}); !errors.Is(err, want) {
			t.Fatalf("scope and one-shot callback: %v want %v", err, want)
		}
	}
	restarted := New(app.privateDirectory)
	callback = beginOAuthFixture(t, app)
	if _, err := restarted.PublicationOAuth(t.Context(), oauthOwner, PublicationOAuthInput{Action: "complete", CallbackURL: callback}); !errors.Is(err, ErrPublicationOAuthCallback) {
		t.Fatalf("callback survived restart: %v", err)
	}
	verified := completeOAuthFixture(t, app, callback)
	safe, _ := json.Marshal(verified)
	if !verified.Enabled || !verified.Verified || !verified.RefreshAvailable || verified.RefreshPending ||
		strings.Contains(string(safe), "fixture-refresh") || strings.Contains(string(safe), enrollmentOpenID) || exchanges != 2 {
		t.Fatalf("invalid authorization view: %s", safe)
	}
}

func TestPublicationOAuthRefreshRotatesOnceOrFencesAfterRestart(t *testing.T) {
	for _, loseResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotated", true: "unknown"}[loseResponse], func(t *testing.T) {
			app := New(t.TempDir())
			refreshes := 0
			app.verifyPublicationAccount = func(context.Context, OpenCredentials) (string, error) {
				return enrollmentOpenID, nil
			}
			app.publicationOAuth = publicationOAuthFake{
				exchange: func(OpenCredentials, string) (publicationOAuthToken, error) {
					return oauthTokenFixture("fixture-access", "fixture-refresh"), nil
				},
				refresh: func(creds OpenCredentials, token string) (publicationOAuthToken, error) {
					refreshes++
					claimed, err := New(app.privateDirectory).loadPublicationSettings()
					if err != nil || !claimed.RefreshPending || claimed.RefreshToken != "" || creds.ClientID != "fixture-app" || token != "fixture-refresh" {
						t.Fatal("refresh call occurred before its durable single-use claim")
					}
					if loseResponse {
						return publicationOAuthToken{}, context.DeadlineExceeded
					}
					next := oauthTokenFixture("new-access", "new-refresh")
					next.Scopes = nil // the official refresh response may omit scopes.
					return next, nil
				},
			}
			view := completeOAuthFixture(t, app, beginOAuthFixture(t, app))
			before, _ := app.loadPublicationSettings()
			_, err := app.PublicationOAuth(t.Context(), oauthOwner, PublicationOAuthInput{Action: "refresh", Revision: view.Revision})
			restarted := New(app.privateDirectory)
			restarted.publicationOAuth = app.publicationOAuth
			after, loadErr := restarted.loadPublicationSettings()
			if loadErr != nil || before.AccountID != after.AccountID || before.version == after.version || refreshes != 1 {
				t.Fatalf("refresh did not preserve the original identity: %v", loadErr)
			}
			grant, grantErr := restarted.PublicationGrant(t.Context(), publicationRef(after.publicationSettings))
			if grantErr != nil {
				t.Fatal(grantErr)
			}
			if !loseResponse {
				if err != nil || after.RefreshToken != "new-refresh" || after.AccessToken != "new-access" || after.RefreshPending || !grant.Allowed {
					t.Fatalf("rotation failed: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrPublicationOAuthRefresh) || !after.RefreshPending || after.RefreshToken != "" || grant.Allowed {
				t.Fatalf("uncertain refresh was not fenced: %v", err)
			}
			fresh, _ := restarted.PublicationAuthorization()
			if _, err := restarted.PublicationOAuth(t.Context(), oauthOwner, PublicationOAuthInput{Action: "refresh", Revision: fresh.Revision}); !errors.Is(err, ErrPublicationOAuthRefreshUnavailable) || refreshes != 1 {
				t.Fatalf("used refresh token was replayed: %v", err)
			}
		})
	}
}
