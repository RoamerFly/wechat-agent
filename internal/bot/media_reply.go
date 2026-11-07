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

type mediaReply struct {
	ReplyContexts    []companion.ReplyContext      `json:"reply_contexts,omitempty"`
	Paused           bool                          `json:"paused,omitempty"`
	WakeReason       string                        `json:"wake_reason,omitempty"`
	WakeAfterSeconds int                           `json:"wake_after_seconds,omitempty"`
	WaitForReply     bool                          `json:"wait_for_reply,omitempty"`
	Updates          []companion.ConversationEvent `json:"updates,omitempty"`
	Quote            *linuxnative.ToolQuote        `json:"quote,omitempty"`
	Trace            string                        `json:"trace"`
	Input            companion.Input               `json:"input"`
	Messages         []string                      `json:"messages"`
	Expected         int                           `json:"expected"`
	Index            int                           `json:"index"`
	State            string                        `json:"state"`
	Next             time.Time                     `json:"next"`
	Created          time.Time                     `json:"created"`
}

func (r *mediaReply) terminal() bool {
	return r.State == "accepted" || r.State == "stopped" || r.State == "queued_plan"
}

func (w *Worker) deferMediaReply(in companion.Input, messages []string, expected int, followup ...companion.Decision) bool {
	if w.mediaTraceCancelled(in.TraceID) {
		return true
	}
	for _, r := range w.runtime.MediaReplies {
		if r.Trace == in.TraceID && r.Input.Thread == in.Thread {
			return true
		}
	}
	if len(w.runtime.MediaReplies) >= 1000 {
		kept := w.runtime.MediaReplies[:0]
		for _, r := range w.runtime.MediaReplies {
			if !r.terminal() || time.Since(r.Created) < 7*24*time.Hour {
				kept = append(kept, r)
			}
		}
		w.runtime.MediaReplies = kept
	}
	if len(w.runtime.MediaReplies) >= 1000 {
		return false
	}
	r := &mediaReply{Trace: in.TraceID, Input: in, Messages: append([]string(nil), messages...), Expected: expected, State: "waiting_delivery", Created: time.Now().UTC()}
	if len(followup) > 0 {
		r.ReplyContexts = append([]companion.ReplyContext(nil), followup[0].ReplyContexts...)
		r.WakeAfterSeconds, r.WaitForReply, r.WakeReason = followup[0].WakeAfterSeconds, followup[0].WaitForReply, followup[0].WakeReason
	}
	if in.QuoteServerID != "" && in.TraceID != "" && (in.QuoteSharedMessage || in.Directed && w.config.Options.QuoteReplies) {
		r.Quote = replyQuote(in)
	}
	w.runtime.MediaReplies = append(w.runtime.MediaReplies, r)
	w.persistErr = w.saveRuntime()
	return w.persistErr == nil
}
func (w *Worker) reconcileMediaReplies(ctx context.Context) {
	c, ok := w.model.(*companion.Engine)
	if !ok {
		return
	}
	for _, r := range w.runtime.MediaReplies {
		if ctx.Err() != nil || w.persistErr != nil {
			return
		}
		if r.Paused || r.terminal() {
			continue
		}
		total, accepted := 0, 0
		failed := false
		for _, d := range w.runtime.Media {
			if d.Trace == r.Trace && d.Input.Thread == r.Input.Thread {
				total++
				if d.State == "accepted" {
					accepted++
				}
				failed = failed || d.State == "stopped" || d.State == "needs_review" || d.UserCancelled
			}
		}
		if total < r.Expected && time.Since(r.Created) > 30*time.Second {
			failed = true
		}
		if failed || !w.taskActive(r.Input) || !w.allowed[r.Input.Thread] || !c.Config.Permissions.ReplyText || r.Input.Mode != "direct" && c.Config.Quiet(time.Now()) || r.Input.Surf && w.config.SurfAllowed != nil && !w.config.SurfAllowed(r.Input.Thread) {
			r.State = "stopped"
			w.activity(Record{Event: "media_reply_stopped", Category: "decision", ThreadID: r.Input.Thread, MessageID: r.Trace, Reason: "media_not_confirmed_no_success_caption", Status: "stopped"})
			w.refreshMediaParent(r.Trace)
			continue
		}
		if total != r.Expected || accepted != total {
			continue
		}
		if w.generation != nil && c.Config.ConversationConfig().Enabled {
			in := r.Input
			in.TraceID = r.Trace
			if r.Quote != nil {
				in.QuoteServerID = r.Quote.ServerID
			}
			messages := append([]string(nil), r.Messages...)
			// The handoff and text plan are persisted together. The attachment
			// keeps its own receipt/cancellation state; only the caption is replanned.
			r.State = "queued_plan"
			w.queueUtteranceFrom(c, in, companion.Decision{Messages: messages, ReplyContexts: r.ReplyContexts, WakeAfterSeconds: r.WakeAfterSeconds, WaitForReply: r.WaitForReply, WakeReason: r.WakeReason}, r.Index, true, r.Updates)
			w.refreshMediaParent(r.Trace)
			continue
		}
		if r.Next.IsZero() {
			r.Next = time.Now().Add(w.replyDelay(c))
			w.persistErr = w.saveRuntime()
			continue
		}
		if time.Now().Before(r.Next) {
			continue
		}
		in := r.Input
		in.TraceID = r.Trace
		if r.Index < len(r.ReplyContexts) {
			rc := r.ReplyContexts[r.Index]
			in.ReplyContext = &rc
		}
		if r.Quote != nil {
			in.QuoteServerID = r.Quote.ServerID
		}
		text := strings.TrimSpace(r.Messages[r.Index])
		if c.Config.ShowAILabel && !strings.HasPrefix(text, "[AI]") {
			text = "[AI] " + text
		}
		if w.beforeSubmit(ctx) != nil {
			return
		}
		r.State = "sending_text"
		w.persistErr = w.saveRuntime()
		if w.persistErr != nil {
			return
		}
		sendCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, err := w.sendText(sendCtx, in, r.Index, text)
		cancel()
		if err != nil {
			if errors.Is(err, errSubmissionDeferred) {
				r.State = "waiting_text"
				w.persistErr = w.saveRuntime()
				return
			} else if errors.Is(err, sending.ErrOutcomeUnknown) {
				r.Next = time.Now().Add(5 * time.Second)
			} else {
				r.State = "stopped"
			}
			w.persistErr = w.saveRuntime()
			continue
		}
		if err = w.acceptTextMemory(c, in, r.Index, text); err != nil {
			w.persistErr = err
			return
		}
		w.activity(Record{Event: "message_sent", Category: "sent", ThreadID: in.Thread, MessageID: r.Trace, Content: text, Status: "accepted", Reason: "media_accepted_then_caption", Origin: "bot"})
		r.Index++
		r.Next = time.Now().Add(w.replyDelay(c))
		if r.Index >= len(r.Messages) {
			r.State = "accepted"
		} else {
			r.State = "waiting_text"
		}
		w.persistErr = w.saveRuntime()
		w.refreshMediaParent(r.Trace)
	}
}
