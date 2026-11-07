package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testPublicationApp(t *testing.T, transport roundTrip) *SearchApp {
	t.Helper()
	app := New(t.TempDir())
	app.now = func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) }
	app.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return app
}

func testPublicationAccount() accountAuthorization {
	return accountAuthorization{PublishVideo: true,
		OpenID: "open-id_123", AccessToken: "act.publish-token", ExpiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestUploadPublicationVideoUsesOfficialStreamingMultipartEndpoint(t *testing.T) {
	payload := "small-video-payload"
	app := testPublicationApp(t, func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Host != "open.douyin.com" || request.URL.Path != "/api/douyin/v1/video/upload_video/" {
			t.Fatalf("unexpected endpoint %s %s", request.Method, request.URL)
		}
		if request.URL.Query().Get("open_id") != "open-id_123" || request.Header.Get("access-token") != "act.publish-token" || request.Header.Get("Cookie") != "" {
			t.Fatalf("credential boundary mismatch: query=%v headers=%v", request.URL.Query(), request.Header)
		}
		contentType := request.Header.Get("Content-Type")
		mediaType, parameters, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "multipart/form-data" || parameters["boundary"] == "" {
			t.Fatalf("unexpected multipart content type %q: %v", contentType, err)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.ContentLength != int64(len(body)) {
			t.Fatalf("content length=%d, got %d", request.ContentLength, len(body))
		}
		reader := multipart.NewReader(strings.NewReader(string(body)), parameters["boundary"])
		part, err := reader.NextPart()
		if err != nil || part.FormName() != "video" || part.FileName() != "sample.mp4" {
			t.Fatalf("unexpected multipart part: name=%q file=%q err=%v", part.FormName(), part.FileName(), err)
		}
		got, err := io.ReadAll(part)
		if err != nil || string(got) != payload {
			t.Fatalf("unexpected multipart payload %q: %v", got, err)
		}
		return jsonResponse(`{"data":{"error_code":0,"video":{"video_id":"uploaded-video-token"}},"extra":{"error_code":0}}`), nil
	})
	videoID, err := app.uploadPublicationVideo(context.Background(), testPublicationAccount(), "sample.mp4", strings.NewReader(payload), int64(len(payload)))
	if err != nil || videoID != "uploaded-video-token" {
		t.Fatalf("upload result id=%q err=%v", videoID, err)
	}
}

func TestPublicationPartsUseDocumentedEndpointsAndNoCreateFallback(t *testing.T) {
	var calls []string
	app := testPublicationApp(t, func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.URL.Path)
		if request.Method != http.MethodPost || request.URL.Query().Get("open_id") != "open-id_123" || request.Header.Get("access-token") != "act.publish-token" {
			t.Fatalf("unexpected request %s %s %v", request.Method, request.URL, request.Header)
		}
		switch request.URL.Path {
		case "/api/douyin/v1/video/init_video_part_upload/":
			return jsonResponse(`{"data":{"error_code":0,"upload_id":"upload-token"},"extra":{"error_code":0}}`), nil
		case "/api/douyin/v1/video/upload_video_part/":
			if request.URL.Query().Get("upload_id") != "upload-token" || request.URL.Query().Get("part_number") != "2" {
				t.Fatalf("unexpected part query %v", request.URL.Query())
			}
			return jsonResponse(`{"data":{"error_code":0},"extra":{"error_code":0}}`), nil
		case "/api/douyin/v1/video/complete_video_part_upload/":
			if request.URL.Query().Get("upload_id") != "upload-token" {
				t.Fatalf("unexpected complete query %v", request.URL.Query())
			}
			return jsonResponse(`{"data":{"error_code":0,"video_id":"uploaded-by-parts"},"extra":{"error_code":0}}`), nil
		default:
			t.Fatalf("unexpected endpoint %q", request.URL.Path)
			return nil, nil
		}
	})
	account := testPublicationAccount()
	uploadID, err := app.initializePublicationUpload(context.Background(), account)
	if err != nil || uploadID != "upload-token" {
		t.Fatalf("init id=%q err=%v", uploadID, err)
	}
	if err := app.uploadPublicationPart(context.Background(), account, uploadID, 2, []byte("part-data")); err != nil {
		t.Fatal(err)
	}
	videoID, err := app.completePublicationUpload(context.Background(), account, uploadID)
	if err != nil || videoID != "uploaded-by-parts" {
		t.Fatalf("complete id=%q err=%v", videoID, err)
	}
	want := []string{"/api/douyin/v1/video/init_video_part_upload/", "/api/douyin/v1/video/upload_video_part/", "/api/douyin/v1/video/complete_video_part_upload/"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls=%v, want %v", calls, want)
	}
}

func TestCreatePublicationVideoValidatesReceiptAndClassifiesOnlyExplicitRejection(t *testing.T) {
	app := testPublicationApp(t, func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/douyin/v1/video/create_video/" || request.URL.Query().Get("open_id") != "open-id_123" {
			t.Fatalf("unexpected request %s", request.URL)
		}
		var payload struct {
			VideoID       string `json:"video_id"`
			Text          string `json:"text"`
			PrivateStatus int    `json:"private_status"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.VideoID != "uploaded-video-token" || payload.Text != "发布说明" || payload.PrivateStatus != 1 {
			t.Fatalf("unexpected payload %#v", payload)
		}
		return jsonResponse(`{"data":{"error_code":0,"video_id":"700123","item_id":"opaque-item-token"},"extra":{"error_code":0}}`), nil
	})
	receipt, err := app.createPublicationVideo(context.Background(), testPublicationAccount(), "uploaded-video-token", "发布说明", 1)
	if err != nil || receipt != (douyinPublishReceipt{VideoID: "700123", ItemID: "opaque-item-token"}) {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	app.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		// Some documented responses expose only the opaque item identifier. It
		// is an accepted request, but the workflow must not invent a numeric ID.
		return jsonResponse(`{"data":{"error_code":0,"item_id":"opaque-only-item"},"extra":{"error_code":0}}`), nil
	})
	receipt, err = app.createPublicationVideo(context.Background(), testPublicationAccount(), "uploaded-video-token", "", 0)
	if err != nil || receipt != (douyinPublishReceipt{ItemID: "opaque-only-item"}) {
		t.Fatalf("opaque-only receipt=%#v err=%v", receipt, err)
	}

	app.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"data":{"error_code":0,"video_id":"700124","item_id":"bad item"},"extra":{"error_code":0}}`), nil
	})
	_, err = app.createPublicationVideo(context.Background(), testPublicationAccount(), "uploaded-video-token", "", 0)
	if !errors.Is(err, errDouyinPublishUnavailable) {
		t.Fatalf("unsafe nonempty returned ID must not be accepted: %v", err)
	}

	app.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"data":{"error_code":2190007},"extra":{"error_code":0}}`), nil
	})
	_, err = app.createPublicationVideo(context.Background(), testPublicationAccount(), "uploaded-video-token", "发布说明", 0)
	if !errors.Is(err, errDouyinPublishRejected) {
		t.Fatalf("expected known create rejection, got %v", err)
	}

	app.client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"data":{"error_code":2100004},"extra":{"error_code":0}}`), nil
	})
	_, err = app.createPublicationVideo(context.Background(), testPublicationAccount(), "uploaded-video-token", "发布说明", 0)
	if errors.Is(err, errDouyinPublishRejected) || !errors.Is(err, errDouyinPublishUnavailable) {
		t.Fatalf("expected ambiguous upstream result, got %v", err)
	}
}

func TestUploadErrorDoesNotUseCreateRejectionSentinel(t *testing.T) {
	app := testPublicationApp(t, func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"data":{"error_code":2190007},"extra":{"error_code":0}}`), nil
	})
	_, err := app.uploadPublicationVideo(context.Background(), testPublicationAccount(), "sample.mp4", strings.NewReader("x"), 1)
	if errors.Is(err, errDouyinPublishRejected) || !errors.Is(err, errDouyinPublishInvalid) {
		t.Fatalf("upload error should be repairable, got %v", err)
	}
}
