package bot

import (
	"context"
	"errors"
	"strings"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/sending"
)

type utterancePlan struct {
	ReviewEvidence   *companion.GenerationEvidence `json:"review_evidence,omitempty"`
	ReplyContexts    []companion.ReplyContext      `json:"reply_contexts,omitempty"`
	Paused           bool                          `json:"paused,omitempty"`
	WakeReason       string                        `json:"wake_reason,omitempty"`
	StopReason       string                        `json:"stop_reason,omitempty"`
	MediaCaption     bool                          `json:"media_caption,omitempty"`
	InboxChecked     bool                          `json:"inbox_checked"`
	Trace            string                        `json:"trace"`
	ID               string                        `json:"id"`
	Input            companion.Input               `json:"input"`
	Quote            *linuxnative.ToolQuote        `json:"quote,omitempty"`
	Messages         []string                      `json:"messages"`
	Sent             []string                      `json:"sent"`
	Index            int                           `json:"index"`
	Slot             int                           `json:"slot"`
	State            string                        `json:"state"`
	Next             time.Time                     `json:"next"`
	Created          time.Time                     `json:"created"`
	Updates          []companion.ConversationEvent `json:"updates,omitempty"`
	ReviewFailures   int                           `json:"review_failures,omitempty"`
	WakeAfterSeconds int                           `json:"wake_after_seconds,omitempty"`
	WaitForReply     bool                          `json:"wait_for_reply,omitempty"`
}

func (p *utterancePlan) terminal() bool { return p.State == "accepted" || p.State == "stopped" }
func (w *Worker) pendingConversationThread(thread string) bool {
	if w.generation != nil && w.generation.jobs[thread] != nil {
		return true
	}
	for _, p := range w.runtime.Plans {
		if p.Input.Thread == thread && !p.terminal() && !(p.MediaCaption && w.mediaTraceOnlyUnknown(p.Input.TraceID)) {
			return true
		}
	}
	for _, d := range w.runtime.Media {
		if d.Input.Thread == thread && !d.terminal() && !awaitingMediaReceipt(d) {
			return true
		}
	}
	for _, r := range w.runtime.MediaReplies {
		if r.Input.Thread == thread && !r.terminal() && !w.mediaTraceOnlyUnknown(r.Trace) {
			return true
		}
	}
	return false
}
func (w *Worker) queueUtterancePlan(c *companion.Engine, in companion.Input, d companion.Decision) bool {
	return w.queueUtteranceFrom(c, in, d, 0, false, nil)
}
func (w *Worker) queueUtteranceFrom(c *companion.Engine, in companion.Input, d companion.Decision, start int, mediaCaption bool, updates []companion.ConversationEvent) bool {
	if w.runtime == nil || !c.Config.ConversationConfig().Enabled || len(d.Messages) == 0 || d.ImageID != "" || len(d.MediaIDs) > 0 {
		return false
	}
	for _, p := range w.runtime.Plans {
		if p.Input.Thread == in.Thread && p.Input.TraceID == in.TraceID {
			return true
		}
	}
	if len(w.runtime.Plans) >= 1000 {
		kept := w.runtime.Plans[:0]
		for _, p := range w.runtime.Plans {
			if !p.terminal() || time.Since(p.Created) < 24*time.Hour {
				kept = append(kept, p)
			}
		}
		w.runtime.Plans = kept
	}
	if len(w.runtime.Plans) >= 1000 {
		w.persistErr = errors.New("utterance plan queue full")
		return true
	}
	now := time.Now().UTC()
	p := &utterancePlan{WakeReason: d.WakeReason, ID: "utterance-" + messageHash(in.Thread + "|" + in.TraceID)[:32], Input: in, Messages: append([]string{}, d.Messages...), Sent: []string{}, State: "queued", Next: now.Add(w.replyDelay(c)), Created: now, WakeAfterSeconds: d.WakeAfterSeconds, WaitForReply: d.WaitForReply}
	p.MediaCaption = mediaCaption
	p.ReplyContexts = append([]companion.ReplyContext(nil), d.ReplyContexts...)
	p.Index, p.Slot = start, start
	p.Sent = append([]string{}, d.Messages[:start]...)
	p.Updates = append([]companion.ConversationEvent(nil), updates...)
	if len(p.Updates) > 0 {
		p.State = "review_pending"
	}
	if in.QuoteServerID != "" && (in.Directed && w.config.Options.QuoteReplies || in.QuoteSharedMessage) {
		p.Quote = replyQuote(in)
	}
	p.Trace = in.TraceID
	w.runtime.Plans = append(w.runtime.Plans, p)
	w.persistErr = w.saveRuntime()
	if w.persistErr != nil {
		return true
	}
	w.queuePhase(in.TraceID, "sending")
	w.publishTaskDelivery(in.TraceID)
	w.activity(Record{Event: "utterance_plan_queued", Category: "decision", ThreadID: in.Thread, MessageID: in.TraceID, Status: "queued", Reason: "semantic_message_plan", Content: strings.Join(d.Messages, "\n")})
	return true
}
func (w *Worker) noticeConversationMessage(m linuxnative.Message, eligible bool) {
	c, ok := w.model.(*companion.Engine)
	if !ok || w.runtime == nil || !c.Config.ConversationConfig().Enabled {
		return
	}
	if !eligible && strings.HasSuffix(m.ThreadID, "@chatroom") && !c.Config.Permissions.ObserveGroups {
		return
	}
	w.noticeGenerationMessage(m, eligible)
	if m.Type != 1 && !m.IsQuotedText() {
		return
	}
	if c.Config.ConversationConfig().RecheckOnMessage {
		for _, r := range w.runtime.MediaReplies {
			if r.terminal() || r.Input.Thread != m.ThreadID || r.Trace == m.ID {
				continue
			}
			if !messageAffectsInput(r.Input, m) {
				r.Input.QuoteSharedMessage = true
				continue
			}
			seen := false
			for _, e := range r.Updates {
				seen = seen || e.MessageID == m.ID
			}
			if seen {
				continue
			}
			r.Updates = append(r.Updates, conversationEvent(m, eligible))
			if len(r.Updates) > 8 {
				r.Updates = r.Updates[len(r.Updates)-8:]
			}
		}
	}
	for _, wake := range w.runtime.Wakes {
		if wake.State == "waiting" && wake.WaitForReply && wake.Input.Thread == m.ThreadID && wake.Parent != m.ID && (wake.Input.Sender == "" || wake.Input.Sender == m.SenderID) {
			wake.State = "stopped"
			w.activity(Record{Event: "conversation_wake_stopped", Category: "decision", ThreadID: m.ThreadID, MessageID: wake.Parent, Status: "stopped", Reason: "new_reply_before_wake"})
		}
	}
	for _, p := range w.runtime.Plans {
		if p.terminal() || p.Input.Thread != m.ThreadID || p.Input.TraceID == m.ID {
			continue
		}
		if !messageAffectsInput(p.Input, m) {
			p.Input.QuoteSharedMessage = true
			if p.Input.QuoteServerID != "" {
				p.Quote = replyQuote(p.Input)
			}
			continue
		}
		if !c.Config.ConversationConfig().RecheckOnMessage {
			continue
		}
		seen := false
		for _, e := range p.Updates {
			seen = seen || e.MessageID == m.ID
		}
		if seen {
			continue
		}
		p.Updates = append(p.Updates, conversationEvent(m, eligible))
		if len(p.Updates) > 8 {
			p.Updates = p.Updates[len(p.Updates)-8:]
		}
		p.State = "review_pending"
		w.activity(Record{Event: "utterance_plan_paused", Category: "decision", ThreadID: m.ThreadID, MessageID: p.Input.TraceID, Status: "waiting", Reason: "new_context_before_unsent_text"})
	}
	w.persistErr = w.saveRuntime()
}
func (w *Worker) planDelivery(p *utterancePlan) *delivery {
	for _, d := range w.runtime.Deliveries {
		if d.Thread == p.Input.Thread && d.Trace == p.Input.TraceID && d.Bubble == p.Slot {
			return d
		}
	}
	return nil
}
func (w *Worker) acceptPlanBubble(c *companion.Engine, p *utterancePlan, text string) {
	in := p.Input
	if p.Index < len(p.ReplyContexts) {
		rc := p.ReplyContexts[p.Index]
		in.ReplyContext = &rc
	}
	if d := w.planDelivery(p); d != nil && d.State == "accepted" && d.Input.ReplyContext != nil {
		in.ReplyContext = d.Input.ReplyContext
	}
	var err error
	if _, native := w.sender.(reliableNative); native {
		// A durable plan that went through the native sender must be tied to
		// its confirmed receipt.  Do not substitute a plan/index-derived key:
		// that can collide with imported history and is not proof of delivery.
		d := w.planDelivery(p)
		if d == nil || d.State != "accepted" || deliveryMemoryKey(d) == "" {
			err = companion.ErrDeliveryIdentityRequired
		} else if d.Text != text {
			err = companion.ErrDeliveryMemoryConflict
		} else {
			err = w.acceptTextMemoryAt(c, in, p.Slot, text, deliveryAcceptedTime(d, time.Now()))
		}
	} else {
		err = c.Accepted(in, text)
	}
	if err != nil {
		w.persistErr = err
		return
	}
	p.Sent = append(p.Sent, text)
	p.Index++
	p.Slot++
	p.Next = time.Now().Add(w.replyDelay(c))
	p.State = "queued"
	if len(p.Updates) > 0 {
		p.State = "review_pending"
	}
	if p.Index >= len(p.Messages) {
		p.State = "accepted"
		w.scheduleConversationWake(c, p.Input, p.WakeAfterSeconds, p.WaitForReply, p.WakeReason)
	}
	w.persistErr = w.saveRuntime()
	if w.persistErr != nil {
		return
	}
	reason := "semantic_plan_bubble_accepted"
	if p.MediaCaption {
		reason = "media_accepted_then_caption"
	}
	sendID := ""
	for _, d := range w.runtime.Deliveries {
		if d.Thread == p.Input.Thread && d.Trace == p.Input.TraceID && d.Bubble == p.Slot-1 {
			sendID = d.ID
			break
		}
	}
	w.activity(Record{Event: "message_sent", Category: "sent", ThreadID: p.Input.Thread, MessageID: p.Input.TraceID, Content: text, Status: "accepted", Origin: "bot", Reason: reason, SendID: sendID})
	w.refreshMediaParent(p.Input.TraceID)
}
func (w *Worker) reconcileUtterancePlans(ctx context.Context) {
	c, ok := w.model.(*companion.Engine)
	if !ok || w.runtime == nil {
		return
	}
	visited := map[string]bool{}
	for _, p := range w.runtime.Plans {
		if ctx.Err() != nil || w.persistErr != nil {
			return
		}
		if p.Paused || p.terminal() || visited[p.Input.Thread] || !p.InboxChecked {
			continue
		}
		visited[p.Input.Thread] = true
		// An already submitted bubble is reconciled independently, never replaced
		// or assumed absent because a new human message arrived.
		if d := w.planDelivery(p); d != nil {
			// The delivery reconciler owns the persisted receipt deadline. Plans
			// only consume its outcome, even if their pacing timer expires first.
			if d.State == "accepted" {
				w.acceptPlanBubble(c, p, d.Text)
			} else if d.State == "stopped" {
				p.State = "stopped"
				w.persistErr = w.saveRuntime()
				w.refreshMediaParent(p.Input.TraceID)
			} else {
				delete(visited, p.Input.Thread)
			}
			continue
		}
		if p.State == "waiting_receipt" {
			p.State = "stopped"
			w.persistErr = w.saveRuntime()
			w.activity(Record{Event: "utterance_plan_stopped", Category: "error", ThreadID: p.Input.Thread, MessageID: p.Input.TraceID, Status: "needs_review", Reason: "text_sender_cannot_reconcile_unknown"})
			continue
		}
		if !w.taskActive(p.Input) || !w.allowed[p.Input.Thread] || !c.Config.Enabled || !c.Config.Permissions.ReplyText || (!c.Config.ConversationConfig().Enabled) || time.Since(p.Created) > 30*time.Minute || p.Input.Mode != "direct" && c.Config.Quiet(time.Now()) || p.Input.Surf && w.config.SurfAllowed != nil && !w.config.SurfAllowed(p.Input.Thread) {
			p.State = "stopped"
			w.persistErr = w.saveRuntime()
			w.activity(Record{Event: "utterance_plan_stopped", Category: "decision", ThreadID: p.Input.Thread, MessageID: p.Input.TraceID, Status: "stopped", Reason: "utterance_permission_quiet_or_expired"})
			w.refreshMediaParent(p.Input.TraceID)
			continue
		}
		if p.State == "review_pending" && !w.draining() && !time.Now().Before(p.Next) {
			if w.generation != nil {
				w.startPlanReview(ctx, c, p)
				continue
			}
			reviewCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			d, err := w.reviewPlan(w.modelContext(reviewCtx, p.Input.Thread, p.Input.TraceID), c, p)
			cancel()
			w.applyPlanReview(p, d, err)
			continue
		}
		if p.State == "review_pending" || time.Now().Before(p.Next) {
			continue
		}
		text := strings.TrimSpace(p.Messages[p.Index])
		if c.Config.ShowAILabel && !strings.HasPrefix(text, "[AI]") {
			text = "[AI] " + text
		}
		in := p.Input
		if p.Index < len(p.ReplyContexts) {
			rc := p.ReplyContexts[p.Index]
			in.ReplyContext = &rc
		}
		if p.Quote != nil {
			in.QuoteServerID = p.Quote.ServerID
		}
		if w.beforeSubmit(ctx) != nil {
			return
		}
		p.State = "submitting"
		w.persistErr = w.saveRuntime()
		if w.persistErr != nil {
			return
		}
		sendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if p.ReviewEvidence != nil {
			sendCtx = companion.WithStoredGenerationEvidence(sendCtx, *p.ReviewEvidence)
		}
		_, err := w.sendText(sendCtx, in, p.Slot, text)
		cancel()
		if err == nil {
			w.acceptPlanBubble(c, p, text)
		} else if errors.Is(err, errSubmissionDeferred) {
			p.State = "queued"
			w.persistErr = w.saveRuntime()
			return
		} else if errors.Is(err, sending.ErrOutcomeUnknown) {
			p.State = "waiting_receipt"
			p.Next = time.Now().Add(time.Second)
			w.persistErr = w.saveRuntime()
		} else {
			p.State, p.StopReason = "stopped", sending.FailureReason(err)
			w.persistErr = w.saveRuntime()
			w.activity(Record{Event: "send_failed", Category: "error", ThreadID: in.Thread, MessageID: in.TraceID, Content: text, Status: "failed", Reason: sending.FailureReason(err)})
		}
	}
}
