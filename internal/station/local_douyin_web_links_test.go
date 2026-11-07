package station

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

func setSyntheticDouyinWebScope(t *testing.T, f *localHomeFixture, view map[string]any, scope string) map[string]any {
	t.Helper()
	code, result := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": scope}, true)
	if code != 200 || result["read_scope"] != scope {
		t.Fatalf("scope change: %d %v", code, result)
	}
	return result
}

func localWebFollowUps(result map[string]any) []string {
	var ids []string
	chunks, _ := result["chunks"].([]any)
	for _, raw := range chunks {
		c := raw.(map[string]any)
		if id, ok := c["follow_up_content_id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestLocalDouyinWebVideosNeedExplicitScopeAndActualHomeLink(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	const first, second, other = "7000000000000000001", "7000000000000000002", "7000000000000000003"
	m.readPage = func(context.Context, string, func() error) (stationbrowser.DouyinWebPage, error) {
		return stationbrowser.DouyinWebPage{
			Title: "合成首页", Text: "今天实际可见的首页正文", Links: []stationbrowser.DouyinWebLink{{VideoID: first, Text: "海边的风"}, {VideoID: second, Text: "夜晚的灯"}}}, nil
	}
	videoReads := 0
	m.readVideo = func(ctx context.Context, _ string, id string, authorize func() error) (stationbrowser.DouyinWebPage, error) {
		videoReads++
		if id != first {
			t.Errorf("unexpected video %s", id)
		}
		if err := authorize(); err != nil {
			return stationbrowser.DouyinWebPage{}, err
		}
		return stationbrowser.DouyinWebPage{Title: "合成视频页", Text: "这只是视频页面实际可见的说明文字。", Links: []stationbrowser.DouyinWebLink{{VideoID: other, Text: "不是首页来源的推荐"}}}, nil
	}
	input := syntheticDouyinWebReadInput(4096)
	code, homeOnly := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || len(localWebFollowUps(homeOnly)) != 0 {
		t.Fatalf("old home-only grant expanded: %d %v", code, homeOnly)
	}
	view = setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	code, home := f.request("POST", "/api/local/apps/observation", input, true)
	ids := localWebFollowUps(home)
	if code != 200 || len(ids) != 2 {
		t.Fatalf("home links: %d %v", code, home)
	}
	chunks := home["chunks"].([]any)
	for _, raw := range chunks {
		if raw.(map[string]any)["source_url"] != localDouyinWebHomeURL {
			t.Fatal("link source replaced by another page")
		}
	}
	input["content_id"] = ids[0]
	code, video := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || videoReads != 1 || len(localWebFollowUps(video)) != 0 {
		t.Fatalf("video: %d %v reads=%d", code, video, videoReads)
	}
	url, _ := stationbrowser.DouyinVideoURL(first)
	if got := video["chunks"].([]any)[0].(map[string]any)["source_url"]; got != url {
		t.Fatal("video text source mismatched", got)
	}
	videoSource := video["chunks"].([]any)[0].(map[string]any)["source_id"].(string)
	var homeSource string
	for _, raw := range chunks {
		if raw.(map[string]any)["follow_up_content_id"] == ids[0] {
			homeSource = raw.(map[string]any)["source_id"].(string)
		}
	}
	for _, forged := range []string{
		localDouyinWebFollowUpID(homeSource, other),  // real source, absent video
		localDouyinWebFollowUpID(videoSource, other), // recommendation not found on home
		localDouyinWebFollowUpID(strings.Repeat("a", 32), first),
		douyinapp.WebVideoContentPrefix + "abc",
	} {
		input["content_id"] = forged
		if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 || videoReads != 1 {
			t.Fatalf("forged source performed a read: %d reads=%d", code, videoReads)
		}
	}
	input["content_id"] = ids[0]
	view = setSyntheticDouyinWebScope(t, f, view, "home")
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 || videoReads != 1 {
		t.Fatal("downgraded scope reused cached video")
	}
	view = setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 {
		t.Fatal("new grant reused old link proof")
	}
	input["content_id"] = "home"
	_, freshHome := f.request("POST", "/api/local/apps/observation", input, true)
	newIDs := localWebFollowUps(freshHome)
	if len(newIDs) != 2 || newIDs[0] == ids[0] {
		t.Fatal("new grant did not capture its own home link proof")
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": view["revision"], "approve_profile": true}, true)
	if view["read_scope"] != "home" || view["read_enabled"] != false {
		t.Fatal("relogin kept widened scope")
	}
}

func TestLocalDouyinWebLinksStayWithinCharacterBudgetAndRemainReachable(t *testing.T) {
	s := localDouyinWebSnapshot{Version: 1, ID: strings.Repeat("b", 32), ContentID: "home", SourceURL: localDouyinWebHomeURL, Title: strings.Repeat("实际标题🙂", 30), Text: "真实正文，顺序应保留。"}
	for i := 0; i < 80; i++ {
		s.Links = append(s.Links, stationbrowser.DouyinWebLink{VideoID: fmt.Sprintf("700000000000000%04d", i), Text: fmt.Sprintf("第%d个实际链接，🙂", i)})
	}
	offset := 0
	seen := map[string]bool{}
	for pages := 0; pages < 1000; pages++ {
		page, err := localDouyinWebPage(s, offset, 64)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, chunk := range page.Chunks {
			n += utf8.RuneCountInString(chunk.Text)
			if chunk.FollowUpContentID != "" {
				seen[chunk.FollowUpContentID] = true
			}
		}
		if n > 64 {
			t.Fatalf("page exceeded requested character budget: %d", n)
		}
		if page.Complete {
			break
		}
		raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		var cursor localDouyinWebCursor
		if json.Unmarshal(raw, &cursor) != nil || cursor.Offset <= offset {
			t.Fatal("pagination did not advance")
		}
		offset = cursor.Offset
	}
	if len(seen) != 80 {
		t.Fatalf("some actual links were silently omitted: %d", len(seen))
	}
	defaultPage, err := localDouyinWebPage(s, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	explicitPage, err := localDouyinWebPage(s, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	count := func(p apphost.ObservationPage) int {
		n := 0
		for _, c := range p.Chunks {
			if c.FollowUpContentID != "" {
				n++
			}
		}
		return n
	}
	if count(defaultPage) != 8 || defaultPage.Complete || count(explicitPage) != 80 || !explicitPage.Complete {
		t.Fatal("default selection became a capability ceiling")
	}
	compact, _ := json.Marshal(defaultPage)
	full, _ := json.Marshal(explicitPage)
	if len(compact) >= len(full)/2 {
		t.Fatal("default candidate preview did not reduce response size")
	}
	t.Logf("synthetic 80-link payload: default=%d bytes; explicit-all=%d bytes", len(compact), len(full))
}

func TestLocalDouyinWebVideoTitleFitsExplicitPageBudget(t *testing.T) {
	s := localDouyinWebSnapshot{ID: strings.Repeat("c", 32), ContentID: "synthetic-video", SourceURL: "https://www.douyin.com/video/7000000000000000001", Title: strings.Repeat("长标题🙂", 20), Text: "实际视频页面文字"}
	page, err := localDouyinWebPage(s, 0, 7)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range page.Chunks {
		n += utf8.RuneCountInString(c.Text)
	}
	if n != 7 || page.Complete || page.NextCursor == "" {
		t.Fatalf("video title exceeded explicit budget: %d %+v", n, page)
	}
}

func TestLocalDouyinWebVideoCursorCannotLoadHomeSnapshot(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	m.readPage = func(context.Context, string, func() error) (stationbrowser.DouyinWebPage, error) {
		return stationbrowser.DouyinWebPage{Text: "原首页正文", Links: []stationbrowser.DouyinWebLink{{VideoID: "7000000000000000001", Text: "确切链接"}}}, nil
	}
	_, home := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebReadInput(4096), true)
	id := localWebFollowUps(home)[0]
	identity, _ := m.home.identity.Read()
	p := &localDouyinWebObservation{home: m.home, installation: "douyin-search"}
	a, err := p.state(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a.record.FishID != identity.FishID {
		t.Fatal("identity differs")
	}
	var source string
	for _, raw := range home["chunks"].([]any) {
		source = raw.(map[string]any)["source_id"].(string)
		break
	}
	raw, _ := json.Marshal(localDouyinWebCursor{ID: source, Offset: 1})
	_, err = p.Read(t.Context(), apphost.ObservationRequest{ObservationAccess: apphost.ObservationAccess{ConnectionRef: p.ref(a), ContentID: id, Field: apphost.ObservationPageText}, Cursor: base64.RawURLEncoding.EncodeToString(raw)})
	if !errors.Is(err, apphost.ErrObservationWebSnapshotChanged) {
		t.Fatalf("cross-content cursor accepted: %v", err)
	}
}

func TestLocalDouyinWebScopeRejectsInvalidOrUnconfirmedExpansion(t *testing.T) {
	f, _ := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": 0, "allow_read": true, "read_scope": "home_and_videos"}, true); code != 409 {
		t.Fatal("unconfirmed scope granted")
	}
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": 0, "allow_read": true, "read_scope": "entire-internet"}, true); code != 400 {
		t.Fatal("unknown scope accepted")
	}
}
