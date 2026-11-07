package bot

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/sending"
)

type reliableNative interface {
	Submit(context.Context, linuxnative.ToolSend) (linuxnative.ToolStatus, error)
	LookupSend(context.Context, string) (linuxnative.ToolStatus, error)
	ToolSendStatus(context.Context, string) (linuxnative.ToolStatus, error)
	Catalog(context.Context, string, string, string, string) (linuxnative.CatalogPage, error)
}
type delivery struct {
	ReviewEvidence *companion.GenerationEvidence `json:"review_evidence,omitempty"`
	Trace          string                        `json:"trace"`
	Thread         string                        `json:"thread"`
	Text           string                        `json:"text"`
	Bubble         int                           `json:"bubble"`
	Key            string                        `json:"key"`
	ID             string                        `json:"id"`
	ServerID       string                        `json:"server_id,omitempty"`
	// AcceptedAt is populated only from an outgoing message record observed in
	// the native catalogue. Tool submission time is not a delivery receipt.
	AcceptedAt      string                 `json:"accepted_at,omitempty"`
	State           string                 `json:"state"`
	Attempts        int                    `json:"attempts"`
	Checks          int                    `json:"checks"`
	Created         time.Time              `json:"created"`
	Next            time.Time              `json:"next"`
	Quote           *linuxnative.ToolQuote `json:"quote,omitempty"`
	Input           companion.Input        `json:"input"`
	Reason          string                 `json:"reason"`
	PreviousKeys    []string               `json:"previous_keys,omitempty"`
	UncertainRounds int                    `json:"uncertain_rounds,omitempty"`
	PreviousTexts   []string               `json:"previous_texts,omitempty"`
	SubmittedText   string                 `json:"submitted_text,omitempty"`
	// Cancelled means the originating task was withdrawn after this delivery
	// might already have reached WeChat.  It deliberately keeps the record in
	// reconciliation mode: we may read the old idempotency key, but must never
	// create a replacement text intent.
	Cancelled bool `json:"cancelled,omitempty"`
}

func (w *Worker) markSending(trace string) { w.queuePhase(trace, "sending") }

// deliveryMemoryKey uses only an identity returned by the native sender. It
// deliberately does not fall back to a request trace, text hash or plan slot:
// one request may yield multiple equal text bubbles, while a recovered native
// receipt must map to exactly one remembered bubble.
func deliveryMemoryKey(d *delivery) string {
	if d == nil {
		return ""
	}
	if d.ID != "" {
		return "native:" + d.ID
	}
	if d.ServerID != "" {
		return "server:" + d.ServerID
	}
	return ""
}

// legacyReceiptMemoryKey is the exact deterministic identity produced by the
// previous receipt-recovery implementation. It is available only while the
// current durable delivery still proves all of its inputs; it must never be
// substituted with a plan slot, request trace, or text-derived value.
func legacyReceiptMemoryKey(d *delivery) string {
	if d == nil || d.ID == "" || d.Key == "" || d.Created.IsZero() {
		return ""
	}
	return "delivery-" + messageHash(d.Key+"|"+d.ID+"|"+d.Created.UTC().Format(time.RFC3339Nano))
}

// acceptTextMemory writes the actual accepted outgoing bubble. Native
// deliveries use their receipt identity so restart/reconciliation is
// idempotent; non-native transports retain the legacy append-only behavior
// because no reliable receipt identity exists to deduplicate against.
func deliveryAcceptedTime(d *delivery, fallback time.Time) time.Time {
	if d != nil && d.AcceptedAt != "" {
		if at, err := time.Parse(time.RFC3339Nano, d.AcceptedAt); err == nil {
			return at
		}
		if at, err := time.Parse(time.RFC3339, d.AcceptedAt); err == nil {
			return at
		}
	}
	return fallback
}

func (w *Worker) acceptTextMemory(c *companion.Engine, input companion.Input, bubble int, text string) error {
	return w.acceptTextMemoryAt(c, input, bubble, text, time.Now())
}

func (w *Worker) acceptTextMemoryAt(c *companion.Engine, input companion.Input, bubble int, text string, fallback time.Time) error {
	if w.runtime != nil {
		for _, d := range w.runtime.Deliveries {
			if d.Thread != input.Thread || d.Trace != input.TraceID || d.Bubble != bubble || d.State != "accepted" {
				continue
			}
			if d.Text != text {
				return companion.ErrDeliveryMemoryConflict
			}
			if key := deliveryMemoryKey(d); key != "" {
				return c.Store.AcceptedDeliveryWithLegacyReceipt(input, text, key, legacyReceiptMemoryKey(d), c.Config, deliveryAcceptedTime(d, fallback))
			}
			break
		}
	}
	return c.Store.Accepted(input, text, c.Config, fallback)
}

func (w *Worker) sendText(ctx context.Context, input companion.Input, bubble int, text string) (sending.Receipt, error) {
	if w.surfOrigin(input) && w.config.SurfAllowed != nil && !w.config.SurfAllowed(input.Thread) {
		return sending.Receipt{}, errors.New("surf_share_permission_changed")
	}
	native, ok := w.sender.(reliableNative)
	if w.beforeSubmit(ctx) != nil {
		return sending.Receipt{}, errSubmissionDeferred
	}
	if !ok || w.runtime == nil {
		var err error
		input, text, err = w.prepareGroundedText(ctx, input, text)
		if err != nil {
			return sending.Receipt{}, replyAdmissionFailure(err)
		}
		if err := w.beforeSubmit(ctx); err != nil {
			return sending.Receipt{}, err
		}
		return w.sender.SendText(ctx, sending.TextRequest{Recipient: input.Thread, Text: text})
	}
	if len(w.runtime.Deliveries) >= 3000 {
		w.pruneRuntime()
		if len(w.runtime.Deliveries) >= 3000 {
			return sending.Receipt{}, errors.New("delivery queue full")
		}
	}
	for _, old := range w.runtime.Deliveries {
		if old.Trace == input.TraceID && old.Thread == input.Thread && old.Bubble == bubble {
			if old.State == "accepted" {
				return sending.Receipt{Recipient: old.Thread}, nil
			}
			return sending.Receipt{}, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: "delivery_reconciliation_pending", IntentID: old.ID}
		}
	}
	var err error
	input, text, err = w.prepareGroundedText(ctx, input, text)
	if err != nil {
		if w.beforeSubmit(ctx) != nil {
			return sending.Receipt{}, errSubmissionDeferred
		}
		return sending.Receipt{}, replyAdmissionFailure(err)
	}
	d := &delivery{Trace: input.TraceID, Thread: input.Thread, Text: text, Bubble: bubble, Created: time.Now().UTC(), State: "prepared", Input: input}
	d.Key = "bot-" + messageHash(input.Thread + "|" + input.TraceID + fmt.Sprint(bubble))[:48]
	if (w.config.Options.QuoteReplies && input.Directed || input.QuoteSharedMessage || input.ReplyContext != nil && input.ReplyContext.QuoteRequired) && input.QuoteServerID != "" && input.TraceID != "" {
		d.Quote = replyQuote(input)
	}
	w.runtime.Deliveries = append(w.runtime.Deliveries, d)
	if err := w.saveRuntime(); err != nil {
		return sending.Receipt{}, err
	}
	w.markSending(input.TraceID)
	if w.persistErr != nil {
		return sending.Receipt{}, w.persistErr
	}
	return w.submitDelivery(ctx, native, d, false)
}
func (w *Worker) submitDelivery(ctx context.Context, n reliableNative, d *delivery, lookupFirst bool) (sending.Receipt, error) {
	if d.ReviewEvidence != nil {
		ctx = companion.WithStoredGenerationEvidence(ctx, *d.ReviewEvidence)
	}
	status := linuxnative.ToolStatus{}
	var err error
	if lookupFirst {
		status, err = n.LookupSend(ctx, d.Key)
		if err != nil && !errors.Is(err, linuxnative.ErrToolNotFound) {
			return w.waitDelivery(d, "receipt_lookup_failed")
		}
	}
	if !lookupFirst || errors.Is(err, linuxnative.ErrToolNotFound) {
		if w.deliveryCancelled(d) {
			return w.cancelledDelivery(d)
		}
		deferred := &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: errSubmissionDeferred.Error()}
		if w.beforeSubmit(ctx) != nil {
			return sending.Receipt{}, deferred
		}
		in, text, validationErr := w.prepareGroundedText(ctx, d.Input, d.Text)
		if w.beforeSubmit(ctx) != nil {
			return sending.Receipt{}, deferred
		}
		if validationErr != nil {
			failure := replyAdmissionFailure(validationErr)
			d.State, d.Reason = "stopped", sending.FailureReason(failure)
			w.persistErr = w.saveRuntime()
			return sending.Receipt{}, failure
		}
		d.Input, d.Text = in, text
		if gate := w.sendGate(d.Thread); time.Now().Before(gate) {
			d.State, d.Next, d.Reason = "prepared", gate, "delivery_pacing"
			w.persistErr = w.saveRuntime()
			return sending.Receipt{}, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: "delivery_pacing"}
		}
		previousState, previousText := d.State, d.SubmittedText
		d.State = "submitting"
		d.SubmittedText = d.Text
		w.advanceSendGate(d.Thread)
		if e := w.saveRuntime(); e != nil {
			return sending.Receipt{}, e
		}
		err = w.guardedDeliverySubmit(ctx, d.Input, func() error {
			var sendErr error
			status, sendErr = n.Submit(ctx, linuxnative.ToolSend{ThreadID: d.Thread, Kind: "text", Text: d.Text, IdempotencyKey: d.Key, Quote: d.Quote})
			return sendErr
		})
		if errors.Is(err, ErrDeliveryCancelled) {
			d.State, d.SubmittedText = previousState, previousText
			return w.cancelledDelivery(d)
		}
		if err != nil {
			if reason := linuxnative.QuoteAdmissionReason(err); reason != "" {
				d.State, d.Reason = "stopped", reason
				w.persistErr = w.saveRuntime()
				return sending.Receipt{}, &sending.OutcomeError{Cause: sending.ErrRejected, Reason: reason}
			}
			return w.waitDelivery(d, "send_outcome_unknown")
		}
	}
	for {
		if !textReceiptMatches(status, d.Thread, d.ID) {
			return w.waitDelivery(d, "receipt_identity_mismatch")
		}
		if status.ID != "" {
			d.ID = status.ID
		}
		if status.ServerID != "" {
			d.ServerID = status.ServerID
		}
		d.State = status.State
		d.Reason = status.Reason
		if e := w.saveRuntime(); e != nil {
			return sending.Receipt{}, e
		}
		switch status.State {
		case "accepted":
			d.State = "accepted"
			if d.SubmittedText != "" {
				d.Text = d.SubmittedText
			}
			w.advanceSendGate(d.Thread)
			w.persistErr = w.saveRuntime()
			return sending.Receipt{Recipient: d.Thread, ServerMessageID: status.ServerID}, w.persistErr
		case "failed", "cancelled":
			if status.State == "failed" && status.Retryable {
				return w.waitDelivery(d, status.Reason)
			}
			d.State = "stopped"
			w.persistErr = w.saveRuntime()
			return sending.Receipt{}, &sending.OutcomeError{Cause: sending.ErrRejected, Reason: status.Reason, IntentID: d.ID}
		case "needs_review", "unknown":
			return w.waitDelivery(d, "submission_outcome_unknown")
		}
		select {
		case <-ctx.Done():
			return w.waitDelivery(d, "receipt_wait_timeout")
		case <-time.After(300 * time.Millisecond):
		}
		status, err = n.ToolSendStatus(ctx, d.ID)
		if err != nil {
			return w.waitDelivery(d, "receipt_lookup_failed")
		}
	}
}
func (w *Worker) waitDelivery(d *delivery, reason string) (sending.Receipt, error) {
	d.State = "waiting"
	d.Checks = 0
	w.deferReceiptLookup(d, reason)
	if w.persistErr != nil {
		return sending.Receipt{}, w.persistErr
	}
	return sending.Receipt{}, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: "delivery_reconciliation_pending", IntentID: d.ID}
}

// Record the sampled deadline once per inconclusive observation. Restarts and
// plan pacing ticks must not sample again before this deadline expires.
func (w *Worker) deferReceiptLookup(d *delivery, reason string) {
	d.Reason = reason
	d.UncertainRounds++
	d.Next = time.Now().Add(deliveryBackoff(w.config.Options.RetryDelaySeconds, d.UncertainRounds))
	w.persistErr = w.saveRuntime()
	if w.persistErr == nil {
		w.recordDeliveryWait(d, reason)
	}
}

func (w *Worker) deliveryCancelled(d *delivery) bool {
	return d.Cancelled || !w.taskActive(d.Input)
}

// cancelledDelivery prevents a not-yet-submitted draft from leaving the
// process.  Once a native intent exists, retain only the immutable key and
// evidence so reconciliation can account for a late receipt; it is never a
// permission to send a replacement message.
func (w *Worker) cancelledDelivery(d *delivery) (sending.Receipt, error) {
	d.Cancelled = true
	d.Reason = "task_cancelled_before_delivery"
	if d.ID == "" && d.SubmittedText == "" && len(d.PreviousKeys) == 0 {
		d.State = "stopped"
	} else {
		d.State = "cancelled_reconciling"
		d.Next = time.Now().Add(deliveryBackoff(w.config.Options.RetryDelaySeconds, d.UncertainRounds+1))
	}
	w.persistErr = w.saveRuntime()
	w.activity(Record{Event: "delivery_stopped", Category: "decision", ThreadID: d.Thread, MessageID: d.Trace, Reason: d.Reason, Status: d.State, SendID: d.ID})
	return sending.Receipt{}, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: "delivery_reconciliation_pending", IntentID: d.ID}
}

// Continuations have synthetic trace IDs. WeChat references must point to the
// original incoming message, not the ID of the background task that completed.
func replyQuote(in companion.Input) *linuxnative.ToolQuote {
	id := in.OriginTraceID
	if in.ReplyContext != nil && in.ReplyContext.AnchorMessageID != "" {
		id = in.ReplyContext.AnchorMessageID
	}
	if id == "" {
		id = in.TraceID
	}
	return &linuxnative.ToolQuote{MessageID: id, ServerID: in.QuoteServerID}
}

// Each uncertain submission doubles its wait, with jitter. At the ceiling the
// interval remains random (one to two hours); the durable job has no retry count
// or age expiry. Read failures never authorize a new send.
func deliveryBackoff(baseSeconds, round int) time.Duration {
	if baseSeconds < 30 {
		baseSeconds = 30
	}
	base := time.Duration(baseSeconds) * time.Second
	for i := 1; i < round && base < time.Hour; i++ {
		base *= 2
	}
	if base > time.Hour {
		base = time.Hour
	}
	return base + time.Duration(rand.Int64N(int64(base)+1))
}

// Shared by durable owner recovery tasks; no retry-count or age expiry.
func DeliveryRetryBackoff(round int) time.Duration { return deliveryBackoff(120, round) }

func (w *Worker) recordDeliveryWait(d *delivery, reason string) {
	event := "delivery_waiting"
	if d.Bubble < 0 {
		// A late acknowledgement for progress does not reopen the parent turn.
		event = "tool_progress_receipt_waiting"
	}
	w.activity(Record{Event: event, Category: "decision", ThreadID: d.Thread, MessageID: d.Trace, Content: d.Text, SendID: d.ID, Reason: reason, Status: "waiting", RetryAttempts: d.Attempts, RetryAt: d.Next.Format(time.RFC3339)})
}

// Prefer the known native job. A missing job can still be resolved by the
// original idempotency key. Neither read creates a new send.
func textReceiptMatches(status linuxnative.ToolStatus, thread, id string) bool {
	return (status.ThreadID == "" || status.ThreadID == thread) && (status.Kind == "" || status.Kind == "text") && (id == "" || status.ID == "" || status.ID == id)
}
func lookupTextReceipt(ctx context.Context, n reliableNative, d *delivery) (linuxnative.ToolStatus, error) {
	checked := func(status linuxnative.ToolStatus, err error) (linuxnative.ToolStatus, error) {
		if err == nil && !textReceiptMatches(status, d.Thread, d.ID) {
			return linuxnative.ToolStatus{}, errors.New("receipt_identity_mismatch")
		}
		return status, err
	}
	if d.ID != "" {
		status, err := n.ToolSendStatus(ctx, d.ID)
		if !errors.Is(err, linuxnative.ErrToolNotFound) {
			return checked(status, err)
		}
	}
	return checked(n.LookupSend(ctx, d.Key))
}

func (w *Worker) reconcileDeliveries(ctx context.Context) {
	n, ok := w.sender.(reliableNative)
	if !ok {
		return
	}
	for _, d := range w.runtime.Deliveries {
		if ctx.Err() != nil {
			return
		}
		if d.State == "accepted" || d.State == "stopped" || time.Now().Before(d.Next) {
			continue
		}
		if d.Bubble < 0 {
			readCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			status, err := lookupTextReceipt(readCtx, n, d)
			cancel()
			if err == nil && status.State == "accepted" {
				if status.ID != "" {
					d.ID = status.ID
				}
				if status.ServerID != "" {
					d.ServerID = status.ServerID
				}
				w.acceptReconciled(d)
				continue
			}
			found, complete, scanErr := w.observedDelivery(ctx, n, d)
			if found {
				w.acceptReconciled(d)
				continue
			}
			if err == nil && (status.State == "queued" || status.State == "sending" || status.State == "running") && time.Since(d.Created) < 5*time.Minute {
				d.Next = time.Now().Add(15 * time.Second)
				w.persistErr = w.saveRuntime()
				continue
			}
			if time.Since(d.Created) < 15*time.Minute && (time.Since(d.Created) < 5*time.Minute || scanErr != nil || !complete) {
				w.deferReceiptLookup(d, "receipt_lookup_failed")
				continue
			}
			d.State = "stopped"
			w.activity(Record{Event: "tool_progress_receipt_unknown", Category: "decision", ThreadID: d.Thread, MessageID: d.Trace, Content: d.Text, Reason: "tool_progress_expired_no_late_resend", Status: "unknown", SendID: d.ID})
			w.persistErr = w.saveRuntime()
			continue
		}
		if d.State == "prepared" && !d.Cancelled {
			superseded := false
			for _, p := range w.runtime.Plans {
				if p.Input.Thread == d.Thread && p.Input.TraceID == d.Trace && p.Slot == d.Bubble && len(p.Updates) > 0 {
					d.State = "stopped"
					d.Reason = "unsubmitted_text_needs_replan"
					p.Slot++
					p.State = "review_pending"
					superseded = true
					break
				}
			}
			if superseded {
				w.persistErr = w.saveRuntime()
				continue
			}
		}
		if w.config.AuthorizeReply != nil && !w.config.AuthorizeReply(d.Input) {
			if d.State != "cancelled_reconciling" {
				_, _ = w.cancelledDelivery(d)
				if w.persistErr != nil {
					return
				}
				continue
			}
		} else if !w.allowed[d.Thread] {
			d.State = "stopped"
			d.Reason = "conversation_not_allowed"
			w.activity(Record{Event: "delivery_stopped", Category: "decision", ThreadID: d.Thread, MessageID: d.Trace, Reason: "conversation_not_allowed", Status: "blocked", SendID: d.ID})
			w.persistErr = w.saveRuntime()
			continue
		}
		if c, ok := w.model.(*companion.Engine); ok && (!c.Config.Permissions.ReplyText || c.Config.Quiet(time.Now()) && d.Input.Mode != "direct") {
			continue
		}
		if w.deliveryCancelled(d) {
			// A withdrawn text may have crossed the process boundary already.
			// Keep looking up that exact intent below, but never enter a submit
			// branch or generate a new idempotency key.
			d.Cancelled = true
			if d.State != "cancelled_reconciling" {
				d.State, d.Reason = "cancelled_reconciling", "task_cancelled_before_delivery"
				w.activity(Record{Event: "delivery_stopped", Category: "decision", ThreadID: d.Thread, MessageID: d.Trace, Reason: d.Reason, Status: d.State, SendID: d.ID})
			}
		}
		// Any earlier attempt may receive a late receipt. Never create a fresh
		// intent until every previous key has been checked successfully.
		historyClear := true
		for _, key := range d.PreviousKeys {
			readCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			old, e := n.LookupSend(readCtx, key)
			cancel()
			if e == nil && !textReceiptMatches(old, d.Thread, "") {
				historyClear = false
				break
			}
			if e == nil && old.State == "accepted" {
				d.ServerID = old.ServerID
				d.ID = old.ID
				for i, oldKey := range d.PreviousKeys {
					if oldKey == key && i < len(d.PreviousTexts) {
						d.Text = d.PreviousTexts[i]
						break
					}
				}
				w.acceptReconciled(d)
				historyClear = false
				break
			}
			if e != nil && !errors.Is(e, linuxnative.ErrToolNotFound) || e == nil && (old.State == "queued" || old.State == "sending" || old.State == "submitting" || old.State == "running") {
				historyClear = false
				break
			}
		}
		if !historyClear {
			if d.State != "accepted" {
				w.deferReceiptLookup(d, "previous_receipt_unverified")
			}
			continue
		}
		if d.State == "prepared" || d.State == "submitting" {
			if d.Cancelled {
				_, _ = w.cancelledDelivery(d)
				continue
			}
			if d.Attempts > 0 && !w.config.Options.RetryUncertain {
				d.Next = time.Now().Add(time.Minute)
				w.persistErr = w.saveRuntime()
				continue
			}
			sendCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			_, err := w.submitDelivery(sendCtx, n, d, true)
			cancel()
			if err == nil {
				w.acceptReconciled(d)
			}
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		status, err := lookupTextReceipt(readCtx, n, d)
		cancel()
		if err == nil && (status.State == "queued" || status.State == "sending" || status.State == "submitting" || status.State == "running") {
			d.Next = time.Now().Add(15 * time.Second)
			w.persistErr = w.saveRuntime()
			continue
		}
		if err == nil && status.State == "accepted" {
			if status.ID != "" {
				d.ID = status.ID
			}
			if status.ServerID != "" {
				d.ServerID = status.ServerID
			}
			if d.SubmittedText != "" {
				d.Text = d.SubmittedText
			}
			w.acceptReconciled(d)
			continue
		}
		if err == nil && (status.State == "failed" && !status.Retryable || status.State == "cancelled") {
			d.State = "stopped"
			w.activity(Record{Event: "send_failed", Category: "error", ThreadID: d.Thread, MessageID: d.Trace, Content: d.Text, Reason: status.Reason, Status: "failed", SendID: d.ID})
			w.persistErr = w.saveRuntime()
			continue
		}
		if d.Cancelled && w.config.AuthorizeReply != nil && !w.config.AuthorizeReply(d.Input) {
			w.deferReceiptLookup(d, "revoked_delivery_receipt_unverified")
			continue
		}
		confirmedRetryable := err == nil && status.State == "failed" && status.Retryable
		if confirmedRetryable {
			d.Reason = status.Reason
		}
		// Healthy catalog scans are mandatory; an unavailable read is never
		// treated as evidence of absence. Search one thread, never other chats.
		found, complete, scanErr := w.observedDelivery(ctx, n, d)
		if found {
			w.acceptReconciled(d)
			continue
		}
		if err != nil && !errors.Is(err, linuxnative.ErrToolNotFound) || scanErr != nil || !complete || !w.sender.Ready() {
			w.deferReceiptLookup(d, "receipt_lookup_failed")
			continue
		}
		if d.Cancelled {
			// Cancellation changes the recovery rule, not the evidence: keep
			// checking the original key for a late receipt, but do not resend.
			w.deferReceiptLookup(d, "delivery_receipt_unverified")
			continue
		}
		if !confirmedRetryable {
			// An absent, unreadable or otherwise unknown idempotency key is never
			// proof that WeChat did not receive the text.  Preserve the original
			// draft and evidence, and retry only the lookup on a randomized delay.
			w.deferReceiptLookup(d, "delivery_receipt_unverified")
			continue
		}
		if !w.config.Options.RetryUncertain {
			d.Next = time.Now().Add(time.Minute)
			d.Reason = "delivery_retry_disabled"
			w.persistErr = w.saveRuntime()
			continue
		}
		if !w.taskActive(d.Input) {
			_, _ = w.cancelledDelivery(d)
			continue
		}
		if w.surfOrigin(d.Input) && w.config.SurfAllowed != nil && !w.config.SurfAllowed(d.Thread) {
			d.State, d.Reason = "stopped", "surf_share_permission_changed"
			w.persistErr = w.saveRuntime()
			w.activity(Record{Event: "delivery_stopped", Category: "decision", ThreadID: d.Thread, MessageID: d.Trace, Reason: d.Reason, Status: "stopped", Origin: "surf"})
			continue
		}
		// A fresh idempotency key is allowed only after the native service has
		// explicitly reported a terminal *retryable* failure.  Unknown outcomes
		// remain lookup-only and never invoke another model review.
		d.Attempts++
		d.Checks = 0
		{
			base := d.Key
			if len(d.PreviousKeys) > 0 {
				base = d.PreviousKeys[0]
			}
			d.PreviousKeys = append(d.PreviousKeys, d.Key)
			oldText := d.SubmittedText
			if oldText == "" {
				oldText = d.Text
			}
			d.PreviousTexts = append(d.PreviousTexts, oldText)
			d.Key = fmt.Sprintf("%s-retry%d", base, d.Attempts)
		}
		d.ID = ""
		d.ServerID = ""
		d.SubmittedText = ""
		d.State = "prepared"
		if w.persistErr = w.saveRuntime(); w.persistErr != nil {
			return
		}
		w.activity(Record{Event: "delivery_retry", Category: "decision", ThreadID: d.Thread, MessageID: d.Trace, Content: d.Text, Reason: "confirmed_retryable_native_failure", Status: "retrying", SendID: d.ID, RetryAttempts: d.Attempts})
		sendCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, err = w.submitDelivery(sendCtx, n, d, true)
		cancel()
		if err == nil {
			w.acceptReconciled(d)
		}
	}
}
func (w *Worker) acceptReconciled(d *delivery) {
	d.State = "accepted"
	w.advanceSendGate(d.Thread)
	if w.persistErr = w.saveRuntime(); w.persistErr != nil {
		return
	}
	event := "message_sent"
	if d.Bubble < 0 {
		event = "message_progress"
	}
	w.activity(Record{Event: event, Category: "sent", ThreadID: d.Thread, MessageID: d.Trace, Content: d.Text, Reason: "delivery_confirmed_by_receipt_or_sync", Status: "accepted", SendID: d.ID, Origin: "bot"})
	if c, ok := w.model.(*companion.Engine); ok {
		for _, p := range w.runtime.Plans {
			if d.Bubble >= 0 && p.Input.Thread == d.Thread && p.Input.TraceID == d.Trace {
				return
			}
		}
		err := w.acceptTextMemory(c, d.Input, d.Bubble, d.Text)
		if err != nil {
			w.config.Log("memory_save_failed_after_accepted_reply")
		}
	}
}
func (w *Worker) observedDelivery(ctx context.Context, n reliableNative, d *delivery) (bool, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cursor := ""
	for i := 0; i < 10; i++ {
		p, e := n.Catalog(ctx, "messages", d.Thread, "", cursor)
		if e != nil {
			return false, false, e
		}
		older := false
		for _, m := range p.Items {
			at, e := time.Parse(time.RFC3339, mString(m, "createdAt"))
			if e != nil {
				continue
			}
			if at.Before(d.Created.Add(-5 * time.Second)) {
				older = true
				continue
			}
			body := mString(m, "body")
			matched := body == d.Text || d.SubmittedText != "" && body == d.SubmittedText
			for _, text := range d.PreviousTexts {
				matched = matched || body == text
			}
			if mString(m, "direction") == "outgoing" && matched && mString(m, "serverId") != "" {
				d.Text = body
				d.ServerID = mString(m, "serverId")
				d.AcceptedAt = mString(m, "createdAt")
				return true, true, nil
			}
		}
		if p.NextCursor == nil || *p.NextCursor == "" || older {
			return false, true, nil
		}
		cursor = *p.NextCursor
	}
	return false, false, nil
}
func mString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}
