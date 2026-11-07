package bot

import (
	"context"
	"errors"
	"time"

	"wechat-bot/internal/linuxnative"
)

// A continuation checkpoints its own immutable identity BEFORE contacting the
// native resume-only endpoint. Neither a crash nor a missing lookup permits a
// generic Submit/Retry of the original full audio.
func (w *Worker) submitMissingMedia(ctx context.Context, native resumeMissingMediaNative, d *mediaDelivery) string {
	if d.State != "resume_prepared" || d.ResumeOriginID == "" {
		return "media_resume_receipt_pending"
	}
	if !w.taskActive(d.Input) || !w.mediaAllowed(d) {
		d.State, d.Reason = "stopped", "media_resume_missing_not_authorized"
		w.mediaRecord(d, "media_stopped", "decision")
		return d.Reason
	}
	if reason := d.voiceMismatch(); reason != "" {
		d.State, d.Reason = "stopped", reason
		w.mediaRecord(d, "media_stopped", "error")
		return d.Reason
	}
	if err := w.validateMediaEvidence(ctx, d); err != nil {
		d.State, d.Reason = "stopped", "media_resume_missing_source_invalid"
		w.mediaRecord(d, "media_stopped", "decision")
		return d.Reason
	}
	if w.beforeSubmit(ctx) != nil || w.draining() {
		return "media_resume_connection_paused"
	}
	// A prepared intent may have survived a restart. Re-read the original
	// inventory before the first native call, rather than trusting its snapshot.
	var original *mediaDelivery
	for _, item := range w.runtime.Media {
		if item.Key == d.ResumedFrom && item.ID == d.ResumeOriginID {
			original = item
			break
		}
	}
	reader, ok := w.sender.(interface {
		ToolSendStatus(context.Context, string) (linuxnative.ToolStatus, error)
	})
	if original == nil || !ok {
		d.State, d.Reason = "stopped", "media_resume_original_identity_mismatch"
		w.mediaRecord(d, "media_stopped", "error")
		return d.Reason
	}
	readCtx, stop := context.WithTimeout(ctx, 8*time.Second)
	current, readErr := reader.ToolSendStatus(readCtx, original.ID)
	stop()
	if readErr != nil || !original.matchesNative(current) {
		d.Reason, d.Next = "media_resume_original_receipt_unavailable", time.Now().Add(deliveryBackoff(w.mediaRetryDelay(), 1))
		w.mediaRecord(d, "media_waiting", "decision")
		return d.Reason
	}
	original.Status = current.WithVoiceReceipts()
	if original.Status.State != "cancelled" || !resumableVoiceChunks(original) {
		d.Reason, d.Next = "media_resume_missing_requires_verified_terminal_chunks", time.Now().Add(deliveryBackoff(w.mediaRetryDelay(), 1))
		w.mediaRecord(d, "media_waiting", "decision")
		return d.Reason
	}
	d.State, d.Reason = "resume_submitting", "media_resume_missing_submitting"
	w.persistErr = w.saveRuntime()
	if w.persistErr != nil {
		return "media_resume_checkpoint_failed"
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var status linuxnative.ToolStatus
	err := w.guardedDeliverySubmit(callCtx, d.Input, func() error {
		var err error
		status, err = native.ResumeMissingMediaJob(callCtx, d.ResumeOriginID, d.Key)
		return err
	})
	if errors.Is(err, ErrDeliveryCancelled) {
		// The guard proves that no native call was entered for this new job.
		d.State, d.Reason, d.UserCancelled = "stopped", "media_cancelled_before_native_submission", true
		w.mediaRecord(d, "media_stopped", "decision")
		return d.Reason
	}
	if err != nil {
		d.State, d.Reason = "checking_receipt", "media_resume_outcome_unknown"
		d.Next = time.Now().Add(deliveryBackoff(w.mediaRetryDelay(), 1))
		w.mediaRecord(d, "media_waiting", "decision")
		return d.Reason
	}
	if !d.matchesNative(status) {
		d.State, d.Reason = "checking_receipt", "native_resume_missing_identity_mismatch"
		d.Next = time.Now().Add(deliveryBackoff(w.mediaRetryDelay(), 1))
		w.mediaRecord(d, "media_waiting", "error")
		return d.Reason
	}
	d.ID, d.Status, d.Phase = status.ID, status.WithVoiceReceipts(), status.Phase
	d.State, d.Reason, d.Next = "queued", "media_resume_missing_requested", time.Now()
	w.mediaRecord(d, "media_resume_missing_queued", "decision")
	return ""
}
