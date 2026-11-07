package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
)

// deliveryRecoveryNative returns a distinct receipt for each status read.  It
// models the receipt changing while the host has completed its paced recheck.
// No wall-clock timing is used to make the transition deterministic.
type deliveryRecoveryNative struct {
	*fakeTools
	statuses   []linuxnative.ToolStatus
	reads      int
	retries    int
	reconciles int
	retriedJob string
	onSecond   func()
}

func (n *deliveryRecoveryNative) ToolSendStatus(context.Context, string) (linuxnative.ToolStatus, error) {
	if n.reads == 1 && n.onSecond != nil {
		n.onSecond()
	}
	i := n.reads
	n.reads++
	if i >= len(n.statuses) {
		i = len(n.statuses) - 1
	}
	return n.statuses[i], nil
}
func (n *deliveryRecoveryNative) RetryMediaJob(_ context.Context, job string) (linuxnative.ToolStatus, error) {
	n.retries++
	n.retriedJob = job
	return n.statuses[len(n.statuses)-1], nil
}
func (n *deliveryRecoveryNative) ReconcileMediaJob(context.Context, string) (linuxnative.ToolStatus, error) {
	n.reconciles++
	return n.statuses[len(n.statuses)-1], nil
}

func TestWechatMediaAdapterDeliveryRechecksAfterBackoff(t *testing.T) {
	newTools := func(t *testing.T, next linuxnative.ToolStatus, onSecond func()) (*permittedTools, *deliveryRecoveryNative) {
		t.Helper()
		f, p := trustedAgent(t)
		f.manager.mu.Lock()
		f.manager.config.Companion.Permissions.SendVoice = true
		// The second status read drives the transition deterministically; this
		// does not depend on a random delay duration (the current host keeps its
		// fixed minimum paced wait even when both configured values are zero).
		f.manager.config.Behavior.DelayMinSeconds = 0
		f.manager.config.Behavior.DelayMaxSeconds = 0
		f.manager.mu.Unlock()
		base := linuxnative.ToolStatus{ID: "same-job", ThreadID: p.trace.Thread, SourceID: "voice-source", Kind: "voice", State: "needs_review", Retryable: true}
		n := &deliveryRecoveryNative{fakeTools: &fakeTools{fakeNative: f.native}, statuses: []linuxnative.ToolStatus{base, next}, onSecond: onSecond}
		f.manager.native = n
		if err := f.manager.tools.upload(stagedUpload{UploadSource: linuxnative.UploadSource{ID: base.SourceID, Filename: "original.wav", ByteLength: 4}, Account: p.trace.Account, Kind: "voice"}); err != nil {
			t.Fatal(err)
		}
		return p, n
	}
	args := json.RawMessage(`{"job_id":"same-job","request_key":"recheck-once"}`)

	for _, tc := range []struct {
		name     string
		next     linuxnative.ToolStatus
		onSecond func(*permittedTools)
		reason   string
	}{
		{name: "cancel requested", next: linuxnative.ToolStatus{ID: "same-job", ThreadID: "wxid_owner", SourceID: "voice-source", Kind: "voice", State: "needs_review", Retryable: true, CancelRequested: true}, reason: "用户已取消原发送"},
		{name: "source changed", next: linuxnative.ToolStatus{ID: "same-job", ThreadID: "wxid_owner", SourceID: "other-source", Kind: "voice", State: "needs_review", Retryable: true}, reason: "原发送任务或媒体来源已变化"},
		{name: "permission revoked", next: linuxnative.ToolStatus{ID: "same-job", ThreadID: "wxid_owner", SourceID: "voice-source", Kind: "voice", State: "needs_review", Retryable: true}, onSecond: func(p *permittedTools) {
			p.manager.mu.Lock()
			p.manager.config.Companion.Permissions.SendVoice = false
			p.manager.mu.Unlock()
		}, reason: "原媒体来源未获当前发送权限"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p *permittedTools
			p, n := newTools(t, tc.next, func() {
				if tc.onSecond != nil {
					tc.onSecond(p)
				}
			})
			if got, err := p.callDelivery(t.Context(), "wechat_delivery_retry", args); err != nil || !strings.Contains(got, `"state":"waiting"`) || !strings.Contains(got, tc.reason) {
				t.Fatalf("changed original receipt was not safely held: %s err=%v reads=%d retry=%d reconcile=%d", got, err, n.reads, n.retries, n.reconciles)
			}
			if n.reads != 2 || n.retries != 0 || n.reconciles != 0 {
				t.Fatalf("reads=%d retry=%d reconcile=%d", n.reads, n.retries, n.reconciles)
			}
		})
	}
}

func TestWechatMediaAdapterDeliveryAcceptedVoiceKeepsAcceptedParts(t *testing.T) {
	accepted := linuxnative.ToolStatus{ID: "same-job", ThreadID: "wxid_owner", SourceID: "voice-source", Kind: "voice", State: "accepted", VoiceChunks: []linuxnative.VoiceChunkStatus{{Index: 0, State: "accepted", HasReceipt: true, ServerID: "101"}, {Index: 1, State: "accepted", HasReceipt: true, ServerID: "102"}}}
	f, p := trustedAgent(t)
	f.manager.mu.Lock()
	f.manager.config.Companion.Permissions.SendVoice = true
	f.manager.config.Behavior.DelayMinSeconds = 0
	f.manager.config.Behavior.DelayMaxSeconds = 0
	f.manager.mu.Unlock()
	n := &deliveryRecoveryNative{fakeTools: &fakeTools{fakeNative: f.native}, statuses: []linuxnative.ToolStatus{{ID: "same-job", ThreadID: p.trace.Thread, SourceID: "voice-source", Kind: "voice", State: "needs_review", Retryable: true}, accepted}}
	f.manager.native = n
	if err := f.manager.tools.upload(stagedUpload{UploadSource: linuxnative.UploadSource{ID: "voice-source", Filename: "original.wav", ByteLength: 4}, Account: p.trace.Account, Kind: "voice"}); err != nil {
		t.Fatal(err)
	}
	got, err := p.callDelivery(t.Context(), "wechat_delivery_retry", json.RawMessage(`{"job_id":"same-job","request_key":"preserve-accepted"}`))
	if err != nil || n.reads != 2 || n.retries != 0 || n.reconciles != 0 {
		t.Fatalf("result=%s err=%v reads=%d retry=%d reconcile=%d", got, err, n.reads, n.retries, n.reconciles)
	}
	var result linuxnative.ToolStatus
	if json.Unmarshal([]byte(got), &result) != nil || len(result.VoiceChunks) != 2 || result.VoiceChunks[0].ServerID != "101" || result.VoiceChunks[1].ServerID != "102" {
		t.Fatalf("accepted voice parts were not preserved: %s", got)
	}
}

func TestWechatMediaAdapterDeliveryPartialVoiceResumesOriginalJob(t *testing.T) {
	f, p := trustedAgent(t)
	f.manager.mu.Lock()
	f.manager.config.Companion.Permissions.SendVoice = true
	f.manager.config.Behavior.DelayMinSeconds = 0
	f.manager.config.Behavior.DelayMaxSeconds = 0
	f.manager.mu.Unlock()
	initial := linuxnative.ToolStatus{ID: "same-job", ThreadID: p.trace.Thread, SourceID: "voice-source", Kind: "voice", State: "needs_review", Retryable: true}
	partial := initial
	partial.VoicePartsIncomplete = true
	partial.VoiceChunks = []linuxnative.VoiceChunkStatus{{Index: 0, State: "accepted", HasReceipt: true, ServerID: "101"}, {Index: 1, State: "needs_review", HasReceipt: false}}
	n := &deliveryRecoveryNative{fakeTools: &fakeTools{fakeNative: f.native}, statuses: []linuxnative.ToolStatus{initial, partial}}
	f.manager.native = n
	if err := f.manager.tools.upload(stagedUpload{UploadSource: linuxnative.UploadSource{ID: "voice-source", Filename: "original.wav", ByteLength: 4}, Account: p.trace.Account, Kind: "voice"}); err != nil {
		t.Fatal(err)
	}
	got, err := p.callDelivery(t.Context(), "wechat_delivery_retry", json.RawMessage(`{"job_id":"same-job","request_key":"continue-parts"}`))
	if err != nil || n.reads != 2 || n.retries != 1 || n.retriedJob != "same-job" || n.reconciles != 0 || n.submits.Load() != 0 || !strings.Contains(got, `"server_id":"101"`) {
		t.Fatalf("result=%s err=%v reads=%d retry=%d job=%q reconcile=%d new_submits=%d", got, err, n.reads, n.retries, n.retriedJob, n.reconciles, n.submits.Load())
	}
}

type mediaAdapterHost struct {
	authorized bool
	claimed    bool
	session    *mediaAdapterSession
}

func (h *mediaAdapterHost) CurrentThread() string { return "source" }
func (*mediaAdapterHost) Invalid(s string) error  { return errors.New(s) }
func (h *mediaAdapterHost) Authorize(context.Context, wechatapp.MediaRequest) error {
	h.authorized = true
	return nil
}
func (h *mediaAdapterHost) ClaimMedia(_ context.Context, _ wechatapp.MediaRequest, fn func(wechatapp.MediaSession) (any, error)) (string, error) {
	h.claimed = true
	v, err := fn(h.session)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(v)
	return string(b), nil
}

type mediaAdapterSession struct {
	order    []string
	existing bool
}

func (s *mediaAdapterSession) Resolve(context.Context) error {
	s.order = append(s.order, "resolve")
	return nil
}
func (s *mediaAdapterSession) Existing(context.Context) (any, bool, error) {
	s.order = append(s.order, "existing")
	if s.existing {
		return map[string]any{"state": "accepted", "parts": 2}, true, nil
	}
	return nil, false, nil
}
func (s *mediaAdapterSession) Prepare(context.Context) error {
	s.order = append(s.order, "prepare")
	return nil
}
func (s *mediaAdapterSession) Queue(context.Context) (any, error) {
	s.order = append(s.order, "queue")
	return map[string]any{"state": "queued"}, nil
}

func TestWechatMediaAdapterMediaSessionOrderAndVoiceBounds(t *testing.T) {
	t.Run("existing batch returns before prepare and queue", func(t *testing.T) {
		h := &mediaAdapterHost{session: &mediaAdapterSession{existing: true}}
		got, err := wechatapp.SendMedia(t.Context(), json.RawMessage(`{"asset_id":"retained-audio","kind":"voice","request_key":"same-request"}`), h)
		if err != nil || !h.authorized || !h.claimed || strings.Join(h.session.order, ",") != "resolve,existing" || !strings.Contains(got, `"state":"accepted"`) {
			t.Fatalf("result=%s err=%v authorized=%t claimed=%t order=%v", got, err, h.authorized, h.claimed, h.session.order)
		}
	})
	t.Run("new batch follows resolve existing prepare queue", func(t *testing.T) {
		h := &mediaAdapterHost{session: &mediaAdapterSession{}}
		if _, err := wechatapp.SendMedia(t.Context(), json.RawMessage(`{"asset_id":"retained-audio","kind":"voice","request_key":"new-request"}`), h); err != nil || strings.Join(h.session.order, ",") != "resolve,existing,prepare,queue" {
			t.Fatalf("err=%v order=%v", err, h.session.order)
		}
	})
	t.Run("sixty seconds accepted and explicit values remain exact", func(t *testing.T) {
		in, err := wechatapp.ParseMediaRequest(json.RawMessage(`{"asset_id":"retained-audio","kind":"voice","request_key":"exact-boundary","conversion":{"start_seconds":12.5,"duration_seconds":60.25,"segment_seconds":60,"timeout_seconds":120.5}}`), "source")
		if err != nil {
			t.Fatal(err)
		}
		got := in.Conversion.Effective()
		if got.StartSeconds != 12.5 || got.DurationSeconds != 60.25 || got.SegmentSeconds != 60 || got.TimeoutSeconds != 120.5 {
			t.Fatalf("conversion was silently changed: %#v", got)
		}
		if _, err := wechatapp.ParseMediaRequest(json.RawMessage(`{"asset_id":"retained-audio","kind":"voice","request_key":"over-boundary","conversion":{"segment_seconds":60.001,"timeout_seconds":120.5}}`), "source"); err == nil {
			t.Fatal("voice segment over 60 seconds was accepted")
		}
	})
}
