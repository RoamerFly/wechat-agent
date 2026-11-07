package bot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

// Once a group turn is eligible, resolve its attachments before composing a
// reply. Private conversations retain on-demand reading for passive media.
func (w *Worker) readMediaBeforeDecision(m linuxnative.Message) bool {
	engine, ok := w.model.(*companion.Engine)
	return strings.HasSuffix(m.ThreadID, "@chatroom") || w.hasScopedMedia(m) || m.Type == 34 || m.FileAttachmentName() != "" || !ok || engine.Tools == nil || w.generation == nil
}

func incomingObservationTool() mcptools.Tool {
	return mcptools.Tool{Name: "incoming_media_read", Description: "Read the actual image, sticker, video or share attached to this request, quote or unread batch. Returns an observation bound to the original message. Supply message_id to select a specific attachment.", InputSchema: json.RawMessage(`{"type":"object","properties":{"message_id":{"type":"string"}},"additionalProperties":false}`)}
}

func (w *Worker) readTurnMedia(ctx context.Context, in companion.Input, args json.RawMessage) (string, error) {
	var q struct {
		Message string `json:"message_id"`
	}
	// The message_id is optional. Native function callers may represent an
	// empty argument object as no bytes at all, which means “read the current
	// attachment”, not malformed JSON.
	if len(strings.TrimSpace(string(args))) > 0 && json.Unmarshal(args, &q) != nil {
		return "", errors.New("invalid_incoming_media_request")
	}
	if q.Message == "" {
		q.Message = in.TraceID
	}
	permitted := q.Message != "" && (q.Message == in.TraceID || in.QuotedSource != nil && in.QuotedSource.MessageID == q.Message)
	for _, m := range in.UnreadMessages {
		permitted = permitted || m.MessageID == q.Message
	}
	if !permitted || in.Preview || in.Surf || in.ReportOnly || in.Account != w.source.Account() || !w.allowed[in.Thread] {
		return "", errors.New("incoming_media_not_authorized")
	}
	m, err := w.source.Message(ctx, q.Message)
	if err != nil {
		return "", err
	}
	if m.ID != q.Message || m.ThreadID != in.Thread || m.AccountID != "" && m.AccountID != in.Account || m.Direction != "incoming" || !w.observationAllowed(m) || m.FileAttachmentName() != "" || m.IsQuotedText() {
		return "", errors.New("incoming_media_not_authorized")
	}
	switch m.Type {
	case 3, 47, 43, 49:
	default:
		return "", errors.New("incoming_media_type_unsupported")
	}
	var ref *companion.SourceRef
	var store *companion.Store
	if e, ok := w.model.(*companion.Engine); ok && e.Config.Permissions.Remember {
		store = e.Store
		lookup := companion.Input{Channel: in.Channel, Account: in.Account, Thread: in.Thread, Sender: m.SenderID, TraceID: m.ID, MessageTime: m.CreatedAt}
		bindInputSource(&lookup, m, e)
		var valid bool
		ref, valid = e.Store.SourceForInput(lookup)
		if !valid {
			return "", errors.New("incoming_media_source_unavailable")
		}
		event, err := e.Store.ReadEvent(ctx, []string{companion.ScopeID(in.Channel, in.Account, in.Thread)}, ref.EventID)
		if err != nil {
			return "", err
		}
		for _, obs := range event.IncomingMedia {
			if obs.Source.EventID == ref.EventID && obs.Source.Revision == ref.Revision && obs.ObservationState == "observed" && obs.Observation != "" {
				// A saved card description is useful context but not a terminal
				// share read. A later explicit tool call must still be allowed to
				// start the authorized page/video reader and replace it with
				// firsthand evidence. Old cards with an explicit metadata-only header can also be retried.
				if m.Type != 49 || !obs.CardDescriptionOnly() {
					return packObservation(ref, m.ID, obs.Observation, MediaObservationEvidence{ObservedRange: obs.ObservedRange, Excerpt: obs.ObservationExcerpt, Parts: obs.ObservationParts}), nil
				}
			}
		}
	}
	body, observationEvidence, err := w.analyzeIncomingMediaEvidence(ctx, m)
	if err != nil {
		return "", err
	}
	if store != nil {
		if err = store.CacheIncomingObservationWithCoverage(in.Channel, *ref, body, observationEvidence.ObservedRange, observationEvidence.Excerpt, observationEvidence.Parts...); err != nil {
			return "", err
		}
	}
	return packObservation(ref, m.ID, body, observationEvidence), nil
}

// packObservation keeps legacy results unchanged. Typed results expose each
// producer's actual text once, as a separately labelled part: publisher card
// copy, sampled visual observation and ASR transcript must never blur together
// in a synthetic display string.
func packObservation(ref *companion.SourceRef, id, body string, result MediaObservationEvidence) string {
	if len(result.Parts) == 0 {
		b, _ := json.Marshal(map[string]any{"source": ref, "source_message_id": id, "observation": body, "observation_state": "observed", "evidence_kind": "untrusted_media_observation"})
		return string(b)
	}
	state := "observed"
	if !companion.HasFirsthandObservation(result.Parts) {
		state = "card_only"
	}
	payload := map[string]any{"source": ref, "source_message_id": id, "observation_state": state, "evidence_kind": "typed_media_observation", "observation_parts": result.Parts}
	if text := strings.TrimSpace(result.ObservedRange); text != "" {
		payload["observed_range"] = text
	}
	b, _ := json.Marshal(payload)
	return string(b)
}
