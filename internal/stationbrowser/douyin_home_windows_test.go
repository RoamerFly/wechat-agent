//go:build windows

package stationbrowser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"wechat-bot/internal/hostfile"
)

func TestDouyinHomeLoginChallengeAndLoginTopic(t *testing.T) {
	if looksLikeLoginOrChallenge(renderedDouyinPage{Title: "普通文章", Text: "介绍登录流程"}) {
		t.Fatal("ordinary login-topic text was rejected")
	}
	if looksLikeLoginOrChallenge(renderedDouyinPage{Title: "普通文章", LoginForm: false, Challenge: false}) {
		t.Fatal("hidden login form state was rejected")
	}
	if !looksLikeLoginOrChallenge(renderedDouyinPage{Title: "登录", LoginForm: true}) {
		t.Fatal("login form was accepted")
	}
	if !looksLikeLoginOrChallenge(renderedDouyinPage{Title: "普通", Challenge: true}) {
		t.Fatal("challenge was accepted")
	}
}

func TestLocalDouyinHomeWaitsForDelayedVisibleBody(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed in Program Files")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<title>fixture</title><div id=x></div><script>setTimeout(()=>x.textContent='delayed visible body',350)</script>`))
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL, func() error { return nil }, true)
	if err != nil || got.Text != "delayed visible body" {
		t.Fatalf("delayed text: %#v %v", got, err)
	}
}

func TestLocalDouyinHomeRejectsVisibleLoginForm(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed in Program Files")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<title>登录</title><form action=/login><input type=password>请登录</form>`))
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL, func() error { return nil }, true)
	if !errors.Is(err, ErrDouyinWebLoginRequired) {
		t.Fatalf("visible login = %v", err)
	}
}

func TestDouyinHomeFixedTargetAndCancellation(t *testing.T) {
	if _, err := readDouyinHome(context.Background(), t.TempDir(), "https://example.invalid/", func() error { return nil }, false); !errors.Is(err, ErrDouyinHomeUnavailable) {
		t.Fatalf("cross origin = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readDouyinHome(ctx, t.TempDir(), "http://fixture.invalid/", func() error { return nil }, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}

func TestDouyinHomeTextSizeBoundary(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<title>fixture</title><p>" + strings.Repeat("中", maxDouyinHomeTextRunes+1) + "</p>"))
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	page, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL, func() error { return nil }, true)
	if !errors.Is(err, ErrDouyinWebTextSize) || page.Text != "" {
		t.Fatalf("oversized result was not rejected: %v text length=%d", err, len(page.Text))
	}
}

func TestLocalDouyinHomeHiddenLoginFormIsNotAChallenge(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>fixture</title><form action=/login style="display:none"><input type=password></form><p>介绍登录流程</p>`))
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	page, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL, func() error { return nil }, true)
	if err != nil || strings.TrimSpace(page.Text) != "介绍登录流程" {
		t.Fatalf("hidden form blocked ordinary text: %#v %v", page, err)
	}
}

func TestLocalDouyinHomeRedirectDoesNotReturnAnotherSource(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<title>other</title>another source"))
	}))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	page, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL, func() error { return nil }, true)
	if !errors.Is(err, ErrDouyinHomeUnavailable) || page.Text != "" {
		t.Fatalf("cross-source redirect returned data: %#v %v", page, err)
	}
}

func TestLocalDouyinHomeReadsOnlyVisibleProfileText(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed in Program Files")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/set" {
			_, _ = w.Write([]byte(`<title>fixture</title><script>document.cookie='home=ok;Max-Age=3600;path=/'</script>set`))
			return
		}
		if r.Header.Get("Cookie") != "" {
			_, _ = w.Write([]byte(`<title>fixture</title>private visible body`))
			return
		}
		_, _ = w.Write([]byte(`<title>fixture</title>public body`))
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	p := t.TempDir() + "\\profile"
	if _, err := readDouyinHome(ctx, p, s.URL+"/set", func() error { return nil }, true); err != nil {
		t.Fatal(err)
	}
	got, err := readDouyinHome(ctx, p, s.URL+"/read", func() error { return nil }, true)
	if err != nil || got.Text != "private visible body" {
		t.Fatalf("same profile: %#v %v", got, err)
	}
	other, err := readDouyinHome(ctx, t.TempDir()+"\\other", s.URL+"/read", func() error { return nil }, true)
	if err != nil || other.Text != "public body" {
		t.Fatalf("other profile: %#v %v", other, err)
	}
}

func TestLocalDouyinHomeExclusiveProfile(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed in Program Files")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<title>x</title>stable body`)) }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := t.TempDir() + "\\profile"
	lock, err := DouyinLoginProfileLockPath(p)
	if err != nil {
		t.Fatal(err)
	}
	release, err := hostfile.TryLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := readDouyinHome(ctx, p, s.URL, func() error { return nil }, true); !errors.Is(err, ErrDouyinLoginActive) {
		t.Fatalf("profile lock = %v", err)
	}
}

func TestLocalDouyinHomeAuthorizationRechecked(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed in Program Files")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<title>fixture</title>body`)) }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	calls := 0
	_, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL, func() error {
		calls++
		if calls > 1 {
			return context.Canceled
		}
		return nil
	}, true)
	if err != context.Canceled {
		t.Fatalf("authorization revocation = %v", err)
	}
}

func TestLocalDouyinHomeCapturesOnlyCanonicalVisibleVideoLinksInDOMOrder(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credentialURL := "http://user:pass@" + r.Host + "/video/1009"
		_, _ = fmt.Fprintf(w, `<title>fixture title</title><main>visible body
<a href="/video/1001"> first link </a><a href="/video/1001">duplicate</a>
<a href="/video/1002#part">fragment</a><a href="/video/1003?q=1">query</a>
<a href="https://evil.example/video/1004">evil</a><a href="/video/0005">zero</a>
<a href="/video/1005/">noncanonical trailing slash</a>
<a href="/video/1006" style="display:none">hidden display</a>
<a href="/video/1007" style="visibility:hidden">hidden visibility</a>
<a href="%s">credential</a><a href="/video/1008">second link</a>
<a href="/video/1010"><span style="display:none">hidden child label</span></a></main>`, credentialURL)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	got, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL, func() error { return nil }, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "fixture title" || !strings.Contains(got.Text, "visible body") {
		t.Fatalf("atomic page capture lost title/text: %#v", got)
	}
	want := []DouyinWebLink{{VideoID: "1001", Text: "first link"}, {VideoID: "1008", Text: "second link"}, {VideoID: "1010", Text: ""}}
	if fmt.Sprintf("%#v", got.Links) != fmt.Sprintf("%#v", want) {
		t.Fatalf("links = %#v, want %#v", got.Links, want)
	}
}

func TestLocalDouyinHomeRejectsLinkResponseTooLarge(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	const links = 120000 // empty anchors do not enlarge body.innerText; links enlarge CDP's JSON value.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>fixture</title><p>body</p>`))
		for i := 0; i < links; i++ {
			_, _ = fmt.Fprintf(w, `<a href="/video/%d"></a>`, 10000000000000000000+uint64(i))
		}
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	page, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL, func() error { return nil }, true)
	if !errors.Is(err, ErrDouyinWebTextSize) || page.Text != "" || len(page.Links) != 0 {
		t.Fatalf("oversized links were not explicitly rejected: page=%#v err=%v", page, err)
	}
}

func TestDouyinVideoURLRequiresStrictCanonicalID(t *testing.T) {
	if got, err := DouyinVideoURL("12345678901234567890"); err != nil || got != "https://www.douyin.com/video/12345678901234567890" {
		t.Fatalf("valid video URL = %q, %v", got, err)
	}
	for _, bad := range []string{"", "0", "01", "123456789012345678901", "-1", "1/evil", "1?q=x", "1#x", "１"} {
		if got, err := DouyinVideoURL(bad); err == nil || got != "" {
			t.Fatalf("bad id %q accepted as %q", bad, got)
		}
	}
	if isDouyinVideoURL("https://www.douyin.com/video/1001?x=1") || isDouyinVideoURL("https://www.douyin.com/video/1001#x") || isDouyinVideoURL("https://www.douyin.com/video/1001/") {
		t.Fatal("non-canonical video target was accepted")
	}
}

func TestLocalDouyinVideoTargetRedirectDoesNotReturnAnotherSource(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<title>other</title>other body`)) }))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	page, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL+"/video/1001", func() error { return nil }, true)
	if !errors.Is(err, ErrDouyinHomeUnavailable) || page.Title != "" || page.Text != "" || len(page.Links) != 0 {
		t.Fatalf("redirected video leaked a different source: %#v %v", page, err)
	}
}

func TestLocalDouyinHomeAuthorizationCheckedBeforeNavigation(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; _, _ = w.Write([]byte(`body`)) }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	denied := errors.New("authorization revoked")
	_, err := readDouyinHome(ctx, t.TempDir()+"\\profile", s.URL, func() error { return denied }, true)
	if !errors.Is(err, denied) || requests != 0 {
		t.Fatalf("authorization did not stop navigation: err=%v requests=%d", err, requests)
	}
}

func TestLocalDouyinHomeCancellationAfterNavigationReturnsNoCapture(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	requested := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requested)
		_, _ = w.Write([]byte(`<title>fixture</title><script>setTimeout(()=>document.body.textContent='late body',5000)</script>`))
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	profile := t.TempDir() + "\\profile"
	result := make(chan struct {
		page DouyinWebPage
		err  error
	}, 1)
	go func() {
		page, err := readDouyinHome(ctx, profile, s.URL, func() error { return nil }, true)
		result <- struct {
			page DouyinWebPage
			err  error
		}{page, err}
	}()
	select {
	case <-requested:
		cancel()
	case <-ctx.Done():
		t.Fatal("fixture was not navigated")
	}
	select {
	case got := <-result:
		if !errors.Is(got.err, context.Canceled) || got.page.Title != "" || got.page.Text != "" || len(got.page.Links) != 0 {
			t.Fatalf("cancellation leaked capture: %#v %v", got.page, got.err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cancellation did not stop the browser read")
	}
}

func TestLocalDouyinHomeIsExclusiveWithActiveLogin(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge unavailable")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<title>fixture</title>body`)) }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	p := t.TempDir() + "\\profile"
	login, err := openDouyinLogin(ctx, p, s.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = login.Close(context.Background()) }()
	if _, err := readDouyinHome(ctx, p, s.URL, func() error { return nil }, true); !errors.Is(err, ErrDouyinLoginActive) {
		t.Fatalf("active login did not exclude reader: %v", err)
	}
}
