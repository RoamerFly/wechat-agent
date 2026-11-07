package bot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type resumeNativeFixture struct {
	*mediaFake
	original, resumed linuxnative.ToolStatus
	onResume          func(string)
	lost              bool
	missing           bool
}

func (n *resumeNativeFixture) ToolSendStatus(_ context.Context, id string) (linuxnative.ToolStatus, error) {
	if id == n.original.ID {
		return n.original, nil
	}
	if id == n.resumed.ID && !n.missing {
		return n.resumed, nil
	}
	return linuxnative.ToolStatus{}, linuxnative.ErrToolNotFound
}
func (n *resumeNativeFixture) LookupSend(_ context.Context, key string) (linuxnative.ToolStatus, error) {
	if n.keys[key] && !n.missing {
		return n.resumed, nil
	}
	return linuxnative.ToolStatus{}, linuxnative.ErrToolNotFound
}
func (n *resumeNativeFixture) ResumeMissingMediaJob(_ context.Context, original, key string) (linuxnative.ToolStatus, error) {
	n.resumes++
	if n.onResume != nil {
		n.onResume(key)
	}
	if original != n.original.ID {
		return linuxnative.ToolStatus{}, errors.New("wrong original task")
	}
	n.keys[key] = true
	if n.lost {
		return linuxnative.ToolStatus{}, errors.New("synthetic resume response lost")
	}
	return n.resumed, nil
}

func resumeFixture(t *testing.T) (*Worker, *resumeNativeFixture, companion.Input, *mediaDelivery) {
	t.Helper()
	w, fake, _, in := mediaWorker(t)
	old := &mediaDelivery{Key: "original-delivery", Trace: in.TraceID, ID: "original-job", Input: in, SourceID: "original-source", Kind: "voice", State: "stopped", UserCancelled: true, DeferredSource: true}
	originalResumeReceipt(fake, old)
	fake.status.VoiceChunks = []linuxnative.VoiceChunkStatus{{Index: 0, State: "accepted", ServerID: "100", HasReceipt: true}, {Index: 1, State: "cancelled"}, {Index: 2, State: "cancelled"}}
	old.Status = fake.status
	old.SynthesisText, old.SynthesisTextKnown = "full original synthetic script", true
	native := &resumeNativeFixture{mediaFake: fake, original: fake.status,
		resumed: linuxnative.ToolStatus{ID: "resumed-job", ThreadID: in.Thread, SourceID: old.SourceID, AccountID: in.Account, Kind: "send", State: "queued"}}
	w.sender, w.runtime.Media = native, []*mediaDelivery{old}
	in.TraceID = "explicit-continuation"
	return w, native, in, old
}

func TestResumeCheckpointsBeforeCallAndOnlyLooksUpLostResponse(t *testing.T) {
	w, n, in, old := resumeFixture(t)
	n.lost = true
	n.onResume = func(key string) {
		t.Helper()
		rows, err := ReadDeliveryEvidence(w.config.StateDir, in.Account, []string{key})
		if err != nil || rows[key].ResumedFrom != old.Key || rows[key].Status.State != "unknown" {
			t.Fatal("resume called before its durable intent", rows, err)
		}
		raw, err := os.ReadFile(filepath.Join(w.config.StateDir, "messages-v2.json"))
		var head struct{ Version int }
		if err != nil || json.Unmarshal(raw, &head) != nil || head.Version != 2 {
			t.Fatal("old workers could replay a new continuation", head, err)
		}
	}
	next, reason := w.resumeMissingMedia(t.Context(), in, old)
	if reason != "media_resume_outcome_unknown" || next == nil || next.ID != "" || next.State != "checking_receipt" || n.resumes != 1 {
		t.Fatal(next, reason, n.resumes)
	}
	if next.SynthesisTextKnown || next.SynthesisText != "" || !old.SynthesisTextKnown {
		t.Fatal("whole-source script assigned to resumed suffix", next, old)
	}
	key := next.Key
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	next, old = w.runtime.Media[1], w.runtime.Media[0]
	n.missing = true
	for range 2 {
		next.Next = time.Time{}
		w.reconcileMedia(t.Context())
	}
	if n.resumes != 1 || n.sends != 0 || n.retries != 0 || next.Key != key || next.State != "checking_receipt" {
		t.Fatal("unknown continuation replayed", n.resumes, n.sends, n.retries, next)
	}
	in.TraceID = "another-continue"
	if got, _ := w.resumeMissingMedia(t.Context(), in, old); got != next || n.resumes != 1 {
		t.Fatal("second instruction cloned unknown continuation")
	}
	n.missing = false
	n.resumed.State, n.resumed.ServerID = "accepted", "102"
	n.resumed.VoiceChunks = []linuxnative.VoiceChunkStatus{{Index: 1, State: "accepted", HasReceipt: true, ServerID: "101"}, {Index: 2, State: "accepted", HasReceipt: true, ServerID: "102"}}
	next.Next = time.Time{}
	w.reconcileMedia(t.Context())
	if next.State != "accepted" || next.ID != n.resumed.ID || !old.UserCancelled || old.Status.VoiceChunks[0].ServerID != "100" || n.resumes != 1 || n.sends != 0 {
		t.Fatal("late continuation receipt lost original lineage", next, old)
	}
}

func TestResumePreparedRestartsWithoutSendingWholeSource(t *testing.T) {
	w, n, in, old := resumeFixture(t)
	next := *old
	next.Key, next.ResumedFrom, next.ResumeOriginID, next.Trace = "prepared-continuation", old.Key, old.ID, in.TraceID
	next.Input, next.ID, next.State, next.UserCancelled, next.Status = in, "", "resume_prepared", false, linuxnative.ToolStatus{}
	w.runtime.Media = append(w.runtime.Media, &next)
	if err := w.saveRuntime(); err != nil {
		t.Fatal(err)
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	w.reconcileMedia(t.Context())
	if n.resumes != 1 || n.sends != 0 || w.runtime.Media[1].ID != n.resumed.ID {
		t.Fatal("prepared continuation recovery bypassed resume-only endpoint", n.resumes, n.sends, w.runtime.Media[1])
	}
}

func TestQueueResumeKeepsPreparedAndUnknownContinuationBoundaries(t *testing.T) {
	for _, state := range []string{"resume_prepared", "checking_receipt"} {
		t.Run(state, func(t *testing.T) {
			w, n, in, old := resumeFixture(t)
			next := *old
			next.Key, next.ResumedFrom, next.ResumeOriginID, next.Trace = "continuation", old.Key, old.ID, in.TraceID
			next.Input, next.ID, next.State, next.UserCancelled, next.Paused, next.Status = in, "", state, false, true, linuxnative.ToolStatus{}
			w.runtime.Media = append(w.runtime.Media, &next)
			control := in
			control.TraceID = "another-control"
			raw := []byte(`{"action":"resume","trace_id":"` + in.TraceID + `"}`)
			if _, err := w.controlQueue(t.Context(), control, false, raw); err != nil {
				t.Fatal(err)
			}
			if next.State != state || next.Paused {
				t.Fatal("resume changed the native-entry boundary", next)
			}
			w.reconcileMedia(t.Context())
			want := 0
			if state == "resume_prepared" {
				want = 1
			}
			if n.resumes != want || n.sends != 0 || n.retries != 0 {
				t.Fatal("ordinary queue resume replayed audio", n.resumes, n.sends, n.retries)
			}
		})
	}
}

func TestResumeUsesLiveInventoryAndRejectsAmbiguousRanges(t *testing.T) {
	for _, scenario := range []string{"in-flight", "all-accepted", "duplicate-index", "gap", "accepted-after-missing", "wrong-account", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			w, n, in, old := resumeFixture(t)
			n.original.VoiceChunks = append([]linuxnative.VoiceChunkStatus(nil), n.original.VoiceChunks...)
			switch scenario {
			case "in-flight":
				n.original.VoiceChunks[1].State = "submitting"
			case "all-accepted":
				for i := range n.original.VoiceChunks {
					n.original.VoiceChunks[i].State, n.original.VoiceChunks[i].HasReceipt, n.original.VoiceChunks[i].ServerID = "accepted", true, "100"
				}
			case "duplicate-index":
				n.original.VoiceChunks[2].Index = 1
			case "gap":
				n.original.VoiceChunks[2].Index = 4
			case "accepted-after-missing":
				n.original.VoiceChunks[2].State, n.original.VoiceChunks[2].HasReceipt, n.original.VoiceChunks[2].ServerID = "accepted", true, "102"
			case "wrong-account":
				n.original.AccountID = "foreign"
			case "incomplete":
				n.original.VoicePartsIncomplete = true
			}
			if _, reason := w.resumeMissingMedia(t.Context(), in, old); reason == "" || n.resumes != 0 || len(w.runtime.Media) != 1 {
				t.Fatal("stale snapshot authorized continuation", scenario, reason, n.resumes)
			}
		})
	}
}

func TestResumeCheckpointFailureAndFinalStopPreventNativeCall(t *testing.T) {
	t.Run("disk", func(t *testing.T) {
		w, n, in, old := resumeFixture(t)
		if err := os.Mkdir(filepath.Join(w.config.StateDir, "messages-v2.json"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, reason := w.resumeMissingMedia(t.Context(), in, old); reason != "media_resume_checkpoint_failed" || n.resumes != 0 {
			t.Fatal("resume ran before checkpoint", reason, n.resumes)
		}
	})
	t.Run("stop", func(t *testing.T) {
		w, n, in, old := resumeFixture(t)
		w.config.WithDeliverySubmit = func(context.Context, companion.Input, func() error) error { return ErrDeliveryCancelled }
		next, reason := w.resumeMissingMedia(t.Context(), in, old)
		if reason != "media_cancelled_before_native_submission" || next == nil || next.State != "stopped" || !next.UserCancelled || n.resumes != 0 {
			t.Fatal("final stop was bypassed", reason, next, n.resumes)
		}
	})
	t.Run("voice", func(t *testing.T) {
		w, n, in, old := resumeFixture(t)
		old.Input.ExpectedVoiceID, old.VoiceID = "private-voice", "default-voice"
		next, reason := w.resumeMissingMedia(t.Context(), in, old)
		if reason != "media_expected_voice_mismatch" || next == nil || next.State != "stopped" || n.resumes != 0 {
			t.Fatal("continuation substituted a different voice", reason, next, n.resumes)
		}
	})
}

func TestResumeRejectsWrongContinuationIdentityWithoutCloning(t *testing.T) {
	for _, scenario := range []string{"old-job", "wrong-account", "wrong-source"} {
		t.Run(scenario, func(t *testing.T) {
			w, n, in, old := resumeFixture(t)
			switch scenario {
			case "old-job":
				n.resumed.ID = old.ID
			case "wrong-account":
				n.resumed.AccountID = "foreign"
			case "wrong-source":
				n.resumed.SourceID = "foreign-source"
			}
			next, reason := w.resumeMissingMedia(t.Context(), in, old)
			if reason != "native_resume_missing_identity_mismatch" || next == nil || next.ID != "" || next.State != "checking_receipt" {
				t.Fatal("wrong continuation accepted", reason, next)
			}
			_, _ = w.resumeMissingMedia(t.Context(), in, old)
			if n.resumes != 1 || n.sends != 0 {
				t.Fatal("wrong response triggered a new send", n.resumes, n.sends)
			}
		})
	}
}
