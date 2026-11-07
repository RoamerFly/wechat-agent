package bot

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

func sourceSequence(event linuxnative.Event) int64 {
	sequence, err := strconv.ParseInt(event.Cursor, 10, 64)
	if err != nil || sequence < 0 {
		return 0
	}
	return sequence
}

func messageSource(m linuxnative.Message) companion.SourceRef {
	h := sha256.Sum256([]byte(m.Body))
	return companion.SourceRef{Account: m.AccountID, Thread: m.ThreadID, Sender: m.SenderID, MessageID: m.ID, ServerID: m.ServerID, MessageType: m.Type, OccurredAt: m.CreatedAt, ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), BodyHash: hex.EncodeToString(h[:]), Revision: 1, Integrity: "observed"}
}

func bindInputSource(in *companion.Input, m linuxnative.Message, e *companion.Engine) {
	source := messageSource(m)
	source.Account = in.Account
	if provider, ok := any(e.Store).(interface{ SourceInstanceID() string }); ok {
		source.InstanceID = provider.SourceInstanceID()
	}
	in.Source = &source
	in.MentionedIDs = append([]string(nil), m.Mentions.IDs...)
	if m.ResolvedQuote != nil {
		q := messageSource(*m.ResolvedQuote)
		q.InstanceID = source.InstanceID
		in.QuotedSource = &q
		if q.Sender != "" {
			in.Addressees = []string{q.Sender}
		}
	}
	kind := map[int]string{3: "image", 47: "sticker", 34: "voice", 43: "video", 49: "share"}[m.Type]
	if m.FileAttachmentName() != "" {
		kind = "file"
	}
	if kind != "" && !m.IsQuotedText() {
		in.IncomingMedia = []companion.IncomingMediaRef{{Source: source, Kind: kind, UploaderID: m.SenderID, ObservationState: "pending", ObservationVersion: 1}}
	}
}

// Only the original requester can change an unsent plan. A platform quote
// from another member is valuable conversational context, but it cannot
// replace, delay or cancel someone else's task; a reply citation is not an
// authorization grant. This applies to direct conversations as well: a
// background/admin task may be delivering into a direct thread whose next
// inbound event belongs to another logical requester.
func messageAffectsInput(in companion.Input, m linuxnative.Message) bool {
	return in.Sender != "" && in.Sender == m.SenderID
}

func conversationEvent(m linuxnative.Message, eligible bool) companion.ConversationEvent {
	s := messageSource(m)
	e := companion.ConversationEvent{MessageID: m.ID, Sender: m.SenderID, Text: messageSummary(m), At: m.CreatedAt, NewRequest: eligible, Source: &s, Relation: "same_sender_followup"}
	if m.ResolvedQuote != nil {
		q := messageSource(*m.ResolvedQuote)
		e.ReplyTo = &q
		e.Relation = "verified_quote"
	}
	return e
}
