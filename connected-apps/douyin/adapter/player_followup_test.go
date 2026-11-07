package adapter

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/apphost"
)

func TestSearchFollowupPlayerRechecksPolicyAndPlatform(t *testing.T) {
	const videoID = "7000000000000000001"
	settings := validSettings()
	settings.FollowPublicVideos = true
	app, directory := configuredApp(t, settings)
	checkpoints := filepath.Join(t.TempDir(), "observation")
	observer := func(provider *SearchApp) *apphost.Observer {
		t.Helper()
		registry, err := apphost.New(provider.Registration())
		if err != nil {
			t.Fatal(err)
		}
		return apphost.NewObserver(registry, checkpoints)
	}
	var searches, playerLookups int
	public := true
	revokeDuringRead := false
	transport := roundTrip(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/dy_open_api/v1/search/video/":
			searches++
			if r.Header.Get("access-token") != settings.ClientToken {
				t.Fatal("search requires its own application credential")
			}
			return jsonResponse(`{"err_no":0,"data":{"data":{"cursor":2,"has_more":false,"video_list":[{"item_id":"7000000000000000001","title":"候选作品","link":"https://www.douyin.com/video/7000000000000000001"},{"item_id":"7000000000000000002","title":"来源链接不符","link":"https://www.douyin.com/video/7000000000000000003"}]}}}`), nil
		case "/api/douyin/v1/video/get_iframe_by_video":
			playerLookups++
			if r.URL.Query().Get("video_id") != videoID || r.Header.Get("access-token") != "" || r.Header.Get("Cookie") != "" {
				t.Fatal("wrong video ID or credentials in anonymous player lookup")
			}
			if revokeDuringRead {
				settings.FollowPublicVideos = false
				writeSettings(t, directory, settings)
			}
			if !public {
				return jsonResponse(`{"err_no":28003004,"err_msg":"private"}`), nil
			}
			return jsonResponse(`{"err_no":0,"data":{"video_title":"官方标题","video_width":720,"video_height":1280,"iframe_code":"<iframe src=\"https://open.douyin.com/player/video?vid=7000000000000000001&autoplay=0\"></iframe>"}}`), nil
		default:
			t.Fatalf("unexpected route: %s", r.URL.Path)
			return nil, nil
		}
	})
	app.client.Transport = transport
	first := observer(app)
	search := apphost.ObservationInput{AppID: ID, ConnectionID: connectionID, ContentID: keywordContentID("亲子教育"), Field: apphost.ObservationSearch}
	page, err := first.Read(t.Context(), search)
	if err != nil || len(page.Chunks) != 2 || page.Chunks[1].FollowUpContentID != "" {
		t.Fatalf("search scope = %+v %v", page, err)
	}
	if page.Chunks[0].FollowUpField != apphost.ObservationPlayerEntry || page.Chunks[0].FollowUpContentID == "" {
		t.Fatal("verified search hit did not return a player follow-up")
	}
	followup := search
	followup.Field = page.Chunks[0].FollowUpField
	followup.ContentID = page.Chunks[0].FollowUpContentID

	restarted := New(directory)
	restarted.client.Transport = transport
	restarted.now = app.now
	read := observer(restarted)
	cached, err := read.Read(t.Context(), search)
	if err != nil || !cached.Cached || searches != 1 || cached.Chunks[0].FollowUpContentID != followup.ContentID {
		t.Fatalf("search cache did not preserve the follow-up: %+v %v", cached, err)
	}

	for _, invalid := range []string{videoID, strings.Replace(followup.ContentID, videoID, "7000000000000000002", 1)} {
		forged := followup
		forged.ContentID = invalid
		if _, err := read.Read(t.Context(), forged); !errors.Is(err, apphost.ErrObservationForbidden) {
			t.Fatalf("forged result not blocked: %v", err)
		}
	}
	if playerLookups != 0 {
		t.Fatal("unapproved result went upstream")
	}

	entry, err := read.Read(t.Context(), followup)
	if err != nil || entry.Cached || !entry.Complete || len(entry.Chunks) != 1 || entry.Chunks[0].Text != "官方标题" ||
		entry.Chunks[0].SourceID != videoID || !strings.HasPrefix(entry.Chunks[0].SourceURL, "https://open.douyin.com/player/video?") ||
		len(entry.Playback) != 0 || entry.PlaybackProof != nil || entry.AudioProof != nil || playerLookups != 1 {
		t.Fatalf("player entry incorrectly reported evidence: %+v %v", entry, err)
	}
	public = false
	if _, err := read.Read(t.Context(), followup); !errors.Is(err, apphost.ErrObservationContentUnavailable) || playerLookups != 2 {
		t.Fatalf("changed visibility reused old availability: %v, requests=%d", err, playerLookups)
	}
	public = true
	revokeDuringRead = true
	if _, err := read.Read(t.Context(), followup); !errors.Is(err, apphost.ErrObservationForbidden) || playerLookups != 3 {
		t.Fatalf("late result after revocation: %v, requests=%d", err, playerLookups)
	}
	if err := read.Check(t.Context(), entry); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("previous entry usable after policy revocation: %v", err)
	}
}
