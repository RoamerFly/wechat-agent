//go:build windows

package stationbrowser

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLocalDouyinInteractionLikeAndSubmittedRecovery(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s := douyinInteractionFixture(t)
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	profile := t.TempDir() + "\\profile"
	before := 0
	got, err := runDouyinInteraction(ctx, profile, s.URL+"/video/123", DouyinInteractionRequest{VideoID: "123", Kind: DouyinInteractionLike, Desired: false, BeforeSubmit: func(context.Context) error { before++; return nil }}, func() error { return nil }, true)
	if err != nil || got.State != DouyinInteractionApplied || before != 0 {
		t.Fatalf("already desired=%+v %v before=%d", got, err, before)
	}
	got, err = runDouyinInteraction(ctx, profile, s.URL+"/video/123", DouyinInteractionRequest{VideoID: "123", Kind: DouyinInteractionLike, Desired: true, BeforeSubmit: func(context.Context) error { before++; return nil }}, func() error { return nil }, true)
	if err != nil || got.State != DouyinInteractionApplied || got.RemoteID != "like:123" || before != 1 {
		t.Fatalf("like=%+v %v before=%d", got, err, before)
	}
	got, err = runDouyinInteraction(ctx, profile, s.URL+"/video/123", DouyinInteractionRequest{VideoID: "123", Kind: DouyinInteractionLike, Desired: true, Submitted: true}, func() error { return nil }, true)
	if err != nil || got.State != DouyinInteractionApplied {
		t.Fatalf("submitted recovery=%+v %v", got, err)
	}
}

func TestLocalDouyinInteractionReplyBoundToParentAndCancellation(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s := douyinInteractionFixture(t)
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	profile := t.TempDir() + "\\profile"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("parent A")))
	req := DouyinInteractionRequest{VideoID: "123", Kind: DouyinInteractionReplyComment, CommentID: "456", Text: "同文回复", ExpectedCommentSHA256: hash, BeforeSubmit: func(context.Context) error { return context.Canceled }}
	got, err := runDouyinInteraction(ctx, profile, s.URL+"/video/123", req, func() error { return nil }, true)
	if !errors.Is(err, context.Canceled) || got.State != DouyinInteractionCancelled {
		t.Fatalf("cancel=%+v %v", got, err)
	}
	req.BeforeSubmit = func(context.Context) error { return nil }
	wrong := req
	wrong.ExpectedCommentSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("outdated parent")))
	wrong.BeforeSubmit = func(context.Context) error { t.Fatal("changed comment reached submit"); return nil }
	if changed, err := runDouyinInteraction(ctx, profile, s.URL+"/video/123", wrong, func() error { return nil }, true); err != nil || changed.State != DouyinInteractionFailed || changed.Code != "comment_changed" {
		t.Fatalf("changed source: %#v %v", changed, err)
	}
	got, err = runDouyinInteraction(ctx, profile, s.URL+"/video/123", req, func() error { return nil }, true)
	if err != nil || got.State != DouyinInteractionApplied || got.RemoteID != "789" {
		t.Fatalf("reply=%+v %v", got, err)
	}
	// A new process with no recorded receipt must not infer delivery from an
	// older same-text reply, even though the self reply is visible in the DOM.
	got, err = runDouyinInteraction(ctx, profile, s.URL+"/video/123", DouyinInteractionRequest{VideoID: "123", Kind: DouyinInteractionReplyComment, CommentID: "456", Text: "同文回复", Submitted: true}, func() error { return nil }, true)
	if !errors.Is(err, ErrDouyinInteractionUnknown) || got.State != DouyinInteractionUnknown {
		t.Fatalf("receipt-less recovery=%+v %v", got, err)
	}
}

func douyinInteractionFixture(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><title>interaction</title><button data-e2e="like-button" data-video-id="123" aria-pressed="false" onclick="let n=this.getAttribute('aria-pressed')!=='true';this.setAttribute('aria-pressed',n);localStorage.like=n?'1':'0'">赞</button><script>document.currentScript.previousElementSibling.setAttribute('aria-pressed',localStorage.like==='1')</script><article data-e2e="comment-item" data-comment-id="456"><p data-e2e="comment-content">parent A</p><button data-e2e="comment-reply-button" onclick="document.querySelector('textarea').style.display='block';document.querySelector('[data-e2e=comment-reply-submit]').style.display='block'">回复</button></article><textarea data-e2e="comment-reply-editor" data-reply-to="456" style="display:none"></textarea><button data-e2e="comment-reply-submit" data-reply-to="456" style="display:none" onclick="let x=document.createElement('div');x.dataset.e2e='comment-reply-item';x.dataset.replyId='789';x.dataset.parentCommentId='456';x.dataset.authorSelf='true';x.textContent=document.querySelector('textarea').value;document.body.append(x)">发</button><div data-e2e="comment-reply-item" data-reply-id="777" data-parent-comment-id="456" data-author-self="false">同文回复</div>`)
	}))
}
