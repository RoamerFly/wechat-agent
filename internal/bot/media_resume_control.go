package bot

import (
	"context"
	"time"
)

// MediaResumeRequest is a host-issued explicit operation, never model input.
// Its original key and new trace remain immutable across restarts.
type MediaResumeRequest struct {
	ID      string `json:"id"`
	Account string `json:"account"`
	Thread  string `json:"thread"`
	Key     string `json:"key"`
	Trace   string `json:"trace"`
}

type MediaResumeResult struct {
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	DeliveryKey string `json:"delivery_key,omitempty"`
}

func (w *Worker) processHostMediaResume(ctx context.Context) {
	if w.runtime == nil || w.draining() || w.config.NextMediaResume == nil || w.config.MediaResumeResult == nil {
		return
	}
	request, err := w.config.NextMediaResume(time.Now())
	if err != nil {
		w.persistErr = err
		return
	}
	if request == nil {
		return
	}
	result := MediaResumeResult{State: "blocked", Reason: "media_resume_original_not_found"}
	if request.Account == w.runtime.Account && request.Trace != "" && request.ID != "" {
		for _, old := range w.runtime.Media {
			if old.Key != request.Key || old.Input.Thread != request.Thread || old.Input.Account != request.Account {
				continue
			}
			if old.ID == "" && old.State == "stopped" {
				result.Reason = "media_resume_original_not_submitted"
				break
			}
			in := old.Input
			in.TraceID = request.Trace
			if !w.taskActive(in) {
				result.State, result.Reason = "cancelled", "media_cancelled_before_native_submission"
				break
			}
			next, reason := w.resumeMissingMedia(ctx, in, old)
			result.Reason = reason
			if next != nil {
				result.DeliveryKey = next.Key
				switch {
				case next.UserCancelled:
					result.State = "cancelled"
				case next.State == "stopped" || next.State == "needs_review":
					result.State = "blocked"
				default:
					result.State = "tracking"
				}
			} else {
				switch reason {
				case "media_resume_original_receipt_unavailable", "media_resume_missing_requires_verified_terminal_chunks", "media_resume_connection_paused":
					result.State = "waiting_receipt"
				}
			}
			break
		}
	}
	if w.persistErr != nil {
		return
	}
	if err := w.config.MediaResumeResult(*request, result); err != nil {
		w.persistErr = err
	}
}
