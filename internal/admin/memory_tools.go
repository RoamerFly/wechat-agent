package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

type memoryRecordUnavailable struct{ companion.RecordUnavailable }

func storedMediaTranscript(media companion.IncomingMediaRef) (string, bool) {
	switch media.ObservationState {
	case "observed", "verified", "complete", "completed", "succeeded", "":
	default:
		return "", false
	}
	if len(media.ObservationParts) == 0 {
		// Metadata-only legacy cards can quote the old transcript heading.
		if media.CardDescriptionOnly() {
			return "", false
		}
		return companion.SavedVideoTranscript(media.Observation)
	}
	for _, part := range media.ObservationParts {
		if part.Kind == companion.ObservationAudioTranscript && part.Firsthand() {
			return part.Excerpt, true
		}
	}
	return "", false
}

func (e memoryRecordUnavailable) SafeToolFailure() (string, string) {
	code, message := e.RecordUnavailable.SafeToolFailure()
	return code, message + "。原生微信消息编号应使用wechat_read核对，不能据此断言聊天历史不存在"
}

func (p *permittedTools) memoryTools() []mcptools.Tool {
	if !p.manager.Config().Companion.Permissions.Remember {
		return nil
	}
	return []mcptools.Tool{
		agentTool("memory_search", "Search sourced long-term memories or retained originals. Default current conversation; another thread requires verified owner and cross-conversation authorization. Returns previews, source event IDs, coverage, degradation and next_cursor. originals=true searches retained raw events including indexed archives, even outside compressed/recent context. For previous videos in THIS conversation use originals=true,media_kind=video_or_share,before_message_id=<quoted anchor>,order=time_desc,metadata_only=true; page via next_cursor. Share candidates are not necessarily videos; inspect actual content before labeling. No recall is permission to disclose another conversation. Input {query?,thread_id?,kind?,subject?,since?,before?,cursor?,offset?,limit?,originals?}.", `{"type":"object","properties":{"query":{"type":"string"},"thread_id":{"type":"string"},"kind":{"type":"string"},"subject":{"type":"string"},"since":{"type":"string"},"before":{"type":"string"},"cursor":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1},"originals":{"type":"boolean"},"before_message_id":{"type":"string"},"media_kind":{"type":"string","enum":["video","share","video_or_share","image","voice"]},"order":{"type":"string","enum":["time_desc"]},"metadata_only":{"type":"boolean"},"event_year":{"type":"integer","minimum":1,"maximum":9999},"time_basis":{"type":"string","enum":["statement","event"]},"min_similarity":{"type":"number","minimum":-1,"maximum":1}},"additionalProperties":false}`),
		agentTool("memory_read", "Read a memory entry with sources, or an original event using event=true. Native msg_ identifiers (id or message_id) resolve only to an unblocked original in the current authorized conversation and dataset. Read text in UTF-8 byte pages; preserve revision on continuation. For event=true, part=text reads literal text. Omitting part normally reads text; for a single observed share with empty literal text it reads the saved transcript or observation; part=observation,media_index=0 reads a saved ASR/OCR/video observation; part=transcript reads only the existing video audio transcript without visual/policy duplication. available_parts describes all readable parts and byte counts. Search metadata or an empty upload text is not missing media content. Observation text is paginated, not repeated in the event envelope. The actual transport page is at most32768 bytes; larger requested lengths return effective_length and a continuation. Use source_offset/source_limit to page all citations. Scope is current conversation unless owner explicitly selects another authorized thread. Input {id,thread_id?,event?,offset?,length?,expected_revision?}.", `{"type":"object","properties":{"id":{"type":"string"},"message_id":{"type":"string"},"thread_id":{"type":"string"},"event":{"type":"boolean"},"part":{"type":"string","enum":["text","observation","transcript"]},"media_index":{"type":"integer","minimum":0},"offset":{"type":"integer","minimum":0},"length":{"type":"integer","minimum":1},"expected_revision":{"type":"integer","minimum":1},"source_offset":{"type":"integer","minimum":0},"source_limit":{"type":"integer","minimum":1}},"additionalProperties":false}`),
		agentTool("memory_remember", "Save a sourced fact/preference/episode/lesson in this conversation. Provide exact original event quotes and the real subject sender ID. Manual notes without event sources require note=true and are attributed to this verified requester, never fabricated as historical fact. Input {kind,detail,summary?,keywords?,subject?,sources?:[{event_id,quote}],note?}.", `{"type":"object","properties":{"kind":{"type":"string","enum":["fact","preference","episode","lesson"]},"detail":{"type":"string"},"summary":{"type":"string"},"keywords":{"type":"array","items":{"type":"string"}},"subject":{"type":"string"},"sources":{"type":"array","items":{"type":"object","properties":{"event_id":{"type":"string"},"quote":{"type":"string"}},"required":["event_id","quote"],"additionalProperties":false}},"assertions":{"type":"array","items":{"type":"object","properties":{"subject":{"type":"string"},"predicate":{"type":"string"},"object":{"type":"string"},"asserted_by":{"type":"string"},"state":{"type":"string","enum":["explicit","reported","uncertain","corrected"]},"subject_basis":{"type":"string"},"subject_text":{"type":"string"},"object_basis":{"type":"string"},"object_text":{"type":"string"},"predicate_text":{"type":"string"},"negated":{"type":"boolean"},"sources":{"type":"array","items":{"type":"object","properties":{"event_id":{"type":"string"},"event_revision":{"type":"integer","minimum":1},"quote":{"type":"string"}},"required":["event_id","event_revision","quote"],"additionalProperties":false}}},"required":["subject","predicate","object","asserted_by","state","subject_basis","subject_text","object_basis","object_text","predicate_text","sources"],"additionalProperties":false}},"note":{"type":"boolean"}},"required":["kind","detail"],"additionalProperties":false}`),
		agentTool("memory_update", "Update an entry owned by this verified requester, or owner. Read first; expected_revision must match. action=forget|pin|unpin|correct. Correction needs new sources; it never changes the original message or task receipts. Forget immediately prevents lexical/semantic injection. Input {id,expected_revision,action,detail?,sources?}.", `{"type":"object","properties":{"id":{"type":"string"},"expected_revision":{"type":"integer","minimum":1},"assertions":{"type":"array","items":{"type":"object","properties":{"subject":{"type":"string"},"predicate":{"type":"string"},"object":{"type":"string"},"asserted_by":{"type":"string"},"state":{"type":"string","enum":["explicit","reported","uncertain","corrected"]},"subject_basis":{"type":"string"},"subject_text":{"type":"string"},"object_basis":{"type":"string"},"object_text":{"type":"string"},"predicate_text":{"type":"string"},"negated":{"type":"boolean"},"sources":{"type":"array","items":{"type":"object","properties":{"event_id":{"type":"string"},"event_revision":{"type":"integer","minimum":1},"quote":{"type":"string"}},"required":["event_id","event_revision","quote"],"additionalProperties":false}}},"required":["subject","predicate","object","asserted_by","state","subject_basis","subject_text","object_basis","object_text","predicate_text","sources"],"additionalProperties":false}},"action":{"type":"string","enum":["forget","pin","unpin","correct"]},"detail":{"type":"string"},"sources":{"type":"array","items":{"type":"object","properties":{"event_id":{"type":"string"},"quote":{"type":"string"}},"required":["event_id","quote"],"additionalProperties":false}}},"required":["id","expected_revision","action"],"additionalProperties":false}`),
	}
}

func (p *permittedTools) memoryScope(thread string) (string, error) {
	if p.trace.Preview || p.trace.ReportOnly || p.trace.Account != p.manager.native.Account() || !p.manager.Config().Companion.Permissions.Remember {
		return "", errors.New("memory permission unavailable")
	}
	if thread == "" {
		thread = p.trace.Thread
	}
	if thread != p.trace.Thread {
		if !p.privileged() || !p.manager.agent.config().CrossConversation {
			return "", errors.New("cross conversation memory denied")
		}
		known, e := p.manager.storedAccountConversations()
		if e != nil {
			return "", e
		}
		if _, ok := known[thread]; !ok {
			return "", errors.New("unknown memory conversation")
		}
	}
	return companion.ScopeID("wechat", p.trace.Account, thread), nil
}

func (p *permittedTools) callMemory(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	queryAuthorization := p.memoryQueryAuthorization()
	var in struct {
		companion.MemoryQuery
		Thread       string                      `json:"thread_id"`
		ID           string                      `json:"id"`
		MessageID    string                      `json:"message_id"`
		Event        bool                        `json:"event"`
		Part         string                      `json:"part"`
		MediaIndex   int                         `json:"media_index"`
		Length       int                         `json:"length"`
		SourceOffset int                         `json:"source_offset"`
		SourceLimit  int                         `json:"source_limit"`
		Expected     int                         `json:"expected_revision"`
		Detail       string                      `json:"detail"`
		Summary      string                      `json:"summary"`
		Keywords     []string                    `json:"keywords"`
		Assertions   []companion.MemoryAssertion `json:"assertions"`
		Sources      []companion.MemorySource    `json:"sources"`
		Note         bool                        `json:"note"`
		Action       string                      `json:"action"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return "", toolInputError("记忆参数无效")
	}
	scope, e := p.memoryScope(in.Thread)
	if e != nil {
		return "", toolInputError(e.Error())
	}
	store := p.manager.memory
	var value any
	var queryEpoch string
	if name == "memory_read" {
		if in.MessageID == "" && strings.HasPrefix(in.ID, "msg_") {
			in.MessageID = in.ID
			in.ID = ""
		}
		if in.MessageID != "" {
			if in.ID != "" {
				return "", toolInputError("请选择id或message_id中的一个来源")
			}
			in.ID, e = store.OriginalEventID(ctx, scope, in.MessageID)
			if errors.Is(e, sql.ErrNoRows) {
				return "", memoryRecordUnavailable{}
			}
			if e != nil {
				return "", e
			}
			in.Event = true
		}
		if in.ID == "" {
			return "", toolInputError("需要记忆id、原始事件id或message_id")
		}
		queryEpoch, e = store.DataEpoch(ctx)
		if e != nil {
			return "", e
		}
	}
	switch name {
	case "memory_search":
		in.Scopes = []string{scope}
		if !in.Originals && in.Query != "" {
			cfg, err := p.manager.loadMemoryServices()
			if err != nil {
				return "", err
			}
			if cfg.Embedding.Configured() {
				r, err := cfg.Embedding.Embed(ctx, []string{in.Query})
				tokens := r.Tokens
				if !r.Reported {
					tokens = int64(companion.EstimateTokens(in.Query))
				}
				_ = store.RecordMaintenanceUsage(context.WithoutCancel(ctx), "explicit_recall_embedding", tokens, r.Reported)
				if err == nil {
					in.Vector = r.Vectors[0]
					in.Space = r.Space
				}
			}
		}
		queryEpoch, e = store.DataEpoch(ctx)
		if e != nil {
			return "", e
		}
		value, e = store.SearchMemory(ctx, in.MemoryQuery)
	case "memory_read":
		if in.Event {
			ev, err := store.ReadEvent(ctx, []string{scope}, in.ID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return "", memoryRecordUnavailable{}
				}
				return "", localAgentError(err)
			}
			if in.Expected > 0 && in.Expected != ev.Revision {
				return "", companion.ErrMemoryConflict
			}
			part := in.Part
			if part == "" {
				part = "text"
			}
			body := ev.Text
			available := []map[string]any{{"part": "text", "total_bytes": len(ev.Text)}}
			for i, m := range ev.IncomingMedia {
				available = append(available, map[string]any{"part": "observation", "media_index": i, "total_bytes": len(m.Observation), "state": m.ObservationState, "version": m.ObservationVersion})
				defaultShare := in.Part == "" && ev.Text == "" && len(ev.IncomingMedia) == 1 && m.Kind == "share" && m.ObservationState == "observed" && m.Observation != ""
				if defaultShare {
					part = "observation"
				}
				if text, ok := storedMediaTranscript(m); ok {
					available = append(available, map[string]any{"part": "transcript", "media_index": i, "total_bytes": len(text), "state": m.ObservationState, "version": m.ObservationVersion})
					if defaultShare {
						part = "transcript"
					}
				}
			}
			if part == "observation" || part == "transcript" {
				if in.MediaIndex < 0 || in.MediaIndex >= len(ev.IncomingMedia) {
					return "", toolInputError("媒体观察编号不存在，请先读取available_parts")
				}
				body = ev.IncomingMedia[in.MediaIndex].Observation
				if part == "transcript" {
					var ok bool
					body, ok = storedMediaTranscript(ev.IncomingMedia[in.MediaIndex])
					if !ok {
						return "", toolInputError("当前保存的观察没有音轨转写片段；可读observation或用video_inspect补读，不表示原视频没有声音")
					}
				}
			} else if part != "text" {
				return "", toolInputError("part应为text、observation或transcript")
			}
			if in.Offset < 0 || in.Offset > len(body) || in.Offset > 0 && in.Offset < len(body) && !utf8.RuneStart(body[in.Offset]) || in.Length < 0 {
				return "", toolInputError("原文分页位置无效")
			}
			length := in.Length
			if length == 0 {
				length = 4000
			}
			requested := length
			length = min(length, 32768)
			end := in.Offset + min(length, len(body)-in.Offset)
			for end > in.Offset && end < len(body) && !utf8.RuneStart(body[end]) {
				end--
			}
			if end == in.Offset && end < len(body) {
				return "", toolInputError("分页长度不足以容纳一个UTF-8字符")
			}
			total := len(body)
			ev.OmitMediaObservationText()
			ev.Text = ""
			if part == "text" {
				ev.Text = body[in.Offset:end]
			}
			value = map[string]any{"event": ev, "part": part, "media_index": in.MediaIndex, "available_parts": available, "total_bytes": total, "next_offset": end, "has_more": end < total, "revision": ev.Revision, "requested_length": requested, "effective_length": end - in.Offset, "transport_page_bytes": 32768, "material_kind": "source_observation_or_original_not_new_user_instruction"}
			if part != "text" {
				value.(map[string]any)["text"] = body[in.Offset:end]
			}
		} else {
			m, err := store.ReadMemory(ctx, []string{scope}, in.ID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return "", memoryRecordUnavailable{}
				}
				return "", localAgentError(err)
			}
			if in.Expected > 0 && in.Expected != m.Version {
				return "", companion.ErrMemoryConflict
			}
			if in.Offset < 0 || in.Offset > len(m.Detail) || in.Length < 0 || in.SourceOffset < 0 || in.SourceOffset > len(m.Sources) || in.SourceLimit < 0 {
				return "", toolInputError("记忆分页位置无效")
			}
			if in.Offset < len(m.Detail) && in.Offset > 0 && !utf8.RuneStart(m.Detail[in.Offset]) {
				return "", toolInputError("分页位置不在UTF-8边界")
			}
			length := in.Length
			if length == 0 {
				length = 4000
			}
			requested := length
			length = min(length, 32768)
			end := in.Offset + min(length, len(m.Detail)-in.Offset)
			for end > in.Offset && end < len(m.Detail) && !utf8.RuneStart(m.Detail[end]) {
				end--
			}
			if end == in.Offset && end < len(m.Detail) {
				return "", toolInputError("分页长度不足以容纳一个UTF-8字符")
			}
			total := len(m.Detail)
			m.Detail = m.Detail[in.Offset:end]
			sourceLimit := in.SourceLimit
			if sourceLimit == 0 {
				sourceLimit = 20
			}
			sourceEnd := in.SourceOffset + min(sourceLimit, len(m.Sources)-in.SourceOffset)
			sourceTotal := len(m.Sources)
			m.Sources = m.Sources[in.SourceOffset:sourceEnd]
			value = map[string]any{"entry": m, "total_bytes": total, "next_offset": end, "has_more": end < total, "revision": m.Version, "requested_length": requested, "effective_length": end - in.Offset, "source_count": sourceTotal, "next_source_offset": sourceEnd, "source_has_more": sourceEnd < sourceTotal, "transport_page_bytes": 32768}
		}
	case "memory_remember":
		if in.Subject == "" {
			in.Subject = p.trace.Sender
		}
		if in.Subject != p.trace.Sender && !p.privileged() {
			return "", toolInputError("只能保存自己的事实或笔记")
		}
		if in.Note {
			in.Subject = p.trace.Sender
		}
		value, e = store.SaveMemory(ctx, companion.MemoryEntry{Scope: scope, Subject: in.Subject, Kind: in.Kind, Detail: in.Detail, Summary: in.Summary, Keywords: in.Keywords, Sources: in.Sources, Assertions: in.Assertions, Manual: in.Note}, 0)
	case "memory_update":
		m, err := store.ReadMemory(ctx, []string{scope}, in.ID)
		if err != nil {
			return "", localAgentError(err)
		}
		if m.Subject != p.trace.Sender && !p.privileged() {
			return "", toolInputError("不能修改其他人的长期记忆")
		}
		if in.Expected < 1 {
			return "", toolInputError("请先读取记忆版本")
		}
		switch in.Action {
		case "forget":
			m.Status = "deleted"
		case "pin":
			m.Pinned = true
		case "unpin":
			m.Pinned = false
		case "correct":
			if strings.TrimSpace(in.Detail) == "" {
				return "", toolInputError("更正内容为空")
			}
			m.Detail = in.Detail
			m.Summary = in.Detail
			m.Sources = in.Sources
			m.Assertions = in.Assertions
			if len(in.Sources) == 0 && !m.Manual {
				return "", toolInputError("更正需要新来源")
			}
		default:
			return "", toolInputError("未知记忆操作")
		}
		value, e = store.SaveMemory(ctx, m, in.Expected)
	default:
		return "", toolInputError("未知记忆工具")
	}
	if e != nil {
		return "", localAgentError(e)
	}
	if _, err := p.memoryScope(in.Thread); err != nil {
		return "", toolInputError("记忆权限已撤销")
	}
	if name == "memory_search" || name == "memory_read" {
		result, err := p.stampMemoryQuery(ctx, queryEpoch, queryAuthorization, value)
		if err == nil {
			p.routeCrossConversationThread(in.Thread)
		}
		return result, err
	}
	b, e := json.Marshal(value)
	return string(b), e
}
