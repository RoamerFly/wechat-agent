package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
)

func validOwnVideoSettings() ownVideoSettings {
	return ownVideoSettings{SchemaVersion: 1, Enabled: true, Scope: "posting.behavior", AccessToken: "act.test-only-token", OpenID: "open-id-101", ExpiresAt: "2027-01-01T00:00:00Z", VideoIDs: []string{"7001", "7002"}}
}

func configuredOwnVideoApp(t *testing.T, value ownVideoSettings) (*SearchApp, string) {
	t.Helper()
	directory := t.TempDir()
	writeOwnVideoSettings(t, directory, value)
	app := New(directory)
	app.now = func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }
	return app, directory
}

func writeOwnVideoSettings(t *testing.T, directory string, value ownVideoSettings) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ownVideoSettingsName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func ownVideoAccess(t *testing.T, app *SearchApp, videoID string) apphost.ObservationAccess {
	t.Helper()
	targets, err := app.ownVideoTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.ContentID == videoID {
			return apphost.ObservationAccess{ConnectionRef: target.ConnectionRef, ContentID: target.ContentID, Field: apphost.ObservationOwnVideo}
		}
	}
	t.Fatalf("own video target %q missing", videoID)
	return apphost.ObservationAccess{}
}

func TestOwnVideoUsesExactOfficialRequestAndOnlyReturnsPublicData(t *testing.T) {
	app, _ := configuredOwnVideoApp(t, validOwnVideoSettings())
	access := ownVideoAccess(t, app, "7001")
	connection := app.ownVideoConnection()
	if connection.ID != ownVideoConnectionID || connection.AccountID == "" || strings.Contains(connection.AccountID, "open-id-101") || connection.Configuration != "ready" {
		t.Fatalf("unsafe connection: %#v", connection)
	}
	app.client = &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Host != "open.douyin.com" || request.URL.Path != "/api/douyin/v1/video/video_basic_info/" {
			t.Fatalf("unexpected endpoint: %s %s", request.Method, request.URL.String())
		}
		if request.URL.Query().Get("open_id") != "open-id-101" || request.URL.Query().Get("access_token") != "" || request.Header.Get("access-token") != "act.test-only-token" || request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Fatalf("credential scope leak: url=%s headers=%v", request.URL, request.Header)
		}
		var requestBody struct {
			VideoIDs []string `json:"video_ids"`
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil || len(requestBody.VideoIDs) != 1 || requestBody.VideoIDs[0] != "7001" {
			t.Fatalf("unexpected body %#v, %v", requestBody, err)
		}
		return jsonResponse(`{"data":{"error_code":0,"list":[{"video_id":"7001","video_status":5,"title":"公开标题","create_time":1760000000}]},"extra":{"error_code":0}}`), nil
	})}
	page, err := app.readOwnVideo(context.Background(), apphost.ObservationRequest{ObservationAccess: access})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Complete || page.OwnVideoProof == nil || page.OwnVideoProof.State != "public" || page.OwnVideoProof.VideoID != "7001" || page.OwnVideoProof.CreatedAt == "" || len(page.Chunks) != 1 || page.Chunks[0].SourceID != "7001" || page.Chunks[0].SourceURL != "" || page.Chunks[0].PublishedAt != "" {
		t.Fatalf("unexpected page: %#v", page)
	}
}

func TestOwnVideoProtectsNonPublicAndUnknownStates(t *testing.T) {
	for _, test := range []struct {
		name  string
		code  int
		state string
	}{
		{"not-public", 2, "not_public"}, {"reviewing", 4, "reviewing"}, {"unknown", 99, "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _ := configuredOwnVideoApp(t, validOwnVideoSettings())
			access := ownVideoAccess(t, app, "7001")
			app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
				return jsonResponse(`{"data":{"error_code":0,"list":[{"video_id":"7001","video_status":` + strconv.Itoa(test.code) + `,"title":"不得泄露","create_time":1760000000}]},"extra":{"error_code":0}}`), nil
			})}
			page, err := app.readOwnVideo(context.Background(), apphost.ObservationRequest{ObservationAccess: access})
			if err != nil {
				t.Fatal(err)
			}
			if page.OwnVideoProof == nil || page.OwnVideoProof.State != test.state || page.OwnVideoProof.CreatedAt != "" || len(page.Chunks) != 0 {
				t.Fatalf("protected page = %#v", page)
			}
		})
	}
}

func TestOwnVideoRejectsOtherRowsAndRevocationDuringRequest(t *testing.T) {
	app, directory := configuredOwnVideoApp(t, validOwnVideoSettings())
	access := ownVideoAccess(t, app, "7001")
	app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		value := validOwnVideoSettings()
		value.Enabled = false
		writeOwnVideoSettings(t, directory, value)
		return jsonResponse(`{"data":{"error_code":0,"list":[{"video_id":"7001","video_status":5,"title":"本次结果","create_time":1760000000}]},"extra":{"error_code":0}}`), nil
	})}
	if _, err := app.readOwnVideo(context.Background(), apphost.ObservationRequest{ObservationAccess: access}); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("revocation error = %v", err)
	}
}

func TestOwnVideoRejectsMismatchedResponseAndMissingStatus(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"other-video", `{"data":{"error_code":0,"list":[{"video_id":"7002","video_status":5,"title":"错误对象"}]},"extra":{"error_code":0}}`},
		{"missing-status", `{"data":{"error_code":0,"list":[{"video_id":"7001","title":"无状态"}]},"extra":{"error_code":0}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _ := configuredOwnVideoApp(t, validOwnVideoSettings())
			access := ownVideoAccess(t, app, "7001")
			app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
				return jsonResponse(test.body), nil
			})}
			if _, err := app.readOwnVideo(context.Background(), apphost.ObservationRequest{ObservationAccess: access}); !errors.Is(err, apphost.ErrObservationUpstreamUnavailable) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOwnVideoClassifiesSettingsAndPlatformErrors(t *testing.T) {
	app, directory := configuredOwnVideoApp(t, validOwnVideoSettings())
	access := ownVideoAccess(t, app, "7001")
	value := validOwnVideoSettings()
	value.ExpiresAt = "2020-01-01T00:00:00Z"
	writeOwnVideoSettings(t, directory, value)
	if _, err := app.readOwnVideo(context.Background(), apphost.ObservationRequest{ObservationAccess: access}); !errors.Is(err, apphost.ErrObservationCredentialExpired) {
		t.Fatalf("expired error = %v", err)
	}
	for _, test := range []struct {
		code int
		want error
	}{{28001008, apphost.ErrObservationCredentialExpired}, {28001014, apphost.ErrObservationCapabilityDenied}, {28003017, apphost.ErrObservationThrottled}, {2100004, apphost.ErrObservationUpstreamUnavailable}, {2118110, apphost.ErrObservationThrottled}, {2100005, apphost.ErrObservationInvalid}, {99999, apphost.ErrObservationContentUnavailable}} {
		if err := ownVideoResponseError(test.code); !errors.Is(err, test.want) {
			t.Fatalf("error code %d = %v", test.code, err)
		}
	}
}
