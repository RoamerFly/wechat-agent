package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"wechat-bot/internal/companion"
)

func TestHostResumeRunsThroughNormalWorkerLoop(t *testing.T) {
	w, n, in, old := resumeFixture(t)
	request := MediaResumeRequest{ID: "host-operation", Account: in.Account, Thread: in.Thread, Key: old.Key, Trace: in.TraceID}
	if err := w.saveRuntime(); err != nil {
		t.Fatal(err)
	}
	w.config.NextMediaResume = func(time.Time) (*MediaResumeRequest, error) { return &request, nil }
	pages := 0
	w.source.(*fakeSource).onPage = func(string) { pages++ }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var result MediaResumeResult
	w.config.MediaResumeResult = func(_ MediaResumeRequest, got MediaResumeResult) error {
		if pages < 2 {
			t.Fatal("host continuation ran before the normal inbound scan", pages)
		}
		result = got
		cancel()
		return nil
	}
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if result.State != "tracking" || result.DeliveryKey == "" || n.resumes != 1 || n.sends != 0 {
		t.Fatal("normal worker loop did not consume explicit host request", result, n.resumes, n.sends)
	}
}

func TestHostResumeRequestSurvivesLostAcknowledgmentWithoutSecondJob(t *testing.T) {
	w, n, in, old := resumeFixture(t)
	request := MediaResumeRequest{ID: "host-operation", Account: in.Account, Thread: in.Thread, Key: old.Key, Trace: in.TraceID}
	w.config.NextMediaResume = func(time.Time) (*MediaResumeRequest, error) { return &request, nil }
	w.config.MediaResumeResult = func(MediaResumeRequest, MediaResumeResult) error {
		return errors.New("synthetic acknowledgment write failure")
	}
	w.processHostMediaResume(t.Context())
	if n.resumes != 1 || w.persistErr == nil || len(w.runtime.Media) != 2 {
		t.Fatal("request not checkpointed independently of its acknowledgment", n.resumes, w.persistErr)
	}
	if err := w.loadRuntime("server-1"); err != nil {
		t.Fatal(err)
	}
	w.persistErr = nil
	var result MediaResumeResult
	w.config.MediaResumeResult = func(got MediaResumeRequest, value MediaResumeResult) error {
		if got != request {
			t.Fatal(got)
		}
		result = value
		return nil
	}
	w.processHostMediaResume(t.Context())
	if result.State != "tracking" || result.DeliveryKey != w.runtime.Media[1].Key || n.resumes != 1 || n.sends != 0 {
		t.Fatal("lost acknowledgment cloned the continuation", result, n.resumes, n.sends)
	}
}

func TestHostResumeWaitsForLiveReceiptAndHonorsStopAndScope(t *testing.T) {
	for _, scenario := range []string{"waiting", "cancelled", "foreign", "not-submitted", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			w, n, in, old := resumeFixture(t)
			request := MediaResumeRequest{ID: "host-operation", Account: in.Account, Thread: in.Thread, Key: old.Key, Trace: in.TraceID}
			var result MediaResumeResult
			w.config.NextMediaResume = func(time.Time) (*MediaResumeRequest, error) { return &request, nil }
			w.config.MediaResumeResult = func(_ MediaResumeRequest, value MediaResumeResult) error { result = value; return nil }
			switch scenario {
			case "waiting":
				n.original.VoiceChunks[1].State = "submitting"
			case "cancelled":
				w.config.DeliveryCancelled = func(got companion.Input) (bool, error) { return got.TraceID == request.Trace, nil }
			case "foreign":
				request.Account = "another-account"
			case "not-submitted":
				old.ID = ""
			case "revoked":
				w.config.AuthorizeMedia = func(companion.Input) bool { return false }
			}
			w.processHostMediaResume(t.Context())
			want := map[string]string{"waiting": "waiting_receipt", "cancelled": "cancelled", "foreign": "blocked", "not-submitted": "blocked", "revoked": "blocked"}[scenario]
			if result.State != want || n.resumes != 0 || n.sends != 0 {
				t.Fatal("resume boundary ignored", scenario, result, n.resumes, n.sends)
			}
			if scenario == "waiting" {
				n.original.VoiceChunks[1].State = "cancelled"
				w.processHostMediaResume(t.Context())
				if result.State != "tracking" || n.resumes != 1 || old.Status.VoiceChunks[0].ServerID != "100" {
					t.Fatal("verified missing suffix did not resume", result, n.resumes)
				}
			}
		})
	}
}
