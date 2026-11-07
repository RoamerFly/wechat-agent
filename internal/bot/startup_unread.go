package bot

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

// Read to the event tail before evaluating offline messages. A full page is not
// a conversation boundary. Cursor advances only after its message is persisted.
func (w *Worker) captureStartupUnread(ctx context.Context, path string, state *checkpoint) error {
	for !w.draining() && ctx.Err() == nil {
		page, err := w.source.Events(ctx, state.Cursor)
		if err != nil {
			return err
		}
		for _, event := range page.Items {
			duplicate := state.contains(event.Data.MessageID)
			for _, old := range w.runtime.Inbox {
				if old.Event.Data.MessageID == event.Data.MessageID {
					duplicate = true
					break
				}
			}
			if event.Kind == "message.committed" && event.Data.MessageID != "" && !duplicate && event.Data.Origin != "local_send" {
				if len(w.runtime.Inbox) >= 4000 {
					w.pruneRuntime()
					if len(w.runtime.Inbox) >= 5000 {
						return errors.New("startup inbox full; source cursor retained for continuation")
					}
				}
				w.runtime.Inbox = append(w.runtime.Inbox, inboxItem{Event: event, Phase: "received", Backlog: true, Updated: time.Now().UTC()})
				if err = w.saveRuntime(); err != nil {
					return err
				}
			}
			state.Cursor = event.Cursor
			if event.Data.MessageID != "" {
				state.remember(event.Data.MessageID)
			}
			if err = saveCheckpoint(path, *state); err != nil {
				return err
			}
		}
		if len(page.Items) < 100 {
			break
		}
	}
	for i := range w.runtime.Inbox {
		item := &w.runtime.Inbox[i]
		if item.OwnerModelReview {
			// An administrator's explicit single-message retry must retain its
			// original trigger/preferences path, not become an offline batch
			// whose policy can silently discard old messages as stale chatter.
			item.Backlog = false
			continue
		}
		if !item.ToolStarted && (item.Phase == "received" || item.Phase == "read_pending" || item.Phase == "processing" || item.Phase == "backlog_grouped") {
			item.Backlog = true
		}
	}
	return w.saveRuntime()
}

func (w *Worker) processUnreadConversation(ctx context.Context, thread string) error {
	return w.processConversationBatch(ctx, thread, nil)
}

func (w *Worker) processConversationBatch(ctx context.Context, thread string, live map[string]bool) error {
	c, ok := w.model.(*companion.Engine)
	if !ok {
		return nil
	}
	if w.generation != nil && (w.generation.jobs[thread] != nil || !w.generation.canGenerate()) {
		return nil
	}
	if w.config.AuthorizeThread != nil {
		check, cancel := context.WithTimeout(ctx, 10*time.Second)
		allowed, err := w.config.AuthorizeThread(check, thread)
		cancel()
		if err != nil {
			return nil
		}
		if allowed {
			w.allowed[thread] = true
		} else {
			delete(w.allowed, thread)
		}
	}
	var messages []linuxnative.Message
	var directedSender string
	sequences := map[string]int64{}
	for i := range w.runtime.Inbox {
		item := &w.runtime.Inbox[i]
		if live != nil && !live[item.Event.Data.MessageID] || live == nil && !item.Backlog || item.Event.Data.ThreadID != thread || item.ToolStarted || time.Now().Before(item.RetryAfter) {
			continue
		}
		if item.Phase != "received" && item.Phase != "read_pending" && item.Phase != "processing" && item.Phase != "backlog_grouped" {
			continue
		}
		if !w.allowed[thread] {
			w.queuePhase(item.Event.Data.MessageID, "skipped")
			continue
		}
		read, cancel := context.WithTimeout(ctx, 10*time.Second)
		m, err := w.source.Message(read, item.Event.Data.MessageID)
		cancel()
		if err != nil {
			w.queuePhase(item.Event.Data.MessageID, "read_pending")
			return w.persistErr
		}
		if m.AccountID != "" && m.AccountID != w.source.Account() || m.ThreadID != thread || m.ID != item.Event.Data.MessageID || m.Direction != item.Event.Data.Direction || !w.authorizedMessage(m) {
			w.queuePhase(item.Event.Data.MessageID, "skipped")
			continue
		}
		if live != nil && w.addressed(m) {
			// Keep independent directed requests under their own sender's authority.
			if directedSender != "" && directedSender != m.SenderID {
				break
			}
			directedSender = m.SenderID
		}
		w.recordIncoming(m)
		messages = append(messages, m)
		sequences[m.ID] = sourceSequence(item.Event)
	}
	if len(messages) == 0 {
		return w.persistErr
	}
	sort.SliceStable(messages, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339, messages[i].CreatedAt)
		b, _ := time.Parse(time.RFC3339, messages[j].CreatedAt)
		return a.Before(b)
	})
	now := time.Now()
	unread := []companion.ConversationEvent{}
	mediaIDs := []string{}
	preferredShares := map[string]bool{}
	var anchor *linuxnative.Message
	var anchorInput companion.Input
	var allIDs []string
	w.recovering = live == nil
	defer func() { w.recovering = false }()
	for _, m := range messages {
		if !w.messageInTime(m, now) {
			w.queuePhase(m.ID, "skipped")
			continue
		}
		if m.Direction != "incoming" && !(thread == "filehelper" && m.Direction == "outgoing") {
			w.queuePhase(m.ID, "completed")
			continue
		}
		working := m
		isMedia := m.Type == 3 || m.Type == 34 || m.Type == 43 || m.Type == 47 || m.Type == 49 && !m.IsQuotedText()
		onDemandAttachment := isMedia && w.config.OnDemandAttachment != nil && w.config.OnDemandAttachment(m)
		body := messageSummary(m)
		original := ""
		if m.Type == 1 {
			original = m.Body
		}
		if m.IsQuotedText() {
			original = quotedPlainBody(m).Body
			body = original + w.readQuoteContext(ctx, &m)
			working.Type = 1
			working.Body = body
		}
		if isMedia {
			working.Type = 1
			working.Body = "收到媒体"
			if !strings.HasSuffix(thread, "@chatroom") {
				working.Body = w.config.Prefix + working.Body
			}
		}
		preferredShare := w.preferredGroupShare(m)
		hostedPreferred := preferredShare && w.config.PreferredGroupShare != nil && w.config.ReadPreferredGroupShare != nil
		_, eligible := w.promptWithSharePreference(working, now, preferredShare)
		w.noticeConversationMessage(m, eligible)
		if stoppedVoice := w.stopRequestedVoice(ctx, working); stoppedVoice {
			if replacesVoice(working.Body) {
				// A verified replacement belongs to the recovered unread turn even
				// when the member did not repeat an @ mention.
				eligible = true
			} else {
				// Plain stops are durable local queue changes, not candidates for
				// the grouped model recovery turn.
				eligible = false
			}
		}
		selectAnchor := eligible && (live == nil || anchor == nil || !w.addressed(*anchor) || w.addressed(m))
		if selectAnchor && m.Type == 43 && w.hasScopedMedia(m) && !w.addressed(m) && anchor != nil && w.addressed(*anchor) && (anchor.Type == 1 || anchor.IsQuotedText()) {
			selectAnchor = false
		}
		if selectAnchor {
			copy := m
			anchor = &copy
		}
		allIDs = append(allIDs, m.ID)
		if hostedPreferred {
			preferredShares[m.ID] = true
		}
		canAnalyze := isMedia && w.mediaAnalyzer(ctx, m) != nil
		needsGroupRead := strings.HasSuffix(thread, "@chatroom") && (!w.hasScopedMedia(m) || canAnalyze)
		if isMedia && !onDemandAttachment && !hostedPreferred && w.readMediaBeforeDecision(m) && (needsGroupRead || eligible && w.observationAllowed(m) && canAnalyze) {
			mediaIDs = append(mediaIDs, m.ID)
		}
		observed := companion.Input{Channel: w.config.Channel, Account: w.source.Account(), Thread: thread, Sender: m.SenderID, TraceID: m.ID, Text: body, MessageTime: m.CreatedAt, OriginalText: &original}
		bindInputSource(&observed, m, c)
		observed.Source.Sequence = sequences[m.ID]
		if err := c.Store.Observe(observed, c.Config, now); err != nil {
			return err
		}
		if selectAnchor {
			anchorInput = observed
		}
		unread = append(unread, companion.ConversationEvent{MessageID: m.ID, Sender: m.SenderID, Text: body, At: m.CreatedAt, NewRequest: eligible, Source: observed.Source, ReplyTo: observed.QuotedSource})
	}
	if anchor == nil {
		for _, id := range allIDs {
			w.queuePhase(id, "skipped")
		}
		return w.persistErr
	}
	// Use a real source message for routing/authorization. Other speakers remain
	// contextual data, never aliases for the anchor's authenticated sender.
	in := companion.Input{Channel: w.config.Channel, Account: w.source.Account(), Thread: thread, Sender: anchor.SenderID, TraceID: anchor.ID, MessageTime: anchor.CreatedAt, QuoteServerID: anchor.ServerID, Mode: "social", IsGroup: strings.HasSuffix(thread, "@chatroom"), Directed: w.addressed(*anchor), PersonaOverride: w.config.GroupRules[thread].Persona, UnreadMessages: unread, UnreadMediaIDs: mediaIDs, HistoryObserved: true,
		Text: "这是重新上线后本会话尚未查看的一段未读消息。按unread_messages里的真实时间和发言者整体理解，结合后续消息判断哪些事项仍需要回复或行动；不逐条追着旧话补答。自然回应，也可以安静。", GroupShareSummary: preferredShares[anchor.ID]}
	if live != nil {
		for i := range unread {
			if unread[i].Sender != anchor.SenderID {
				unread[i].NewRequest = false
			}
		}
		in.Text = "这是当前群聊同一收集窗口的消息。按时间、发送者、引用和实际附件内容一起理解，仅对当前仍有效的请求作一次决定。其他成员不能变更原请求者的任务或授权。"
	}
	// The digest is runtime context. Bind its dispatch to the actual incoming
	// message, without treating the digest as the anchor author's literal words.
	in.Source, in.QuotedSource = anchorInput.Source, anchorInput.QuotedSource
	in.IncomingMedia = append([]companion.IncomingMediaRef(nil), anchorInput.IncomingMedia...)
	in.HistoryObserved = true
	if c.Config.Permissions.Remember {
		if source, ok := c.Store.SourceForInput(anchorInput); ok {
			in.Source = source
		}
	}
	if !in.IsGroup || in.Directed || w.config.GroupRules[thread].Mode == "all" {
		in.Mode = "direct"
	}
	if in.GroupShareSummary {
		// This mode is host-authorized but not a fabricated @ mention. The
		// durable marker is preserved through model retry/restart and is
		// rechecked by the host before delivery.
		if !w.addressed(*anchor) {
			in.Mode, in.Directed, in.MustReply = "direct", false, false
		}
		if err := w.persistPreferredGroupShareSource(c, &in); err != nil {
			w.stopUnreadableTurn(in, err)
			return w.persistErr
		}
	}
	if in.Directed && anchor.Mentions.Self != nil && *anchor.Mentions.Self {
		in.MustReply = w.config.GroupRules[thread].ForceReplyWhenMentioned
	}
	for i := range w.runtime.Inbox {
		item := &w.runtime.Inbox[i]
		for _, id := range allIDs {
			if item.Event.Data.MessageID == id {
				item.BacklogParent = anchor.ID
				item.Phase = "backlog_grouped"
				if id == anchor.ID {
					item.Phase = "received"
				}
				break
			}
		}
	}
	if err := w.saveRuntime(); err != nil {
		return err
	}
	eventName, groupEvent, reason, title := "backlog_conversation_started", "backlog_message_grouped", "offline_conversation_batch", "按会话综合处理未读消息"
	if live != nil {
		eventName, groupEvent, reason, title = "conversation_batch_started", "message_aggregated", "group_collection_window", "合并处理本轮群消息"
	}
	for _, id := range allIDs {
		if id != anchor.ID {
			w.activity(Record{Event: groupEvent, Category: "decision", ThreadID: thread, MessageID: id, RelatedMessageID: anchor.ID, Reason: reason, Status: "aggregated"})
		}
	}
	w.activity(Record{Event: eventName, Category: "decision", ThreadID: thread, MessageID: anchor.ID, Content: title, Reason: reason, Status: "processing"})
	if w.generation != nil {
		w.startGeneration(ctx, c, in, nil)
	} else {
		if in.GroupShareSummary {
			if err := w.readPreparedPreferredGroupShare(ctx, c, &in, *anchor); err != nil {
				w.stopUnreadableTurn(in, err)
				return w.persistErr
			}
		}
		if err := w.prepareBatchObservations(ctx, c, &in, w.activity); err != nil {
			var failure *groupObservationFailure
			if errors.As(err, &failure) {
				w.stopUnreadableTurn(in, failure)
				return w.persistErr
			}
			return err
		}
		w.respondCharacter(ctx, c, in)
	}
	return w.persistErr
}

// Reads may block; the production caller runs this on its generation worker.
// The store serializes each source-scoped observation update transaction.
func (w *Worker) prepareBatchObservations(ctx context.Context, c *companion.Engine, in *companion.Input, report func(Record)) error {
	required := make(map[string]bool, len(in.UnreadMediaIDs))
	for _, id := range in.UnreadMediaIDs {
		required[id] = true
	}
	for i := range in.UnreadMessages {
		event := &in.UnreadMessages[i]
		id := event.MessageID
		if !required[id] {
			continue
		}
		delete(required, id)
		read, stop := context.WithTimeout(ctx, 3*time.Minute)
		m, err := w.source.Message(read, id)
		var summary string
		var observationEvidence MediaObservationEvidence
		if err == nil && (m.ID != id || m.ThreadID != in.Thread || m.SenderID != event.Sender || m.Direction != "incoming" && in.Thread != "filehelper" || m.AccountID != "" && m.AccountID != in.Account) {
			err = errors.New("media_source_scope_mismatch")
		}
		if err == nil {
			if !w.observationAllowed(m) {
				err = errors.New("media_understanding_disabled")
			} else {
				summary, observationEvidence, err = w.readRequiredMediaWithEvidence(read, m)
			}
		}
		stop()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil && in.IsGroup {
			if w.hasScopedMedia(linuxnative.Message{Type: 43}) && independentGroupTextRequest(in, *event) {
				event.MediaReadFailed = true
				report(Record{Event: "background_video_read_failed", Category: "tool", ThreadID: in.Thread, MessageID: in.TraceID,
					RelatedMessageID: id, Status: "incomplete", Reason: observationReason(err), Content: "背景视频未读到内容，继续独立文字请求。"})
				continue
			}
			return &groupObservationFailure{reason: observationReason(err), messageID: id}
		}
		if err != nil {
			event.Text += "\n媒体读取未完成：\n" + failedObservation(err)
			continue
		}
		before := event.Text
		event.Text += "\n实际媒体观察（对话资料）：\n" + summary
		if _, err := c.Store.UpdateObservationWithCoverage(companion.ScopeID(in.Channel, in.Account, in.Thread), id, before, event.Text, observationEvidence.ObservedRange, observationEvidence.Excerpt, observationEvidence.Parts...); err != nil {
			return err
		}
	}
	if len(required) > 0 {
		return &groupObservationFailure{reason: "media_batch_source_missing", messageID: in.TraceID}
	}
	return nil
}

// A different sender's ordinary video can remain background for a verified
// text request. Keep the existing failure policy when it is the actual target,
// shares the requester, or is explicitly referenced anywhere in this batch.
func independentGroupTextRequest(in *companion.Input, media companion.ConversationEvent) bool {
	if !in.Directed || media.Source == nil || media.Source.MessageType != 43 || media.MessageID == in.TraceID || media.Sender == "" || media.Sender == in.Sender {
		return false
	}
	anchorText := false
	for _, event := range in.UnreadMessages {
		if event.ReplyTo != nil && event.ReplyTo.MessageID == media.MessageID {
			return false
		}
		if event.MessageID == in.TraceID && event.Sender == in.Sender && event.NewRequest && event.Source != nil {
			anchorText = event.Source.MessageType == 1 || event.Source.MessageType == 49 && event.ReplyTo != nil
		}
	}
	return anchorText
}
