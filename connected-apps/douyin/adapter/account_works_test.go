package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"wechat-bot/internal/apphost"
)

func authorizeWorksFixture(t *testing.T, app *SearchApp, optIn bool, callbackScope, tokenScope string) AccountAuthorizationView {
	t.Helper()
	ctx := context.Background()
	view, err := app.AccountAuthorization()
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{
		Action: "begin", Revision: view.Revision, ClientKey: "fixture-client", ClientSecret: "fixture-private-secret",
		RedirectURI: "https://owner.example.test/callback", ReadWorks: optIn,
	})
	if err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(result.(accountOAuthStart).AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if scope := link.Query().Get("scope"); scope != accountScope && !optIn || scope != accountScope+","+accountWorksScope && optIn {
		t.Fatalf("authorization URL scope = %q, opt-in = %t", scope, optIn)
	}
	transport := app.client.Transport
	defer func() { app.client.Transport = transport }()
	app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/oauth/access_token/":
			return jsonResponse(fmt.Sprintf(`{"data":{"error_code":0,"open_id":"fixture-owner","scope":%q,"access_token":"act.fixture-authorized-works","refresh_token":"fixture-refresh","expires_in":3600,"refresh_expires_in":7200}}`, tokenScope)), nil
		case "/oauth/userinfo/":
			return jsonResponse(`{"err_no":0,"data":{"error_code":"0","open_id":"fixture-owner","nickname":"fixture-profile"}}`), nil
		default:
			t.Fatalf("unexpected authorization request: %s", r.URL.Path)
			return nil, nil
		}
	})
	query := url.Values{"code": {"fixture-code"}, "state": {link.Query().Get("state")}, "scopes": {callbackScope}}
	result, err = app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "complete", CallbackURL: "https://owner.example.test/callback?" + query.Encode()})
	if err != nil {
		t.Fatal(err)
	}
	return result.(AccountAuthorizationView)
}

func TestAccountWorksAuthorizationRequiresOwnerAndBothPlatformProofs(t *testing.T) {
	for _, item := range []struct {
		name            string
		optIn           bool
		callback, token string
		allowed         bool
	}{
		{"unexpected-scope", false, "user_info,video.list.bind", "user_info,video.list.bind", false},
		{"callback-missing", true, "user_info", "user_info,video.list.bind", false},
		{"token-missing", true, "user_info,video.list.bind", "user_info", false},
		{"full", true, "user_info,video.list.bind", "user_info,video.list.bind", true},
		{"legacy", true, "user_info,video.list", "user_info,video.list", true},
	} {
		t.Run(item.name, func(t *testing.T) {
			dir := t.TempDir()
			app := New(dir)
			view := authorizeWorksFixture(t, app, item.optIn, item.callback, item.token)
			if !view.Configured || view.WorksAuthorized != item.allowed {
				t.Fatalf("unexpected authorization %+v", view)
			}
			registry, err := apphost.New(New(dir).Registration())
			if err != nil {
				t.Fatal(err)
			}
			if items := apphost.NewObserver(registry, dir).Discover(context.Background()); (len(items) == 1) != item.allowed {
				t.Fatalf("unexpected discovery after restart %+v", items)
			}
		})
	}
}

func TestAccountWorksHostPaginationAndRevocation(t *testing.T) {
	ctx := context.Background()
	app := New(filepath.Join(t.TempDir(), "account"))
	const scopes = "user_info,video.list.bind"
	view := authorizeWorksFixture(t, app, true, scopes, scopes)
	registry, err := apphost.New(app.Registration())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	observer := apphost.NewObserver(registry, dir)
	targets := observer.Discover(ctx)
	if len(targets) != 1 || targets[0].AccountID == "" || strings.Contains(targets[0].AccountID, "fixture-owner") || targets[0].Label == "" {
		t.Fatalf("unexpected discovery: %+v", targets)
	}
	input := apphost.ObservationInput{AppID: ID, ConnectionID: accountConnectionID, ContentID: accountWorksContentID, Field: apphost.ObservationAccountWorks, Limit: 127}
	const secretTitle = "private-fixture-title"
	const opaqueItem = "@opaque-fixture-item"
	calls := 0
	expectedCount := "127"
	expectedCursor := int64(0)
	app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if q.Get("open_id") != "fixture-owner" || q.Get("count") != expectedCount || q.Get("cursor") != strconv.FormatInt(expectedCursor, 10) {
			t.Fatalf("unexpected pagination: %v", q)
		}
		return jsonResponse(fmt.Sprintf(`{"data":{"error_code":0,"has_more":true,"cursor":%d,"list":[{"item_id":%q,"title":%q,"create_time":1700000000}]},"extra":{"error_code":0}}`,
			expectedCursor+1, opaqueItem, secretTitle)), nil
	})
	bad := input
	bad.ContentID = "different-account-works"
	if _, err := observer.Read(ctx, bad); !errors.Is(err, apphost.ErrObservationForbidden) || calls != 0 {
		t.Fatalf("unauthorized item: calls=%d, error=%v", calls, err)
	}
	first, err := observer.Read(ctx, input)
	if err != nil || first.Complete || len(first.Chunks) != 1 || first.Chunks[0].Position != 0 || !strings.HasPrefix(first.Chunks[0].SourceID, "account-work-") || first.Chunks[0].SourceURL != "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	encoded, _ := json.Marshal(first)
	if strings.Contains(string(encoded), secretTitle) || strings.Contains(string(encoded), opaqueItem) {
		t.Fatal("private provider metadata was disclosed")
	}
	bad = input
	bad.Limit++
	if _, err := observer.Read(ctx, bad); !errors.Is(err, apphost.ErrObservationAccountWorksPageSize) || calls != 1 {
		t.Fatalf("changed page size: calls=%d, error=%v", calls, err)
	}
	input.Limit = 0
	observer = apphost.NewObserver(registry, dir)
	for expectedCursor = 1; expectedCursor < 4; expectedCursor++ {
		page, err := observer.Read(ctx, input)
		if err != nil || !page.Resumed || page.Complete != (expectedCursor == 3) || page.Chunks[0].Position != int(expectedCursor) {
			t.Fatalf("page %d: %+v %v", expectedCursor+1, page, err)
		}
		if expectedCursor == 3 && !strings.Contains(page.Coverage, "四页上限") {
			t.Fatalf("partial coverage was hidden: %+v", page)
		}
	}
	if repeated, err := observer.Read(ctx, input); err != nil || !repeated.Cached || len(repeated.Chunks) != 0 || calls != 4 {
		t.Fatalf("terminal checkpoint: %+v %v", repeated, err)
	}
	files, err := os.ReadDir(filepath.Join(dir, "application-observations"))
	if err != nil || len(files) != 1 {
		t.Fatalf("checkpoint files: %v", err)
	}
	checkpoint, err := os.ReadFile(filepath.Join(dir, "application-observations", files[0].Name()))
	if err != nil || strings.Contains(string(checkpoint), first.Chunks[0].SourceID) {
		t.Fatalf("private work was persisted: %v", err)
	}
	input.Refresh = true
	expectedCount = "10"
	expectedCursor = 0
	if page, err := observer.Read(ctx, input); err != nil || page.Resumed || page.Chunks[0].Position != 0 || calls != 5 {
		t.Fatalf("refreshed page: %+v %v", page, err)
	}
	if _, err := app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "disconnect", Revision: view.Revision}); err != nil {
		t.Fatal(err)
	}
	if err := observer.Check(ctx, first); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("past observation retained access: %v", err)
	}
	if _, err := observer.Read(ctx, input); !errors.Is(err, apphost.ErrObservationForbidden) || calls != 5 {
		t.Fatalf("revoked work read: calls=%d error=%v", calls, err)
	}
}

func TestAccountWorksRejectsCursorCycle(t *testing.T) {
	cursor := accountWorksCursor{Count: 10, Previous: []int64{7, 9}}
	page := accountWorksWirePage{HasMore: true, Cursor: 7}
	if _, err := accountWorksPage(page, cursor, "authorized"); !errors.Is(err, apphost.ErrObservationUpstreamUnavailable) {
		t.Fatalf("repeated continuation: %v", err)
	}
}

func TestAccountWorksDropsInFlightPageAfterScopeRemoved(t *testing.T) {
	ctx := context.Background()
	app := New(t.TempDir())
	const scopes = "user_info,video.list.bind"
	view := authorizeWorksFixture(t, app, true, scopes, scopes)
	registry, err := apphost.New(app.Registration())
	if err != nil {
		t.Fatal(err)
	}
	observer := apphost.NewObserver(registry, t.TempDir())
	entered, release := make(chan struct{}), make(chan struct{})
	app.client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return jsonResponse(`{"data":{"error_code":0,"has_more":false,"list":[{"video_id":"7000000000000000001"}]},"extra":{"error_code":0}}`), nil
		}
	})
	completed := make(chan error, 1)
	go func() {
		_, err := observer.Read(ctx, apphost.ObservationInput{AppID: ID, ConnectionID: accountConnectionID, ContentID: accountWorksContentID, Field: apphost.ObservationAccountWorks})
		completed <- err
	}()
	select {
	case err := <-completed:
		t.Fatalf("request never reached the provider: %v", err)
	case <-entered:
	}
	_, err = app.AuthorizeAccount(ctx, "owner/session", AccountAuthorizationInput{Action: "disconnect", Revision: view.Revision})
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-completed; !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("late private observation escaped revocation: %v", err)
	}
}
