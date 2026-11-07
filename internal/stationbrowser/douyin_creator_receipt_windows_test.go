//go:build windows

package stationbrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func syntheticCreatorWork(t *testing.T) (CreatorPublicationQuery, map[string]any) {
	t.Helper()
	created := time.Now().Add(-time.Hour).UTC()
	q := CreatorPublicationQuery{Title: "合成标题", Body: "合成创作", Visibility: "private", CreatedAt: created.Format(time.RFC3339Nano)}
	w := map[string]any{"aweme_id": "7391234567890123456", "item_id": 7391234567890123000,
		"item_title": q.Title, "caption": q.Body, "desc": q.Title + " " + q.Body, "create_time": created.Add(time.Minute).Unix(),
		"status": map[string]any{"aweme_id": "7391234567890123456", "is_private": true, "private_status": 1, "in_reviewing": false, "is_delete": false, "is_prohibited": false, "self_see": true}}
	return q, w
}

func creatorWorkList(works ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"status_code": 0, "aweme_list": works})
	return b
}

func TestCreatorManageRecoveryRequiresExactWorkAndPrivacy(t *testing.T) {
	q, w := syntheticCreatorWork(t)
	if got := creatorManageReceipt(creatorWorkList(w), &q, ""); !got.Published || got.RemoteID != "7391234567890123456" {
		t.Fatalf("private work not identified: %#v", got)
	}
	for _, field := range []string{"item_title", "caption", "desc", "create_time", "status", "aweme_id"} {
		t.Run(field, func(t *testing.T) {
			_, changed := syntheticCreatorWork(t)
			switch field {
			case "create_time":
				changed[field] = int64(1)
			case "status":
				changed[field] = map[string]any{"aweme_id": w["aweme_id"], "is_private": false, "private_status": 0, "in_reviewing": false, "is_delete": false, "is_prohibited": false}
			default:
				changed[field] = "other"
			}
			if got := creatorManageReceipt(creatorWorkList(changed), &q, ""); got.RemoteID != "" {
				t.Fatalf("mismatched work accepted: %#v", got)
			}
		})
	}
	_, duplicate := syntheticCreatorWork(t)
	duplicate["aweme_id"] = "7391234567890123457"
	duplicate["status"].(map[string]any)["aweme_id"] = duplicate["aweme_id"]
	if got := creatorManageReceipt(creatorWorkList(w, duplicate), &q, ""); got.RemoteID != "" {
		t.Fatal("ambiguous caption accepted")
	}
	if got := creatorManageReceipt(creatorWorkList(w), nil, "7391234567890123457"); got.RemoteID != "" {
		t.Fatal("wrong known work accepted")
	}
	w["status"].(map[string]any)["in_reviewing"] = true
	if got := creatorManageReceipt(creatorWorkList(w), nil, "7391234567890123456"); got.Stage != "accepted" || got.Published {
		t.Fatalf("moderation treated as published: %#v", got)
	}
}

func TestLocalCreatorManageRecoveryDoesNotUploadOrPublish(t *testing.T) {
	if !Available() {
		t.Skip("Edge unavailable")
	}
	q, w := syntheticCreatorWork(t)
	posts := new(atomic.Int32)
	s := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || strings.Contains(r.URL.Path, "create") {
			posts.Add(1)
		}
		switch r.URL.Path {
		case "/creator-micro/content/manage":
			rw.Header().Set("Content-Type", "text/html")
			_, _ = rw.Write([]byte(`<p>作品管理</p><script>fetch('/janus/douyin/creator/pc/work_list')</script>`))
		case "/janus/douyin/creator/pc/work_list":
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write(creatorWorkList(w))
		default:
			http.NotFound(rw, r)
		}
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	got, err := runDouyinCreator(ctx, filepath.Join(t.TempDir(), "profile"), s.URL+"/creator-micro/content/manage", CreatorPublicationRequest{Submitted: true, Query: &q}, func() error { return nil }, true)
	if err != nil || !got.Published || got.RemoteID != "7391234567890123456" || posts.Load() != 0 {
		t.Fatalf("read-only recovery: %#v err=%v writes=%d", got, err, posts.Load())
	}
}
