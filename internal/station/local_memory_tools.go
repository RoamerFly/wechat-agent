package station

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

// localMemoryTools exposes only the current local conversation's persisted
// records. Its scope is minted once from the host-provided input: tool JSON
// never carries an account, thread, or scope selector.
type localMemoryTools struct {
	store *companion.Store
	scope string
}

func newLocalMemoryTools(store *companion.Store, input companion.Input) companion.ToolAccess {
	return &localMemoryTools{store: store, scope: companion.ScopeID(input.Channel, input.Account, input.Thread)}
}

func (t *localMemoryTools) List(context.Context) ([]mcptools.Tool, error) {
	return []mcptools.Tool{
		mcptools.NewTool("memory_search", "Search this local conversation's sourced memories or retained originals. Results include source/version and a continuation cursor.", `{"type":"object","properties":{"query":{"type":"string"},"kind":{"type":"string"},"subject":{"type":"string"},"since":{"type":"string"},"before":{"type":"string"},"cursor":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1},"originals":{"type":"boolean"},"before_message_id":{"type":"string"},"media_kind":{"type":"string","enum":["video","share","video_or_share","image","voice"]},"order":{"type":"string","enum":["time_desc"]},"metadata_only":{"type":"boolean"},"event_year":{"type":"integer","minimum":1,"maximum":9999},"time_basis":{"type":"string","enum":["statement","event"]},"min_similarity":{"type":"number","minimum":-1,"maximum":1},"pinned":{"type":"boolean"},"continuity":{"type":"boolean"},"commitment_state":{"type":"string","enum":["open","ended","cancelled"]}},"additionalProperties":false}`),
		mcptools.NewTool("memory_read", "Read one current local memory or original event by id. Event text and memory detail support UTF-8 byte pages; preserve the returned revision and continuation offsets.", `{"type":"object","properties":{"id":{"type":"string"},"event":{"type":"boolean"},"offset":{"type":"integer","minimum":0},"length":{"type":"integer","minimum":1,"maximum":32768},"limit":{"type":"integer","minimum":1,"maximum":32768},"expected_revision":{"type":"integer","minimum":1},"source_offset":{"type":"integer","minimum":0},"source_limit":{"type":"integer","minimum":1}},"required":["id"],"additionalProperties":false}`),
	}, nil
}

func strictLocalMemoryDecode(raw json.RawMessage, value any) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return errors.New("本地记忆参数无效")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("本地记忆参数无效")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("本地记忆参数无效")
	}
	return nil
}

type localMemorySearchInput struct {
	Query           string   `json:"query"`
	Kind            string   `json:"kind"`
	Subject         string   `json:"subject"`
	Since           string   `json:"since"`
	Before          string   `json:"before"`
	Cursor          string   `json:"cursor"`
	Offset          *int     `json:"offset"`
	Limit           *int     `json:"limit"`
	Originals       bool     `json:"originals"`
	BeforeMessageID string   `json:"before_message_id"`
	MediaKind       string   `json:"media_kind"`
	Order           string   `json:"order"`
	MetadataOnly    bool     `json:"metadata_only"`
	EventYear       int      `json:"event_year"`
	TimeBasis       string   `json:"time_basis"`
	MinSimilarity   *float64 `json:"min_similarity"`
	Pinned          bool     `json:"pinned"`
	Continuity      bool     `json:"continuity"`
	CommitmentState string   `json:"commitment_state"`
}

func (in localMemorySearchInput) query(scope string) (companion.MemoryQuery, error) {
	q := companion.MemoryQuery{Scopes: []string{scope}, Query: in.Query, Kind: in.Kind, Subject: in.Subject, Since: in.Since, Before: in.Before, Cursor: in.Cursor, Originals: in.Originals, BeforeMessageID: in.BeforeMessageID, MediaKind: in.MediaKind, Order: in.Order, MetadataOnly: in.MetadataOnly, EventYear: in.EventYear, TimeBasis: in.TimeBasis, MinSimilarity: in.MinSimilarity, Pinned: in.Pinned, Continuity: in.Continuity, CommitmentState: in.CommitmentState}
	if in.Offset != nil {
		if *in.Offset < 0 {
			return q, errors.New("记忆分页位置无效")
		}
		q.Offset = *in.Offset
	}
	if in.Limit != nil {
		if *in.Limit < 1 {
			return q, errors.New("记忆页大小须为正数；省略limit使用存储默认页大小")
		}
		// Store.SearchMemory intentionally owns its page-size boundary. Do not
		// silently reduce an explicit request here.
		q.Limit = *in.Limit
	}
	return q, nil
}

type localMemoryReadInput struct {
	ID           string `json:"id"`
	Event        bool   `json:"event"`
	Offset       *int   `json:"offset"`
	Length       *int   `json:"length"`
	Limit        *int   `json:"limit"`
	Expected     *int   `json:"expected_revision"`
	SourceOffset *int   `json:"source_offset"`
	SourceLimit  *int   `json:"source_limit"`
}

func (in localMemoryReadInput) page() (offset, length, sourceOffset, sourceLimit int, expected int, err error) {
	if strings.TrimSpace(in.ID) == "" {
		return 0, 0, 0, 0, 0, errors.New("需要记忆id或原始事件id")
	}
	if in.Offset != nil {
		offset = *in.Offset
	}
	if offset < 0 {
		return 0, 0, 0, 0, 0, errors.New("原文分页位置无效")
	}
	if in.Length != nil && in.Limit != nil {
		return 0, 0, 0, 0, 0, errors.New("请只提供length或limit中的一个原文页大小")
	}
	page := in.Length
	if page == nil {
		page = in.Limit
	}
	if page == nil {
		length = 4000
	} else {
		length = *page
		if length < 1 || length > 32768 {
			return 0, 0, 0, 0, 0, errors.New("原文页大小须为1–32768字节；超过传输页上限请按next_offset续读")
		}
	}
	if in.Expected != nil {
		expected = *in.Expected
		if expected < 1 {
			return 0, 0, 0, 0, 0, errors.New("expected_revision须为正数")
		}
	}
	if in.SourceOffset != nil {
		sourceOffset = *in.SourceOffset
	}
	if sourceOffset < 0 {
		return 0, 0, 0, 0, 0, errors.New("来源分页位置无效")
	}
	if in.SourceLimit != nil {
		sourceLimit = *in.SourceLimit
		if sourceLimit < 1 {
			return 0, 0, 0, 0, 0, errors.New("来源页大小须为正数")
		}
	} else {
		sourceLimit = 20
	}
	return offset, length, sourceOffset, sourceLimit, expected, nil
}

func localMemoryPage(body string, offset, length int) (string, int, error) {
	if offset > len(body) || offset > 0 && offset < len(body) && !utf8.RuneStart(body[offset]) {
		return "", 0, errors.New("原文分页位置无效")
	}
	end := offset + min(length, len(body)-offset)
	for end > offset && end < len(body) && !utf8.RuneStart(body[end]) {
		end--
	}
	if end == offset && end < len(body) {
		return "", 0, errors.New("分页长度不足以容纳一个UTF-8字符")
	}
	return body[offset:end], end, nil
}

func (t *localMemoryTools) Call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if t == nil || t.store == nil || t.scope == "" {
		return "", errors.New("本地记忆不可用")
	}
	switch name {
	case "memory_search":
		var in localMemorySearchInput
		if err := strictLocalMemoryDecode(raw, &in); err != nil {
			return "", err
		}
		query, err := in.query(t.scope)
		if err != nil {
			return "", err
		}
		value, err := t.store.SearchMemory(ctx, query)
		if err != nil {
			return "", err
		}
		return localMemoryJSON(value)
	case "memory_read":
		var in localMemoryReadInput
		if err := strictLocalMemoryDecode(raw, &in); err != nil {
			return "", err
		}
		offset, length, sourceOffset, sourceLimit, expected, err := in.page()
		if err != nil {
			return "", err
		}
		if in.Event {
			return t.readEvent(ctx, in.ID, offset, length, expected)
		}
		return t.readMemory(ctx, in.ID, offset, length, sourceOffset, sourceLimit, expected)
	default:
		return "", fmt.Errorf("未知本地记忆工具: %s", name)
	}
}

func (t *localMemoryTools) readEvent(ctx context.Context, id string, offset, length, expected int) (string, error) {
	ev, err := t.store.ReadEvent(ctx, []string{t.scope}, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", companion.RecordUnavailable{}
	}
	if err != nil {
		return "", err
	}
	if expected > 0 && expected != ev.Revision {
		return "", companion.ErrMemoryConflict
	}
	page, end, err := localMemoryPage(ev.Text, offset, length)
	if err != nil {
		return "", err
	}
	total := len(ev.Text)
	ev.OmitMediaObservationText()
	ev.Text = page
	return localMemoryJSON(map[string]any{"event": ev, "part": "text", "total_bytes": total, "next_offset": end, "has_more": end < total, "revision": ev.Revision, "requested_length": length, "effective_length": end - offset, "transport_page_bytes": 32768})
}

func (t *localMemoryTools) readMemory(ctx context.Context, id string, offset, length, sourceOffset, sourceLimit, expected int) (string, error) {
	memory, err := t.store.ReadMemory(ctx, []string{t.scope}, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", companion.RecordUnavailable{}
	}
	if err != nil {
		return "", err
	}
	if expected > 0 && expected != memory.Version {
		return "", companion.ErrMemoryConflict
	}
	if sourceOffset > len(memory.Sources) {
		return "", errors.New("来源分页位置无效")
	}
	page, end, err := localMemoryPage(memory.Detail, offset, length)
	if err != nil {
		return "", err
	}
	total, sourceTotal := len(memory.Detail), len(memory.Sources)
	sourceEnd := sourceOffset + min(sourceLimit, sourceTotal-sourceOffset)
	memory.Detail = page
	memory.Sources = memory.Sources[sourceOffset:sourceEnd]
	return localMemoryJSON(map[string]any{"entry": memory, "total_bytes": total, "next_offset": end, "has_more": end < total, "revision": memory.Version, "requested_length": length, "effective_length": end - offset, "source_count": sourceTotal, "next_source_offset": sourceEnd, "source_has_more": sourceEnd < sourceTotal, "transport_page_bytes": 32768})
}

func localMemoryJSON(value any) (string, error) {
	b, err := json.Marshal(value)
	return string(b), err
}

var _ companion.ToolAccess = (*localMemoryTools)(nil)
