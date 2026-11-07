package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

// Native jobs own upload offsets and receipts. Never reconstruct a voice job
// when a receipt is missing: retry the original only if native permits it.
type mediaNative interface {
	Submit(context.Context, linuxnative.ToolSend) (linuxnative.ToolStatus, error)
	LookupSend(context.Context, string) (linuxnative.ToolStatus, error)
	ToolSendStatus(context.Context, string) (linuxnative.ToolStatus, error)
	RetryMediaJob(context.Context, string) (linuxnative.ToolStatus, error)
	CancelMediaJob(context.Context, string) (linuxnative.ToolStatus, error)
}
type MediaSeed struct {
	OriginInput  *companion.Input         `json:"origin_input,omitempty"`
	SourceThread string                   `json:"source_thread,omitempty"`
	OriginTask   string                   `json:"origin_task,omitempty"`
	Key          string                   `json:"key,omitempty"`
	OriginThread string                   `json:"origin_thread,omitempty"`
	Requester    string                   `json:"requester,omitempty"`
	Trace        string                   `json:"trace"`
	Input        companion.Input          `json:"input"`
	Media        companion.GeneratedMedia `json:"media"`
	SourceID     string                   `json:"source_id"`
	JobID        string                   `json:"job_id"`
}
type mediaDelivery struct {
	// Cancellation is a durable decision made by a particular incoming
	// message.  It is deliberately separate from a native job state: native
	// may still report receipts for chunks which had already been claimed when
	// the user interrupted the send.
	CancelTrace        string                 `json:"cancel_trace,omitempty"`
	CancelSender       string                 `json:"cancel_sender,omitempty"`
	CancelledAt        time.Time              `json:"cancelled_at,omitempty"`
	StateVersion       int                    `json:"state_version,omitempty"`
	ResumedFrom        string                 `json:"resumed_from,omitempty"`
	ResumeOriginID     string                 `json:"resume_origin_id,omitempty"`
	OriginInput        *companion.Input       `json:"origin_input,omitempty"`
	VoiceProfile       string                 `json:"voice_profile_id,omitempty"`
	VoiceID            string                 `json:"voice_id,omitempty"`
	VoiceEngine        string                 `json:"voice_engine,omitempty"`
	SynthesisText      string                 `json:"synthesis_text,omitempty"`
	SynthesisTextKnown bool                   `json:"synthesis_text_known,omitempty"`
	Paused             bool                   `json:"paused,omitempty"`
	RepairReported     bool                   `json:"repair_reported,omitempty"`
	ReconcileAfter     time.Time              `json:"reconcile_after,omitempty"`
	CrossSettled       bool                   `json:"cross_settled,omitempty"`
	CrossTask          string                 `json:"cross_task,omitempty"`
	CrossRequester     string                 `json:"cross_requester,omitempty"`
	CrossOrigin        string                 `json:"cross_origin,omitempty"`
	CrossSource        string                 `json:"cross_source,omitempty"`
	ParentAssetID      string                 `json:"parent_asset_id,omitempty"`
	CachedMIME         string                 `json:"cached_mime,omitempty"`
	NotBefore          time.Time              `json:"not_before,omitempty"`
	CompletedAt        time.Time              `json:"completed_at,omitempty"`
	DurationMS         int64                  `json:"duration_ms,omitempty"`
	URL                string                 `json:"url,omitempty"`
	Description        string                 `json:"description,omitempty"`
	Key                string                 `json:"key"`
	Trace              string                 `json:"trace"`
	Input              companion.Input        `json:"input"`
	SourceID           string                 `json:"source_id"`
	AssetID            string                 `json:"asset_id,omitempty"`
	DeferredSource     bool                   `json:"deferred_source,omitempty"`
	Kind               string                 `json:"kind"`
	Title              string                 `json:"title"`
	ID                 string                 `json:"id"`
	State              string                 `json:"state"`
	Phase              string                 `json:"phase"`
	Reason             string                 `json:"reason"`
	Retries            int                    `json:"retries"`
	Checks             int                    `json:"checks"`
	UserCancelled      bool                   `json:"user_cancelled,omitempty"`
	Created            time.Time              `json:"created"`
	Next               time.Time              `json:"next"`
	Status             linuxnative.ToolStatus `json:"status"`
	Signature          string                 `json:"signature,omitempty"`
}

func (d *mediaDelivery) terminal() bool {
	return d.State == "accepted" || d.State == "stopped" || d.State == "needs_review"
}

func (d *mediaDelivery) matchesNative(s linuxnative.ToolStatus) bool {
	if d.ResumeOriginID != "" && (s.ID == d.ResumeOriginID || s.SourceID != d.SourceID) {
		return false
	}
	if s.ID == "" || d.ID != "" && d.ID != s.ID || s.ThreadID != d.Input.Thread || s.SourceID != "" && s.SourceID != d.SourceID {
		return false
	}
	return !d.DeferredSource || s.AccountID == d.Input.Account && d.SourceID != "" && s.SourceID == d.SourceID && (s.Kind == "send" || s.Kind == d.Kind)
}

func (d *mediaDelivery) unconfirmedVoiceParts() bool {
	if !d.DeferredSource || d.Kind != "voice" {
		return false
	}
	for _, part := range d.Status.VoiceChunks {
		if part.State == "submitting" || part.State == "needs_review" {
			return true
		}
	}
	return d.Status.VoicePartsIncomplete
}
func (w *Worker) draining() bool { return w.config.Draining != nil && w.config.Draining() }
func awaitingMediaReceipt(d *mediaDelivery) bool {
	return d.State == "checking_receipt" || (d.State == "waiting" || d.State == "unknown") && (d.Status.State == "needs_review" || d.Status.State == "unknown" || d.Reason == "media_receipt_id_missing")
}

// Once a submission is uncertain and resending is disabled, this lane may
// only read the original receipt. In particular, "key not found" is not
// permission to drop back into the submit path, even after a restart.
func (w *Worker) awaitMediaReceipt(d *mediaDelivery) {
	d.State, d.Reason = "checking_receipt", "media_readonly_receipt_pending"
	d.Checks++
	d.Next = time.Now().Add(deliveryBackoff(w.mediaRetryDelay(), d.Checks))
	w.mediaRecord(d, "media_waiting", "decision")
}

func (w *Worker) checkMediaReceipt(ctx context.Context, n mediaNative, d *mediaDelivery) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var s linuxnative.ToolStatus
	err := linuxnative.ErrToolNotFound
	if d.ID != "" {
		s, err = n.ToolSendStatus(ctx, d.ID)
	}
	if errors.Is(err, linuxnative.ErrToolNotFound) && d.Key != "" {
		s, err = n.LookupSend(ctx, d.Key)
	}
	if err == nil {
		if !d.matchesNative(s) {
			d.State, d.Reason = "needs_review", "media_identity_mismatch"
			w.mediaRecord(d, "media_stopped", "error")
			return
		}
		s = s.WithVoiceReceipts()
		// Some status reads omit per-part detail. Preserve previously
		// observed receipts for display, without using the cached list to
		// infer that every part of an uncertain job has now been accepted.
		if len(s.VoiceChunks) == 0 && len(d.Status.VoiceChunks) > 0 {
			s.VoiceChunks, s.VoicePartsIncomplete = d.Status.VoiceChunks, true
		}
		d.ID, d.Status, d.Phase = s.ID, s, s.Phase
		if d.DeferredSource && !w.mediaAllowed(d) && (s.State == "queued" || s.State == "running" || s.State == "submitting") {
			w.cancelMedia(n, d, true, "media_permission_or_quiet_changed")
			return
		}
		switch s.State {
		case "accepted":
			if s.ServerID != "" {
				d.State, d.Reason, d.CompletedAt = "accepted", "media_server_accepted", time.Now().UTC()
				w.advanceSendGate(d.Input.Thread)
				w.mediaRecord(d, "message_sent", "sent")
				return
			}
		case "failed", "cancelled", "rejected":
			d.State, d.Reason = "stopped", s.Reason
			w.mediaRecord(d, "media_stopped", "error")
			return
		}
	}
	w.awaitMediaReceipt(d)
}

func (w *Worker) mediaPredecessor(index int, d *mediaDelivery) (*mediaDelivery, bool) {
	// Proactive shares can wait behind a real conversation request that has not
	// started uploading. Never interrupt a live native upload for priority.
	if d.Input.Surf {
		for _, other := range w.runtime.Media {
			if other.Input.Thread == d.Input.Thread && !other.Input.Surf && !other.terminal() && other.ID == "" {
				return nil, true
			}
		}
	}
	var previous *mediaDelivery
	for _, part := range w.runtime.Media[:index] {
		if part.Input.Thread != d.Input.Thread {
			continue
		}
		if !part.terminal() {
			// Unknown outcomes remain durable and are reconciled independently;
			// they must not freeze every later unrelated chat forever.
			if awaitingMediaReceipt(part) && part.Trace != d.Trace {
				continue
			}
			if !d.Input.Surf && part.Input.Surf && part.ID == "" && part.Trace != d.Trace {
				continue
			}
			return nil, true
		}
		previous = part
	}
	return previous, false
}
func (w *Worker) mediaTraceOnlyUnknown(trace string) bool {
	found := false
	for _, d := range w.runtime.Media {
		if d.Trace != trace || d.terminal() {
			continue
		}
		if !awaitingMediaReceipt(d) {
			return false
		}
		found = true
	}
	return found
}
func (w *Worker) pendingMedia() bool {
	if w.draining() {
		// Prepared files have no native send intent yet. Keep them on disk for
		// the replacement worker instead of both refusing to send and waiting
		// forever for them to finish. Unknown receipts are parked as before.
		for _, d := range w.runtime.Media {
			if !d.terminal() && !awaitingMediaReceipt(d) && !(d.State == "prepared" && d.ID == "") {
				return true
			}
		}
		return false
	}
	for _, r := range w.runtime.MediaReplies {
		if !r.terminal() {
			if !w.mediaTraceOnlyUnknown(r.Trace) {
				return true
			}
		}
	}
	for _, d := range w.runtime.Media {
		if !d.terminal() && !awaitingMediaReceipt(d) {
			return true
		}
	}
	return false
}
func (w *Worker) mediaAllowed(d *mediaDelivery) bool {
	in := d.Input
	in.TraceID = d.Trace
	if w.config.AuthorizeMediaAsset != nil && !w.config.AuthorizeMediaAsset(in, d.Kind, d.AssetID) {
		return false
	}
	if w.config.AuthorizeMedia != nil && !w.config.AuthorizeMedia(d.Input) {
		return false
	}
	routeAllowed := w.allowed[d.Input.Thread]
	if d.CrossRequester != "" {
		if w.config.MediaSeedAllowed == nil || !w.config.MediaSeedAllowed(MediaSeed{OriginInput: d.OriginInput, Key: d.Key, Trace: d.Trace, SourceID: d.SourceID, Input: d.Input, Requester: d.CrossRequester, OriginThread: d.CrossOrigin, SourceThread: d.CrossSource, OriginTask: d.CrossTask}) {
			return false
		}
		// An authenticated owner task may target a known account conversation
		// without enabling ordinary automatic replies in that conversation.
		routeAllowed = true
	}
	c, ok := w.model.(*companion.Engine)
	explicitAction := d.Input.Mode == "direct"
	if !explicitAction && d.CrossRequester != "" && d.OriginInput != nil && hostActionProof(*d.OriginInput) {
		// A protected GUI click is an explicit delivery request. Its origin
		// proof and canonical route must both remain valid; the mode string
		// alone never grants a quiet-hours exception.
		explicitAction = w.validateMediaEvidence(context.Background(), d) == nil
	}
	return ok && routeAllowed && companion.MediaPermitted(c.Config.Permissions, d.Kind) &&
		(explicitAction || !c.Config.Quiet(time.Now())) && (!d.Input.Surf || w.config.SurfAllowed == nil || w.config.SurfAllowed(d.Input.Thread))
}

// mediaTraceCancelled is the durable tombstone used by late plugin callbacks,
// seed adoption and retry recovery.  A new request always gets a new trace;
// reusing a cancelled trace is never permission to revive its delivery.
func (w *Worker) mediaTraceCancelled(trace string) bool {
	if w.runtime == nil || trace == "" {
		return false
	}
	for _, d := range w.runtime.Media {
		if d.Trace == trace && d.UserCancelled {
			return true
		}
	}
	return false
}

func (w *Worker) retryUncertainMedia(d *mediaDelivery) bool {
	return d.ResumedFrom == "" && !d.DeferredSource && w.config.Options.RetryUncertain
}

func (w *Worker) mediaRetryDelay() int {
	if w.config.Options == nil {
		return 0
	}
	return w.config.Options.RetryDelaySeconds
}

func (w *Worker) mediaCancelDelay(d *mediaDelivery) time.Duration {
	delay := deliveryBackoff(w.mediaRetryDelay(), d.Checks)
	if d.DeferredSource && d.UserCancelled {
		// A late native job may already be pacing a following voice part.
		// Keep seeking the original cancellation target before normal backoff.
		delay = min(delay, 2*time.Second)
	}
	return delay
}

func (w *Worker) queueMedia(ctx context.Context, c *companion.Engine, input companion.Input, m companion.GeneratedMedia) bool {
	_, ok := w.sender.(mediaNative)
	if !ok || w.runtime == nil {
		return false
	}
	if w.mediaTraceCancelled(input.TraceID) {
		w.activity(Record{Event: "media_callback_ignored", Category: "decision", ThreadID: input.Thread, MessageID: input.TraceID, Status: "stopped", Reason: "media_trace_cancelled"})
		return true
	}
	source := m.SourceID
	deferred := source == "" && w.config.PrepareMediaSource != nil
	if source == "" {
		source = m.ID
	}
	key := "media-" + messageHash(w.source.Account() + "|" + input.Thread + "|" + input.TraceID + "|" + source + "|" + m.Kind)[:48]
	for _, old := range w.runtime.Media {
		if old.Key == key {
			return true
		}
	}
	if len(w.runtime.Media) >= 1000 {
		kept := w.runtime.Media[:0]
		for _, old := range w.runtime.Media {
			if !old.terminal() || time.Since(old.Created) < 7*24*time.Hour {
				kept = append(kept, old)
			}
		}
		w.runtime.Media = kept
	}
	if len(w.runtime.Media) >= 1000 {
		w.activity(Record{Event: "send_failed", Category: "error", ThreadID: input.Thread, MessageID: input.TraceID, Reason: "media_queue_full", Status: "failed"})
		return true
	}
	d := &mediaDelivery{VoiceProfile: m.VoiceProfile, VoiceID: m.VoiceID, VoiceEngine: m.VoiceEngine, SynthesisText: m.SynthesisText, SynthesisTextKnown: m.SynthesisTextKnown, ParentAssetID: m.ParentAssetID, CachedMIME: m.CachedMIME, DurationMS: m.DurationMS, URL: m.URL, Description: m.Description, Key: key, Trace: input.TraceID, Input: input, SourceID: source, AssetID: m.AssetID, Kind: m.Kind, Title: m.Title, State: "prepared", Created: time.Now().UTC()}
	if deferred {
		d.DeferredSource, d.SourceID = true, ""
	}
	if bound, err := w.prepareMediaEvidence(ctx, input); err != nil {
		// The native journal might already contain this deterministic key after
		// a lost checkpoint. Always reconcile that receipt before stopping a
		// newly attempted send because its source proof is unavailable.
		d.Reason = "media_source_proof_invalid"
	} else {
		d.Input = bound
	}
	w.runtime.Media = append(w.runtime.Media, d)
	w.persistErr = w.saveRuntime()
	if w.persistErr != nil {
		return true
	}
	w.mediaRecord(d, "media_queued", "decision")
	w.reconcileMedia(ctx)
	return true
}
func (w *Worker) adoptMediaSeeds() {
	if w.config.MediaSeeds == nil {
		return
	}
	for _, seed := range w.config.MediaSeeds() {
		if w.mediaTraceCancelled(seed.Trace) {
			continue
		}
		if seed.Key != "" {
			known := w.config.MediaSeedKnown
			if known == nil {
				known = w.config.MediaSeedAllowed
			}
			if known == nil || !known(seed) {
				continue
			}
		}
		exists := false
		for _, d := range w.runtime.Media {
			if seed.JobID != "" && d.ID == seed.JobID || seed.Key != "" && d.Key == seed.Key {
				exists = true
			}
		}
		if exists || !w.allowed[seed.Input.Thread] && seed.Key == "" {
			continue
		}
		d := &mediaDelivery{OriginInput: seed.OriginInput, VoiceProfile: seed.Media.VoiceProfile, VoiceID: seed.Media.VoiceID, VoiceEngine: seed.Media.VoiceEngine, SynthesisText: seed.Media.SynthesisText, SynthesisTextKnown: seed.Media.SynthesisTextKnown, ParentAssetID: seed.Media.ParentAssetID, CachedMIME: seed.Media.CachedMIME, URL: seed.Media.URL, Description: seed.Media.Description, DurationMS: seed.Media.DurationMS, Key: "adopt-" + seed.JobID, Trace: seed.Trace, Input: seed.Input, SourceID: seed.SourceID, AssetID: seed.Media.AssetID, Kind: seed.Media.Kind, Title: seed.Media.Title, ID: seed.JobID, State: "queued", Created: time.Now().UTC()}
		if seed.Key != "" {
			d.CrossTask = seed.OriginTask
			d.CrossRequester = seed.Requester
			d.CrossOrigin = seed.OriginThread
			d.CrossSource = seed.SourceThread
			d.Key = seed.Key
			d.State = "prepared"
		}
		w.runtime.Media = append(w.runtime.Media, d)
		w.persistErr = w.saveRuntime()
		if w.persistErr != nil {
			return
		}
	}
}
func (w *Worker) reconcileMedia(ctx context.Context) {
	n, ok := w.sender.(mediaNative)
	if !ok || w.runtime == nil {
		return
	}
	defer func() {
		for _, d := range w.runtime.Media {
			w.refreshMediaParent(d.Trace)
			if d.State == "accepted" && !d.CrossSettled && d.CrossRequester != "" && w.config.MediaSeedSettled != nil {
				if e := w.config.MediaSeedSettled(d.Trace, d.Key, d.Status); e != nil {
					w.activity(Record{Event: "cross_media_receipt_save_failed", Category: "error", ThreadID: d.Input.Thread, MessageID: d.Trace, Reason: "durable_receipt_pending"})
				} else {
					d.CrossSettled = true
					w.persistErr = w.saveRuntime()
				}
			}
		}
	}()
	for index, d := range w.runtime.Media {
		if ctx.Err() != nil || w.persistErr != nil {
			return
		}
		// The receipt journal may have committed just before a crash while this
		// worker checkpoint still says prepared/unknown. Restore the fact before
		// considering a new native lookup, cancellation or send.
		if d.CrossRequester != "" && d.State != "accepted" && w.config.MediaSeedReceipt != nil {
			if receipt, known := w.config.MediaSeedReceipt(d.Trace, d.Key); known && receipt.State == "accepted" && receipt.ServerID != "" && receipt.ThreadID == d.Input.Thread && (receipt.SourceID == "" || receipt.SourceID == d.SourceID) {
				d.State, d.Status, d.Phase = "accepted", receipt, receipt.Phase
				if receipt.ID != "" {
					d.ID = receipt.ID
				}
				if d.CompletedAt.IsZero() {
					d.CompletedAt = time.Now().UTC()
				}
				d.Reason = "cross_media_receipt_restored"
				w.mediaRecord(d, "cross_media_receipt_restored", "decision")
				if w.persistErr != nil {
					return
				}
			}
		}
		if d.Paused && !d.terminal() && !time.Now().Before(d.Next) && d.ID != "" && d.Status.State != "cancelled" && d.Status.State != "failed" {
			// Persisted pauses must also retry a lost native cancel response.
			// Cancellation never resubmits a part, including after process restart.
			w.cancelMedia(n, d, false, "media_paused_for_conversation")
			if d.State == "stopped" {
				d.State = "waiting"
			}
			d.Next = time.Now().Add(5 * time.Second)
			w.persistErr = w.saveRuntime()
		}
		if d.Paused || d.terminal() || time.Now().Before(d.Next) {
			continue
		}
		if d.ResumedFrom != "" && d.ID == "" && !d.UserCancelled {
			if d.State == "resume_prepared" {
				if resumeNative, ok := w.sender.(resumeMissingMediaNative); ok {
					w.submitMissingMedia(ctx, resumeNative, d)
				}
			} else {
				// An uncertain continuation is read by its NEW key. It must never
				// fall through to Submit, which would send the full original source.
				w.checkMediaReceipt(ctx, n, d)
			}
			continue
		}
		// A user cancellation is a one-way delivery fence.  In particular, a
		// lost native response may leave ID empty while State is submitting or
		// cancelling; lookup is allowed solely to settle/cancel the original
		// idempotency key, never to submit it again.
		if d.UserCancelled && d.ID == "" {
			lookupCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			status, err := n.LookupSend(lookupCtx, d.Key)
			cancel()
			if err != nil {
				if errors.Is(err, linuxnative.ErrToolNotFound) && !d.DeferredSource {
					d.State, d.Reason = "stopped", "media_cancelled_before_native_submission"
					d.Next = time.Time{}
					w.mediaRecord(d, "media_stopped", "decision")
					w.refreshMediaParent(d.Trace)
				} else {
					d.State, d.Reason = "cancelling", "media_cancel_lookup_pending"
					d.Checks++
					d.Next = time.Now().Add(w.mediaCancelDelay(d))
					w.mediaRecord(d, "media_cancel_lookup_pending", "decision")
				}
				continue
			}
			if !d.matchesNative(status) {
				d.State, d.Reason = "cancelling", "media_cancel_identity_pending"
				d.Checks++
				d.Next = time.Now().Add(w.mediaCancelDelay(d))
				w.mediaRecord(d, "media_cancel_lookup_pending", "error")
				continue
			}
			d.ID, d.Status, d.Phase = status.ID, status.WithVoiceReceipts(), status.Phase
			if d.Status.State == "accepted" && d.Status.ServerID != "" {
				d.State, d.Reason, d.CompletedAt = "accepted", "media_server_accepted_before_cancel", time.Now().UTC()
				w.mediaRecord(d, "message_sent", "sent")
				w.refreshMediaParent(d.Trace)
				continue
			}
			if d.Status.State == "queued" || d.Status.State == "running" || d.Status.State == "submitting" {
				w.cancelMedia(n, d, true, "media_cancel_requested_by_user")
				continue
			}
			if d.Status.State == "unknown" || d.Status.State == "needs_review" || d.unconfirmedVoiceParts() {
				d.State, d.Reason = "cancelling", "media_cancel_receipt_pending"
				d.Checks++
				d.Next = time.Now().Add(deliveryBackoff(w.mediaRetryDelay(), d.Checks))
				w.mediaRecord(d, "media_cancel_receipt_pending", "decision")
				continue
			}
			d.State, d.Reason = "stopped", "media_cancelled_by_user"
			w.mediaRecord(d, "media_stopped", "decision")
			w.refreshMediaParent(d.Trace)
			continue
		}
		cancelledTask := !w.taskActive(d.Input)
		if cancelledTask && !d.UserCancelled {
			w.cancelMedia(n, d, true, "task_cancelled_before_delivery")
			continue
		}
		if cancelledTask && d.ID == "" {
			d.State = "stopped"
			w.mediaRecord(d, "media_stopped", "decision")
			continue
		}
		if (d.State == "checking_receipt" || d.State == "submitting" && !w.retryUncertainMedia(d)) && !d.UserCancelled {
			w.checkMediaReceipt(ctx, n, d)
			continue
		}
		if w.draining() && d.State == "prepared" && d.ID == "" {
			continue
		}
		// Serialize unsent media per conversation. Persist the sampled gap once.
		if d.ID == "" && d.State == "prepared" {
			if d.CrossRequester != "" {
				blocked := false
				for _, part := range w.runtime.Media[:index] {
					if part.Trace == d.Trace && part.CrossRequester != "" && part.State != "accepted" {
						blocked = true
						break
					}
				}
				if blocked {
					continue
				}
			}
			previous, blocked := w.mediaPredecessor(index, d)
			if blocked {
				continue
			}
			if previous != nil {
				if d.CrossRequester != "" && previous.Trace == d.Trace && previous.State != "accepted" {
					continue
				}
				if !previous.terminal() {
					continue
				}
				if d.NotBefore.IsZero() {
					ended := previous.CompletedAt
					if ended.IsZero() {
						ended = previous.Created
					}
					var engine *companion.Engine
					engine, _ = w.model.(*companion.Engine)
					d.NotBefore = ended.Add(w.replyDelay(engine))
					w.persistErr = w.saveRuntime()
					if w.persistErr != nil {
						return
					}
				}
				if time.Now().Before(d.NotBefore) {
					continue
				}
			}
		}
		checkCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		var status linuxnative.ToolStatus
		var err error
		if d.ID != "" {
			status, err = n.ToolSendStatus(checkCtx, d.ID)
		} else {
			status, err = n.LookupSend(checkCtx, d.Key)
			if errors.Is(err, linuxnative.ErrToolNotFound) {
				if w.beforeSubmit(checkCtx) != nil {
					cancel()
					return
				}
				if !w.mediaAllowed(d) {
					cancel()
					w.cancelMedia(n, d, false, "media_permission_or_quiet_changed")
					continue
				}
				if reason := d.voiceMismatch(); reason != "" {
					cancel()
					d.State, d.Reason = "needs_review", reason
					w.mediaRecord(d, "media_stopped", "error")
					continue
				}
				proofErr := w.validateMediaEvidence(checkCtx, d)
				if w.beforeSubmit(checkCtx) != nil {
					cancel()
					return
				}
				if proofErr != nil {
					cancel()
					d.State, d.Reason = "needs_review", "media_source_proof_invalid"
					w.mediaRecord(d, "media_stopped", "error")
					continue
				}
				if gate := w.sendGate(d.Input.Thread); time.Now().Before(gate) {
					d.Next = gate
					cancel()
					w.persistErr = w.saveRuntime()
					continue
				}
				if d.DeferredSource && d.SourceID == "" {
					cancel()
					if !w.stageMediaSource(ctx, d) {
						continue
					}
					checkCtx, cancel = context.WithTimeout(ctx, 8*time.Second)
				}
				// Persist before sending; a lost HTTP response is recovered using this
				// same idempotency key, never by creating a different send request.
				d.State = "submitting"
				w.advanceSendGate(d.Input.Thread)
				w.persistErr = w.saveRuntime()
				if w.persistErr == nil {
					if d.DeferredSource && w.beforeSubmit(checkCtx) != nil {
						d.State = "prepared" // The native call was never entered.
						w.persistErr = w.saveRuntime()
						cancel()
						return
					}
					status, err = w.submitMedia(checkCtx, n, d)
					if errors.Is(err, errMediaAuthorityChanged) {
						d.State, d.Reason = "stopped", "media_source_or_authorization_changed"
						w.mediaRecord(d, "media_stopped", "error")
						w.refreshMediaParent(d.Trace)
						cancel()
						continue
					}
					if errors.Is(err, ErrDeliveryCancelled) {
						d.State = "prepared" // Gate proves the native call was not entered.
						w.cancelMedia(n, d, true, "media_cancelled_before_native_submission")
						cancel()
						continue
					}
				}
			}
		}
		cancel()
		d.Next = time.Now().Add(2 * time.Second)
		if err != nil {
			if !w.retryUncertainMedia(d) && (d.ID != "" || d.State == "submitting") && !d.UserCancelled {
				w.awaitMediaReceipt(d)
				continue
			} else {
				d.Reason = "media_receipt_lookup_pending"
				d.Checks++
				d.Next = time.Now().Add(w.mediaCancelDelay(d))
			}
			w.mediaRecord(d, "media_waiting", "decision")
			continue
		}
		if !d.matchesNative(status) {
			d.State = "needs_review"
			d.Reason = "media_identity_mismatch"
			w.mediaRecord(d, "media_stopped", "error")
			continue
		}
		d.ID = status.ID
		d.Status = status
		d.Phase = status.Phase
		status = status.WithVoiceReceipts()
		d.Status = status
		if status.State == "accepted" && status.ServerID != "" {
			d.State = "accepted"
			if d.CompletedAt.IsZero() {
				d.CompletedAt = time.Now().UTC()
				w.advanceSendGate(d.Input.Thread)
			}
			d.Reason = "media_server_accepted"
			w.mediaRecord(d, "message_sent", "sent")
			w.refreshMediaParent(d.Trace)
			continue
		}
		if status.State == "accepted" {
			if !w.retryUncertainMedia(d) && !d.UserCancelled {
				w.awaitMediaReceipt(d)
				continue
			}
			d.State = "unknown"
			d.Reason = "media_receipt_id_missing"
			w.mediaRecord(d, "media_waiting", "decision")
			continue
		}
		if !cancelledTask && !w.mediaAllowed(d) {
			w.cancelMedia(n, d, false, "media_permission_or_quiet_changed")
			continue
		}
		if d.UserCancelled {
			if status.State == "queued" || status.State == "running" || status.State == "submitting" {
				w.cancelMedia(n, d, true, "media_cancel_requested_by_user")
			} else if status.State == "unknown" || status.State == "needs_review" || d.unconfirmedVoiceParts() {
				d.State, d.Reason = "cancelling", "media_cancel_receipt_pending"
				d.Checks++
				d.Next = time.Now().Add(deliveryBackoff(w.mediaRetryDelay(), d.Checks))
				w.mediaRecord(d, "media_cancel_receipt_pending", "decision")
			} else {
				d.State = "stopped"
				d.Reason = "media_cancelled_by_user"
				w.mediaRecord(d, "media_stopped", "decision")
				w.refreshMediaParent(d.Trace)
			}
			continue
		}
		switch status.State {
		case "failed", "cancelled":
			d.Reason = status.Reason
			if status.Retryable && w.retryUncertainMedia(d) {
				if d.State != "retry_wait" {
					d.State = "retry_wait"
					d.Checks = 0
					d.Next = time.Now().Add(deliveryBackoff(w.config.Options.RetryDelaySeconds, d.Retries))
				} else {
					d.Checks++
					d.Next = time.Now().Add(15 * time.Second)
					if d.Checks >= 2 {
						if reason := d.voiceMismatch(); reason != "" {
							d.State, d.Reason = "needs_review", reason
							w.mediaRecord(d, "media_stopped", "error")
							continue
						}
						if proofErr := w.validateMediaEvidence(ctx, d); proofErr != nil {
							d.State, d.Reason = "needs_review", "media_source_proof_invalid"
							w.mediaRecord(d, "media_stopped", "error")
							continue
						}
						if w.beforeSubmit(ctx) != nil {
							return
						}
						d.Retries++
						d.State = "retry_submitting"
						w.persistErr = w.saveRuntime()
						if w.persistErr != nil {
							return
						}
						retryCtx, stop := context.WithTimeout(ctx, 8*time.Second)
						next, e := n.RetryMediaJob(retryCtx, d.ID)
						stop()
						d.State = "queued"
						d.Checks = 0
						d.Reason = "media_retry_same_job_pending"
						if e == nil {
							d.Status = next
							d.Phase = next.Phase
						}
					}
				}
				w.mediaRecord(d, "media_retry_waiting", "decision")
			} else {
				d.State = "stopped"
				if d.Reason == "" {
					d.Reason = "media_retry_not_permitted"
				}
				w.mediaRecord(d, "media_stopped", "error")
				w.refreshMediaParent(d.Trace)
			}
		case "needs_review", "unknown":
			if !w.retryUncertainMedia(d) {
				w.awaitMediaReceipt(d)
				continue
			}
			if d.Input.Surf {
				// Continue reading the old receipt, but let explicit user delivery
				// finish before resuming a proactive upload in the same thread.
				if _, blocked := w.mediaPredecessor(index, d); blocked {
					d.State = "waiting"
					d.Next = time.Now().Add(15 * time.Second)
					continue
				}
			}
			if time.Now().Before(d.ReconcileAfter) {
				d.State = "waiting"
				d.Next = time.Now().Add(15 * time.Second)
				continue
			}
			// Reconcile real same-thread messages, then resume the original plan.
			// Explicit retry policy accepts that a delayed sync may hide delivery.
			if retry, ok := w.sender.(interface {
				ReconcileMediaJob(context.Context, string) (linuxnative.ToolStatus, error)
			}); ok && w.retryUncertainMedia(d) && d.Checks > 0 {
				if proofErr := w.validateMediaEvidence(ctx, d); proofErr != nil {
					// An uncertain native result may still become accepted. Keep
					// polling its original receipt, but never resume an upload whose
					// source/permission proof has expired.
					d.State, d.Reason = "waiting", "media_source_proof_invalid"
					d.Next = time.Now().Add(15 * time.Second)
					w.mediaRecord(d, "media_waiting", "error")
					continue
				}
				if gate := w.sendGate(d.Input.Thread); time.Now().Before(gate) {
					d.Next = gate
					continue
				}
				if w.beforeSubmit(ctx) != nil {
					return
				}
				w.advanceSendGate(d.Input.Thread)
				retryCtx, stop := context.WithTimeout(ctx, 30*time.Second)
				next, e := retry.ReconcileMediaJob(retryCtx, d.ID)
				stop()
				d.Retries++
				if e == nil {
					d.ReconcileAfter = time.Time{}
					d.Status = next
					d.State = next.State
					d.Phase = next.Phase
					d.Reason = "media_reconciled_original_job_resumed"
					d.Next = time.Now().Add(2 * time.Second)
					w.mediaRecord(d, "media_retry_waiting", "decision")
					continue
				}
				d.Reason = "media_reconciliation_failed_" + status.Reason
				if !d.RepairReported && w.config.MediaRepairRequired != nil {
					input := d.Input
					input.TraceID = d.Trace
					if reportErr := w.config.MediaRepairRequired(input, d.ID, status.Reason); reportErr != nil {
						w.persistErr = reportErr
						return
					}
					d.RepairReported = true
				}
				d.State = "waiting"
				d.Checks++
				d.ReconcileAfter = time.Now().Add(deliveryBackoff(w.config.Options.RetryDelaySeconds, d.Checks))
				d.Next = time.Now().Add(15 * time.Second)
				w.mediaRecord(d, "media_reconciliation_failed", "error")
				continue
			}
			d.State = "waiting"
			d.Checks++
			d.Reason = "media_receipt_reconciliation_pending"
			d.ReconcileAfter = time.Now().Add(deliveryBackoff(w.config.Options.RetryDelaySeconds, d.Checks))
			d.Next = time.Now().Add(15 * time.Second)
			w.mediaRecord(d, "media_waiting", "error")
			w.refreshMediaParent(d.Trace)
		default:
			d.State = status.State
			d.Reason = "media_" + status.Phase
			w.mediaRecord(d, "media_sending", "decision")
		}
	}
	w.persistErr = w.saveRuntime()
}

type mediaPreparation struct {
	key     string
	cancel  context.CancelFunc
	result  chan string
	discard bool
}

func (w *Worker) stopMediaPreparation(key string) {
	if p := w.mediaPreparation; p != nil && (key == "" || p.key == key) {
		p.discard = true
		p.cancel()
	}
}

// Upload runs outside the event loop so an unrelated large file cannot delay
// an inbound stop for a voice job. Keep at most one native staging upload per
// worker; native staging may buffer the input before it becomes a source.
func (w *Worker) stageMediaSource(ctx context.Context, d *mediaDelivery) bool {
	if w.config.PrepareMediaSource == nil {
		d.State, d.Reason = "stopped", "media_source_preparation_unavailable"
		w.mediaRecord(d, "media_stopped", "error")
		return false
	}
	if p := w.mediaPreparation; p != nil {
		if p.key != d.Key && !p.discard {
			return false
		}
		select {
		case source := <-p.result:
			w.mediaPreparation = nil
			if !p.discard {
				if ctx.Err() != nil || w.beforeSubmit(ctx) != nil {
					return false
				}
				if source == "" {
					d.State, d.Reason = "stopped", "media_verified_upload_failed"
					w.mediaRecord(d, "media_stopped", "error")
					return false
				}
				d.SourceID = source
				w.persistErr = w.saveRuntime()
				return w.persistErr == nil
			}
		default:
			return false
		}
	}
	prepareCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	p := &mediaPreparation{key: d.Key, cancel: cancel, result: make(chan string, 1)}
	w.mediaPreparation = p
	in := d.Input
	in.TraceID = d.Trace
	prepare, kind, asset := w.config.PrepareMediaSource, d.Kind, d.AssetID
	go func() {
		defer cancel()
		source, err := prepare(prepareCtx, in, kind, asset)
		if err != nil || prepareCtx.Err() != nil {
			source = ""
		}
		p.result <- source
	}()
	return false
}
func (w *Worker) mediaRecord(d *mediaDelivery, event, category string) {
	memory := companion.MediaMemory{VoiceProfile: d.VoiceProfile, VoiceID: d.VoiceID, VoiceEngine: d.VoiceEngine, SynthesisText: d.SynthesisText, SynthesisTextKnown: d.SynthesisTextKnown, ParentAssetID: d.ParentAssetID, CachedMIME: d.CachedMIME, URL: d.URL, Key: d.Key, Kind: d.Kind, Title: d.Title, SourceID: d.SourceID, AssetID: d.AssetID, JobID: d.ID, State: d.State, Phase: d.Phase, Reason: d.Reason, Retries: d.Retries, TotalParts: len(d.Status.VoiceChunks)}
	for _, p := range d.Status.VoiceChunks {
		memory.DurationMS += p.DurationMS
		if p.State == "accepted" && p.HasReceipt {
			memory.AcceptedParts++
		}
	}
	if len(d.Status.VoiceChunks) == 0 {
		memory.DurationMS = d.DurationMS
	}
	sig := fmt.Sprintf("%s|%s|%s|%t|%s|%d|%d|%d|%d", d.State+"|"+d.VoiceID+"|"+d.VoiceEngine+"|"+d.VoiceProfile, d.Phase, d.Reason, memory.SynthesisTextKnown, memory.SynthesisText, d.Retries, memory.AcceptedParts, memory.TotalParts, memory.DurationMS)
	if sig == d.Signature {
		return
	}
	d.Signature = sig
	if c, ok := w.model.(*companion.Engine); ok {
		input := d.Input
		input.TraceID = d.Trace
		if err := c.Store.ObserveMedia(input, c.Config, memory, time.Now().UTC()); err != nil {
			w.persistErr = err
			return
		}
	}
	content := "[" + d.Kind + "] " + d.Title
	if memory.TotalParts > 0 {
		content += fmt.Sprintf(" · 已接受 %d/%d 段", memory.AcceptedParts, memory.TotalParts)
	}
	w.activity(Record{RelatedMessageID: d.Input.OriginTraceID, Event: event, Category: category, ThreadID: d.Input.Thread, MessageID: d.Trace, Content: content, SendID: d.ID, Status: d.State, Reason: d.Reason, Origin: "bot"})
	if !d.terminal() {
		w.markSending(d.Trace)
	}
	w.persistErr = w.saveRuntime()
}
func (w *Worker) refreshMediaParent(trace string) {
	pending, review, accepted := false, false, false
	for _, p := range w.runtime.Plans {
		if p.Input.TraceID == trace {
			pending = pending || !p.terminal()
			accepted = accepted || len(p.Sent) > 0
		}
	}
	for _, r := range w.runtime.MediaReplies {
		if r.Trace == trace {
			pending = pending || !r.terminal()
		}
	}
	for _, d := range w.runtime.Deliveries {
		if d.Trace == trace && d.Bubble >= 0 {
			pending = pending || d.State != "accepted" && d.State != "stopped"
			accepted = accepted || d.State == "accepted"
		}
	}
	for _, d := range w.runtime.Media {
		if d.Trace == trace {
			pending = pending || !d.terminal()
			review = review || d.State == "needs_review"
			accepted = accepted || d.State == "accepted"
		}
	}
	phase := "stopped"
	if accepted {
		phase = "replied"
	}
	if review {
		phase = "needs_review"
	}
	if pending {
		phase = "sending"
	}
	w.queuePhase(trace, phase)
	w.publishTaskDelivery(trace)
}
func (w *Worker) cancelMedia(n mediaNative, d *mediaDelivery, user bool, reason string) {
	w.stopMediaPreparation(d.Key)
	wasCancelled := d.UserCancelled
	d.UserCancelled = d.UserCancelled || user
	if user && !wasCancelled {
		d.StateVersion++
	}
	if user && d.ID == "" {
		// "submitting" is the crash window after the durable intent was
		// written but before the native job id reached us.  Do not assume it
		// was never submitted: discover the original idempotency key and stop
		// that native job before marking this delivery terminal.
		if d.State == "submitting" || d.State == "resume_submitting" || d.State == "queued" || d.State == "running" || d.State == "waiting" || d.State == "checking_receipt" {
			lookupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			status, err := n.LookupSend(lookupCtx, d.Key)
			cancel()
			if err == nil && d.matchesNative(status) {
				d.ID, d.Status, d.Phase = status.ID, status, status.Phase
			} else if d.DeferredSource || err != nil && !errors.Is(err, linuxnative.ErrToolNotFound) {
				d.State, d.Reason, d.Next = "cancelling", reason, time.Now().Add(time.Second)
				w.persistErr = w.saveRuntime()
				if w.persistErr == nil {
					w.mediaRecord(d, "media_cancel_lookup_pending", "decision")
				}
				return
			}
		}
		if d.ID == "" {
			d.State, d.Reason = "stopped", reason
			w.persistErr = w.saveRuntime()
			if w.persistErr == nil {
				w.mediaRecord(d, "media_stopped", "decision")
				w.refreshMediaParent(d.Trace)
			}
			return
		}
	}
	// Persist cancellation intent before the API call so restart never retries it.
	if user {
		d.Reason = reason
		w.persistErr = w.saveRuntime()
		if w.persistErr == nil {
			w.mediaRecord(d, "media_cancel_requested", "decision")
		}
		if w.persistErr != nil {
			return
		}
	}
	if d.ID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := n.CancelMediaJob(ctx, d.ID)
		cancel()
		if err == nil && (!d.DeferredSource || d.matchesNative(s)) {
			d.Status = s
			d.Phase = s.Phase
		}
	}
	d.Reason = reason
	if user || d.ID != "" && d.Status.State != "cancelled" && d.Status.State != "failed" && d.Status.State != "accepted" {
		d.State = "cancelling"
		d.Next = time.Now().Add(time.Second)
	} else {
		d.State = "stopped"
	}
	w.mediaRecord(d, "media_cancel_requested", "decision")
	w.refreshMediaParent(d.Trace)
}

// voiceInterrupt describes a direct, local delivery instruction.  It must be
// intentionally conservative: a sentence which merely mentions a song must
// never stop a different member's delivery.
type voiceInterrupt struct {
	stop    bool
	replace bool
}

func parseVoiceInterrupt(text string) voiceInterrupt {
	text = strings.ToLower(strings.NewReplacer(" ", "", "　", "", "，", "", "。", "", "！", "", "？", "").Replace(text))
	if text == "" {
		return voiceInterrupt{}
	}
	for _, phrase := range []string{"不要换", "不用换", "别换", "不换了", "不用重新找", "不要重新找"} {
		if strings.Contains(text, phrase) {
			return voiceInterrupt{}
		}
	}
	// These are complete-meaning guards.  In particular, “不是这首也没关
	// 系，继续发” is not a cancellation merely because it contains 不是这首.
	for _, phrase := range []string{"继续发", "继续这首", "别停", "不要停", "不用停", "不用取消", "别取消", "不是这首也没关系", "不是这首没关系"} {
		if strings.Contains(text, phrase) {
			return voiceInterrupt{}
		}
	}
	for _, phrase := range []string{"重新找一首", "重新找首", "换成", "换一首", "换首歌", "换首", "换歌", "换个歌", "下一首"} {
		if strings.Contains(text, phrase) {
			return voiceInterrupt{stop: true, replace: true}
		}
	}
	for _, phrase := range []string{"停", "停一下", "停下", "先停", "先停一下", "先停下"} {
		if text == phrase {
			return voiceInterrupt{stop: true}
		}
	}
	for _, phrase := range []string{"不用发语音", "不用语音", "别发语音", "不要发语音", "停止语音", "停下语音", "后面别发", "后面的别发", "发错了", "发错歌", "不是这首", "别放这首", "改发文件", "换成文件"} {
		if strings.Contains(text, phrase) {
			return voiceInterrupt{stop: true}
		}
	}
	return voiceInterrupt{}
}

func mediaQuoteMatches(m linuxnative.Message, d *mediaDelivery) bool {
	if m.ResolvedQuote != nil {
		q := m.ResolvedQuote
		if q.ID != "" && (q.ID == d.Trace || q.ID == d.Input.TraceID || q.ID == d.Input.OriginTraceID) {
			return true
		}
		if q.ServerID != "" && d.Input.QuoteServerID == q.ServerID {
			return true
		}
	}
	serverID := ""
	if m.Quote != nil {
		serverID = m.Quote.ServerID
	}
	if serverID == "" {
		return false
	}
	if serverID == d.Input.QuoteServerID || serverID == d.Status.ServerID {
		return true
	}
	for _, chunk := range d.Status.VoiceChunks {
		if chunk.ServerID == serverID {
			return true
		}
	}
	return false
}

// pendingMediaFollowup is the text/caption tail belonging to an otherwise
// accepted voice delivery.  A replacement still needs to identify this trace:
// the audio cannot be unsent, but its late caption must not leak out.
func (w *Worker) pendingMediaFollowup(trace, thread string) bool {
	for _, r := range w.runtime.MediaReplies {
		if r.Trace == trace && r.Input.Thread == thread && !r.terminal() {
			return true
		}
	}
	for _, p := range w.runtime.Plans {
		if p.MediaCaption && p.Input.TraceID == trace && p.Input.Thread == thread && !p.terminal() {
			return true
		}
	}
	return false
}

// voiceCancellationCandidates returns delivery *turns*, rather than raw
// chunks.  Cancelling by trace is what prevents a later plugin callback or
// queued caption from reviving the rest of the same request.
func (w *Worker) voiceCancellationCandidates(m linuxnative.Message) map[string][]*mediaDelivery {
	byTrace := map[string][]*mediaDelivery{}
	quoted := m.ResolvedQuote != nil || m.Quote != nil
	for _, d := range w.runtime.Media {
		if d.Kind != "voice" || d.Input.Thread != m.ThreadID || d.UserCancelled {
			continue
		}
		if d.terminal() && !w.pendingMediaFollowup(d.Trace, d.Input.Thread) {
			continue
		}
		if quoted {
			if !mediaQuoteMatches(m, d) {
				continue
			}
		} else if m.SenderID == "" && !strings.HasSuffix(m.ThreadID, "@chatroom") {
			// Older direct-message records did not always retain sender IDs.
			// They are still safe if there is only one candidate turn below.
		} else if m.SenderID == "" || d.Input.Sender == "" || d.Input.Sender != m.SenderID {
			// Old records without a requester remain manageable through an
			// explicit queue action, but never by an ambiguous natural phrase.
			continue
		}
		byTrace[d.Trace] = append(byTrace[d.Trace], d)
	}
	return byTrace
}

func (w *Worker) stopVoiceTrace(n mediaNative, m linuxnative.Message, trace, reason string) {
	// A model-requested check can outlive the immediate send even without
	// a persistent task. Cancel it with the same authorized delivery scope.
	for _, wake := range w.runtime.Wakes {
		if wake != nil && wake.State == "waiting" && wake.Parent == trace && wake.Input.Thread == m.ThreadID && wake.Input.Sender == m.SenderID {
			wake.State = "stopped"
		}
	}
	for _, d := range w.runtime.Media {
		if d.Trace != trace || d.Input.Thread != m.ThreadID || d.Kind != "voice" || d.terminal() {
			continue
		}
		d.CancelTrace, d.CancelSender, d.CancelledAt = m.ID, m.SenderID, time.Now().UTC()
		w.cancelMedia(n, d, true, reason)
	}
	for _, r := range w.runtime.MediaReplies {
		if r.Trace == trace && r.Input.Thread == m.ThreadID && !r.terminal() {
			r.State, r.Paused = "stopped", false
			w.activity(Record{Event: "media_reply_stopped", Category: "decision", ThreadID: m.ThreadID, MessageID: trace, Reason: reason, Status: "stopped"})
		}
	}
	for _, p := range w.runtime.Plans {
		if p.MediaCaption && p.Input.TraceID == trace && p.Input.Thread == m.ThreadID && !p.terminal() {
			p.State, p.Paused, p.StopReason = "stopped", false, reason
		}
	}
	w.refreshMediaParent(trace)
}

func (w *Worker) pauseAmbiguousVoiceTraces(n mediaNative, m linuxnative.Message, candidates map[string][]*mediaDelivery) {
	for trace, deliveries := range candidates {
		for _, d := range deliveries {
			d.Paused, d.Reason = true, "media_stop_ambiguous"
			if d.ID != "" {
				w.cancelMedia(n, d, false, "media_stop_ambiguous")
				if d.State == "stopped" {
					d.State = "waiting"
				}
			}
		}
		w.activity(Record{Event: "media_stop_ambiguous", Category: "decision", ThreadID: m.ThreadID, MessageID: m.ID, RelatedMessageID: trace, Status: "waiting", Reason: "multiple_requester_voice_tasks"})
	}
	// An ambiguous interruption must not guess.  Queue exactly one brief,
	// non-model clarification under the interruption message id; deduplication
	// in queueUtteranceFrom makes repeated delivery of the same incoming event
	// harmless.
	if c, ok := w.model.(*companion.Engine); ok && c.Config.Permissions.ReplyText {
		in := companion.Input{Channel: w.config.Channel, Account: w.source.Account(), Thread: m.ThreadID, Sender: m.SenderID, TraceID: m.ID, QuoteServerID: m.ServerID, Text: m.Body, Mode: "direct", IsGroup: strings.HasSuffix(m.ThreadID, "@chatroom"), Directed: true, MessageTime: m.CreatedAt}
		w.queueUtteranceFrom(c, in, companion.Decision{Action: "reply", Messages: []string{"我先暂停了你正在发送的内容。你指的是哪一项？"}}, 0, false, nil)
	}
}
func (w *Worker) stopRequestedVoice(ctx context.Context, m linuxnative.Message) bool {
	if !w.messageInTime(m, time.Now()) {
		return false
	}
	if w.runtime == nil || m.Type != 1 || !w.allowed[m.ThreadID] {
		return false
	}
	interrupt := parseVoiceInterrupt(m.Body)
	if !interrupt.stop {
		return false
	}
	n, ok := w.sender.(mediaNative)
	if !ok {
		return false
	}
	candidates := w.voiceCancellationCandidates(m)
	if len(candidates) == 0 {
		return false
	}
	// A quoted segment is an explicit target.  Without a quote, a member may
	// stop only one of their own active delivery turns.  This permits the
	// natural group “发错了” path without making it possible to stop someone
	// else's song or to guess among the sender's concurrent requests.
	if len(candidates) != 1 {
		w.pauseAmbiguousVoiceTraces(n, m, candidates)
		w.persistErr = w.saveRuntime()
		return true
	}
	for trace := range candidates {
		reason := "media_cancel_requested_by_user"
		if interrupt.replace {
			reason = "media_superseded_by_new_request"
		}
		w.stopVoiceTrace(n, m, trace, reason)
	}
	w.persistErr = w.saveRuntime()
	return w.persistErr == nil
}
func replacesVoice(text string) bool {
	return parseVoiceInterrupt(text).replace
}

func (w *Worker) cancelVoiceForIntent(ctx context.Context, in companion.Input, intent string) {
	if w.runtime == nil || !w.allowed[in.Thread] {
		return
	}
	n, ok := w.sender.(mediaNative)
	if !ok {
		return
	}
	for _, d := range w.runtime.Media {
		legacyDirect := !strings.HasSuffix(in.Thread, "@chatroom") && d.Input.Sender == ""
		if d.Kind == "voice" && d.Input.Thread == in.Thread && !d.terminal() && d.Trace != in.TraceID && (legacyDirect || d.Input.Sender == in.Sender) {
			w.cancelMedia(n, d, true, "media_superseded_by_new_request")
		}
	}
	for _, r := range w.runtime.MediaReplies {
		voice := false
		for _, d := range w.runtime.Media {
			voice = voice || d.Trace == r.Trace && d.Kind == "voice"
		}
		legacyDirect := !strings.HasSuffix(in.Thread, "@chatroom") && r.Input.Sender == ""
		if voice && r.Input.Thread == in.Thread && r.Trace != in.TraceID && !r.terminal() && (legacyDirect || r.Input.Sender == in.Sender) {
			r.State = "stopped"
			w.activity(Record{Event: "media_reply_stopped", Category: "decision", ThreadID: in.Thread, MessageID: r.Trace, Reason: "media_superseded_by_new_request", Status: "stopped"})
			w.refreshMediaParent(r.Trace)
		}
	}
	w.persistErr = w.saveRuntime()
}
func rejectVoice(text string) bool {
	return parseVoiceInterrupt(text).stop
}
func (w *Worker) suspendMedia() {
	w.stopMediaPreparation("")
	if w.runtime == nil || w.draining() && !w.pendingMedia() {
		return
	}
	n, ok := w.sender.(mediaNative)
	if !ok {
		return
	}
	// Leave recoverable state for the next worker; operator pause stops native
	// pacing as well, rather than allowing autonomous sends after the pause.
	deadline := time.Now().Add(10 * time.Second)
	for _, d := range w.runtime.Media {
		if time.Now().After(deadline) {
			break
		}
		if d.DeferredSource && !d.terminal() && (d.ID != "" || d.State == "checking_receipt" || d.State == "submitting" || d.State == "cancelling") {
			w.cancelMedia(n, d, true, "media_worker_suspended")
			continue
		}
		if d.State == "checking_receipt" && (d.Status.State == "needs_review" || d.Status.State == "unknown") {
			continue // No active native upload; preserve the scheduled receipt query.
		}
		if !d.terminal() && d.ID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			status, err := n.CancelMediaJob(ctx, d.ID)
			cancel()
			if err == nil {
				d.Status = status
			}
			d.Next = time.Now().Add(time.Duration(w.config.Options.RetryDelaySeconds) * time.Second)
			d.Reason = "media_worker_suspended"
			w.mediaRecord(d, "media_worker_suspended", "decision")
		}
	}
	w.persistErr = w.saveRuntime()
}
