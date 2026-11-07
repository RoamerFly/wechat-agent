package adapter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

func authorizePublishFixture(t *testing.T, readWorks, publishVideo bool, callbackScope, tokenScope string) AccountAuthorizationView {
	t.Helper()
	app := New(t.TempDir())
	t.Cleanup(func() {
		app.accountMu.Lock()
		defer app.accountMu.Unlock()
		app.clearAccountAttempt()
	})
	view, err := app.AccountAuthorization()
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.AuthorizeAccount(context.Background(), "owner/session", AccountAuthorizationInput{
		Action: "begin", Revision: view.Revision, ClientKey: "fixture-client", ClientSecret: "fixture-private-secret",
		RedirectURI: "https://owner.example.test/callback", ReadWorks: readWorks, PublishVideo: publishVideo,
	})
	if err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(result.(accountOAuthStart).AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	expectedScope := accountScope
	if readWorks {
		expectedScope += "," + accountWorksScope
	}
	if publishVideo {
		expectedScope += "," + accountPublishScope + "," + accountPostingScope
	}
	if scope := link.Query().Get("scope"); scope != expectedScope {
		t.Fatalf("authorization URL scope = %q, want %q", scope, expectedScope)
	}
	app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/oauth/access_token/":
			return jsonResponse(fmt.Sprintf(`{"data":{"error_code":0,"open_id":"fixture-owner","scope":%q,"access_token":"act.fixture-publish","refresh_token":"fixture-refresh","expires_in":3600,"refresh_expires_in":7200}}`, tokenScope)), nil
		case "/oauth/userinfo/":
			return jsonResponse(`{"err_no":0,"data":{"error_code":"0","open_id":"fixture-owner","nickname":"fixture-profile"}}`), nil
		default:
			t.Fatalf("unexpected authorization request: %s", r.URL.Path)
			return nil, nil
		}
	})
	query := url.Values{"code": {"fixture-code"}, "state": {link.Query().Get("state")}, "scopes": {callbackScope}}
	result, err = app.AuthorizeAccount(context.Background(), "owner/session", AccountAuthorizationInput{Action: "complete", CallbackURL: "https://owner.example.test/callback?" + query.Encode()})
	if err != nil {
		t.Fatal(err)
	}
	return result.(AccountAuthorizationView)
}

func TestAccountPublishAuthorizationRequiresExplicitOptInAndBothPlatformProofs(t *testing.T) {
	const complete = "user_info,video.create.bind,posting.behavior"
	for _, item := range []struct {
		name                              string
		readWorks, publishVideo           bool
		callbackScope, tokenScope          string
		worksAuthorized, publishAuthorized bool
	}{
		{"not-requested", false, false, complete, complete, false, false},
		{"callback-missing-create", false, true, "user_info,posting.behavior", complete, false, false},
		{"callback-missing-posting", false, true, "user_info,video.create.bind", complete, false, false},
		{"token-missing-create", false, true, complete, "user_info,posting.behavior", false, false},
		{"token-missing-posting", false, true, complete, "user_info,video.create.bind", false, false},
		{"full", false, true, complete, complete, false, true},
		{"independent-works", true, false, "user_info,video.list.bind", "user_info,video.list.bind", true, false},
		{"both", true, true, "user_info,video.list.bind,video.create.bind,posting.behavior", "user_info,video.list.bind,video.create.bind,posting.behavior", true, true},
	} {
		t.Run(item.name, func(t *testing.T) {
			view := authorizePublishFixture(t, item.readWorks, item.publishVideo, item.callbackScope, item.tokenScope)
			if !view.Configured || view.WorksAuthorized != item.worksAuthorized || view.PublishAuthorized != item.publishAuthorized {
				t.Fatalf("unexpected authorization %+v", view)
			}
		})
	}
}
