package bot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type inboxItem struct {
	Backlog          bool                              `json:"backlog,omitempty"`
	BacklogParent    string                            `json:"backlog_parent,omitempty"`
	FollowupParent   string                            `json:"followup_parent,omitempty"`
	FollowupConsumed bool                              `json:"followup_consumed,omitempty"`
	OwnerModelReview bool                              `json:"owner_model_review,omitempty"`
	ReceivedRecorded bool                              `json:"received_recorded,omitempty"`
	AggregateUntil   time.Time                         `json:"aggregate_until,omitempty"`
	CollectedAt      time.Time                         `json:"collected_at,omitempty"`
	ToolStarted      bool                              `json:"tool_started,omitempty"`
	Event            linuxnative.Event                 `json:"event"`
	Phase            string                            `json:"phase"`
	Updated          time.Time                         `json:"updated"`
	RetryAfter       time.Time                         `json:"retry_after,omitempty"`
	RecoveryTools    []companion.RecoveryToolResult    `json:"recovery_tools,omitempty"`
	RecoveryEvidence *companion.GenerationEvidence     `json:"recovery_evidence,omitempty"`
	RecoveryPrivacy  *companion.RecoveryPrivacyBinding `json:"recovery_privacy,omitempty"`
}

func (w *Worker) ownerModelReview(id string) bool {
	if w.runtime != nil {
		for _, item := range w.runtime.Inbox {
			if item.Event.Data.MessageID == id {
				return item.OwnerModelReview
			}
		}
	}
	return false
}

type runtimeState struct {
	SendGates    map[string]time.Time `json:"send_gates,omitempty"`
	Wakes        []*conversationWake  `json:"conversation_wakes,omitempty"`
	Plans        []*utterancePlan     `json:"utterance_plans,omitempty"`
	Version      int                  `json:"version"`
	Server       string               `json:"server"`
	Account      string               `json:"account"`
	Inbox        []inboxItem          `json:"inbox"`
	Deliveries   []*delivery          `json:"deliveries"`
	Media        []*mediaDelivery     `json:"media,omitempty"`
	MediaReplies []*mediaReply        `json:"media_replies,omitempty"`
}

func atomicState(path string, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	if len(b) > 24<<20 {
		return errors.New("durable message queue exceeds limit")
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".queue-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(path))
}
func (w *Worker) saveRuntime() error {
	if w.runtime == nil {
		return nil
	}
	return atomicState(filepath.Join(w.config.StateDir, "messages-v2.json"), w.runtime)
}
func (w *Worker) queuePhase(id, phase string) {
	if w.runtime == nil || id == "" {
		return
	}
	for i := range w.runtime.Inbox {
		item := &w.runtime.Inbox[i]
		if item.Event.Data.MessageID == id {
			if item.Phase == phase && (phase == "sending" || phase == "replied" || phase == "needs_review" || phase == "stopped") {
				return
			}
			if phase == "tool_running" {
				item.ToolStarted = true
			}
			item.Phase = phase
			for j := range w.runtime.Inbox {
				child := &w.runtime.Inbox[j]
				if child.BacklogParent == id && child.Event.Data.MessageID != id {
					if phase == "model_running" || phase == "tool_running" || phase == "processing" {
						child.Phase = "backlog_grouped"
					} else {
						child.Phase = phase
					}
					child.Updated = time.Now().UTC()
				}
			}
			item.Updated = time.Now().UTC()
			if phase == "read_pending" {
				item.RetryAfter = time.Now().Add(30 * time.Second)
			}
			w.persistErr = w.saveRuntime()
			return
		}
	}
}
func (w *Worker) loadRuntime(server string) error {
	s := runtimeState{Version: 1, Server: server, Account: w.source.Account(), Inbox: []inboxItem{}, Deliveries: []*delivery{}}
	b, e := os.ReadFile(filepath.Join(w.config.StateDir, "messages-v2.json"))
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if e == nil && (len(b) > 24<<20 || json.Unmarshal(b, &s) != nil || !SupportedDeliveryCheckpointVersion(s.Version) || s.Server != server || s.Account != w.source.Account()) {
		return errors.New("persistent queue corrupt or account mismatch")
	}
	if len(s.Inbox) > 5000 || len(s.Deliveries) > 3000 || len(s.Media) > 1000 || len(s.MediaReplies) > 1000 {
		return errors.New("persistent queue exceeds bound")
	}
	w.runtime = &s
	// Preserve autonomous-surf provenance across legacy wake chains on restart.
	for _, p := range s.Plans {
		if p != nil && w.surfOrigin(p.Input) {
			p.Input.Surf = true
		}
	}
	for _, d := range s.Media {
		if d != nil && w.surfOrigin(d.Input) {
			d.Input.Surf = true
		}
	}
	for _, r := range s.MediaReplies {
		if r != nil && w.surfOrigin(r.Input) {
			r.Input.Surf = true
		}
	}
	for _, d := range s.Deliveries {
		if d != nil && w.surfOrigin(d.Input) {
			d.Input.Surf = true
		}
	}
	for _, media := range s.Media {
		if media == nil {
			return errors.New("persistent media invalid")
		}
		media.Input.TraceID = media.Trace
		if media.ResumeOriginID != "" {
			s.Version = 2
		}
		restoreOriginInputTrace(media.OriginInput)
		if media.State != "checking_receipt" && awaitingMediaReceipt(media) && media.ReconcileAfter.IsZero() && media.Next.After(time.Now()) {
			// Legacy queues used one deadline for both reads and writes. Keep
			// their retry backoff, but discover late receipts immediately.
			media.ReconcileAfter, media.Next = media.Next, time.Now()
		}
		if !media.UserCancelled && !media.Paused && media.State == "needs_review" && (media.ID != "" || media.Key != "") &&
			(media.Reason == "media_receipt_unverified_native_retry_denied" || media.Reason == "media_status_unavailable_after_deadline") {
			media.State, media.Reason = "checking_receipt", "media_readonly_receipt_pending"
			media.Next = time.Now()
		}
	}
	for _, d := range s.Deliveries {
		if d == nil {
			return errors.New("persistent delivery invalid")
		}
		d.Input.TraceID = d.Trace
	}
	if len(s.Wakes) > 200 {
		return errors.New("conversation wakes exceed bound")
	}
	for _, wake := range s.Wakes {
		if wake == nil || wake.Input.Thread == "" || wake.Parent == "" {
			return errors.New("conversation wake invalid")
		}
		if wake.State == "claimed" || wake.State == "running" {
			wake.State = "interrupted"
		}
	}
	if len(s.Plans) > 1000 {
		return errors.New("persistent utterance plans exceed bound")
	}
	for _, p := range s.Plans {
		if p != nil {
			p.Input.TraceID = p.Trace
			p.Input.PersonaOverride = w.config.GroupRules[p.Input.Thread].Persona
			p.InboxChecked = false
			if p.Quote != nil {
				p.Input.QuoteServerID = p.Quote.ServerID
			}
		}
		if p == nil || len(p.Messages) < 1 || p.Index < 0 || p.Index > len(p.Messages) || p.Index == len(p.Messages) && !p.terminal() || p.Input.Thread == "" || p.Slot < 0 {
			return errors.New("persistent utterance plan invalid; retained")
		}
	}
	for _, r := range s.MediaReplies {
		if r == nil || len(r.Messages) < 1 || r.Expected < 1 || r.Index < 0 || r.Index > len(r.Messages) || r.Index == len(r.Messages) && !r.terminal() {
			return errors.New("persistent media reply invalid; not discarded")
		}
	}
	return nil
}

// Fix a group turn's deadline when its first receipt arrives. Later receipts
// share it; a steady conversation cannot postpone the decision indefinitely.
func (w *Worker) collectionDeadline(thread string, at time.Time, window time.Duration) time.Time {
	if window <= 0 {
		return time.Time{}
	}
	if strings.HasSuffix(thread, "@chatroom") {
		for i := len(w.runtime.Inbox) - 1; i >= 0; i-- {
			prior := w.runtime.Inbox[i]
			if prior.Event.Data.ThreadID != thread || prior.AggregateUntil.IsZero() {
				continue
			}
			if prior.Phase == "received" && at.Before(prior.AggregateUntil) {
				return prior.AggregateUntil
			}
			break
		}
	}
	return at.Add(window)
}

func (w *Worker) drainInbox(ctx context.Context) error {
	for i := range w.runtime.Inbox {
		if w.draining() {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		item := &w.runtime.Inbox[i]
		if item.Backlog && !item.OwnerModelReview && (item.Phase == "received" || item.Phase == "read_pending" || item.Phase == "backlog_grouped") {
			if err := w.processUnreadConversation(ctx, item.Event.Data.ThreadID); err != nil {
				return err
			}
			continue
		}
		if time.Now().Before(item.RetryAfter) {
			continue
		}
		if time.Now().Before(item.AggregateUntil) {
			continue
		}
		switch item.Phase {
		case "received", "processing", "read_pending", "retrying_model":
		default:
			continue
		}
		// Group messages already collected during the same short window share
		// one decision. Keep explicit reviews, active tasks and delayed retries
		// on their own lifecycle, including their cancellation fast path.
		thread := item.Event.Data.ThreadID
		if w.generation != nil && item.Phase == "received" && !item.OwnerModelReview && !item.AggregateUntil.IsZero() && strings.HasSuffix(thread, "@chatroom") && !w.pendingConversationThread(thread) && w.generation.canGenerate() {
			ids := map[string]bool{}
			for j := i; j < len(w.runtime.Inbox); j++ {
				next := &w.runtime.Inbox[j]
				if next.Phase == "received" && !next.Backlog && !next.OwnerModelReview && !next.ToolStarted && next.Event.Data.ThreadID == thread && next.Event.Data.Direction == "incoming" && next.AggregateUntil.Equal(item.AggregateUntil) && !time.Now().Before(next.RetryAfter) {
					ids[next.Event.Data.MessageID] = true
				}
			}
			if len(ids) > 1 && ids[item.Event.Data.MessageID] {
				if err := w.processConversationBatch(ctx, thread, ids); err != nil {
					return err
				}
				continue
			}
		}
		item.Phase = "processing"
		if err := w.saveRuntime(); err != nil {
			return err
		}
		w.recovering = w.config.Options.RecoverBacklog
		w.handle(ctx, item.Event)
		w.recovering = false
		if w.persistErr != nil {
			return w.persistErr
		}
		if ctx.Err() != nil {
			return nil
		}
		if item.Phase == "processing" {
			w.activity(Record{Event: "message_processed", Category: "decision", ThreadID: item.Event.Data.ThreadID, MessageID: item.Event.Data.MessageID, Reason: "processed_without_reply", Status: "completed"})
			w.queuePhase(item.Event.Data.MessageID, "completed")
		}
	}
	return w.persistErr
}
func (w *Worker) runDurable(ctx context.Context, path string, state checkpoint) error {
	if err := w.loadRuntime(state.ServerID); err != nil {
		return err
	}
	w.enableGenerations()
	defer w.closeGenerations()
	defer w.suspendMedia()
	w.recoverFollowups()
	w.applyDeliveryCancellations()
	if w.persistErr != nil {
		return w.persistErr
	}
	for i := range w.runtime.Inbox {
		item := &w.runtime.Inbox[i]
		if item.ToolStarted && (item.Phase == "tool_running" || item.Phase == "processing" || item.Phase == "model_running") {
			item.Phase = "interrupted"
			w.activity(Record{Event: "tool_task_interrupted", Category: "error", ThreadID: item.Event.Data.ThreadID, MessageID: item.Event.Data.MessageID, Reason: "tool_task_interrupted_not_replayed", Status: "interrupted"})
			continue
		}
		if item.Phase == "model_running" {
			item.Phase = "received"
		}
		if item.Phase != "sending" {
			continue
		}
		tracked := false
		pending, accepted := false, false
		for _, p := range w.runtime.Plans {
			if p.Input.TraceID == item.Event.Data.MessageID {
				tracked = true
				pending = pending || !p.terminal()
				accepted = accepted || len(p.Sent) > 0
			}
		}
		for _, d := range w.runtime.Deliveries {
			if d.Trace == item.Event.Data.MessageID && d.Bubble >= 0 {
				tracked = true
				accepted = accepted || d.State == "accepted"
				pending = pending || d.State != "accepted" && d.State != "stopped"
			}
		}
		for _, d := range w.runtime.Media {
			if d.Trace == item.Event.Data.MessageID {
				tracked = true
				accepted = accepted || d.State == "accepted"
				pending = pending || !d.terminal()
			}
		}
		for _, r := range w.runtime.MediaReplies {
			if r.Trace == item.Event.Data.MessageID {
				tracked = true
				pending = pending || !r.terminal()
			}
		}
		if tracked && !pending {
			if accepted {
				item.Phase = "replied"
			} else {
				item.Phase = "failed"
			}
		}
		if !tracked {
			item.Phase = "needs_review"
			w.activity(Record{Event: "message_interrupted_after_send", Category: "error", ThreadID: item.Event.Data.ThreadID, MessageID: item.Event.Data.MessageID, Reason: "interrupted_media_or_legacy_send_check_receipt", Status: "needs_review"})
		}
	}
	if err := w.saveRuntime(); err != nil {
		return err
	}
	if w.config.Options.RecoverBacklog {
		if _, ok := w.model.(*companion.Engine); ok {
			if err := w.captureStartupUnread(ctx, path, &state); err != nil {
				return err
			}
		}
	}
	if err := w.drainInbox(ctx); err != nil {
		return err
	}
	peekedInterruptions := map[string]bool{}
	for ctx.Err() == nil {
		w.applyDeliveryCancellations()
		if w.persistErr != nil {
			return w.persistErr
		}
		w.collectGenerations(ctx)
		w.adoptMediaSeeds()
		if w.draining() {
			// During an update, do not start a new model task or advance the
			// cursor. Still allow fresh stop/change-song controls to cancel the
			// current voice, so a graceful update cannot make it uninterruptible.
			w.peekVoiceInterruptions(ctx, state.Cursor, peekedInterruptions)
			w.reviewActiveGenerations(ctx)
			w.drainGenerations(ctx)
			w.reconcileMedia(ctx)
			w.reconcileMediaReplies(ctx)
			if w.persistErr != nil {
				return w.persistErr
			}
			if !w.pendingMedia() && (w.generation == nil || w.generation.occupied() == 0) {
				return nil
			}
			w.waitGeneration(ctx, time.Second)
			continue
		}
		page, err := w.source.Events(ctx, state.Cursor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, event := range page.Items {
			duplicate := state.contains(event.Data.MessageID)
			for _, item := range w.runtime.Inbox {
				if item.Event.Data.MessageID == event.Data.MessageID {
					duplicate = true
					break
				}
			}
			if event.Kind == "message.committed" && event.Data.MessageID != "" && !duplicate && event.Data.Origin != "local_send" {
				if len(w.runtime.Inbox) >= 4000 {
					w.pruneRuntime()
					if len(w.runtime.Inbox) >= 5000 {
						return errors.New("inbox full; no message cursor discarded")
					}
				}
				item := inboxItem{Event: event, Phase: "received", Updated: time.Now().UTC()}
				if c, ok := w.model.(*companion.Engine); ok && c.Config.ConversationConfig().Enabled && !w.pendingConversationThread(event.Data.ThreadID) {
					item.AggregateUntil = w.collectionDeadline(event.Data.ThreadID, item.Updated, c.Config.ConversationConfig().CollectionWindow(event.Data.ThreadID))
					if !item.AggregateUntil.IsZero() && strings.HasSuffix(event.Data.ThreadID, "@chatroom") {
						item.CollectedAt = item.Updated
					}
				}
				w.runtime.Inbox = append(w.runtime.Inbox, item)
				if err = w.saveRuntime(); err != nil {
					return err
				}
				w.activity(Record{Event: "message_queued", Category: "received", ThreadID: event.Data.ThreadID, MessageID: event.Data.MessageID, Status: "queued", Reason: "durable_inbox", Origin: "wechat"})
			}
			state.Cursor = event.Cursor
			if event.Data.MessageID != "" {
				state.remember(event.Data.MessageID)
			}
			if err = saveCheckpoint(path, state); err != nil {
				return err
			}
		}
		if len(page.Items) < 100 {
			for _, p := range w.runtime.Plans {
				p.InboxChecked = true
			}
		}
		if err = w.drainInbox(ctx); err != nil {
			return err
		}
		// Exhaust the current event pages before any old result/caption is
		// submitted. A stop request on page two is already-arrived context too.
		if len(page.Items) >= 100 {
			continue
		}
		// Consume all already-arrived interruptions before starting an explicit
		// host continuation, just as with deferred media and captions below.
		w.processHostMediaResume(ctx)
		if w.persistErr != nil {
			return w.persistErr
		}
		w.reviewActiveGenerations(ctx)
		w.drainGenerations(ctx)
		// Read new instructions before emitting deferred old captions. In
		// particular, a queued change-song request must stop the old job first.
		w.reconcileMedia(ctx)
		w.reconcileMediaReplies(ctx)
		w.reconcileDeliveries(ctx)
		w.reconcileUtterancePlans(ctx)
		if w.persistErr != nil {
			return w.persistErr
		}
		if !w.draining() {
			w.tickCharacter(ctx)
		}
		w.waitGeneration(ctx, w.config.PollInterval)
	}
	return nil
}

func (w *Worker) peekVoiceInterruptions(ctx context.Context, cursor string, seen map[string]bool) {
	active := map[string]bool{}
	if w.generation != nil {
		for thread, j := range w.generation.jobs {
			if j.resourceKind == "voice" {
				active[thread] = true
			}
		}
	}
	for _, d := range w.runtime.Media {
		if d.Kind == "voice" && !d.terminal() && !d.UserCancelled {
			active[d.Input.Thread] = true
		}
	}
	if len(active) == 0 {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	page, err := w.source.Events(readCtx, cursor)
	if err != nil {
		return
	}
	for _, event := range page.Items {
		if !active[event.Data.ThreadID] || seen[event.Data.MessageID] || event.Kind != "message.committed" || event.Data.Origin == "local_send" {
			continue
		}
		message, err := w.source.Message(readCtx, event.Data.MessageID)
		if err != nil || message.ThreadID != event.Data.ThreadID || message.Direction != event.Data.Direction {
			continue
		}
		if message.Direction != "incoming" && message.ThreadID != "filehelper" {
			continue
		}
		seen[event.Data.MessageID] = true
		stoppedVoice := w.stopRequestedVoice(readCtx, message)
		if stoppedVoice && !replacesVoice(message.Body) {
			// Do not append a plain cancellation to an active generation.  The
			// stop has already been durably applied; a replacement is the only
			// interruption which needs to continue into normal intent handling.
			continue
		}
		if w.generation != nil {
			working := quotedPlainBody(message)
			if _, eligible := w.prompt(working, time.Now()); eligible {
				w.deferBusyGeneration(readCtx, working)
			}
		}
		if readCtx.Err() != nil {
			return
		}
	}
}
func (w *Worker) pruneRuntime() {
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	keep := []inboxItem{}
	for _, item := range w.runtime.Inbox {
		if !item.Updated.Before(cutoff) || inboxActivePhase(item.Phase) {
			keep = append(keep, item)
		}
	}
	if len(keep) > 3000 {
		kept := []inboxItem{}
		for i, item := range keep {
			if i >= len(keep)-3000 || inboxActivePhase(item.Phase) {
				kept = append(kept, item)
			}
		}
		keep = kept
	}
	w.runtime.Inbox = keep
	deliveries := []*delivery{}
	for _, d := range w.runtime.Deliveries {
		if d.State != "accepted" && d.State != "stopped" || d.Created.After(cutoff) {
			deliveries = append(deliveries, d)
		}
	}
	w.runtime.Deliveries = deliveries
}

func inboxActivePhase(phase string) bool {
	switch phase {
	case "received", "processing", "read_pending", "sending", "model_running", "tool_running", "following":
		return true
	}
	return false
}
