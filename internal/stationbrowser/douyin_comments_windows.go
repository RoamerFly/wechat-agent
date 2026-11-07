//go:build windows

package stationbrowser

import (
	"context"
	"fmt"
	"strings"
	"time"

	"wechat-bot/internal/hostfile"
)

// ReadDouyinComments reads rendered comment cards from one fixed, official
// Douyin video URL. It never types, posts, reacts, calls a hidden API, or
// follows links supplied by the page.
func ReadDouyinComments(ctx context.Context, profile string, req DouyinCommentsRequest, authorize func() error) (DouyinCommentPage, error) {
	target, err := DouyinVideoURL(req.VideoID)
	if err != nil {
		return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
	}
	return readDouyinComments(ctx, profile, target, req, authorize, false)
}

// readDouyinComments permits package tests to use a local synthetic page.
// Production callers can only reach the fixed official video route above.
func readDouyinComments(ctx context.Context, profile, target string, req DouyinCommentsRequest, authorize func() error, localFixture bool) (DouyinCommentPage, error) {
	if err := ctx.Err(); err != nil {
		return DouyinCommentPage{}, err
	}
	limit, err := normalizedDouyinCommentsLimit(req.Limit)
	if err != nil {
		return DouyinCommentPage{}, err
	}
	if !douyinCommentID.MatchString(req.Cursor) && req.Cursor != "" {
		return DouyinCommentPage{}, ErrDouyinCommentsCursorExpired
	}
	if authorize == nil {
		return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
	}
	if !localFixture && !isDouyinVideoURL(target) {
		return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
	}
	lockPath, err := DouyinLoginProfileLockPath(profile)
	if err != nil {
		return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
	}
	release, err := hostfile.TryLock(lockPath)
	if err != nil {
		return DouyinCommentPage{}, ErrDouyinLoginActive
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
		return DouyinCommentPage{}, fmt.Errorf("%w: %v", ErrDouyinCommentsUnavailable, err)
	}
	defer closeDouyinHomeSession(session)
	page, readErr := readDouyinCommentsSession(readCtx, session, target, req.Cursor, limit, authorize, localFixture)
	if ctxErr := readCtx.Err(); ctxErr != nil {
		return DouyinCommentPage{}, ctxErr
	}
	return page, readErr
}

func readDouyinCommentsSession(ctx context.Context, session *cdpSession, target, cursor string, limit int, authorize func() error, localFixture bool) (DouyinCommentPage, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := session.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &created); err != nil || created.TargetID == "" {
		return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := session.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil || attached.SessionID == "" {
		return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
	}
	for _, call := range []struct {
		method string
		params any
	}{
		{"Page.enable", map[string]any{}},
		{"Browser.setDownloadBehavior", map[string]any{"behavior": "deny", "eventsEnabled": false}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.open=()=>null;"}},
	} {
		if err := session.Call(ctx, attached.SessionID, call.method, call.params, nil); err != nil {
			return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
		}
	}
	if err := authorize(); err != nil {
		return DouyinCommentPage{}, err
	}
	var nav struct {
		ErrorText  string `json:"errorText"`
		IsDownload bool   `json:"isDownload"`
	}
	if err := session.Call(ctx, attached.SessionID, "Page.navigate", map[string]any{"url": target}, &nav); err != nil || nav.ErrorText != "" || nav.IsDownload {
		return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
	}

	// A rendered item alone is insufficient to claim that the whole comment
	// section has been read. We retain the explicit end marker as the only
	// completeness proof and otherwise return a resumable partial page.
	var latest renderedDouyinComments
	stable, stableCount := "", 0
	for until := time.Now().Add(6 * time.Second); time.Now().Before(until); time.Sleep(80 * time.Millisecond) {
		page, err := renderedDouyinCommentsPage(ctx, session, attached.SessionID, target, localFixture)
		if err != nil {
			return DouyinCommentPage{}, err
		}
		if !page.Ready {
			continue
		}
		if page.LoginForm || page.Challenge {
			return DouyinCommentPage{}, ErrDouyinWebLoginRequired
		}
		if !sameDouyinHomeLocation(page.URL, target) {
			return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
		}
		if page.Invalid || !page.Loaded {
			continue
		}
		fingerprint := page.fingerprint()
		if fingerprint == stable {
			stableCount++
		} else {
			stable, stableCount = fingerprint, 1
		}
		latest = page
		if stableCount >= 2 {
			break
		}
	}
	if !latest.Loaded {
		return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
	}
	// Long comment lists may virtualize cards while scrolling. Keep only the
	// verified public fields, keyed by their exact platform IDs, so a later
	// page can still locate a cursor that has scrolled out of the DOM.
	observed := append([]DouyinComment(nil), latest.Comments...)
	lastProgress := time.Now()
	for {
		combined := latest
		combined.Comments = observed
		page, err := douyinCommentsAfterCursor(combined, cursor, limit)
		if err != nil && !errorsIsCursorExpired(err) {
			return DouyinCommentPage{}, err
		}
		cursorMissing := errorsIsCursorExpired(err)
		if cursorMissing && latest.End {
			return DouyinCommentPage{}, ErrDouyinCommentsCursorExpired
		}
		if page.Complete || len(page.Comments) == limit {
			if err := authorize(); err != nil {
				return DouyinCommentPage{}, err
			}
			return page, nil
		}
		deadline, bounded := ctx.Deadline()
		nearDeadline := bounded && time.Until(deadline) < 500*time.Millisecond
		if time.Since(lastProgress) >= 1500*time.Millisecond || nearDeadline {
			// We did not receive a platform end marker. This is a partial read,
			// never a claim that the currently sampled comments are all comments.
			if cursorMissing {
				// A resource pause is not proof that a once valid anchor was deleted.
				return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
			}
			if len(page.Comments) == 0 {
				return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
			}
			page.Complete = false
			page.NextCursor = page.Comments[len(page.Comments)-1].ID
			if err := authorize(); err != nil {
				return DouyinCommentPage{}, err
			}
			return page, nil
		}
		if err := authorize(); err != nil {
			return DouyinCommentPage{}, err
		}
		if err := scrollDouyinComments(ctx, session, attached.SessionID); err != nil {
			return DouyinCommentPage{}, err
		}
		time.Sleep(150 * time.Millisecond)
		next, err := renderedDouyinCommentsPage(ctx, session, attached.SessionID, target, localFixture)
		if err != nil {
			return DouyinCommentPage{}, err
		}
		if next.LoginForm || next.Challenge {
			return DouyinCommentPage{}, ErrDouyinWebLoginRequired
		}
		if next.Invalid || !next.Loaded || !sameDouyinHomeLocation(next.URL, target) {
			return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
		}
		if next.fingerprint() != latest.fingerprint() {
			lastProgress = time.Now()
		}
		observed = appendDistinctDouyinComments(observed, next.Comments)
		latest = next
	}
}

func douyinCommentsAfterCursor(page renderedDouyinComments, cursor string, limit int) (DouyinCommentPage, error) {
	if page.Empty {
		return DouyinCommentPage{Comments: []DouyinComment{}, Complete: true}, nil
	}
	start := 0
	if cursor != "" {
		found := -1
		for i, c := range page.Comments {
			if c.ID == cursor {
				found = i
				break
			}
		}
		if found < 0 {
			return DouyinCommentPage{}, ErrDouyinCommentsCursorExpired
		}
		start = found + 1
	}
	batchLimit := limit
	if batchLimit > maxDouyinCommentsBatch {
		batchLimit = maxDouyinCommentsBatch
	}
	out := DouyinCommentPage{Comments: make([]DouyinComment, 0, minDouyinCommentCapacity(batchLimit, len(page.Comments)-start))}
	for _, c := range page.Comments[start:] {
		if len(out.Comments) == batchLimit {
			break
		}
		out.Comments = append(out.Comments, c)
	}
	if len(out.Comments) > 0 {
		out.NextCursor = out.Comments[len(out.Comments)-1].ID
	}
	if len(out.Comments) == batchLimit && (len(page.Comments[start:]) > batchLimit || !page.End) {
		out.Complete = false
		return out, nil
	}
	if page.End {
		out.Complete = true
		out.NextCursor = ""
		return out, nil
	}
	return out, nil
}

func minDouyinCommentCapacity(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func appendDistinctDouyinComments(existing, incoming []DouyinComment) []DouyinComment {
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	for _, comment := range existing {
		seen[comment.ID] = struct{}{}
	}
	for _, comment := range incoming {
		if _, exists := seen[comment.ID]; exists {
			continue
		}
		seen[comment.ID] = struct{}{}
		existing = append(existing, comment)
	}
	return existing
}

func errorsIsCursorExpired(err error) bool { return err == ErrDouyinCommentsCursorExpired }

func scrollDouyinComments(ctx context.Context, session *cdpSession, id string) error {
	var out struct {
		Result struct {
			Type  string `json:"type"`
			Value bool   `json:"value"`
		} `json:"result"`
	}
	err := session.Call(ctx, id, "Runtime.evaluate", map[string]any{
		"expression":    "(()=>{const c=document.querySelector('[data-e2e=comment-list],[data-e2e=comment-container],[data-e2e=comment-panel]');if(c){c.scrollTop=Math.max(c.scrollHeight,c.scrollTop+c.clientHeight);return true}window.scrollTo(0,Math.max(document.body.scrollHeight,document.documentElement.scrollHeight));return true})()",
		"returnByValue": true,
		"awaitPromise":  true,
	}, &out)
	if err != nil || out.Result.Type != "boolean" || !out.Result.Value {
		return ErrDouyinCommentsUnavailable
	}
	return nil
}

type renderedDouyinComments struct {
	URL       string          `json:"url"`
	Ready     bool            `json:"ready"`
	Loaded    bool            `json:"loaded"`
	Empty     bool            `json:"empty"`
	End       bool            `json:"end"`
	LoginForm bool            `json:"loginForm"`
	Challenge bool            `json:"challenge"`
	Invalid   bool            `json:"invalid"`
	TooLarge  bool            `json:"tooLarge"`
	Comments  []DouyinComment `json:"comments"`
}

func (p renderedDouyinComments) fingerprint() string {
	var b strings.Builder
	b.WriteString(p.URL)
	if p.Empty {
		b.WriteString("|empty")
	}
	if p.End {
		b.WriteString("|end")
	}
	for _, c := range p.Comments {
		b.WriteByte('|')
		b.WriteString(c.ID)
	}
	return b.String()
}

func renderedDouyinCommentsExpression(_ string, _ bool) string {
	return `(() => {
 const visible=e=>!!e&&e.getClientRects().length>0&&getComputedStyle(e).display!=='none'&&getComputedStyle(e).visibility!=='hidden';
 const txt=e=>String(e?.textContent||'').trim();
 const relative=/^(?:刚刚|\d+\s*(?:秒|分钟|小时|天|周|月|年)前)$/;
 const rfc3339=/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/;
 const comments=[], seen=new Set();let invalid=false;
 for(const e of document.querySelectorAll('[data-e2e="comment-item"],[data-comment-id],[data-cid]')){
   if(!visible(e)) continue;
   const id=String(e.getAttribute('data-comment-id')||e.dataset.commentId||e.getAttribute('data-cid')||e.dataset.cid||'').trim();
   if(!/^[1-9][0-9]{0,19}$/.test(id)){invalid=true;continue}if(seen.has(id))continue;
   const authorEl=e.querySelector('[data-e2e="comment-user-name"],[data-e2e="comment-username"],[data-author-id],[data-sec-uid]');
   const authorID=String(authorEl?.getAttribute('data-author-id')||authorEl?.dataset.authorId||authorEl?.getAttribute('data-sec-uid')||authorEl?.dataset.secUid||e.getAttribute('data-author-id')||e.dataset.authorId||e.getAttribute('data-sec-uid')||e.dataset.secUid||'').trim();
   const body=e.querySelector('[data-e2e="comment-content"],[data-e2e="comment-text"],[data-comment-text]');
   if(!/^[A-Za-z0-9._-]{1,256}$/.test(authorID)||!body){invalid=true;continue}
   const text=txt(body);if(!text){invalid=true;continue}
   const rawTime=String(e.getAttribute('data-comment-created-at')||e.dataset.commentCreatedAt||e.querySelector('time[datetime]')?.getAttribute('datetime')||'').trim();
   const createdAt=(rawTime&&!relative.test(rawTime)&&rfc3339.test(rawTime)&&!Number.isNaN(Date.parse(rawTime)))?new Date(rawTime).toISOString():'';
   comments.push({id,authorID,author:txt(authorEl),text,createdAt});seen.add(id);
 }
 const hasItems=document.querySelector('[data-e2e="comment-item"],[data-comment-id],[data-cid]')!==null;
 const page={url:String(location.href||''),ready:document.readyState==='complete',loaded:hasItems||visible(document.querySelector('[data-e2e="comment-empty"]')),empty:visible(document.querySelector('[data-e2e="comment-empty"]')),end:visible(document.querySelector('[data-e2e="comments-end"]')),loginForm:visible(document.querySelector('input[type=password],form[action*="login"],form[action*="passport"]')),challenge:visible(document.querySelector('iframe[src*="captcha"],[class*="captcha"],[class*="verify"]')),invalid,comments};
 if(new TextEncoder().encode(JSON.stringify(page)).length>3600000)return {tooLarge:true};return page;
})()`
}

func renderedDouyinCommentsPage(ctx context.Context, session *cdpSession, id, target string, localFixture bool) (renderedDouyinComments, error) {
	var out struct {
		Result struct {
			Type  string                 `json:"type"`
			Value renderedDouyinComments `json:"value"`
		} `json:"result"`
	}
	if err := session.Call(ctx, id, "Runtime.evaluate", map[string]any{"expression": renderedDouyinCommentsExpression(target, localFixture), "returnByValue": true, "awaitPromise": true}, &out); err != nil || out.Result.Type != "object" {
		return renderedDouyinComments{}, ErrDouyinCommentsUnavailable
	}
	if out.Result.Value.TooLarge {
		return renderedDouyinComments{}, ErrDouyinCommentsTooLarge
	}
	for _, c := range out.Result.Value.Comments {
		if !douyinCommentID.MatchString(c.ID) || !douyinAuthorID.MatchString(c.AuthorID) || strings.TrimSpace(c.Text) == "" {
			return renderedDouyinComments{}, ErrDouyinCommentsUnavailable
		}
		if c.CreatedAt != "" {
			if _, err := time.Parse(time.RFC3339, c.CreatedAt); err != nil {
				return renderedDouyinComments{}, ErrDouyinCommentsUnavailable
			}
		}
	}
	return out.Result.Value, nil
}
