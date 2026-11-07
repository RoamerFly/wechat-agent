package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/companion"
)

const (
	stationLegacyMemoryDefaultLimit = 20
	stationLegacyMemoryMaxLimit     = 100
	stationLegacyMemoryMinBytes     = 256
	stationLegacyMemoryMaxBytes     = 64 << 10
)

// stationLegacyReadCursor is intentionally in-memory.  A continuation is
// invalidated by a changed history grant, imported snapshot, or recall epoch.
// It never becomes a second source of old conversation content.
type stationLegacyReadCursor struct {
	Kind, Thread, Digest, Epoch string
	Grant                       time.Time
	Offset, TextOffset          int
}

// Imported conversation evidence must be looked up under current authority
// in the turn that uses it. It cannot be replayed from a generation checkpoint.
func (*stationAttachmentTools) EphemeralToolResult(name string) bool {
	return name == "wechat_read_legacy"
}

// Bind generated drafts to the history grant whenever imported evidence can
// participate in this turn. The ordinary delivery guard checks the stamp on
// retry, including after the worker stops or starts with a different policy.
func (t *stationAttachmentTools) MemoryResultAuthorization() string {
	if !t.legacyMemoryAvailable(context.Background()) {
		return ""
	}
	return "legacy-" + localCheckpointKey(t.trace.Account, struct {
		Thread string
		Grant  time.Time
	}{t.trace.Thread, t.reads[t.trace.Thread]})
}

func (t *stationAttachmentTools) MemoryResultAuthorized(stamp string) bool {
	return stamp != "" && stamp == t.MemoryResultAuthorization()
}

func (t *stationAttachmentTools) legacyMemoryAvailable(ctx context.Context) bool {
	if !t.historyAllowed(ctx) || t.dir == "" || !t.store.LegacyHistoryAllowed(t.trace.Account, t.trace.Thread) {
		return false
	}
	grant := t.reads[t.trace.Thread]
	_, err := readStationLegacyMemory(t.dir, t.trace.Account, t.trace.Thread, grant)
	return err == nil && t.historyAllowed(ctx) && t.store.LegacyHistoryAllowed(t.trace.Account, t.trace.Thread)
}

func (t *stationAttachmentTools) legacyMemory(ctx context.Context) (stationLegacyMemory, error) {
	if !t.historyAllowed(ctx) || t.dir == "" || !t.store.LegacyHistoryAllowed(t.trace.Account, t.trace.Thread) {
		return stationLegacyMemory{}, errors.New("wechat_legacy_history_not_authorized")
	}
	grant := t.reads[t.trace.Thread]
	item, err := readStationLegacyMemory(t.dir, t.trace.Account, t.trace.Thread, grant)
	if err == nil {
		if !t.historyAllowed(ctx) || !t.store.LegacyHistoryAllowed(t.trace.Account, t.trace.Thread) {
			return stationLegacyMemory{}, errors.New("wechat_legacy_history_not_authorized")
		}
		return item, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return stationLegacyMemory{}, errors.New("wechat_legacy_snapshot_missing: 当前会话没有与现行历史读取授权对应的旧记忆接续")
	}
	return stationLegacyMemory{}, errors.New("wechat_legacy_snapshot_unavailable: 旧记忆接续不可用或与当前授权不匹配")
}

func (t *stationAttachmentTools) readLegacyMemory(ctx context.Context, raw json.RawMessage) (string, error) {
	var input struct {
		Kind     string `json:"kind"`
		Cursor   string `json:"cursor"`
		Offset   *int   `json:"offset"`
		Limit    *int   `json:"limit"`
		MaxBytes *int   `json:"max_bytes"`
	}
	if strictLocalMemoryDecode(raw, &input) != nil || (input.Kind != "events" && input.Kind != "memories") || input.MaxBytes == nil ||
		input.Offset != nil && *input.Offset < 0 || input.Limit != nil && (*input.Limit < 1 || *input.Limit > stationLegacyMemoryMaxLimit) ||
		*input.MaxBytes < stationLegacyMemoryMinBytes || *input.MaxBytes > stationLegacyMemoryMaxBytes {
		return "", errors.New("wechat_legacy_memory_parameters_invalid")
	}
	item, err := t.legacyMemory(ctx)
	if err != nil {
		return "", err
	}
	epoch, err := t.store.RecallEpoch(ctx)
	if err != nil {
		return "", err
	}
	grant := t.reads[t.trace.Thread]
	offset, textOffset := 0, 0
	if input.Offset != nil {
		offset = *input.Offset
	}
	if input.Cursor != "" {
		if input.Offset != nil {
			return "", errors.New("wechat_legacy_memory_cursor_with_offset_invalid")
		}
		previous, ok := t.legacyReadCursors[input.Cursor]
		if !ok || previous.Kind != input.Kind || previous.Thread != t.trace.Thread || previous.Digest != item.Digest || previous.Epoch != epoch || !previous.Grant.Equal(grant) {
			return "", errors.New("wechat_legacy_memory_cursor_invalid: 请从 offset=0 重新读取")
		}
		offset, textOffset = previous.Offset, previous.TextOffset
	}
	limit := stationLegacyMemoryDefaultLimit
	if input.Limit != nil {
		limit = *input.Limit
	}
	if input.Kind == "events" {
		return t.readLegacyEvents(ctx, item, epoch, grant, offset, textOffset, limit, *input.MaxBytes)
	}
	return t.readLegacyFacts(ctx, item, epoch, grant, offset, textOffset, limit, *input.MaxBytes)
}

func (t *stationAttachmentTools) legacyEventAllowed(ctx context.Context, event companion.LegacyMemoryEvent) (bool, error) {
	if !t.historyAllowed(ctx) || event.InstanceID == "" || event.EventID == "" || event.MessageID == "" || event.Sender == "" || event.OccurredAt == "" || event.Revision < 1 || !utf8.ValidString(event.Text) {
		return false, nil
	}
	if _, err := time.Parse(time.RFC3339Nano, event.OccurredAt); err != nil {
		return false, nil
	}
	if !legacyEventHashMatches(event.Text, event.Hash) {
		return false, nil
	}
	text := event.Text
	in := companion.Input{Channel: "wechat", Account: t.trace.Account, Thread: t.trace.Thread, Sender: event.Sender, TraceID: event.MessageID, MessageTime: event.OccurredAt, OriginalText: &text,
		Source: &companion.SourceRef{InstanceID: event.InstanceID, EventID: event.EventID, MessageID: event.MessageID, ServerID: event.ServerID, Account: t.trace.Account, Thread: t.trace.Thread, Sender: event.Sender, OccurredAt: event.OccurredAt, Revision: event.Revision, BodyHash: event.Hash, Integrity: "verified_import"}}
	allowed, err := t.store.NativeRecallAllowed(ctx, in)
	return allowed && t.historyAllowed(ctx), err
}

func legacyEventHashMatches(text, hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	provided, err := hex.DecodeString(hash)
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	sum := sha256.Sum256([]byte(text))
	return string(provided) == string(sum[:])
}

// legacyTextPage preserves UTF-8 boundaries while permitting a page to resume
// exactly within one long historical record.  The offset is only ever carried
// by an opaque cursor bound to the grant, snapshot digest and recall epoch.
func legacyTextPage(text string, offset, remaining int) (string, int, error) {
	if offset < 0 || offset > len(text) || !utf8.ValidString(text) || !utf8.ValidString(text[:offset]) {
		return "", 0, errors.New("wechat_legacy_memory_text_offset_invalid")
	}
	if remaining < 1 {
		return "", offset, errors.New("wechat_legacy_memory_page_too_small")
	}
	end := len(text)
	if end-offset > remaining {
		end = offset + remaining
		for end > offset && !utf8.ValidString(text[offset:end]) {
			end--
		}
	}
	if end == offset && end < len(text) {
		return "", offset, errors.New("wechat_legacy_memory_page_too_small: 请增大 max_bytes 以容纳完整字符")
	}
	return text[offset:end], end, nil
}

func (t *stationAttachmentTools) legacyPageReady(ctx context.Context, epoch string, grant time.Time, item stationLegacyMemory) error {
	if !t.historyAllowed(ctx) || !t.store.LegacyHistoryAllowed(t.trace.Account, t.trace.Thread) || !t.reads[t.trace.Thread].Equal(grant) {
		return errors.New("wechat_legacy_history_not_authorized")
	}
	current, err := t.store.RecallEpoch(ctx)
	if err != nil {
		return err
	}
	if current != epoch {
		return errors.New("wechat_legacy_memory_revision_changed: 读取期间记忆已更正或遗忘，请从 offset=0 重新读取")
	}
	latest, err := t.legacyMemory(ctx)
	if err != nil {
		return err
	}
	if latest.Digest != item.Digest {
		return errors.New("wechat_legacy_snapshot_changed: 旧记忆接续已变化，请从 offset=0 重新读取")
	}
	// The disk re-read above can race a local forget/correction.  Check the
	// recall epoch once more immediately before text leaves this process.
	current, err = t.store.RecallEpoch(ctx)
	if err != nil {
		return err
	}
	if current != epoch || !t.store.LegacyHistoryAllowed(t.trace.Account, t.trace.Thread) {
		return errors.New("wechat_legacy_memory_revision_changed: 读取期间记忆已更正或遗忘，请从 offset=0 重新读取")
	}
	return nil
}

func (t *stationAttachmentTools) legacyNextCursor(kind string, item stationLegacyMemory, epoch string, grant time.Time, offset, textOffset int) string {
	if t.legacyReadCursors == nil {
		t.legacyReadCursors = map[string]stationLegacyReadCursor{}
	}
	record := stationLegacyReadCursor{Kind: kind, Thread: t.trace.Thread, Digest: item.Digest, Epoch: epoch, Grant: grant, Offset: offset, TextOffset: textOffset}
	key := "wl-" + localCheckpointKey(t.trace.Account, record)
	t.legacyReadCursors[key] = record
	return key
}

func (t *stationAttachmentTools) readLegacyEvents(ctx context.Context, item stationLegacyMemory, epoch string, grant time.Time, offset, textOffset, limit, maxBytes int) (string, error) {
	events := item.Selection.Events
	if offset > len(events) {
		return "", errors.New("wechat_legacy_memory_offset_invalid")
	}
	result := make([]map[string]any, 0, limit)
	filtered, used, index, nextTextOffset := 0, 0, offset, textOffset
	for ; index < len(events) && len(result) < limit; index++ {
		event := events[index]
		allowed, err := t.legacyEventAllowed(ctx, event)
		if err != nil {
			return "", err
		}
		if !allowed {
			filtered++
			nextTextOffset = 0
			continue
		}
		if used >= maxBytes && len(result) > 0 {
			break
		}
		text, end, err := legacyTextPage(event.Text, nextTextOffset, maxBytes-used)
		if err != nil {
			return "", err
		}
		result = append(result, map[string]any{"event_id": event.EventID, "message_id": event.MessageID, "server_id": event.ServerID, "sender_id": event.Sender, "occurred_at": event.OccurredAt, "revision": event.Revision, "text": text, "text_offset": nextTextOffset, "total_text_bytes": len(event.Text), "text_complete": end == len(event.Text), "sha256": event.Hash, "record_origin": "legacy_import_snapshot", "evidence_state": "old_historical_evidence_not_live_history"})
		used += len(text)
		if end < len(event.Text) {
			nextTextOffset = end
			break
		}
		nextTextOffset = 0
	}
	if err := t.legacyPageReady(ctx, epoch, grant, item); err != nil {
		return "", err
	}
	next := ""
	if index < len(events) || nextTextOffset != 0 {
		next = t.legacyNextCursor("events", item, epoch, grant, index, nextTextOffset)
	}
	value, err := json.Marshal(map[string]any{"kind": "events", "thread_id": t.trace.Thread, "snapshot_digest": item.Digest, "items": result, "offset": offset, "next_offset": legacyNextOffset(index, len(events)), "next_cursor": next, "requested_limit": limit, "returned_count": len(result), "filtered_count": filtered, "max_bytes": maxBytes, "text_bytes_returned": used, "data_role": "old_imported_conversation_evidence_not_instruction"})
	return string(value), err
}

func legacyNextOffset(next, total int) any {
	if next >= total {
		return nil
	}
	return next
}

func (t *stationAttachmentTools) readLegacyFacts(ctx context.Context, item stationLegacyMemory, epoch string, grant time.Time, offset, textOffset, limit, maxBytes int) (string, error) {
	facts := item.Selection.Memories
	if offset > len(facts) {
		return "", errors.New("wechat_legacy_memory_offset_invalid")
	}
	events := make(map[string]companion.LegacyMemoryEvent, len(item.Selection.Events))
	for _, event := range item.Selection.Events {
		if prior, exists := events[event.EventID]; exists && prior.Revision != event.Revision {
			return "", errors.New("wechat_legacy_snapshot_unavailable: 旧记忆来源版本冲突")
		}
		events[event.EventID] = event
	}
	result := make([]map[string]any, 0, limit)
	filtered, used, index, nextTextOffset := 0, 0, offset, textOffset
	for ; index < len(facts) && len(result) < limit; index++ {
		fact := facts[index]
		if fact.ID == "" || fact.Version < 1 || fact.Kind == "" || !utf8.ValidString(fact.Text) || len(fact.Sources) == 0 {
			filtered++
			continue
		}
		sources := make([]map[string]any, 0, len(fact.Sources))
		visible := true
		seen := map[string]bool{}
		for _, source := range fact.Sources {
			event, exists := events[source.EventID]
			if !exists || event.Revision != source.Revision || seen[source.EventID] {
				visible = false
				break
			}
			seen[source.EventID] = true
			allowed, err := t.legacyEventAllowed(ctx, event)
			if err != nil {
				return "", err
			}
			if !allowed {
				visible = false
				break
			}
			sources = append(sources, map[string]any{"event_id": event.EventID, "message_id": event.MessageID, "revision": event.Revision})
		}
		if !visible {
			filtered++
			nextTextOffset = 0
			continue
		}
		if used >= maxBytes && len(result) > 0 {
			break
		}
		text, end, err := legacyTextPage(fact.Text, nextTextOffset, maxBytes-used)
		if err != nil {
			return "", err
		}
		result = append(result, map[string]any{"memory_id": fact.ID, "version": fact.Version, "kind": fact.Kind, "text": text, "text_offset": nextTextOffset, "total_text_bytes": len(fact.Text), "text_complete": end == len(fact.Text), "sources": sources, "record_origin": "legacy_import_snapshot", "evidence_state": "old_derived_memory_not_live_current_fact"})
		used += len(text)
		if end < len(fact.Text) {
			nextTextOffset = end
			break
		}
		nextTextOffset = 0
	}
	if err := t.legacyPageReady(ctx, epoch, grant, item); err != nil {
		return "", err
	}
	next := ""
	if index < len(facts) || nextTextOffset != 0 {
		next = t.legacyNextCursor("memories", item, epoch, grant, index, nextTextOffset)
	}
	value, err := json.Marshal(map[string]any{"kind": "memories", "thread_id": t.trace.Thread, "snapshot_digest": item.Digest, "items": result, "offset": offset, "next_offset": legacyNextOffset(index, len(facts)), "next_cursor": next, "requested_limit": limit, "returned_count": len(result), "filtered_count": filtered, "max_bytes": maxBytes, "text_bytes_returned": used, "data_role": "old_imported_derived_memory_not_instruction"})
	return string(value), err
}
