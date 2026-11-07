package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (fn roundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func configuredApp(t *testing.T, value settings) (*SearchApp, string) {
	t.Helper()
	directory := t.TempDir()
	writeSettings(t, directory, value)
	app := New(directory)
	app.now = func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }
	return app, directory
}

func validSettings() settings {
	return settings{
		SchemaVersion: 1, Enabled: true, ClientToken: "clt.test-token-only", ExpiresAt: "2027-01-01T00:00:00Z", DeviceID: "101", Keywords: []string{"亲子教育", "演示搜索"},
	}
}

func writeSettings(t *testing.T, directory string, value settings) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, settingsName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func targetFor(t *testing.T, app *SearchApp, keyword string) apphost.ObservationAccess {
	t.Helper()
	targets, err := app.ObservationTargets(context.Background(), connectionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.Label == keyword {
			return apphost.ObservationAccess{ConnectionRef: target.ConnectionRef, ContentID: target.ContentID, Field: apphost.ObservationSearch}
		}
	}
	t.Fatalf("target for %q not found", keyword)
	return apphost.ObservationAccess{}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestSearchUsesExactOfficialCredentialScopeAndStablePaging(t *testing.T) {
	app, _ := configuredApp(t, validSettings())
	access := targetFor(t, app, "亲子教育")
	var calls int
	app.client = &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "open.douyin.com" || request.URL.Path != "/dy_open_api/v1/search/video/" {
			t.Fatalf("unexpected endpoint: %s %s", request.Method, request.URL.String())
		}
		if request.Header.Get("access-token") != "clt.test-token-only" || request.Header.Get("Cookie") != "" || request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("credential scope leak: header=%q cookie=%q content-type=%q", request.Header.Get("access-token"), request.Header.Get("Cookie"), request.Header.Get("Content-Type"))
		}
		query := request.URL.Query()
		if query.Get("keyword") != "亲子教育" || query.Get("device_id") != "101" || query.Get("count") != "2" || query.Get("access_token") != "" {
			t.Fatalf("unexpected query: %v", query)
		}
		if calls == 1 {
			if query.Get("cursor") != "0" || query.Get("search_id") != "" {
				t.Fatalf("unexpected first cursor: %v", query)
			}
			return jsonResponse(`{"err_no":0,"data":{"data":{"cursor":2,"has_more":true,"search_id":"opaque-search","video_list":[{"item_id":"7001","title":"第一条标题","create_time":1760000000,"link":"https://www.douyin.com/video/7001"},{"item_id":7002,"title":"第二条标题","create_time":0,"link":"https://www.douyin.com/video/9000"}]}}}`), nil
		}
		if query.Get("cursor") != "2" || query.Get("search_id") != "opaque-search" {
			t.Fatalf("unexpected continuation cursor: %v", query)
		}
		return jsonResponse(`{"err_no":0,"data":{"data":{"cursor":4,"has_more":false,"search_id":"opaque-search","video_list":[{"item_id":"7003","title":"第三条标题","create_time":1760000002}]}}}`), nil
	})}

	first, err := app.ReadObservation(context.Background(), apphost.ObservationRequest{ObservationAccess: access, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if first.Complete || first.NextCursor == "" || len(first.Chunks) != 2 || first.Chunks[0].SourceID != "7001" || first.Chunks[0].PublishedAt == "" || first.Chunks[0].SourceURL != "https://www.douyin.com/video/7001" || first.Chunks[1].SourceURL != "" || first.Chunks[1].PublishedAt != "" {
		t.Fatalf("unexpected first page: %#v", first)
	}
	for _, chunk := range first.Chunks {
		if chunk.FollowUpField != "" || chunk.FollowUpContentID != "" {
			t.Fatal("legacy settings exposed a player follow-up without authorization")
		}
	}
	if bytes.Contains([]byte(first.NextCursor), []byte("亲子教育")) || bytes.Contains([]byte(first.NextCursor), []byte("clt.test-token-only")) {
		t.Fatalf("cursor leaks owner keyword or token: %q", first.NextCursor)
	}
	second, err := app.ReadObservation(context.Background(), apphost.ObservationRequest{ObservationAccess: access, Cursor: first.NextCursor, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Complete || second.NextCursor != "" || len(second.Chunks) != 1 || second.Chunks[0].Position != 2 {
		t.Fatalf("unexpected second page: %#v", second)
	}
	_, err = app.ReadObservation(context.Background(), apphost.ObservationRequest{ObservationAccess: access, Cursor: first.NextCursor, Limit: 3})
	if !errors.Is(err, apphost.ErrObservationSearchPageSize) {
		t.Fatalf("page-size change error = %v", err)
	}
	view, err := app.View()
	if err != nil || strings.Contains(mustJSON(t, view), "clt.test-token-only") || strings.Contains(mustJSON(t, view), "101") {
		t.Fatalf("unsafe view: %#v, %v", view, err)
	}
}

func TestSearchRejectsUnapprovedOrExpiredPolicy(t *testing.T) {
	app, directory := configuredApp(t, validSettings())
	access := targetFor(t, app, "亲子教育")
	access.ContentID = keywordContentID("未授权关键词")
	if _, err := app.ReadObservation(context.Background(), apphost.ObservationRequest{ObservationAccess: access}); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("unapproved keyword error = %v", err)
	}
	value := validSettings()
	value.ExpiresAt = "2020-01-01T00:00:00Z"
	writeSettings(t, directory, value)
	access = apphost.ObservationAccess{ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: connectionID, AccountID: accountID}, ContentID: keywordContentID("亲子教育"), Field: apphost.ObservationSearch}
	if _, err := app.ReadObservation(context.Background(), apphost.ObservationRequest{ObservationAccess: access}); !errors.Is(err, apphost.ErrObservationCredentialExpired) {
		t.Fatalf("expired error = %v", err)
	}
	view, err := app.View()
	if err != nil || view.Status != "expired" {
		t.Fatalf("expired view = %#v, %v", view, err)
	}
}

func TestSearchClassifiesPlatformFailures(t *testing.T) {
	tests := []struct {
		name string
		code int
		want error
	}{
		{"expired-token", 28001008, apphost.ErrObservationCredentialExpired},
		{"missing-capability", 28001014, apphost.ErrObservationCapabilityDenied},
		{"throttled", 28003017, apphost.ErrObservationThrottled},
		{"upstream-status", 28001005, apphost.ErrObservationUpstreamUnavailable},
		{"unavailable-content", 999999, apphost.ErrObservationContentUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, _ := configuredApp(t, validSettings())
			access := targetFor(t, app, "亲子教育")
			app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
				return jsonResponse(`{"err_no":` + strconv.Itoa(test.code) + `,"err_msg":"provider status","data":{"data":{}}}`), nil
			})}
			_, err := app.ReadObservation(context.Background(), apphost.ObservationRequest{ObservationAccess: access})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSearchRejectsMissingSuccessfulPayload(t *testing.T) {
	app, _ := configuredApp(t, validSettings())
	access := targetFor(t, app, "亲子教育")
	app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"err_no":0}`), nil
	})}
	if _, err := app.ReadObservation(context.Background(), apphost.ObservationRequest{ObservationAccess: access}); !errors.Is(err, apphost.ErrObservationUpstreamUnavailable) {
		t.Fatalf("missing payload error = %v", err)
	}
}

func TestSearchConnectionStatesRemainVisible(t *testing.T) {
	directory := t.TempDir()
	app := New(directory)
	connections, err := app.Connections(context.Background())
	if err != nil || len(connections) != 3 || connections[0].ID != connectionID || connections[0].Configuration != "unconfigured" ||
		connections[2].ID != accountConnectionID || connections[2].Configuration != "unconfigured" {
		t.Fatalf("unconfigured connection = %#v, %v", connections, err)
	}
	value := validSettings()
	value.Enabled = false
	writeSettings(t, directory, value)
	connections, err = app.Connections(context.Background())
	if err != nil || connections[0].Configuration != "disabled" {
		t.Fatalf("disabled connection = %#v, %v", connections, err)
	}
	if err := os.WriteFile(filepath.Join(directory, settingsName), []byte(`{"schema_version":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	connections, err = app.Connections(context.Background())
	if err != nil || connections[0].Configuration != "invalid" {
		t.Fatalf("invalid connection = %#v, %v", connections, err)
	}
}

func TestSearchRejectsRevocationDuringRequest(t *testing.T) {
	app, directory := configuredApp(t, validSettings())
	access := targetFor(t, app, "亲子教育")
	app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		value := validSettings()
		value.Enabled = false
		writeSettings(t, directory, value)
		return jsonResponse(`{"err_no":0,"data":{"data":{"cursor":1,"has_more":false,"search_id":"unused","video_list":[]}}}`), nil
	})}
	_, err := app.ReadObservation(context.Background(), apphost.ObservationRequest{ObservationAccess: access})
	if !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("revocation error = %v", err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
