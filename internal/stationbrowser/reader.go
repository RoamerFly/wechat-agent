// Package stationbrowser implements the narrowly scoped browser reader used by
// connected applications. It deliberately does not expose a general browser
// or JavaScript execution API to the rest of the program.
package stationbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	// ErrPageUnavailable means the requested public page could not be safely
	// read. It covers invalid targets, challenge pages, blocked redirects and
	// non-success document responses without leaking browser implementation.
	ErrPageUnavailable    = errors.New("公开页面暂时无法安全读取")
	ErrBrowserUnavailable = errors.New("独立浏览器暂不可用")
)

const (
	maxPageFieldRunes = 8_000
	pageLoadTimeout   = 25 * time.Second
)

// Page is the small factual result a connected application may use. An empty
// Description means the page did not expose a supported description.
type Page struct {
	Title       string
	Description string
}

// Reader is a dedicated, read-only page reader. The browser transport is
// private, so callers cannot issue arbitrary CDP commands.
type Reader struct {
	profile string
	opener  func(context.Context, string) (readerSession, error)
	allow   func(string) bool
	mu      sync.Mutex
}

// readerSession is private. Production uses Edge's private CDP pipe; tests may
// supply a deterministic transport without opening a TCP debugging port.
type readerSession interface {
	Call(context.Context, string, string, any, any) error
	Events() <-chan cdpEvent
	Close() error
}

// New constructs a reader for an exact HTTPS URL supplied by a trusted
// application adapter. It remains read-only even for otherwise trusted URLs.
func New(profile string) *Reader {
	return &Reader{
		profile: profile,
		opener: func(ctx context.Context, profile string) (readerSession, error) {
			return openCDP(ctx, profile)
		},
		allow: isExactHTTPSTarget,
	}
}

// Read navigates exactly once, allows only that top-level document request and
// extracts two fixed public page fields. authorize is called immediately before
// navigation and again before return. Its errors are deliberately unchanged.
func (r *Reader) Read(ctx context.Context, target string, authorize func() error) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if r == nil || r.opener == nil || r.allow == nil || !r.allow(target) || authorize == nil {
		return Page{}, ErrPageUnavailable
	}
	// One Reader owns one private Edge profile. Serializing reads prevents a
	// second task from attaching to, or sharing state with, the first task.
	r.mu.Lock()
	defer r.mu.Unlock()
	readCtx, cancel := context.WithTimeout(ctx, pageLoadTimeout)
	defer cancel()
	session, err := r.opener(readCtx, r.profile)
	if err != nil {
		return Page{}, fmt.Errorf("%w: %v", ErrBrowserUnavailable, err)
	}
	defer session.Close()

	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := session.Call(readCtx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &created); err != nil || created.TargetID == "" {
		return Page{}, readerCallError(err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := session.Call(readCtx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil || attached.SessionID == "" {
		return Page{}, readerCallError(err)
	}

	for _, method := range []string{"Page.enable", "Network.enable"} {
		if err := session.Call(readCtx, attached.SessionID, method, map[string]any{}, nil); err != nil {
			return Page{}, readerCallError(err)
		}
	}
	if err := session.Call(readCtx, attached.SessionID, "Network.setBypassServiceWorker", map[string]any{"bypass": true}, nil); err != nil {
		return Page{}, readerCallError(err)
	}
	if err := session.Call(readCtx, attached.SessionID, "Emulation.setScriptExecutionDisabled", map[string]any{"value": true}, nil); err != nil {
		return Page{}, readerCallError(err)
	}
	if err := session.Call(readCtx, attached.SessionID, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}, nil); err != nil {
		return Page{}, readerCallError(err)
	}
	if err := session.Call(readCtx, attached.SessionID, "Fetch.enable", map[string]any{
		"patterns":           []map[string]any{{"urlPattern": "*", "requestStage": "Request"}},
		"handleAuthRequests": true,
	}, nil); err != nil {
		return Page{}, readerCallError(err)
	}
	if err := session.Call(readCtx, "", "Browser.setDownloadBehavior", map[string]any{"behavior": "deny", "eventsEnabled": false}, nil); err != nil {
		return Page{}, readerCallError(err)
	}
	state := &readerNavigation{target: target, sessionID: attached.SessionID}
	state.start(readCtx, session)
	defer state.stop()

	if err := authorize(); err != nil { // intentionally immediately before navigation
		return Page{}, err
	}
	var navigate struct {
		ErrorText  string `json:"errorText"`
		IsDownload bool   `json:"isDownload"`
	}
	if err := session.Call(readCtx, attached.SessionID, "Page.navigate", map[string]any{"url": target}, &navigate); err != nil {
		return Page{}, readerCallError(err)
	}
	if navigate.ErrorText != "" || navigate.IsDownload {
		return Page{}, ErrPageUnavailable
	}
	if err := state.wait(readCtx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Page{}, err
		}
		return Page{}, ErrPageUnavailable
	}
	page, err := readFixedPageFields(readCtx, session, attached.SessionID)
	if err != nil {
		return Page{}, err
	}
	if err := state.validate(page); err != nil {
		return Page{}, err
	}
	if err := authorize(); err != nil {
		return Page{}, err
	}
	if !withinPageLimit(page.Title) || !withinPageLimit(page.Description) {
		return Page{}, ErrPageUnavailable
	}
	return Page{Title: boundedText(page.Title), Description: boundedText(page.Description)}, nil
}

type readerNavigation struct {
	target    string
	sessionID string
	done      chan struct{}
	err       error
	mu        sync.Mutex
	loaded    bool
	status    int
	finalURL  string
	mainFrame bool
	continued bool
	stopCh    chan struct{}
	once      sync.Once
}

func (n *readerNavigation) start(ctx context.Context, session readerSession) {
	n.done = make(chan struct{})
	n.stopCh = make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				n.finish(ctx.Err())
				return
			case <-n.stopCh:
				return
			case event, ok := <-session.Events():
				if !ok {
					n.finish(ErrPageUnavailable)
					return
				}
				if event.SessionID != n.sessionID {
					continue
				}
				switch event.Method {
				case "Fetch.requestPaused":
					if err := n.handlePaused(ctx, session, event.Params); err != nil {
						n.finish(err)
						return
					}
				case "Fetch.authRequired":
					if err := n.cancelAuth(ctx, session, event.Params); err != nil {
						n.finish(err)
						return
					}
				case "Network.responseReceived":
					n.recordResponse(event.Params)
				case "Page.frameNavigated":
					n.recordFrame(event.Params)
				case "Page.loadEventFired", "Page.lifecycleEvent":
					n.markLoaded(event.Method, event.Params)
				}
			}
		}
	}()
}

func (n *readerNavigation) stop() { close(n.stopCh) }

func (n *readerNavigation) finish(err error) {
	n.once.Do(func() {
		n.mu.Lock()
		n.err = err
		n.mu.Unlock()
		close(n.done)
	})
}

func (n *readerNavigation) handlePaused(ctx context.Context, session readerSession, raw json.RawMessage) error {
	var paused struct {
		RequestID    string `json:"requestId"`
		ResourceType string `json:"resourceType"`
		Request      struct {
			URL    string `json:"url"`
			Method string `json:"method"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &paused); err != nil || paused.RequestID == "" {
		return ErrPageUnavailable
	}
	n.mu.Lock()
	allowed := !n.continued && paused.Request.Method == "GET" && paused.Request.URL == n.target && paused.ResourceType == "Document"
	if allowed {
		n.continued = true
	}
	n.mu.Unlock()
	method := "Fetch.failRequest"
	params := map[string]any{"requestId": paused.RequestID, "errorReason": "BlockedByClient"}
	if allowed {
		method = "Fetch.continueRequest"
		params = map[string]any{"requestId": paused.RequestID}
	}
	if err := session.Call(ctx, n.sessionID, method, params, nil); err != nil {
		return err
	}
	return nil
}

func (n *readerNavigation) cancelAuth(ctx context.Context, session readerSession, raw json.RawMessage) error {
	var challenge struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(raw, &challenge); err != nil || challenge.RequestID == "" {
		return ErrPageUnavailable
	}
	return session.Call(ctx, n.sessionID, "Fetch.continueWithAuth", map[string]any{
		"requestId":             challenge.RequestID,
		"authChallengeResponse": map[string]any{"response": "CancelAuth"},
	}, nil)
}

func (n *readerNavigation) recordResponse(raw json.RawMessage) {
	var response struct {
		Type     string `json:"type"`
		Response struct {
			URL    string `json:"url"`
			Status int    `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Type != "Document" || response.Response.URL != n.target {
		return
	}
	n.mu.Lock()
	n.status = response.Response.Status
	n.mu.Unlock()
}

func (n *readerNavigation) recordFrame(raw json.RawMessage) {
	var frame struct {
		Frame struct {
			URL      string `json:"url"`
			ParentID string `json:"parentId"`
		} `json:"frame"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Frame.URL == "" || frame.Frame.ParentID != "" {
		return
	}
	n.mu.Lock()
	n.finalURL = frame.Frame.URL
	n.mainFrame = true
	n.mu.Unlock()
}

func (n *readerNavigation) markLoaded(method string, raw json.RawMessage) {
	if method == "Page.lifecycleEvent" {
		var lifecycle struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &lifecycle) != nil || lifecycle.Name != "load" {
			return
		}
	}
	n.mu.Lock()
	if !n.mainFrame || n.finalURL != n.target || n.status < 200 || n.status >= 300 {
		n.mu.Unlock()
		return
	}
	n.loaded = true
	n.mu.Unlock()
	n.finish(nil)
}

func (n *readerNavigation) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-n.done:
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.err
	}
}

type evaluatedPage struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

// The expression is source-controlled, read-only and has no interpolated
// caller input. It consults only common public title/description elements.
const fixedPageFieldsExpression = `(() => {
  const clean = (value) => String(value || '').replace(/\s+/g, ' ').trim();
  const meta = (selector) => document.querySelector(selector)?.getAttribute('content') || '';
  const title = clean(document.querySelector('h1.video-title, h1')?.textContent || meta('meta[property="og:title"]') || meta('meta[name="title"]') || document.title);
  const description = clean(document.querySelector('#v_desc, .desc-info-text, .video-desc-container, [data-e2e="video-desc"]')?.textContent || meta('meta[property="og:description"]') || meta('meta[name="description"]'));
  return { title, description, url: String(location.href || '') };
})()`

func readFixedPageFields(ctx context.Context, session readerSession, sessionID string) (evaluatedPage, error) {
	var response struct {
		Result struct {
			Type  string        `json:"type"`
			Value evaluatedPage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{
		"expression":    fixedPageFieldsExpression,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &response); err != nil {
		return evaluatedPage{}, readerCallError(err)
	}
	if response.Result.Type != "object" || len(response.ExceptionDetails) != 0 {
		return evaluatedPage{}, ErrPageUnavailable
	}
	return response.Result.Value, nil
}

func (n *readerNavigation) validate(page evaluatedPage) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil || !n.loaded || n.finalURL != n.target || page.URL != n.target || n.status < 200 || n.status >= 300 {
		return ErrPageUnavailable
	}
	if suspiciousPage(page.Title) || suspiciousPage(page.Description) || strings.TrimSpace(page.Title) == "" {
		return ErrPageUnavailable
	}
	return nil
}

func isExactHTTPSTarget(target string) bool {
	u, err := url.Parse(target)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Port() == ""
}

func suspiciousPage(text string) bool {
	value := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(value, "just a moment") || strings.Contains(value, "captcha") || strings.Contains(value, "安全验证") || strings.Contains(value, "访问验证") || strings.Contains(value, "风险验证")
}

func boundedText(text string) string {
	text = strings.TrimSpace(strings.Join(strings.Fields(text), " "))
	if utf8.RuneCountInString(text) <= maxPageFieldRunes {
		return text
	}
	return string([]rune(text)[:maxPageFieldRunes])
}

func withinPageLimit(text string) bool {
	return utf8.RuneCountInString(text) <= maxPageFieldRunes
}

func readerCallError(err error) error {
	if err == nil {
		return ErrPageUnavailable
	}
	return fmt.Errorf("%w: %v", ErrBrowserUnavailable, err)
}
