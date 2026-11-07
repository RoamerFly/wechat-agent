package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
)

func startSyntheticAccount(t *testing.T, app *SearchApp, revision string) (string, string) {
	t.Helper()
	result, err := app.AuthorizeAccount(context.Background(), "owner/session", AccountAuthorizationInput{
		Action: "begin", Revision: revision, ClientKey: "fixture-client", ClientSecret: "fixture-private-secret",
		RedirectURI: "https://owner.example.test/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(result.(accountOAuthStart).AuthorizeURL)
	if err != nil || link.Query().Get("state") == "" {
		t.Fatalf("invalid authorization link: %v", err)
	}
	query := url.Values{"code": {"synthetic-code"}, "state": {link.Query().Get("state")}, "scopes": {"user_info"}}
	return "https://owner.example.test/callback?" + query.Encode(), link.Query().Get("state")
}

func TestAccountAuthorizationPersistsOnlyVerifiedConnection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	app := New(dir)
	t.Cleanup(func() {
		app.accountMu.Lock()
		defer app.accountMu.Unlock()
		app.clearAccountAttempt()
	})
	ctx := context.Background()
	now := time.Now().UTC()
	app.now = func() time.Time { return now }
	calls := 0
	app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		switch r.URL.Path {
		case "/oauth/access_token/":
			return jsonResponse(`{"message":"success","data":{"error_code":0,"open_id":"fixture-open-id","scope":"user_info","access_token":"fixture-access-token","refresh_token":"fixture-refresh-token","expires_in":3600,"refresh_expires_in":7200}}`), nil
		case "/oauth/userinfo/":
			return jsonResponse(`{"err_no":0,"data":{"error_code":"0","open_id":"fixture-open-id","nickname":" 合成测试账号 "}}`), nil
		}
		t.Fatalf("unexpected official request: %s", r.URL.Path)
		return nil, nil
	})
	callback, state := startSyntheticAccount(t, app, "")
	complete := func(binding, callback string) (any, error) {
		return app.AuthorizeAccount(ctx, binding, AccountAuthorizationInput{Action: "complete", CallbackURL: callback})
	}
	for _, input := range []struct{ binding, callback string }{
		{"other/session", callback},
		{"owner/session", strings.Replace(callback, "owner.example.test", "other.example.test", 1)},
		{"owner/session", strings.Replace(callback, state, "forged", 1)},
		{"owner/session", callback + "&state=" + state},
	} {
		if _, err := complete(input.binding, input.callback); !errors.Is(err, ErrAccountCallback) {
			t.Fatalf("forged authorization: %v", err)
		}
	}
	if _, err := complete("owner/session", strings.Replace(callback, "scopes=user_info", "scopes=other", 1)); !errors.Is(err, ErrAccountScope) {
		t.Fatalf("invalid scope: %v", err)
	}
	if calls != 0 {
		t.Fatal("invalid callback called the platform")
	}
	result, err := complete("owner/session", callback)
	if err != nil {
		t.Fatal(err)
	}
	view := result.(AccountAuthorizationView)
	if !view.Configured || !view.RefreshAvailable || view.Label != "合成测试账号" || view.Status != "ready" || view.Revision == "" || calls != 2 {
		t.Fatalf("authorization state=%+v requests=%d", view, calls)
	}
	encoded, _ := json.Marshal(view)
	for _, private := range []string{"fixture-access-token", "fixture-refresh-token", "fixture-open-id", "fixture-private-secret", "fixture-client", "synthetic-code", state} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("private value reached management response")
		}
	}
	if _, err := complete("owner/session", callback); !errors.Is(err, ErrAccountCallback) || calls != 2 {
		t.Fatalf("replayed callback: calls=%d err=%v", calls, err)
	}
	saved, err := os.ReadFile(filepath.Join(dir, accountFile))
	if err != nil || !strings.Contains(string(saved), "fixture-access-token") || !strings.Contains(string(saved), "fixture-refresh-token") ||
		strings.Contains(string(saved), "fixture-private-secret") || strings.Contains(string(saved), "synthetic-code") {
		t.Fatalf("private file shape or permissions: %v", err)
	}
	restarted := New(dir)
	restarted.now = func() time.Time { return now }
	if restored, err := restarted.AccountAuthorization(); err != nil || restored != view {
		t.Fatalf("restored authorization: %+v %v", restored, err)
	}
	now = now.Add(2 * time.Hour)
	if expired, err := restarted.AccountAuthorization(); err != nil || expired.Status != "expired" || expired.RefreshAvailable {
		t.Fatalf("expired authorization: %+v %v", expired, err)
	}
	restarted.client.Transport = app.client.Transport
	reauthorize, _ := startSyntheticAccount(t, restarted, view.Revision)
	if _, err := restarted.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "complete", CallbackURL: callback}); !errors.Is(err, ErrAccountCallback) || calls != 2 {
		t.Fatalf("old callback interfered with new scan: calls=%d err=%v", calls, err)
	}
	result, err = restarted.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "complete",
		CallbackURL: strings.Replace(reauthorize, "synthetic-code", "synthetic-second-code", 1)})
	if err != nil {
		t.Fatal(err)
	}
	reconnected := result.(AccountAuthorizationView)
	if reconnected.Status != "ready" || !reconnected.RefreshAvailable || reconnected.Revision == view.Revision || calls != 4 {
		t.Fatalf("new scan did not reconnect the account: %+v requests=%d", reconnected, calls)
	}
	restarted = New(dir)
	restarted.now = func() time.Time { return now }
	if restored, err := restarted.AccountAuthorization(); err != nil || restored != reconnected {
		t.Fatalf("reconnected account did not survive restart: %+v %v", restored, err)
	}
	if _, err := restarted.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "disconnect", Revision: view.Revision}); !errors.Is(err, ErrAccountRevision) {
		t.Fatalf("stale disconnect: %v", err)
	}
	result, err = restarted.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "disconnect", Revision: reconnected.Revision})
	if err != nil || result.(AccountAuthorizationView).Configured {
		t.Fatalf("disconnect: %v %v", result, err)
	}
	saved, err = os.ReadFile(filepath.Join(dir, accountFile))
	if err != nil || strings.Contains(string(saved), "fixture-access-token") || strings.Contains(string(saved), "fixture-open-id") || strings.Contains(string(saved), "fixture-refresh-token") {
		t.Fatalf("revocation retained credentials: %v", err)
	}
}

func TestAccountAuthorizationCancelMatchesAttemptAndOwner(t *testing.T) {
	app := New(t.TempDir())
	ctx := context.Background()
	_, staleState := startSyntheticAccount(t, app, "")
	callback, state := startSyntheticAccount(t, app, "")
	cancel := func(binding, state string) {
		t.Helper()
		if _, err := app.AuthorizeAccount(ctx, binding, AccountAuthorizationInput{Action: "cancel", State: state}); err != nil {
			t.Fatal(err)
		}
	}
	cancel("owner/session", staleState)
	cancel("other/session", state)
	if app.accountPending == nil || app.accountPending.state != state {
		t.Fatal("a stale or unrelated session cancelled the active attempt")
	}
	cancel("owner/session", state)
	cancel("owner/session", state)
	app.client.Transport = roundTrip(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("a cancelled authorization must never reach the provider")
		return nil, nil
	})
	if _, err := app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "complete", CallbackURL: callback}); !errors.Is(err, ErrAccountCallback) {
		t.Fatalf("late callback after cancel: %v", err)
	}
}

func TestAccountAuthorizationDisconnectWinsOverLateVerification(t *testing.T) {
	app := New(t.TempDir())
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/oauth/access_token/" {
			close(entered)
			<-release
			return jsonResponse(`{"message":"success","data":{"error_code":0,"open_id":"late-account","scope":"user_info","access_token":"late-access","refresh_token":"late-refresh","expires_in":3600,"refresh_expires_in":7200}}`), nil
		}
		return jsonResponse(`{"err_no":0,"data":{"error_code":"0","open_id":"late-account","nickname":"迟到授权"}}`), nil
	})
	callback, _ := startSyntheticAccount(t, app, "")
	completed := make(chan error, 1)
	go func() {
		_, err := app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "complete", CallbackURL: callback})
		completed <- err
	}()
	select {
	case <-entered:
	case err := <-completed:
		t.Fatalf("expected dispatch before result: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("expected code exchange dispatch")
	}
	_, err := app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "disconnect"})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-completed; !errors.Is(err, ErrAccountRevision) {
		t.Fatalf("late verification: %v", err)
	}
	view, err := app.AccountAuthorization()
	if err != nil || view.Configured {
		t.Fatalf("late callback restored access: %+v %v", view, err)
	}
}

func TestAccountRefreshPreservesIdentityAndOriginalPublicationGrant(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	app := New(dir)
	now := time.Now().UTC().Truncate(time.Second)
	app.now = func() time.Time { return now }
	record := syntheticRefreshRecord(t, app, now)
	target := apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: accountConnectionID, AccountID: accountWorksAccountID(record)}
	works := apphost.ObservationAccess{ConnectionRef: target, ContentID: accountWorksContentID, Field: apphost.ObservationAccountWorks}
	if _, err := app.PublicationGrant(ctx, target); !errors.Is(err, apphost.ErrPublicationDenied) {
		t.Fatalf("expired grant still permits publication: %v", err)
	}

	calls := 0
	app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if r.Method != http.MethodPost || r.URL.String() != refreshEndpoint || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Cookie") != "" {
				t.Fatalf("unexpected refresh request: %s %s", r.Method, r.URL)
			}
			raw, _ := io.ReadAll(r.Body)
			form, err := url.ParseQuery(string(raw))
			if err != nil || len(form) != 3 || form.Get("client_key") != record.ClientKey || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != record.RefreshToken {
				t.Fatal("unexpected refresh form")
			}
			// Works was never granted locally. The platform cannot add it here.
			return jsonResponse(`{"data":{"error_code":0,"open_id":"fixture-owner","scope":"user_info,video.list.bind,video.create.bind,posting.behavior","access_token":"fixture-next-access","refresh_token":"fixture-next-refresh","expires_in":"3600","refresh_expires_in":"2592000"}}`), nil
		}
		if r.URL.String() != profileEndpoint || calls != 2 {
			t.Fatalf("unexpected verification request: %s", r.URL)
		}
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		if form.Get("access_token") != "fixture-next-access" || form.Get("open_id") != record.OpenID {
			t.Fatal("profile did not verify the refreshed credentials")
		}
		return jsonResponse(`{"data":{"error_code":"0","open_id":"fixture-owner","nickname":"合成账号"}}`), nil
	})
	value, err := app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "refresh", Revision: record.Revision})
	if err != nil {
		t.Fatal(err)
	}
	view := value.(AccountAuthorizationView)
	if view.Status != "ready" || !view.RefreshAvailable || !view.PublishAuthorized || view.WorksAuthorized || view.Revision == record.Revision || calls != 2 {
		t.Fatalf("unexpected result: %+v (%d calls)", view, calls)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), "fixture-") {
		t.Fatal("private renewal values reached the owner view")
	}
	restarted := New(dir)
	restarted.now = app.now
	current, err := restarted.currentAccount()
	if err != nil || current.AccessToken != "fixture-next-access" || current.RefreshToken != "fixture-next-refresh" ||
		!current.RefreshExpiresAt.Equal(record.RefreshExpiresAt) || !current.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("refresh was not persisted without extending its renewal window: %v", err)
	}
	publish, err := restarted.PublicationGrant(ctx, target)
	if err != nil || publish.Version != record.Revision {
		t.Fatalf("old tasks lost their still-valid publication grant: %+v %v", publish, err)
	}
	if _, err := restarted.accountWorksGrant(works); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("new scope expanded original consent: %v", err)
	}
	if _, err := app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "refresh", Revision: record.Revision}); !errors.Is(err, ErrAccountRevision) || calls != 2 {
		t.Fatalf("old management revision reached provider: %v", err)
	}
}

func TestAccountRefreshClassifiesFailures(t *testing.T) {
	for _, c := range []struct {
		name, tokenResponse, profileOpenID string
		providerErr, wantError             error
		profileCalls                       int
		loseRenewal, losePublish           bool
	}{
		{name: "transport_unknown", providerErr: io.ErrUnexpectedEOF, wantError: ErrAccountRefreshFailed},
		{name: "refresh_rejected", tokenResponse: `{"data":{"error_code":10010}}`, wantError: ErrAccountRefreshUnavailable, loseRenewal: true},
		{name: "different_token_account", tokenResponse: `{"data":{"error_code":0,"open_id":"other","scope":"user_info","access_token":"next-access","refresh_token":"next-refresh","expires_in":60,"refresh_expires_in":120}}`, wantError: ErrAccountRefreshFailed},
		{name: "missing_base_scope", tokenResponse: `{"data":{"error_code":0,"open_id":"fixture-owner","scope":"video.create.bind","access_token":"next-access","refresh_token":"next-refresh","expires_in":60,"refresh_expires_in":120}}`, wantError: ErrAccountRefreshFailed},
		{name: "different_profile_account", tokenResponse: `{"data":{"error_code":0,"open_id":"fixture-owner","scope":"user_info","access_token":"next-access","refresh_token":"next-refresh","expires_in":60,"refresh_expires_in":120}}`, profileOpenID: "other", profileCalls: 1, wantError: ErrAccountRefreshFailed},
		{name: "withdrawn_publish_scope", tokenResponse: `{"data":{"error_code":0,"open_id":"fixture-owner","scope":"user_info,video.create.bind","access_token":"next-access","refresh_token":"next-refresh","expires_in":60,"refresh_expires_in":120}}`, profileOpenID: "fixture-owner", profileCalls: 1, losePublish: true},
		{name: "missing_provider_status", tokenResponse: `{"data":{"open_id":"fixture-owner","scope":"user_info","access_token":"next-access","refresh_token":"next-refresh","expires_in":60,"refresh_expires_in":120}}`, wantError: ErrAccountRefreshFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			app := New(dir)
			now := time.Now().UTC().Truncate(time.Second)
			app.now = func() time.Time { return now }
			record := syntheticRefreshRecord(t, app, now)
			refreshCalls, profileCalls := 0, 0
			app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case refreshEndpoint:
					refreshCalls++
					if c.providerErr != nil {
						return nil, c.providerErr
					}
					return jsonResponse(c.tokenResponse), nil
				case profileEndpoint:
					profileCalls++
					return jsonResponse(`{"data":{"error_code":"0","open_id":"` + c.profileOpenID + `","nickname":"合成账号"}}`), nil
				default:
					t.Fatal("unexpected provider request")
					return nil, nil
				}
			})
			_, err := app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "refresh", Revision: record.Revision})
			if !errors.Is(err, c.wantError) || refreshCalls != 1 || profileCalls != c.profileCalls {
				t.Fatalf("unexpected renewal error or duplicate network request: %v (%d refresh, %d profile)", err, refreshCalls, profileCalls)
			}
			restarted := New(dir)
			restarted.now = app.now
			current, err := restarted.currentAccount()
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case c.loseRenewal:
				if current.RefreshToken != "" || current.AccessToken != record.AccessToken || current.Revision == record.Revision {
					t.Fatal("explicit renewal rejection must discard only the renewal credential")
				}
			case c.losePublish:
				if current.PublishVideo || current.AccessToken != "next-access" {
					t.Fatal("a removed publishing permission remained enabled")
				}
			default:
				if current != record {
					t.Fatal("unverified refresh changed persistent authorization")
				}
			}
		})
	}
}

func TestAccountRefreshDisconnectWinsOverLateResponse(t *testing.T) {
	ctx := context.Background()
	app := New(t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	app.now = func() time.Time { return now }
	record := syntheticRefreshRecord(t, app, now)
	entered, release := make(chan struct{}), make(chan struct{})
	app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == refreshEndpoint {
			close(entered)
			<-release
			return jsonResponse(`{"data":{"error_code":0,"open_id":"fixture-owner","scope":"user_info","access_token":"late-access","refresh_token":"late-refresh","expires_in":60,"refresh_expires_in":120}}`), nil
		}
		return jsonResponse(`{"data":{"error_code":"0","open_id":"fixture-owner","nickname":"合成账号"}}`), nil
	})
	refresh := AccountAuthorizationInput{Action: "refresh", Revision: record.Revision}
	result := make(chan error, 1)
	go func() {
		_, err := app.AuthorizeAccount(ctx, "owner/session", refresh)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not reach provider")
	}
	if _, err := app.AuthorizeAccount(ctx, "owner/session", refresh); !errors.Is(err, ErrAccountRefreshBusy) {
		close(release)
		t.Fatalf("concurrent refresh was not rejected: %v", err)
	}
	_, err := app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "disconnect", Revision: record.Revision})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrAccountRevision) {
		t.Fatalf("late refresh must lose to disconnect: %v", err)
	}
	view, err := app.AccountAuthorization()
	if err != nil || view.Configured || view.RefreshAvailable {
		t.Fatalf("late response restored disconnected account: %+v %v", view, err)
	}
}

func syntheticRefreshRecord(t *testing.T, app *SearchApp, now time.Time) accountAuthorization {
	t.Helper()
	revision, err := accountNonce()
	if err != nil {
		t.Fatal(err)
	}
	record := accountAuthorization{SchemaVersion: 1, Revision: revision, ClientKey: "fixture-client",
		OpenID: "fixture-owner", AccessToken: "fixture-original-access", Label: "合成账号",
		PublishVideo: true, ExpiresAt: now.Add(-time.Hour), RefreshToken: "fixture-original-refresh", RefreshExpiresAt: now.Add(2 * time.Hour)}
	if err := app.writeAccount(record); err != nil {
		t.Fatal(err)
	}
	return record
}
