package companion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
	"wechat-bot/internal/agentrun"
)

var ErrWakePermission = errors.New("conversation wake permission denied")
var ErrWakeContext = errors.New("conversation wake context unavailable")

// ConversationSettings controls scheduling, never extra recipient/tool permissions.
type ConversationSettings struct {
	Enabled            bool `json:"enabled"`
	AggregateMS        int  `json:"aggregate_ms"`
	GroupAggregateMS   *int `json:"group_aggregate_ms,omitempty"`
	RecheckOnMessage   bool `json:"recheck_on_message"`
	AllowWake          bool `json:"allow_wake"`
	WakeMinSeconds     int  `json:"wake_min_seconds"`
	WakeMaxSeconds     int  `json:"wake_max_seconds"`
	TaskTimeoutSeconds int  `json:"task_timeout_seconds,omitempty"`
}

func ConversationDefaults() *ConversationSettings {
	return &ConversationSettings{Enabled: true, AggregateMS: 2000, RecheckOnMessage: true, AllowWake: false, WakeMinSeconds: 30, WakeMaxSeconds: 600}
}
func (c Config) ConversationConfig() ConversationSettings {
	if c.Conversation == nil {
		return *ConversationDefaults()
	}
	return *c.Conversation
}

// Online group turns have a separate collection window. Retain explicit legacy
// tuning (including zero) while upgrading the former two-second default.
func (c ConversationSettings) CollectionWindow(thread string) time.Duration {
	ms := c.AggregateMS
	if strings.HasSuffix(thread, "@chatroom") {
		if c.GroupAggregateMS != nil {
			ms = *c.GroupAggregateMS
		} else if ms == 2000 {
			ms = 20000
		}
	}
	return time.Duration(ms) * time.Millisecond
}

func validateConversation(c *ConversationSettings) error {
	if c == nil {
		return nil
	}
	if c.GroupAggregateMS != nil && (*c.GroupAggregateMS < 0 || int64(*c.GroupAggregateMS) > int64((time.Duration(1<<63-1))/time.Millisecond)) {
		return errors.New("群消息收集时长无效")
	}
	if c.AggregateMS < 0 || c.AggregateMS > 5000 || c.WakeMinSeconds < 1 || c.WakeMaxSeconds < c.WakeMinSeconds || int64(c.WakeMaxSeconds) > int64((time.Duration(1<<63-1))/time.Second) || c.TaskTimeoutSeconds < 0 || int64(c.TaskTimeoutSeconds) > int64((time.Duration(1<<63-1))/time.Second) {
		return errors.New("请检查消息聚合窗口与延时补充范围")
	}
	return nil
}

type PendingReview struct {
	generationEvidence *GenerationEvidence
	EvidenceMessageID  string   `json:"evidence_message_id,omitempty"`
	Reason             string   `json:"reason,omitempty"`
	Action             string   `json:"action"`
	Messages           []string `json:"messages,omitempty"`
	DelaySeconds       int      `json:"delay_seconds,omitempty"`
}

func (s *Store) ClaimConversationWake(in Input, c Config, now time.Time) error {
	return s.scopeTransaction(ScopeID(in.Channel, in.Account, in.Thread), func(data *persisted) error {
		if !c.Enabled || !c.ConversationConfig().AllowWake || !c.Permissions.Proactive || !c.Permissions.ReplyText || !c.Permissions.Remember || c.Quiet(now) {
			return ErrWakePermission
		}
		selected := false
		for _, id := range c.ProactiveThreads {
			selected = selected || id == in.Thread
		}
		if !selected {
			return ErrWakePermission
		}
		id := ScopeID(in.Channel, in.Account, in.Thread)
		scope, ok := data.Scopes[id]
		if !ok || len(scope.Messages) == 0 || now.Sub(scope.LastInteraction) > time.Duration(c.MemoryDays)*24*time.Hour {
			return ErrWakeContext
		}
		day := localDay(c, now)
		if data.ProactiveDay != day {
			data.ProactiveDay = day
			data.ProactiveCount = 0
		}
		scope.ProactiveCount = proactiveToday(scope, c, now) + 1
		scope.ProactiveDay = day
		scope.LastProactive = now
		scope.NextProactiveAt = nextProactiveEvaluation(c.ProactiveRule(in.Thread), now)
		data.Scopes[id] = scope
		data.ProactiveCount++
		return nil
	})
}

// ReviewPending cannot execute tools or send. Only the unsent tail can change.
func (e *Engine) ReviewPending(ctx context.Context, in Input, sent, pending []string, updates []ConversationEvent) (PendingReview, error) {
	return e.reviewPending(ctx, in, sent, pending, updates, nil, false)
}

// Caption review includes verified attachment delivery, not just the original
// search request. The model may still rewrite, delay or cancel the unsent text.
type DeliveredAttachment struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	State string `json:"state"`
}

func (e *Engine) ReviewMediaCaption(ctx context.Context, in Input, sent, pending []string, updates []ConversationEvent, media []DeliveredAttachment) (PendingReview, error) {
	return e.reviewPending(ctx, in, sent, pending, updates, nil, false, media)
}

// Running requests are not the unsent tail of an obsolete text reply.
func (e *Engine) ReviewGeneration(ctx context.Context, in Input, pending []string, updates []ConversationEvent) (PendingReview, error) {
	return e.reviewPending(ctx, in, nil, pending, updates, nil, true)
}

// DeliveryReview carries actual delivery evidence. This call cannot use tools.
type DeliveryReview struct {
	Reason   string    `json:"reason"`
	Attempts int       `json:"retry_attempts"`
	Created  time.Time `json:"first_attempt_at"`
}

func (e *Engine) ReviewDelivery(ctx context.Context, in Input, sent []string, text string, delivery DeliveryReview) (PendingReview, error) {
	return e.reviewPending(ctx, in, sent, []string{text}, nil, &delivery, false)
}

func (e *Engine) reviewPending(ctx context.Context, in Input, sent, pending []string, updates []ConversationEvent, delivery *DeliveryReview, generation bool, caption ...[]DeliveredAttachment) (PendingReview, error) {
	// Persisted plans can be reviewed by the base Engine while other requests
	// are running. Trace-dependent authorization must belong to this plan, not
	// whichever conversation last used a shared mutable adapter.
	scoped, err := e.ForEvidenceInput(in)
	if err != nil {
		return PendingReview{}, err
	}
	return scoped.reviewPendingScoped(ctx, in, sent, pending, updates, delivery, generation, caption...)
}

// ForEvidenceInput checks the original stamp with the original actor's current
// permissions. The shared engine itself never acquires a conversation identity.
func (e *Engine) ForEvidenceInput(in Input) (*Engine, error) {
	if _, mutable := e.Tools.(interface{ SetTrace(Input) }); !mutable {
		return e, nil
	}
	fork, err := e.Fork()
	if err != nil {
		return nil, err
	}
	traced, ok := fork.Tools.(interface{ SetTrace(Input) })
	if !ok {
		return nil, errors.New("isolated evidence trace unavailable")
	}
	traced.SetTrace(in)
	return fork, nil
}

func (e *Engine) reviewPendingScoped(ctx context.Context, in Input, sent, pending []string, updates []ConversationEvent, delivery *DeliveryReview, generation bool, caption ...[]DeliveredAttachment) (PendingReview, error) {
	if in.ReplyContext != nil {
		if err := e.ValidateReplyContext(ctx, in, *in.ReplyContext); err != nil {
			return PendingReview{}, err
		}
		if in.ReplyContext.EvidenceEpoch != "" || in.ReplyContext.EvidenceAuthorization != "" {
			original := GenerationEvidence{RecallEpoch: in.ReplyContext.EvidenceEpoch, Authorization: in.ReplyContext.EvidenceAuthorization}
			if previous, ok := GenerationEvidenceFromContext(ctx); ok && previous != original {
				return PendingReview{}, generationEvidenceChanged()
			}
			ctx = withGenerationEvidence(ctx, original)
		}
	}
	evidence, err := e.captureGenerationEvidence(ctx)
	if err != nil {
		return PendingReview{}, err
	}
	ctx = withGenerationEvidence(ctx, evidence)
	if err := e.validateGenerationInputSource(ctx, in); err != nil {
		return PendingReview{}, err
	}
	data := map[string]any{"kind": "review_unsent_message_plan", "original_request": e.safeCurrentEvidence(in), "original_requester_id": in.Sender, "already_sent": sent, "pending_messages": pending, "new_events": e.pendingEventEvidence(ctx, in, updates), "current_time": time.Now().In(location(e.Config.Timezone)).Format(time.RFC3339)}
	if generation {
		data["kind"] = "review_running_task"
	}
	if delivery != nil {
		data["kind"] = "review_uncertain_delivery"
		data["delivery_evidence"] = delivery
	}
	if len(caption) > 0 {
		data["kind"] = "review_delivered_media_caption"
		data["delivered_attachments"] = caption[0]
	}
	scope := e.conversationScope(in, e.Config, time.Now())
	data["relationship_notes"] = map[string]any{"evidence_kind": "legacy_unverified_notes", "items": scope.Notes}
	data["recent_conversation"] = e.contextTurns(scope, in)
	if scope.Preferences != "" {
		data["conversation_preferences"] = scope.Preferences
	}
	encoded, _ := json.Marshal(data)
	system := e.policy(in) + "\n本轮只复核旧发言计划的未发部分，不是回复最新用户的独立任务。新请求由另一轮处理，别抢答或重复生成其答案。只输出JSON {action:continue|cancel|replace|delay,messages:[语义完整的未发短句],delay_seconds:整数}。continue保留，cancel取消旧尾部，replace只修改有必要保留的旧补充，delay暂缓。根据新上下文判断；无关群友互聊不必打断。不能改已经发送的内容，不调用工具、不生成媒体、不承诺执行。取消需识别具体哪条新消息使原请求失效；原请求者真正叫停或改需求时才取消执行任务。其他人说机器人未开机、笑脸、生日祝福、聊别的话题、催促或重复原请求，都不代表撤销；不因最新话题变化就丢弃尚未完成的唱歌/下载请求。图片、视频、语音及分享的观察内容属于原请求的一部分；同一人紧跟发来的短说明、名称、图注通常是在补充媒体，要结合实际观察继续理解，不能仅因后发文字被标记为new_request就当成替代。只有明确撤销或相冲突的新要求才取消媒体任务；读取完成后仍可按正常社交判断选择安静。无法确定时continue。旧内容已过时、用户改意或明确叫停时取消。不要机械按标点拆分，不固定气泡数。"
	if delivery != nil {
		system += "\n本轮是发送回执不确定后的补发复核。你已知原文、补发次数和已确认成功内容。当前状态是等待确认，已接受内容见回执；后续结合目标会话实际记录核对。结合最近对话决定自然地沿用或改写这条补充，不机械重复、无需每次道歉；不要再次发送已接受的语音或文件。不因单次失败就放弃仍有效的请求。只有明确撤销、确认重复或确已过时才cancel，并给reason。replace只返回一条短消息。无法判断则delay，等待稍后核对。"
	}
	if in.IsGroup {
		system += "\n群里补发应像正常聊天，不能添加本系统的发送错误、回执、重试次数、任务编号或修复进度等运维说明；这些只私聊主人。若待发内容本身是纯运维说明，可cancel这条说明并解释改走私聊，不代表取消原用户任务或附件补发。"
	}
	if generation {
		system = e.policy(in) + "\n本轮检查正在处理的原始请求是否被真正撤销，不是修改旧回复或取消发言尾部。original_request包含原入站内容和已经取得的真实媒体观察；new_events是处理期间的后续消息。只输出JSON {action:continue|cancel|delay,evidence_message_id:字符串,reason:简短依据,delay_seconds:整数}，不调用工具、不回答用户。图片/视频之后同一人发短说明、名称或图注，是补充同一条媒体，应continue，让原主任务结合这些资料完成理解并决定回复或安静。不要因为后续消息标记new_request、另有一轮会处理文字、缺少明确提问或群话题变化就cancel。阅读完后是否接话由主决策的reply/silent决定，不能拿cancel代替安静。只有原请求者明确叫停，或明确提出与原任务相冲突的替代要求，才cancel；必须给真实依据消息ID和具体撤销含义。催促、补充说明、其他人聊天均不撤销原请求。不能撤回已发生的外部操作。无法确认撤销时continue。"
	}
	if len(caption) > 0 {
		system += "\n当前复核的是已发附件的后续配文，delivered_attachments来自真实微信接受状态。pending_messages是主模型为这些附件写好的介绍，尚未发送；附件已发不代表配文重复，也不代表分享已完整结束。按事实继续介绍、压缩为自然短句或根据新上下文调整，不因无人提问、原冲浪请求已经执行或话题普通而丢弃全部配文。允许真正不需要配文、重复、错误或用户叫停时cancel，但需给出具体reason；不编造附件画面。already_sent仅指已发文字，附件见独立字段。"
	}
	if err := e.ValidateGenerationEvidence(ctx, evidence); err != nil {
		return PendingReview{}, err
	}
	answer, err := e.model.Chat(ctx, system, []map[string]string{{"role": "user", "content": string(encoded)}})
	if err != nil {
		return PendingReview{}, err
	}
	if err := e.ValidateGenerationEvidence(ctx, evidence); err != nil {
		return PendingReview{}, err
	}
	var d PendingReview
	if len(answer) > 12<<10 || json.Unmarshal([]byte(strings.TrimSpace(answer)), &d) != nil {
		return d, errors.New("invalid pending-plan review")
	}
	switch d.Action {
	case "continue", "cancel", "delay":
	case "replace":
		if generation {
			return d, errors.New("running task review cannot replace a reply plan")
		}
		if len(d.Messages) < 1 {
			return d, errors.New("invalid replacement plan")
		}
	default:
		return d, errors.New("invalid pending-plan action")
	}
	for _, s := range d.Messages {
		if strings.TrimSpace(s) == "" || len(s) > 3000 || !utf8.ValidString(s) {
			return d, errors.New("invalid pending-plan message")
		}
	}
	if d.DelaySeconds < 0 || d.DelaySeconds > 300 {
		return d, errors.New("invalid pending-plan delay")
	}
	if in.IsGroup && !generation && d.Action != "cancel" {
		candidate := pending
		if d.Action == "replace" {
			candidate = d.Messages
		}
		if diagnosticDraft(candidate) {
			// Reviews run concurrently with other conversations. Give the
			// report route this plan's identity without mutating shared tools.
			fork, err := e.Fork()
			if err != nil {
				return PendingReview{}, err
			}
			if _, ok := fork.Tools.(GroupReplyPrivacy); !ok {
				return PendingReview{}, errors.New("private_diagnostic_route_unavailable")
			}
			if traced, ok := fork.Tools.(interface{ SetTrace(Input) }); ok {
				traced.SetTrace(in)
			}
			clean, err := fork.reviewReplyPrivacy(ctx, in, Decision{Action: "reply", Messages: candidate})
			if err != nil {
				return PendingReview{}, err
			}
			if len(clean.Messages) == 0 {
				d.Action, d.Messages, d.Reason = "cancel", nil, "private_diagnostic_only"
			} else {
				d.Messages = clean.Messages
				if d.Action != "delay" {
					d.Action = "replace"
				}
			}
		}
	}
	if err := e.ValidateGenerationEvidence(ctx, evidence); err != nil {
		return PendingReview{}, err
	}
	d.generationEvidence = &evidence
	return d, nil
}

// Pending updates come from the native scheduler, rather than model prose.
// Reuse the same source/literal/observation serializer used by live followups;
// preserve the native quote edge and task-relevance metadata independently.
func (e *Engine) pendingEventEvidence(ctx context.Context, in Input, updates []ConversationEvent) []map[string]any {
	base, _ := json.Marshal(map[string]any{"kind": "current_message", "channel": in.Channel, "account": in.Account, "thread": in.Thread, "speaker_id": in.Sender})
	events := make([]agentrun.Event, 0, len(updates))
	for _, update := range updates {
		kind := 0
		if update.Source != nil {
			kind = update.Source.MessageType
		}
		events = append(events, agentrun.Event{ID: update.MessageID, Thread: in.Thread, Sender: update.Sender, Text: update.Text, At: update.At, Kind: kind})
	}
	rows := e.followupEvidence([]map[string]string{{"role": "user", "content": string(base)}}, events)
	for i, row := range rows {
		update := updates[i]
		resolved := row["source_event_id"] != nil
		invalid := false
		if resolved && update.Source != nil {
			// followupEvidence's native fallback has only message/sender IDs.
			// A pending plan also carries the full original native reference;
			// do not let the weaker lookup upgrade its revision or identity.
			if _, ok := e.replyReferenceEvent(ctx, in, update.Source); !ok {
				invalid = true
			}
		}
		row["new_request"] = update.NewRequest
		row["relation"] = update.Relation
		row["new_request_is_not_authorization"] = true
		if update.Source != nil && !resolved {
			row["source"] = update.Source
		}
		if update.ReplyTo != nil {
			row["quoted_source"] = update.ReplyTo
			row["quote_chain"] = quoteRoles(update.Sender, update.Source, update.ReplyTo)
			row["quoted_by_id"] = update.Sender
			row["quoted_author_id"] = update.ReplyTo.Sender
		}
		if resolved && !invalid {
			continue
		}
		if source := update.Source; source != nil {
			invalid = invalid || source.EventID != "" || source.Account != "" && source.Account != in.Account || source.Thread != "" && source.Thread != in.Thread || source.Sender != "" && source.Sender != update.Sender
			if (source.MessageType == 0 || source.MessageType == 1) && source.BodyHash != "" && source.BodyHash != sourceHash(update.Text) {
				invalid = true
			}
		}
		// An existing but unreadable/revised source must not re-enter through
		// the unpersisted native-message fallback after forgetting or collision.
		if e.Store != nil && e.Store.db != nil && update.MessageID != "" {
			var count int
			if err := e.Store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE scope=? AND trace=? AND role='user'", ScopeID(in.Channel, in.Account, in.Thread), update.MessageID).Scan(&count); err != nil || count > 0 {
				invalid = true
			}
		}
		if invalid {
			row["text"] = ""
			row["media_observations"] = nil
			row["user_text_state"] = "invalidated"
			row["evidence_kind"] = "unavailable_event_reference"
			delete(row, "source_event_id")
			if update.Source != nil {
				row["source"] = update.Source
				if update.Source.EventID != "" {
					row["source_event_id"] = update.Source.EventID
				}
			}
		} else if row["user_text_state"] == "session_original" {
			row["user_text_state"] = "native_pending_original"
		}
	}
	return rows
}

type ConversationEvent struct {
	Source     *SourceRef `json:"source,omitempty"`
	ReplyTo    *SourceRef `json:"reply_to,omitempty"`
	Relation   string     `json:"relation,omitempty"`
	SenderName string     `json:"sender_name,omitempty"`
	MessageID  string     `json:"message_id"`
	Sender     string     `json:"sender"`
	Text       string     `json:"text"`
	At         string     `json:"at"`
	NewRequest bool       `json:"new_request"`
	// MediaReadFailed is a host observation status, never retained user text.
	MediaReadFailed bool `json:"media_read_failed,omitempty"`
}
