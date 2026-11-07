//go:build windows

package stationbrowser

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wechat-bot/internal/hostfile"
)

//go:embed douyin_interaction_ui.js
var douyinInteractionUI string

// RunDouyinInteraction performs only a like state change or an exact reply to
// one rendered comment on a fixed official video page. It never exposes CDP,
// browser storage, selectors, cookies, or arbitrary navigation to callers.
func RunDouyinInteraction(ctx context.Context, profile string, req DouyinInteractionRequest, authorize func() error) (DouyinInteractionResult, error) {
	target, err := DouyinVideoURL(req.VideoID)
	if err != nil {
		return DouyinInteractionResult{State: DouyinInteractionFailed, Code: "invalid_video"}, ErrDouyinInteractionInput
	}
	return runDouyinInteraction(ctx, profile, target, req, authorize, false)
}

// runDouyinInteraction permits only package tests to use a local synthetic
// fixture. Production callers can reach only DouyinVideoURL.
func runDouyinInteraction(ctx context.Context, profile, target string, req DouyinInteractionRequest, authorize func() error, fixture bool) (DouyinInteractionResult, error) {
	result := DouyinInteractionResult{State: DouyinInteractionUnknown, RemoteID: req.RemoteID, Submitted: req.Submitted}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if authorize == nil || !validDouyinInteraction(req) || (!fixture && !isDouyinVideoURL(target)) {
		result.State, result.Code = DouyinInteractionFailed, "invalid_request"
		return result, ErrDouyinInteractionInput
	}
	if err := authorize(); err != nil {
		return result, err
	}
	lock, err := DouyinLoginProfileLockPath(profile)
	if err != nil {
		return result, ErrDouyinInteractionUnavailable
	}
	release, err := hostfile.TryLock(lock)
	if err != nil {
		return result, ErrDouyinLoginActive
	}
	defer release()

	workCtx, cancel := context.WithTimeout(ctx, pageLoadTimeout)
	defer cancel()
	var session *cdpSession
	if fixture {
		session, err = openCDP(workCtx, profile)
	} else {
		session, err = openRestrictedCDP(workCtx, profile, isDouyinHomeHost)
	}
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrDouyinInteractionUnavailable, err)
	}
	defer closeDouyinHomeSession(session)
	return runDouyinInteractionSession(workCtx, session, target, req, authorize, fixture)
}

type douyinInteractionPageState struct {
	Ready, Invalid, Login, Challenge, Prepared    bool
	URL, Code, RemoteID, ParentText, ParentSHA256 string
	Desired, Actual, Self, Rejected               bool
}

func douyinInteractionPage(ctx context.Context, s readerSession, id string, world int64, target, action string, req DouyinInteractionRequest) (douyinInteractionPageState, error) {
	input, _ := json.Marshal(map[string]any{"target": target, "action": action, "kind": req.Kind, "videoID": req.VideoID,
		"commentID": req.CommentID, "text": req.Text, "desired": req.Desired, "remoteID": req.RemoteID, "expectedParentText": req.expectedParentText})
	var out struct {
		Result struct {
			Type  string
			Value douyinInteractionPageState
		}
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	err := s.Call(ctx, id, "Runtime.evaluate", map[string]any{"expression": "(" + douyinInteractionUI + ")(" + string(input) + ")", "contextId": world, "returnByValue": true}, &out)
	if err != nil || out.Result.Type != "object" || len(out.Exception) != 0 || out.Result.Value.Invalid {
		return douyinInteractionPageState{}, ErrDouyinInteractionUnavailable
	}
	if out.Result.Value.Login || out.Result.Value.Challenge {
		return out.Result.Value, ErrDouyinWebLoginRequired
	}
	return out.Result.Value, nil
}

func runDouyinInteractionSession(ctx context.Context, session readerSession, target string, req DouyinInteractionRequest, authorize func() error, fixture bool) (DouyinInteractionResult, error) {
	result := DouyinInteractionResult{State: DouyinInteractionUnknown, RemoteID: req.RemoteID, Submitted: req.Submitted}
	var created struct {
		TargetID string `json:"targetId"`
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := session.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &created); err != nil || created.TargetID == "" ||
		session.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached) != nil || attached.SessionID == "" {
		return result, ErrDouyinInteractionUnavailable
	}
	id := attached.SessionID
	for _, call := range []struct {
		method string
		params any
	}{{"Page.enable", map[string]any{}}, {"Browser.setDownloadBehavior", map[string]any{"behavior": "deny", "eventsEnabled": false}}, {"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.open=()=>null;"}}} {
		if err := session.Call(ctx, id, call.method, call.params, nil); err != nil {
			return result, ErrDouyinInteractionUnavailable
		}
	}
	if err := authorize(); err != nil {
		return result, err
	}
	var nav struct {
		FrameID, ErrorText string
		IsDownload         bool
	}
	if err := session.Call(ctx, id, "Page.navigate", map[string]any{"url": target}, &nav); err != nil || nav.ErrorText != "" || nav.IsDownload {
		return result, ErrDouyinInteractionUnavailable
	}
	var world int64
	for world == 0 {
		var err error
		world, err = createPlaybackWorld(ctx, session, id, nav.FrameID)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(60 * time.Millisecond):
		}
	}
	var state douyinInteractionPageState
	for until := time.Now().Add(6 * time.Second); time.Now().Before(until); time.Sleep(80 * time.Millisecond) {
		var err error
		state, err = douyinInteractionPage(ctx, session, id, world, target, "query", req)
		if err != nil {
			return result, err
		}
		if state.Ready && state.Code == "" && sameDouyinHomeLocation(state.URL, target) {
			break
		}
	}
	if !state.Ready || !sameDouyinHomeLocation(state.URL, target) {
		return result, ErrDouyinInteractionUnavailable
	}
	if state.Code != "" {
		result.State, result.Code = DouyinInteractionFailed, state.Code
		return result, ErrDouyinInteractionUnavailable
	}
	if req.Kind == DouyinInteractionReplyComment && !req.Submitted && sha256Text(state.ParentText) != req.ExpectedCommentSHA256 {
		result.State, result.Code = DouyinInteractionFailed, "comment_changed"
		return result, nil
	}
	if req.Kind == DouyinInteractionReplyComment && !req.Submitted {
		req.expectedParentText = state.ParentText
	}
	if state.Actual && (req.Kind == DouyinInteractionLike || req.RemoteID != "") {
		result.State, result.Code, result.RemoteID = DouyinInteractionApplied, "already_applied", state.RemoteID
		if result.RemoteID == "" {
			result.RemoteID = req.RemoteID
		}
		return result, nil
	}
	// A persisted request is recovery only. In particular, a reply without its
	// observed receipt ID must not mistake an older same-text self reply for a
	// newly submitted action.
	if req.Submitted {
		result.Code = "receipt_not_found"
		return result, ErrDouyinInteractionUnknown
	}
	if state.Rejected {
		result.State, result.Code = DouyinInteractionFailed, "platform_rejected"
		return result, ErrDouyinInteractionRejected
	}
	if req.BeforeSubmit == nil {
		result.State, result.Code = DouyinInteractionFailed, "missing_submission_guard"
		return result, ErrDouyinInteractionInput
	}
	// Opening an editor and validating its target/text is preparation, not
	// an external reply. Reserve the irreversible submit only once it is ready.
	if req.Kind == DouyinInteractionReplyComment {
		for until := time.Now().Add(6 * time.Second); ; {
			if err := authorize(); err != nil {
				return result, err
			}
			prepared, err := douyinInteractionPage(ctx, session, id, world, target, "prepare", req)
			if err != nil {
				return result, err
			}
			if prepared.Prepared {
				break
			}
			result.Code = prepared.Code
			if prepared.Code == "comment_changed" || strings.HasPrefix(prepared.Code, "reply_text_limit_") {
				result.State = DouyinInteractionFailed
				return result, ErrDouyinInteractionInput
			}
			if time.Now().After(until) {
				return result, ErrDouyinInteractionUnavailable
			}
			if err := creatorWait(ctx); err != nil {
				return result, err
			}
		}
	}
	if err := req.BeforeSubmit(ctx); err != nil {
		result.State, result.Code = DouyinInteractionCancelled, "cancelled_before_submit"
		return result, err
	}
	// Persisting a submit intent is not enough: authorization can be withdrawn
	// while it is being recorded. Re-check at the last possible safe point.
	if err := authorize(); err != nil {
		result.State, result.Code = DouyinInteractionCancelled, "authorization_changed"
		return result, err
	}
	state, err := douyinInteractionPage(ctx, session, id, world, target, "submit", req)
	if err != nil {
		return result, err
	}
	if state.Code != "" {
		result.State, result.Code = DouyinInteractionFailed, state.Code
		return result, ErrDouyinInteractionUnavailable
	}
	if state.Rejected {
		result.State, result.Code = DouyinInteractionFailed, "platform_rejected"
		return result, ErrDouyinInteractionRejected
	}
	if !state.Ready {
		return result, ErrDouyinInteractionUnavailable
	}
	result.Submitted = true
	// A single click is all that is permitted. Querying the exact receipt is
	// safe; an unknown result is deliberately left for the durable ledger.
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until); time.Sleep(80 * time.Millisecond) {
		state, err = douyinInteractionPage(ctx, session, id, world, target, "query", req)
		if err != nil {
			return result, err
		}
		if state.Actual {
			result.State, result.Code, result.RemoteID = DouyinInteractionApplied, "applied", state.RemoteID
			if req.Kind == DouyinInteractionLike {
				result.RemoteID = "like:" + req.VideoID
			}
			return result, nil
		}
		if state.Rejected {
			result.State, result.Code = DouyinInteractionFailed, "platform_rejected"
			return result, ErrDouyinInteractionRejected
		}
	}
	result.Code = "receipt_unknown"
	return result, ErrDouyinInteractionUnknown
}

func sha256Text(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }
