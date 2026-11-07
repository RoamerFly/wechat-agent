package station

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/stationbrowser"
)

func enableSyntheticDouyinComments(t *testing.T, f *localHomeFixture, view map[string]any, allow bool) map[string]any {
	t.Helper()
	code, out := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home_and_videos", "allow_comments": allow}, true)
	if code != 200 || out["comments_enabled"] != allow {
		t.Fatalf("comments permission: %d %#v", code, out)
	}
	return out
}

func TestLocalDouyinCommentsAvailableToNormalChatWithoutModelNetwork(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	enableSyntheticDouyinComments(t, f, enableSyntheticDouyinWebRead(t, f), true)
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	m.readComments = func(context.Context, string, stationbrowser.DouyinCommentsRequest, func() error) (stationbrowser.DouyinCommentPage, error) {
		return stationbrowser.DouyinCommentPage{Comments: []stationbrowser.DouyinComment{{ID: "103", AuthorID: "commenter-103", Author: "合成用户", Text: "我喜欢视频中的蓝色天空"}}, Complete: true}, nil
	}
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return &syntheticObservationModel{call: func(ctx context.Context, system string, turns []map[string]string, tools []mcptools.Tool) (string, error) {
			calls++
			if calls == 1 {
				return `{"action":"tool","tool":"discover_tools","arguments":{"query":"app_observe"}}`, nil
			}
			if calls == 2 {
				if !hasTool(tools, "app_observe") {
					t.Fatal("comments not discoverable")
				}
				request := map[string]any{"action": "tool", "tool": "app_observe", "arguments": map[string]any{"action": "read", "app_id": "douyin", "connection_id": "web-page-text", "content_id": id, "field": "comments"}}
				raw, _ := json.Marshal(request)
				return string(raw), nil
			}
			if !strings.Contains(localModelHistory(turns), "commenter-103") || !strings.Contains(localModelHistory(turns), "蓝色天空") {
				t.Fatal("actual comment did not reach model")
			}
			return `{"action":"reply","messages":["这位评论者喜欢蓝色天空。"],"reason":"synthetic"}`, nil
		}}, nil
	}
	if code, result := localChatSend(f, "douyin-comments-offline", "看看那条视频的评论"); code != 200 || result["status"] != "completed" {
		t.Fatalf("chat: %d %#v", code, result)
	}
}

func syntheticDouyinCommentsInput(id string, limit int) map[string]any {
	return map[string]any{"app_id": "douyin", "connection_id": "web-page-text", "content_id": id, "field": "comments", "limit": limit}
}

func TestLocalDouyinCommentsNormalObserverPreservesSourcesAndResumes(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	view = setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	reads := 0
	m.readComments = func(ctx context.Context, profile string, req stationbrowser.DouyinCommentsRequest, authorize func() error) (stationbrowser.DouyinCommentPage, error) {
		reads++
		if req.VideoID != "7000000000000000011" || req.Limit != 71 {
			t.Fatal("source or explicit limit changed", req)
		}
		if err := authorize(); err != nil {
			return stationbrowser.DouyinCommentPage{}, err
		}
		if req.Cursor == "" {
			return stationbrowser.DouyinCommentPage{Comments: []stationbrowser.DouyinComment{{ID: "101", AuthorID: "author-A", Author: "甲", Text: "评论里引用的内容并不是视频原话"}}, NextCursor: "101"}, nil
		}
		if req.Cursor != "101" {
			t.Fatal("cursor changed", req)
		}
		return stationbrowser.DouyinCommentPage{Comments: []stationbrowser.DouyinComment{{ID: "102", AuthorID: "author-B", Author: "乙", Text: "另一位评论者"}}, Complete: true}, nil
	}
	in := syntheticDouyinCommentsInput(id, 71)
	if code, _ := f.request("POST", "/api/local/apps/observation", in, true); code != 403 || reads != 0 {
		t.Fatal("text permission implied comments")
	}
	view = enableSyntheticDouyinComments(t, f, view, true)
	id = syntheticDouyinWebPlaybackTarget(t, f, m)
	in["content_id"] = id
	if code, _ := f.request("POST", "/api/local/apps/observation", in, false); code != 403 || reads != 0 {
		t.Fatal("missing CSRF accepted")
	}
	code, first := f.request("POST", "/api/local/apps/observation", in, true)
	if code != 200 || first["complete"] != false || reads != 1 {
		t.Fatalf("first: %d %#v", code, first)
	}
	chunk := first["chunks"].([]any)[0].(map[string]any)
	if chunk["source_id"] != "101" || chunk["source_url"] != "https://www.douyin.com/video/7000000000000000011" || !strings.Contains(chunk["text"].(string), "author-A") || first["audio_proof"] != nil {
		t.Fatal("comment lost attribution or became audio", first)
	}
	// The public entry point persists the opaque cursor; callers repeat the
	// original request instead of manufacturing a platform cursor.
	code, second := f.request("POST", "/api/local/apps/observation", in, true)
	if code != 200 || second["resumed"] != true || second["complete"] != true || reads != 2 {
		t.Fatalf("next: %d %#v", code, second)
	}
	chunk = second["chunks"].([]any)[0].(map[string]any)
	if chunk["source_id"] != "102" || chunk["position"] != float64(1) {
		t.Fatal("continuation source or position lost", chunk)
	}
	_, cached := f.request("POST", "/api/local/apps/observation", in, true)
	if cached["cached"] != true || reads != 2 {
		t.Fatal("unnecessary browser replay")
	}
	in["refresh"] = true
	if code, _ := f.request("POST", "/api/local/apps/observation", in, true); code != 200 || reads != 3 {
		t.Fatal("new comments cannot refresh", code)
	}
	view = enableSyntheticDouyinComments(t, f, view, false)
	if code, _ := f.request("POST", "/api/local/apps/observation", in, true); code != 403 || reads != 3 {
		t.Fatal("revocation reused comments")
	}
}

func TestLocalDouyinCommentsRevocationAndBrokenEvidence(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinComments(t, f, enableSyntheticDouyinWebRead(t, f), true)
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	in := syntheticDouyinCommentsInput(id, 20)
	m.readComments = func(context.Context, string, stationbrowser.DouyinCommentsRequest, func() error) (stationbrowser.DouyinCommentPage, error) {
		return stationbrowser.DouyinCommentPage{Comments: []stationbrowser.DouyinComment{{ID: "1", AuthorID: "someone", Text: "loaded"}}, NextCursor: "wrong-other-comment"}, nil
	}
	if code, _ := f.request("POST", "/api/local/apps/observation", in, true); code == 200 {
		t.Fatal("bad continuation accepted")
	}
	m.readComments = func(context.Context, string, stationbrowser.DouyinCommentsRequest, func() error) (stationbrowser.DouyinCommentPage, error) {
		return stationbrowser.DouyinCommentPage{}, stationbrowser.ErrDouyinCommentsCursorExpired
	}
	if code, out := f.request("POST", "/api/local/apps/observation", in, true); code == 200 || !strings.Contains(localAppObservationJSONString(out), "变化") {
		t.Fatal("cursor error hidden", code, out)
	}
	m.readComments = func(context.Context, string, stationbrowser.DouyinCommentsRequest, func() error) (stationbrowser.DouyinCommentPage, error) {
		view = enableSyntheticDouyinComments(t, f, view, false)
		return stationbrowser.DouyinCommentPage{Comments: []stationbrowser.DouyinComment{{ID: "1", AuthorID: "someone", Text: "late private result"}}, Complete: true}, nil
	}
	if code, out := f.request("POST", "/api/local/apps/observation", in, true); code != 403 || strings.Contains(localAppObservationJSONString(out), "late private") {
		t.Fatal("late revoked result escaped", code, out)
	}
}
