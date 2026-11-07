package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

func originalResumeReceipt(n *mediaFake, d *mediaDelivery) {
	n.status = d.Status
	n.status.ID, n.status.State, n.status.ThreadID = d.ID, "cancelled", d.Input.Thread
	n.status.SourceID, n.status.AccountID, n.status.Kind = d.SourceID, d.Input.Account, "send"
}

func TestQueuePauseResumeCancelPreservesIdentityAndIsolation(t *testing.T) {
	w, n, _, _ := mediaWorker(t)
	old := companion.Input{Account: "test", Thread: "room@chatroom", Sender: "owner", TraceID: "old"}
	d := &mediaDelivery{Key: "stable", Trace: "old", Input: old, ID: "job-old", Kind: "voice", State: "waiting"}
	n.status = linuxnative.ToolStatus{ID: d.ID, ThreadID: old.Thread, State: "running"}
	surf := &mediaDelivery{Key: "surf", Trace: "surf", Input: companion.Input{Thread: old.Thread, Surf: true}, Kind: "video", State: "prepared"}
	foreign := &mediaDelivery{Key: "foreign", Trace: "foreign", Input: companion.Input{Thread: old.Thread, Sender: "another"}, State: "prepared"}
	accepted := &mediaDelivery{Key: "accepted", Trace: "old", Input: old, State: "accepted"}
	w.runtime.Media = []*mediaDelivery{d, surf, foreign, accepted}
	p := &utterancePlan{Trace: "old", Input: old, State: "queued", MediaCaption: true, Messages: []string{"caption"}}
	w.runtime.Plans = append(w.runtime.Plans, p)
	in := old
	in.TraceID = "new"
	call := func(action string) {
		t.Helper()
		if _, e := w.controlQueue(context.Background(), in, false, json.RawMessage(`{"action":"`+action+`"}`)); e != nil {
			t.Fatal(e)
		}
	}
	call("pause")
	if !d.Paused || !surf.Paused || foreign.Paused || !p.Paused || accepted.State != "accepted" || n.cancels != 1 {
		t.Fatal("pause failed", d, surf, foreign, p, n.cancels)
	}
	if e := w.loadRuntime("server-1"); e != nil {
		t.Fatal(e)
	}
	d = w.runtime.Media[0]
	if !d.Paused || d.ID != "job-old" {
		t.Fatal("lost durable pause")
	}
	d.Next = time.Time{}
	w.reconcileMedia(context.Background())
	if n.sends != 0 || n.retries != 0 {
		t.Fatal("paused work advanced")
	}
	call("resume")
	if d.Paused || d.ID != "job-old" || n.sends != 0 {
		t.Fatal("resume cloned queue")
	}
	call("cancel")
	if !d.UserCancelled || foreign.UserCancelled {
		t.Fatal("cancellation scope")
	}
	call("resume")
	if !d.UserCancelled {
		t.Fatal("cancelled task resurrected")
	}
	if _, e := w.controlQueue(context.Background(), in, false, json.RawMessage(`{"action":"pause|cancel"}`)); e == nil {
		t.Fatal("invalid action")
	}
}

func TestQueueResumeReportsNativeBoundaryWithoutCloning(t *testing.T) {
	w, n, _, _ := mediaWorker(t)
	in := companion.Input{Account: "test", Thread: "room@chatroom", Sender: "owner", TraceID: "new"}
	d := &mediaDelivery{Key: "same-key", Trace: "old", ID: "same-job", Input: companion.Input{Thread: in.Thread, Sender: in.Sender}, State: "waiting", Paused: true}
	w.runtime.Media = []*mediaDelivery{d}
	n.status = linuxnative.ToolStatus{ID: d.ID, ThreadID: in.Thread, State: "cancelled", Retryable: false}
	result, err := w.controlQueue(context.Background(), in, true, json.RawMessage(`{"action":"resume"}`))
	if err != nil || !d.Paused || d.ID != "same-job" || n.sends != 0 || n.retries != 0 || !strings.Contains(result, "native_cancelled_job_not_retryable") {
		t.Fatal("hid native resume boundary or duplicated queue", result, err, d)
	}
}

func TestQueueResumeCancelledVoiceReportsSafeContinuationBoundary(t *testing.T) {
	w, n, _, base := mediaWorker(t)
	d := &mediaDelivery{Key: "cancelled-segments", Trace: base.TraceID, ID: "cancelled-job", Input: base, Kind: "voice", State: "stopped", UserCancelled: true, Status: linuxnative.ToolStatus{VoiceChunks: []linuxnative.VoiceChunkStatus{{Index: 1, State: "accepted", HasReceipt: true, ServerID: "one"}, {Index: 2, State: "cancelled"}}}}
	w.runtime.Media = []*mediaDelivery{d}
	in := companion.Input{Account: "test", Thread: d.Input.Thread, Sender: d.Input.Sender, TraceID: "new-control"}
	originalResumeReceipt(n, d)
	result, err := w.controlQueue(context.Background(), in, false, json.RawMessage(`{"action":"resume","trace_id":"incoming"}`))
	if err != nil || !strings.Contains(result, "native_resume_missing_identity_mismatch") || !strings.Contains(result, `"accepted_parts":1`) || !strings.Contains(result, `"total_parts":2`) || !d.UserCancelled || d.State != "stopped" || n.sends != 0 || n.retries != 0 {
		t.Fatalf("cancelled voice was presented as resumable: result=%s err=%v delivery=%+v sends=%d retries=%d", result, err, d, n.sends, n.retries)
	}
}

func TestQueueResumeMissingCreatesNewTraceAndNeverMutatesCancelledJob(t *testing.T) {
	w, n, _, base := mediaWorker(t)
	old := &mediaDelivery{Key: "cancelled-segments", Trace: base.TraceID, ID: "cancelled-job", Input: base, SourceID: "source", Kind: "voice", State: "stopped", UserCancelled: true, Status: linuxnative.ToolStatus{VoiceChunks: []linuxnative.VoiceChunkStatus{{Index: 1, State: "accepted", HasReceipt: true, ServerID: "one"}, {Index: 2, State: "cancelled"}}}}
	w.runtime.Media = []*mediaDelivery{old}
	originalResumeReceipt(n, old)
	n.resumeStatus = linuxnative.ToolStatus{ID: "new-missing-parts-job", State: "queued", ThreadID: old.Input.Thread, SourceID: old.SourceID, VoiceChunks: []linuxnative.VoiceChunkStatus{{Index: 2, State: "queued"}}}
	in := companion.Input{Account: "test", Thread: old.Input.Thread, Sender: old.Input.Sender, TraceID: "explicit-resume-message"}
	raw := json.RawMessage(`{"action":"resume","trace_id":"incoming"}`)
	result, err := w.controlQueue(context.Background(), in, false, raw)
	if err != nil || n.resumes != 1 || len(w.runtime.Media) != 2 || !old.UserCancelled || old.State != "stopped" || w.runtime.Media[1].Trace != in.TraceID || w.runtime.Media[1].ResumedFrom != old.Key || w.runtime.Media[1].UserCancelled || !strings.Contains(result, `"resumed_job_id":"new-missing-parts-job"`) {
		t.Fatalf("explicit missing-part resume did not create a safe new delivery: result=%s err=%v resumes=%d old=%+v new=%+v", result, err, n.resumes, old, w.runtime.Media)
	}
	// Repeating this explicit user message is idempotent.
	_, err = w.controlQueue(context.Background(), in, false, raw)
	if err != nil || n.resumes != 1 || len(w.runtime.Media) != 2 {
		t.Fatalf("duplicate explicit resume created another job: err=%v resumes=%d media=%+v", err, n.resumes, w.runtime.Media)
	}
	// A second “continue” is also not a licence to clone the same missing
	// suffix under a different trace.
	second := in
	second.TraceID = "another-explicit-resume-message"
	_, err = w.controlQueue(context.Background(), second, false, raw)
	if err != nil || n.resumes != 1 || len(w.runtime.Media) != 2 {
		t.Fatalf("second explicit resume duplicated missing parts: err=%v resumes=%d media=%+v", err, n.resumes, w.runtime.Media)
	}
}

func TestQueueResumeMissingRejectsUnsettledChunks(t *testing.T) {
	w, n, _, base := mediaWorker(t)
	d := &mediaDelivery{Key: "unsettled", Trace: base.TraceID, ID: "old-job", Input: base, SourceID: "source", Kind: "voice", State: "cancelling", UserCancelled: true, Status: linuxnative.ToolStatus{VoiceChunks: []linuxnative.VoiceChunkStatus{{Index: 1, State: "accepted", HasReceipt: true, ServerID: "one"}, {Index: 2, State: "running"}}}}
	w.runtime.Media = []*mediaDelivery{d}
	originalResumeReceipt(n, d)
	in := companion.Input{Account: "test", Thread: d.Input.Thread, Sender: d.Input.Sender, TraceID: "explicit-resume-message"}
	result, err := w.controlQueue(context.Background(), in, false, json.RawMessage(`{"action":"resume","trace_id":"incoming"}`))
	if err != nil || n.resumes != 0 || len(w.runtime.Media) != 1 || !strings.Contains(result, "media_resume_missing_requires_verified_terminal_chunks") {
		t.Fatalf("unsettled chunks were resumed: result=%s err=%v resumes=%d media=%+v", result, err, n.resumes, w.runtime.Media)
	}
}

func TestQueueResumeMissingRejectsIncompleteChunkInventory(t *testing.T) {
	w, n, _, base := mediaWorker(t)
	d := &mediaDelivery{Key: "incomplete", Trace: base.TraceID, ID: "old-job", Input: base, SourceID: "source", Kind: "voice", State: "stopped", UserCancelled: true, Status: linuxnative.ToolStatus{VoicePartsIncomplete: true, VoiceChunks: []linuxnative.VoiceChunkStatus{{Index: 1, State: "accepted", HasReceipt: true, ServerID: "one"}, {Index: 2, State: "cancelled"}}}}
	w.runtime.Media = []*mediaDelivery{d}
	originalResumeReceipt(n, d)
	in := companion.Input{Account: "test", Thread: d.Input.Thread, Sender: d.Input.Sender, TraceID: "explicit-resume-message"}
	result, err := w.controlQueue(context.Background(), in, false, json.RawMessage(`{"action":"resume","trace_id":"incoming"}`))
	if err != nil || n.resumes != 0 || !strings.Contains(result, "media_resume_missing_requires_verified_terminal_chunks") {
		t.Fatalf("incomplete chunk inventory was resumed: result=%s err=%v resumes=%d", result, err, n.resumes)
	}
}

func TestQueueResumeMissingRechecksCurrentAuthorizationAndSource(t *testing.T) {
	newCancelled := func(base companion.Input) *mediaDelivery {
		return &mediaDelivery{Key: "recheck", Trace: base.TraceID, ID: "old-job", Input: base, SourceID: "source", Kind: "voice", State: "stopped", UserCancelled: true, Status: linuxnative.ToolStatus{VoiceChunks: []linuxnative.VoiceChunkStatus{{Index: 1, State: "accepted", HasReceipt: true, ServerID: "one"}, {Index: 2, State: "cancelled"}}}}
	}
	t.Run("revoked route", func(t *testing.T) {
		w, n, _, base := mediaWorker(t)
		d := newCancelled(base)
		w.runtime.Media = []*mediaDelivery{d}
		w.allowed[base.Thread] = false
		in := companion.Input{Account: "test", Thread: base.Thread, Sender: base.Sender, TraceID: "resume-message"}
		result, err := w.controlQueue(context.Background(), in, false, json.RawMessage(`{"action":"resume","trace_id":"incoming"}`))
		if err != nil || n.resumes != 0 || !strings.Contains(result, "media_resume_missing_not_authorized") {
			t.Fatalf("revoked route resumed old media: result=%s err=%v resumes=%d", result, err, n.resumes)
		}
	})
	t.Run("invalid source proof", func(t *testing.T) {
		w, n, _, base := mediaWorker(t)
		base.ReplyContext = nil
		d := newCancelled(base)
		w.runtime.Media = []*mediaDelivery{d}
		in := companion.Input{Account: "test", Thread: base.Thread, Sender: base.Sender, TraceID: "resume-message"}
		result, err := w.controlQueue(context.Background(), in, false, json.RawMessage(`{"action":"resume","trace_id":"incoming"}`))
		if err != nil || n.resumes != 0 || !strings.Contains(result, "media_resume_missing_source_invalid") {
			t.Fatalf("invalid source proof resumed old media: result=%s err=%v resumes=%d", result, err, n.resumes)
		}
	})
}

func TestAddressedInterruptionPausesOwnAndSurfMedia(t *testing.T) {
	w, _, _, _ := mediaWorker(t)
	old := companion.Input{Thread: "filehelper", Sender: "requester", TraceID: "old"}
	w.runtime.Media = []*mediaDelivery{{Key: "one", Trace: "old", Input: old, State: "prepared"}}
	w.pauseInterruptedMedia(linuxnative.Message{ID: "new", ThreadID: "filehelper", SenderID: "requester", Type: 1, Body: "换成文件，不发语音了"})
	if !w.runtime.Media[0].Paused {
		t.Fatal("interruption did not pause pending upload")
	}
}
