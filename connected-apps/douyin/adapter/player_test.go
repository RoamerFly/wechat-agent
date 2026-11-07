package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"wechat-bot/internal/apphost"
)

func TestFetchPlayerUsesAnonymousOfficialEndpointAndCanonicalizesResult(t *testing.T) {
	app := New(t.TempDir())
	app.client = &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "open.douyin.com" || request.URL.Path != "/api/douyin/v1/video/get_iframe_by_video" {
			t.Fatalf("unexpected endpoint: %s %s", request.Method, request.URL.String())
		}
		if request.URL.RawQuery != "video_id=7001" || request.Header.Get("Cookie") != "" || request.Header.Get("access-token") != "" || request.Header.Get("device-id") != "" {
			t.Fatalf("anonymous lookup leaked state: query=%q headers=%v", request.URL.RawQuery, request.Header)
		}
		return jsonResponse(`{"err_no":0,"data":{"iframe_code":"<iframe title='official player' src='https://open.douyin.com/player/video?vid=7001&amp;autoplay=0' frameborder='0' allowfullscreen> </iframe>","video_title":" 官方标题 ","video_width":720,"video_height":1280}}`), nil
	})}

	entry, err := app.fetchPlayer(context.Background(), "7001")
	if err != nil {
		t.Fatal(err)
	}
	if entry.URL != "https://open.douyin.com/player/video?autoplay=0&vid=7001" || entry.Title != "官方标题" || entry.Width != 720 || entry.Height != 1280 {
		t.Fatalf("unexpected sanitized entry: %#v", entry)
	}
}

func TestFetchPlayerRejectsUnsafeIFrame(t *testing.T) {
	cases := []string{
		`<iframe src="https://open.douyin.com/player/video?vid=7001&autoplay=1"></iframe>`,
		`<iframe src="https://open.douyin.com/player/video?vid=other&autoplay=0"></iframe>`,
		`<iframe src="https://evil.example/player/video?vid=7001&autoplay=0"></iframe>`,
		`<iframe src="https://open.douyin.com/player/video?vid=7001&autoplay=0&extra=1"></iframe>`,
		`<iframe src="https://open.douyin.com/player/video?vid=7001;autoplay=0"></iframe>`,
		`<iframe src="https://open.douyin.com/player/video?vid=7001&autoplay=0#part"></iframe>`,
		`<iframe src="https://open.douyin.com/player/video?vid=7001&autoplay=0"></iframe><iframe src="https://open.douyin.com/player/video?vid=7001&autoplay=0"></iframe>`,
		`<iframe src="https://open.douyin.com/player/video?vid=7001&autoplay=0">content</iframe>`,
	}
	for _, iframe := range cases {
		t.Run(iframe, func(t *testing.T) {
			app := New(t.TempDir())
			app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
				return jsonResponse(`{"err_no":0,"data":{"iframe_code":` + quoteJSON(iframe) + `,"video_title":"title","video_width":720,"video_height":1280}}`), nil
			})}
			_, err := app.fetchPlayer(context.Background(), "7001")
			if !errors.Is(err, apphost.ErrObservationUpstreamUnavailable) {
				t.Fatalf("error = %v, want unsafe upstream response", err)
			}
		})
	}
}

func TestFetchPlayerClassifiesNonPublicAndLocalInvalidIDs(t *testing.T) {
	app := New(t.TempDir())
	app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"err_no":28003004,"err_msg":"not public"}`), nil
	})}
	if _, err := app.fetchPlayer(context.Background(), "7001"); !errors.Is(err, apphost.ErrObservationContentUnavailable) {
		t.Fatalf("error = %v, want non-public content", err)
	}
	called := false
	app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { called = true; return nil, nil })}
	if _, err := app.fetchPlayer(context.Background(), "https://www.douyin.com/video/7001"); !errors.Is(err, apphost.ErrObservationInvalid) || called {
		t.Fatalf("invalid VideoID error=%v called=%v", err, called)
	}
}

func TestFetchPlayerAllowsOmittedOptionalDimensions(t *testing.T) {
	app := New(t.TempDir())
	app.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"err_no":0,"data":{"iframe_code":"<iframe src='https://open.douyin.com/player/video?vid=7001&amp;autoplay=0'></iframe>","video_title":"title"}}`), nil
	})}
	entry, err := app.fetchPlayer(context.Background(), "7001")
	if err != nil || entry.Width != 0 || entry.Height != 0 {
		t.Fatalf("entry=%#v err=%v", entry, err)
	}
}

func TestFetchPlayerDoesNotFollowRedirects(t *testing.T) {
	app := New(t.TempDir())
	calls := 0
	app.client = &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://evil.example/"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})}
	if _, err := app.fetchPlayer(context.Background(), "7001"); !errors.Is(err, apphost.ErrObservationUpstreamUnavailable) || calls != 1 {
		t.Fatalf("redirect error=%v calls=%d", err, calls)
	}
}

func quoteJSON(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, value := range value {
		switch value {
		case '\\', '"':
			builder.WriteByte('\\')
			builder.WriteRune(value)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		default:
			builder.WriteRune(value)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
