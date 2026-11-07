//go:build windows

package stationbrowser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLocalDouyinCommentsPagingAndSourceFields(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s := douyinCommentsFixture(t)
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	profile := t.TempDir() + "\\profile"
	first, err := readDouyinComments(ctx, profile, s.URL+"/video/123", DouyinCommentsRequest{VideoID: "123", Limit: 2}, func() error { return nil }, true)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Comments) != 2 || first.Comments[0].ID != "101" || first.Comments[0].AuthorID != "sec_alpha" || first.Comments[0].Text != "第一条" || first.Comments[0].CreatedAt != "2026-01-02T03:04:05.000Z" || first.Complete || first.NextCursor != "102" {
		t.Fatalf("first page=%+v", first)
	}
	second, err := readDouyinComments(ctx, profile, s.URL+"/video/123", DouyinCommentsRequest{VideoID: "123", Cursor: first.NextCursor, Limit: 2}, func() error { return nil }, true)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Comments) != 2 || second.Comments[0].ID != "103" || second.Comments[1].ID != "104" || !second.Complete || second.NextCursor != "" {
		t.Fatalf("second page=%+v", second)
	}
	if first.Comments[1].CreatedAt != "" {
		t.Fatal("relative date was guessed", first.Comments[1].CreatedAt)
	}
	// This anchor is not in the initial two cards. A resumed read must scroll
	// to find it instead of declaring the original cursor expired immediately.
	last, err := readDouyinComments(ctx, profile, s.URL+"/video/123", DouyinCommentsRequest{VideoID: "123", Cursor: "103", Limit: 1}, func() error { return nil }, true)
	if err != nil || len(last.Comments) != 1 || last.Comments[0].ID != "104" || !last.Complete {
		t.Fatalf("later anchor: %#v %v", last, err)
	}
}

func TestLocalDouyinCommentsEmptyLoginCursorRevocationAndNavigation(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s := douyinCommentsFixture(t)
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	profile := t.TempDir() + "\\profile"
	empty, err := readDouyinComments(ctx, profile, s.URL+"/empty", DouyinCommentsRequest{Limit: 0}, func() error { return nil }, true)
	if err != nil || !empty.Complete || len(empty.Comments) != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	_, err = readDouyinComments(ctx, profile, s.URL+"/video/123", DouyinCommentsRequest{Cursor: "999", Limit: 1}, func() error { return nil }, true)
	if !errors.Is(err, ErrDouyinCommentsCursorExpired) {
		t.Fatalf("expired cursor=%v", err)
	}
	_, err = readDouyinComments(ctx, profile, s.URL+"/login", DouyinCommentsRequest{Limit: 1}, func() error { return nil }, true)
	if !errors.Is(err, ErrDouyinWebLoginRequired) {
		t.Fatalf("login=%v", err)
	}
	revoked := errors.New("authorization revoked")
	calls := 0
	_, err = readDouyinComments(ctx, profile, s.URL+"/video/123", DouyinCommentsRequest{Limit: 1}, func() error {
		calls++
		if calls > 1 {
			return revoked
		}
		return nil
	}, true)
	if !errors.Is(err, revoked) {
		t.Fatalf("revocation=%v", err)
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("other")) }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer redirect.Close()
	_, err = readDouyinComments(ctx, profile, redirect.URL+"/video/123", DouyinCommentsRequest{Limit: 1}, func() error { return nil }, true)
	if !errors.Is(err, ErrDouyinCommentsUnavailable) {
		t.Fatalf("redirect=%v", err)
	}
}

func TestDouyinCommentsLargeLimitIsResumableNotClamped(t *testing.T) {
	if got, err := normalizedDouyinCommentsLimit(10000); err != nil || got != 10000 {
		t.Fatalf("large limit=%d %v", got, err)
	}
	comments := make([]DouyinComment, 0, 201)
	for i := 1; i <= 201; i++ {
		comments = append(comments, DouyinComment{ID: fmtCommentID(i), AuthorID: "sec_a", Text: "x"})
	}
	page, err := douyinCommentsAfterCursor(renderedDouyinComments{Comments: comments, End: true}, "", 10000)
	if err != nil || page.Complete || len(page.Comments) != maxDouyinCommentsBatch || page.NextCursor != "200" {
		t.Fatalf("large page=%+v err=%v", page, err)
	}
}

func fmtCommentID(i int) string { return fmt.Sprintf("%d", i) }

func douyinCommentsFixture(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/empty":
			_, _ = w.Write([]byte(`<title>empty</title><div data-e2e="comment-empty">暂无评论</div>`))
		case "/login":
			_, _ = w.Write([]byte(`<title>login</title><form action="/login"><input type="password"></form>`))
		default:
			_, _ = w.Write([]byte(`<!doctype html><title>comments</title><main id="comments"></main><script>
const data=[['101','sec_alpha','甲','第一条','2026-01-02T03:04:05Z'],['102','sec_beta','乙','第二条','几分钟前'],['103','sec_gamma','丙','第三条','2026-01-02T03:06:05Z'],['104','sec_delta','丁','第四条','2026-01-02T03:07:05Z']];let n=0;
function add(k){while(n<data.length&&k-->0){let x=data[n++],e=document.createElement('article');e.dataset.e2e='comment-item';e.dataset.commentId=x[0];e.dataset.authorId=x[1];e.innerHTML='<span data-e2e="comment-user-name" data-sec-uid="'+x[1]+'">'+x[2]+'</span><p data-e2e="comment-content">'+x[3]+'</p><time datetime="'+x[4]+'">'+x[4]+'</time>';comments.append(e)}if(n===data.length){let e=document.createElement('i');e.dataset.e2e='comments-end';comments.append(e)}}add(2);window.scrollTo=()=>add(2);
</script>`))
		}
	}))
}
