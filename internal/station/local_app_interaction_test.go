package station

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/stationbrowser"
)

func syntheticInteractionFixture(t *testing.T, reply bool) (*localHomeFixture, *localDouyinWebLogin, string, map[string]any) {
	t.Helper()
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinComments(t, f, enableSyntheticDouyinWebRead(t, f), true)
	code, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_interactions", "revision": view["revision"], "allow_likes": true, "allow_replies": reply}, true)
	if code != 200 || view["likes_enabled"] != true || view["replies_enabled"] != reply {
		t.Fatalf("permission: %d %#v", code, view)
	}
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	return f, m, id, view
}

func TestLocalAppInteractionNormalChatCreatesSubmitsAndKeepsReceipt(t *testing.T) {
	f, m, id, _ := syntheticInteractionFixture(t, false)
	clicks := 0
	m.interact = func(ctx context.Context, profile string, in stationbrowser.DouyinInteractionRequest, authorize func() error) (stationbrowser.DouyinInteractionResult, error) {
		if in.Submitted {
			t.Fatal("completed action unexpectedly queried platform")
		}
		if in.VideoID != "7000000000000000011" || in.Kind != "like" || !in.Desired {
			t.Fatal("target changed", in)
		}
		if err := in.BeforeSubmit(ctx); err != nil {
			return stationbrowser.DouyinInteractionResult{}, err
		}
		if err := authorize(); err != nil {
			return stationbrowser.DouyinInteractionResult{}, err
		}
		clicks++
		return stationbrowser.DouyinInteractionResult{State: "applied", RemoteID: "like:" + in.VideoID, Submitted: true, Code: "applied"}, nil
	}
	calls := 0
	job := ""
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return &syntheticObservationModel{call: func(_ context.Context, _ string, turns []map[string]string, _ []mcptools.Tool) (string, error) {
			calls++
			switch calls {
			case 1:
				return localToolCall("discover_tools", map[string]any{"query": "app_action"}), nil
			case 2:
				return localToolCall("app_action", map[string]any{"action": "create", "app_id": "douyin", "connection_id": "web-page-text", "content_id": id, "kind": "like", "desired": true}), nil
			case 3:
				ids := localPublicationJobIDs(localModelHistory(turns))
				if len(ids) != 1 {
					t.Fatal("missing interaction task", localModelHistory(turns))
				}
				job = ids[0]
				return localToolCall("app_action", map[string]any{"action": "submit", "job_id": job}), nil
			default:
				if !strings.Contains(localModelHistory(turns), `applied`) {
					t.Fatal("verified receipt not in model context")
				}
				return `{"action":"reply","messages":["已核对点赞状态。"]}`, nil
			}
		}}, nil
	}
	if code, out := localChatSend(f, "interaction-like-normal", "给这条视频点个赞"); code != 200 || out["status"] != "completed" || clicks != 1 {
		t.Fatalf("chat: %d %#v clicks=%d", code, out, clicks)
	}
	if code, _ := localChatSend(f, "interaction-like-normal", "给这条视频点个赞"); code != 200 || clicks != 1 || calls != 4 {
		t.Fatal("replay resubmitted action")
	}
	if code, _ := f.request("POST", "/api/local/interactions", map[string]any{"action": "status", "job_id": job}, false); code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if code, out := f.request("POST", "/api/local/interactions", map[string]any{"action": "status", "job_id": job}, true); code != 200 || out["state"] != "applied" {
		t.Fatal("receipt not durable", code, out)
	}
	code, page := f.request("GET", "/api/local/interactions?app_id=douyin", nil, false)
	if code != 200 || len(page["tasks"].(map[string]any)["items"].([]any)) != 1 {
		t.Fatal("ledger not visible", code, page)
	}
	if strings.Contains(localAppObservationJSONString(page), "origin") || strings.Contains(localAppObservationJSONString(page), "给这条视频") {
		t.Fatal("source escaped metadata view")
	}
	_, pub := f.request("GET", "/api/local/publications?app_id=douyin", nil, false)
	if len(pub["tasks"].(map[string]any)["items"].([]any)) != 0 {
		t.Fatal("interaction appeared as publication")
	}
}

func TestLocalAppReplyBindsReadCommentVersionAndRejectsUnseenSubject(t *testing.T) {
	f, m, id, _ := syntheticInteractionFixture(t, true)
	const original = "原评论的完整合成内容"
	m.readComments = func(context.Context, string, stationbrowser.DouyinCommentsRequest, func() error) (stationbrowser.DouyinCommentPage, error) {
		return stationbrowser.DouyinCommentPage{Comments: []stationbrowser.DouyinComment{{ID: "105", AuthorID: "author105", Author: "甲", Text: original}}, Complete: true}, nil
	}
	clicks := 0
	m.interact = func(ctx context.Context, _ string, in stationbrowser.DouyinInteractionRequest, authorize func() error) (stationbrowser.DouyinInteractionResult, error) {
		if in.ExpectedCommentSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(original))) || in.CommentID != "105" || in.Text != "谢谢你的分享" {
			t.Fatal("reply source lost", in)
		}
		if err := in.BeforeSubmit(ctx); err != nil {
			return stationbrowser.DouyinInteractionResult{}, err
		}
		if err := authorize(); err != nil {
			return stationbrowser.DouyinInteractionResult{}, err
		}
		clicks++
		return stationbrowser.DouyinInteractionResult{State: "applied", RemoteID: "501", Submitted: true}, nil
	}
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return &syntheticObservationModel{call: func(_ context.Context, _ string, turns []map[string]string, _ []mcptools.Tool) (string, error) {
			calls++
			switch calls {
			case 1:
				return localToolCall("discover_tools", map[string]any{"query": "app_action"}), nil
			case 2:
				return localToolCall("app_action", map[string]any{"action": "create", "app_id": "douyin", "connection_id": "web-page-text", "content_id": id, "kind": "reply_comment", "comment_id": "105", "text": "谢谢你的分享"}), nil
			case 3:
				if !strings.Contains(localModelHistory(turns), "先读取") {
					t.Fatal("unread subject feedback", localModelHistory(turns))
				}
				return localToolCall("discover_tools", map[string]any{"query": "app_observe"}), nil
			case 4:
				return localToolCall("app_observe", map[string]any{"action": "read", "app_id": "douyin", "connection_id": "web-page-text", "content_id": id, "field": "comments"}), nil
			case 5:
				return localToolCall("app_action", map[string]any{"action": "create", "app_id": "douyin", "connection_id": "web-page-text", "content_id": id, "kind": "reply_comment", "comment_id": "105", "text": "谢谢你的分享"}), nil
			case 6:
				ids := localPublicationJobIDs(localModelHistory(turns))
				if len(ids) != 1 {
					t.Fatal("reply task missing", localModelHistory(turns))
				}
				return localToolCall("app_action", map[string]any{"action": "submit", "job_id": ids[0]}), nil
			default:
				return `{"action":"reply","messages":["回复已核对。"]}`, nil
			}
		}}, nil
	}
	if code, out := localChatSend(f, "reply-version-normal", "回复刚才的评论"); code != 200 || clicks != 1 {
		t.Fatalf("reply: %d %#v clicks=%d", code, out, clicks)
	}
}

func TestLocalAppInteractionResumeUsesOriginalSubmissionBoundary(t *testing.T) {
	for _, submitted := range []bool{false, true} {
		t.Run(fmt.Sprint(submitted), func(t *testing.T) {
			f, m, id, _ := syntheticInteractionFixture(t, false)
			clicks, calls := 0, 0
			m.interact = func(ctx context.Context, _ string, in stationbrowser.DouyinInteractionRequest, authorize func() error) (stationbrowser.DouyinInteractionResult, error) {
				calls++
				if in.Submitted {
					if in.BeforeSubmit != nil {
						t.Fatal("query retained submit callback")
					}
					return stationbrowser.DouyinInteractionResult{State: "unknown", Code: "receipt_not_found", Submitted: true}, stationbrowser.ErrDouyinInteractionUnknown
				}
				if calls == 1 && !submitted {
					return stationbrowser.DouyinInteractionResult{State: "failed", Code: "like_control_unavailable"}, stationbrowser.ErrDouyinInteractionUnavailable
				}
				if err := in.BeforeSubmit(ctx); err != nil {
					return stationbrowser.DouyinInteractionResult{}, err
				}
				clicks++
				if submitted {
					return stationbrowser.DouyinInteractionResult{State: "unknown", Code: "receipt_unknown", Submitted: true}, stationbrowser.ErrDouyinInteractionUnknown
				}
				return stationbrowser.DouyinInteractionResult{State: "applied", RemoteID: "like:" + in.VideoID, Submitted: true}, nil
			}
			modelCalls := 0
			job := ""
			f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
				return &syntheticObservationModel{call: func(_ context.Context, _ string, turns []map[string]string, _ []mcptools.Tool) (string, error) {
					modelCalls++
					switch modelCalls {
					case 1:
						return localToolCall("discover_tools", map[string]any{"query": "app_action"}), nil
					case 2:
						return localToolCall("app_action", map[string]any{"action": "create", "app_id": "douyin", "connection_id": "web-page-text", "content_id": id, "kind": "like", "desired": true}), nil
					case 3:
						ids := localPublicationJobIDs(localModelHistory(turns))
						if len(ids) != 1 {
							t.Fatal("missing task")
						}
						job = ids[0]
						return localToolCall("app_action", map[string]any{"action": "submit", "job_id": job}), nil
					default:
						return `{"action":"reply","messages":["操作进度已保存。"]}`, nil
					}
				}}, nil
			}
			if code, out := localChatSend(f, "interaction-boundary", "给这条视频点个赞"); code != 200 {
				t.Fatal(code, out)
			}
			if code, out := f.request("POST", "/api/local/interactions", map[string]any{"action": "resume", "job_id": job}, true); code != 200 || out["job_id"] != job {
				t.Fatal("original task not resumed", code, out)
			} else if submitted && out["state"] != "unknown" {
				t.Fatal("unknown submit inferred success", out)
			} else if !submitted && out["state"] != "applied" {
				t.Fatal("pre-submit recovery failed", out)
			}
			if clicks != 1 {
				t.Fatal("duplicate or missing click", clicks)
			}
			if submitted {
				code, out := f.request("POST", "/api/local/interactions", map[string]any{"action": "cancel", "job_id": job}, true)
				if code != 200 || out["cancel_requested"] != true || clicks != 1 {
					t.Fatal("cancel lost or resubmitted", code, out)
				}
				if code, _ := f.request("POST", "/api/local/interactions", map[string]any{"action": "resume", "job_id": job}, true); code == 200 {
					t.Fatal("cancelled intent could resume")
				}
			}
		})
	}
}
