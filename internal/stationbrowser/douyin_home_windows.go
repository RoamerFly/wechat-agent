//go:build windows

package stationbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"wechat-bot/internal/hostfile"
)

const douyinHomeURL = "https://www.douyin.com/"
const maxDouyinHomeTextRunes = 500000 // leaves room for JSON escaping within the 4 MiB CDP frame.

var (
	ErrDouyinHomeUnavailable  = errors.Join(ErrPageUnavailable, errors.New("抖音主页正文暂时无法安全读取"))
	ErrDouyinHomeTooLarge     = errors.New("抖音主页正文超过安全读取大小")
	ErrDouyinWebLoginRequired = errors.New("抖音网页登录状态不可用")
	ErrDouyinWebTextSize      = ErrDouyinHomeTooLarge
)

// DouyinWebPage is deliberately limited to rendered public text. It contains
// no HTML, cookies, form values, request headers, or DevTools details.
type DouyinWebLink struct{ VideoID, Text string }
type DouyinWebPage struct {
	Title, Text string
	Links       []DouyinWebLink
}

var douyinVideoID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

func DouyinVideoURL(videoID string) (string, error) {
	if !douyinVideoID.MatchString(videoID) {
		return "", ErrDouyinHomeUnavailable
	}
	return "https://www.douyin.com/video/" + videoID, nil
}
func ReadDouyinVideo(ctx context.Context, profile, videoID string, authorize func() error) (DouyinWebPage, error) {
	u, e := DouyinVideoURL(videoID)
	if e != nil {
		return DouyinWebPage{}, e
	}
	return readDouyinHome(ctx, profile, u, authorize, false)
}

// ReadDouyinHome reads only the fixed Douyin home page using the caller's
// dedicated application profile. The authorize callback is checked immediately
// before navigation and again before a result is returned.
func ReadDouyinHome(ctx context.Context, profile string, authorize func() error) (DouyinWebPage, error) {
	return readDouyinHome(ctx, profile, douyinHomeURL, authorize, false)
}

// readDouyinHome allows only package tests to use a local fixture. Production
// always takes the fixed, proxied route above.
func readDouyinHome(ctx context.Context, profile, target string, authorize func() error, localFixture bool) (DouyinWebPage, error) {
	if err := ctx.Err(); err != nil {
		return DouyinWebPage{}, err
	}
	if authorize == nil {
		return DouyinWebPage{}, ErrDouyinHomeUnavailable
	}
	if !localFixture && target != douyinHomeURL && !isDouyinVideoURL(target) {
		return DouyinWebPage{}, ErrDouyinHomeUnavailable
	}
	lockPath, err := DouyinLoginProfileLockPath(profile)
	if err != nil {
		return DouyinWebPage{}, ErrDouyinHomeUnavailable
	}
	release, err := hostfile.TryLock(lockPath)
	if err != nil {
		return DouyinWebPage{}, ErrDouyinLoginActive
	}
	defer release()

	readCtx, cancel := context.WithTimeout(ctx, pageLoadTimeout)
	defer cancel()
	var session *cdpSession
	if localFixture {
		session, err = openCDP(readCtx, profile)
	} else {
		session, err = openRestrictedCDP(readCtx, profile, isDouyinHomeHost)
	}
	if err != nil {
		return DouyinWebPage{}, fmt.Errorf("%w: %v", ErrDouyinHomeUnavailable, err)
	}
	defer closeDouyinHomeSession(session)
	page, readErr := readDouyinHomeSession(readCtx, session, target, authorize, localFixture)
	if ctxErr := readCtx.Err(); ctxErr != nil {
		return DouyinWebPage{}, ctxErr
	}
	return page, readErr
}

func closeDouyinHomeSession(session *cdpSession) {
	if session == nil {
		return
	}
	session.requestGracefulClose()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = session.Call(ctx, "", "Browser.close", map[string]any{}, nil)
	select {
	case <-session.teardownDone:
	case <-ctx.Done():
		_ = session.Close()
		<-session.teardownDone
	}
	// The profile lock is released by the caller only after all Job/process
	// cleanup is complete, never merely when the CDP pipe first closes.
}

func readDouyinHomeSession(ctx context.Context, session *cdpSession, target string, authorize func() error, localFixture bool) (DouyinWebPage, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := session.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &created); err != nil || created.TargetID == "" {
		return DouyinWebPage{}, ErrDouyinHomeUnavailable
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := session.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil || attached.SessionID == "" {
		return DouyinWebPage{}, ErrDouyinHomeUnavailable
	}
	for _, call := range []struct {
		method string
		params any
	}{
		{"Page.enable", map[string]any{}},
		{"Runtime.enable", map[string]any{}},
		{"Browser.setDownloadBehavior", map[string]any{"behavior": "deny", "eventsEnabled": false}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.open=()=>null;"}},
	} {
		if err := session.Call(ctx, attached.SessionID, call.method, call.params, nil); err != nil {
			return DouyinWebPage{}, ErrDouyinHomeUnavailable
		}
	}
	if err := authorize(); err != nil {
		return DouyinWebPage{}, err
	}
	var nav struct {
		ErrorText  string `json:"errorText"`
		IsDownload bool   `json:"isDownload"`
	}
	if err := session.Call(ctx, attached.SessionID, "Page.navigate", map[string]any{"url": target}, &nav); err != nil || nav.ErrorText != "" || nav.IsDownload {
		return DouyinWebPage{}, ErrDouyinHomeUnavailable
	}
	// JS-driven pages can settle after Page.navigate. Poll a fixed expression;
	// page contents are data only and never influence host control flow.
	stableText, stableCount := "", 0
	for until := time.Now().Add(6 * time.Second); time.Now().Before(until); time.Sleep(80 * time.Millisecond) {
		page, ready, err := renderedDouyinText(ctx, session, attached.SessionID, target, localFixture)
		if err != nil {
			return DouyinWebPage{}, err
		}
		if !ready || strings.TrimSpace(page.Text) == "" {
			continue
		}
		if page.Text == stableText {
			stableCount++
		} else {
			stableText, stableCount = page.Text, 1
		}
		if stableCount < 2 {
			continue
		} // avoid returning the JavaScript shell.
		if !sameDouyinHomeLocation(page.URL, target) {
			return DouyinWebPage{}, ErrDouyinHomeUnavailable
		}
		if looksLikeLoginOrChallenge(page) {
			return DouyinWebPage{}, ErrDouyinWebLoginRequired
		}
		if err := authorize(); err != nil {
			return DouyinWebPage{}, err
		}
		return DouyinWebPage{Title: page.Title, Text: page.Text, Links: page.Links}, nil
	}
	return DouyinWebPage{}, ErrDouyinHomeUnavailable
}

type renderedDouyinPage struct {
	Title     string          `json:"title"`
	Text      string          `json:"text"`
	URL       string          `json:"url"`
	Length    int             `json:"length"`
	LoginForm bool            `json:"loginForm"`
	Challenge bool            `json:"challenge"`
	Ready     bool            `json:"ready"`
	TooLarge  bool            `json:"tooLarge"`
	Links     []DouyinWebLink `json:"links"`
}

// Text, source, and links are captured atomically, so a navigation between
// two CDP calls cannot bind a new document's content to an old document's URL.
// The returned value must also leave room for the CDP reply envelope.  The
// transport rejects a frame at 4 MiB, so check the actual UTF-8 JSON value
// (including every discovered link) before returning it to Go.
const maxDouyinRenderedValueBytes = maxCDPMessage - 64*1024

func renderedDouyinTextExpression(target string, localFixture bool) string {
	allowedOrigin := "https://www.douyin.com"
	allowedHost := "www.douyin.com"
	allowedProtocol := "https:"
	if localFixture {
		u, err := url.Parse(target)
		if err != nil {
			return "(()=>({tooLarge:true}))()"
		}
		allowedOrigin = u.Scheme + "://" + u.Host
		allowedHost = u.Host
		allowedProtocol = u.Scheme + ":"
	}
	originJSON, _ := json.Marshal(allowedOrigin)
	hostJSON, _ := json.Marshal(allowedHost)
	protocolJSON, _ := json.Marshal(allowedProtocol)
	return fmt.Sprintf(`(() => {
  const visible=s=>[...document.querySelectorAll(s)].some(e=>e.getClientRects().length&&getComputedStyle(e).visibility!=='hidden'&&getComputedStyle(e).display!=='none');
  const text=String(document.body?.innerText||''), title=String(document.title||''), url=String(location.href||''), length=[...text].length;
  if(length>%d||[...title].length>8192||[...url].length>8192)return {tooLarge:true};
  const seen=new Set(),links=[];for(const a of document.querySelectorAll('a[href]')){if(!a.getClientRects().length||getComputedStyle(a).visibility==='hidden'||getComputedStyle(a).display==='none')continue;let u;try{u=new URL(a.getAttribute('href'),location.href)}catch{continue}let m=/^\/video\/([1-9][0-9]{0,19})$/.exec(u.pathname);if(u.origin!==%s||u.host!==%s||u.protocol!==%s||u.username||u.password||u.search||u.hash||!m||seen.has(m[1]))continue;seen.add(m[1]);links.push({videoID:m[1],text:String(a.innerText||'').trim()})}
  const page={title,text,url,length,links,ready:document.readyState==='complete',loginForm:visible('input[type=password],form[action*="login"],form[action*="passport"]'),challenge:visible('iframe[src*="captcha"],[class*="captcha"],[class*="verify"]')};
  if(new TextEncoder().encode(JSON.stringify(page)).length>%d)return {tooLarge:true};
  return page;
})()`, maxDouyinHomeTextRunes, string(originJSON), string(hostJSON), string(protocolJSON), maxDouyinRenderedValueBytes)
}

func renderedDouyinText(ctx context.Context, session *cdpSession, id, target string, localFixture bool) (renderedDouyinPage, bool, error) {
	var out struct {
		Result struct {
			Type  string             `json:"type"`
			Value renderedDouyinPage `json:"value"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails"`
	}
	if err := session.Call(ctx, id, "Runtime.evaluate", map[string]any{"expression": renderedDouyinTextExpression(target, localFixture), "returnByValue": true, "awaitPromise": true}, &out); err != nil {
		return renderedDouyinPage{}, false, ErrDouyinHomeUnavailable
	}
	if out.Result.Type != "object" {
		return renderedDouyinPage{}, false, ErrDouyinHomeUnavailable
	}
	if out.Result.Value.TooLarge || out.Result.Value.Length > maxDouyinHomeTextRunes {
		return renderedDouyinPage{}, false, ErrDouyinHomeTooLarge
	}
	return out.Result.Value, out.Result.Value.Ready && strings.TrimSpace(out.Result.Value.Text) != "", nil
}

func sameDouyinHomeLocation(actual, expected string) bool {
	a, err := url.Parse(actual)
	if err != nil || a.User != nil {
		return false
	}
	e, err := url.Parse(expected)
	if err != nil || e.User != nil {
		return false
	}
	path := func(u *url.URL) string {
		if u.EscapedPath() == "" {
			return "/"
		}
		return u.EscapedPath()
	}
	return a.Scheme == e.Scheme && strings.EqualFold(a.Host, e.Host) && path(a) == path(e) && a.RawQuery == e.RawQuery && a.Fragment == e.Fragment
}
func looksLikeLoginOrChallenge(page renderedDouyinPage) bool {
	// A post discussing “登录流程” is ordinary text. Treat only explicit
	// challenge markers or a visible login form as an unavailable session.
	return page.LoginForm || page.Challenge || suspiciousPage(page.Title)
}
func isDouyinHomeHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	h, _, err := net.SplitHostPort(host)
	if err == nil {
		host = h
	}
	return host == "www.douyin.com" || strings.HasSuffix(host, ".douyin.com") || strings.HasSuffix(host, ".douyinvod.com") || strings.HasSuffix(host, ".bytednsdoc.com") || strings.HasSuffix(host, ".byteimg.com") || strings.HasSuffix(host, ".ibytedtos.com")
}
func isDouyinVideoURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host != "www.douyin.com" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return strings.HasPrefix(u.Path, "/video/") && douyinVideoID.MatchString(strings.TrimPrefix(u.Path, "/video/"))
}
