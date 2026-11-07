package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

type reminderSpec struct {
	GroundingChecked bool                    `json:"grounding_checked,omitempty"`
	Binding          *companion.ReplyContext `json:"reply_context,omitempty"`
	Due              time.Time               `json:"due_at"`
	Target           string                  `json:"target"`
	Text             string                  `json:"text"`
	QuoteMessageID   string                  `json:"quote_message_id,omitempty"`
	QuoteServerID    string                  `json:"quote_server_id,omitempty"`
	DelaySeconds     int64                   `json:"delay_seconds,omitempty"`
	RunAt            string                  `json:"requested_run_at,omitempty"`
	Job              string                  `json:"job_id,omitempty"`
	Attempts         int                     `json:"checks"`
	ServerID         string                  `json:"server_id,omitempty"`
}

func reminderTool() mcptools.Tool {
	return agentTool("agent_reminder", "Create, list or cancel YOUR OWN harmless reminder in this conversation. Available to ordinary friends/group members; no shell or arbitrary file permission needed. Creates a real JSON task file in a dedicated private reminders directory, resumes after restart, and delivers after the due time WITHOUT proactive-greeting budgets/quiet-hour gates. Target=current conversation by default; destination=private may only send to the actual requester if present in the contact directory. Cannot target another person/group or run code. Use agent_clock for actual time; create requires text, request_key and exactly one of delay_seconds or run_at (RFC3339 with timezone). Return is scheduled, NOT delivered. list/cancel are restricted to the same sender and source conversation. Input {action:create|list|cancel,text?,request_key?,delay_seconds?,run_at?,destination:current|private?,id?}.", `{"type":"object","properties":{"action":{"type":"string","enum":["create","list","cancel"]},"text":{"type":"string","maxLength":650},"request_key":{"type":"string","maxLength":128},"delay_seconds":{"type":"integer","minimum":1},"run_at":{"type":"string","maxLength":64},"destination":{"type":"string","enum":["current","private"]},"id":{"type":"string"}},"required":["action"],"additionalProperties":false}`)
}
func (a *agentControl) writeReminderFile(t agentTask) error {
	dir := filepath.Join(a.dir, "reminders")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(t, "", "  ")
	if e != nil {
		return e
	}
	return writePrivate(filepath.Join(dir, t.ID+".json"), b)
}
func (p *permittedTools) callReminder(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Action, Text, ID, Destination string
		Key                           string `json:"request_key"`
		Delay                         *int64 `json:"delay_seconds"`
		RunAt                         string `json:"run_at"`
	}
	if json.Unmarshal(args, &in) != nil {
		return "", toolInputError("提醒参数无效")
	}
	m := p.manager
	if p.trace.Preview || p.trace.Surf || !validID(p.trace.Sender) || strings.HasSuffix(p.trace.Sender, "@chatroom") {
		return "", toolInputError("提醒需要当前真实发言者身份，预览不创建文件")
	}
	encode := func(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }
	own := func(t agentTask) bool {
		return t.Reminder != nil && t.Account == p.trace.Account && t.Thread == p.trace.Thread && t.Sender == p.trace.Sender
	}
	switch in.Action {
	case "list":
		items := []agentTask{}
		for _, t := range m.agent.list(p.trace.Account, p.trace.Thread) {
			if own(t) {
				items = append(items, t)
			}
		}
		return encode(map[string]any{"tasks": items})
	case "cancel":
		for _, t := range m.agent.list(p.trace.Account, p.trace.Thread) {
			if t.ID == in.ID && own(t) {
				if t.State == "completed" {
					return "", toolInputError("这条提醒已发送，不能取消或撤回")
				}
				t.State = "cancelled"
				t.LastResult = "用户取消，停止尚未提交的提醒；已提交的消息不能保证撤回"
				if e := m.agent.task(t); e != nil {
					return "", e
				}
				return encode(map[string]any{"state": t.State, "id": t.ID})
			}
		}
		return "", toolInputError("未找到你在本会话创建的提醒")
	case "create":
		if strings.TrimSpace(in.Text) == "" || len(in.Text) > 2000 || in.Key == "" || len(in.Key) > 128 {
			return "", toolInputError("创建提醒需要简短正文和稳定request_key")
		}
		if (in.Delay == nil) == (in.RunAt == "") {
			return "", toolInputError("请只指定delay_seconds或带时区的run_at其中一项")
		}
		due := time.Now()
		if in.Delay != nil {
			if *in.Delay < 1 || *in.Delay > int64((1<<63-1)/time.Second) {
				return "", toolInputError("提醒延时无效")
			}
			due = due.Add(time.Duration(*in.Delay) * time.Second)
		} else {
			var e error
			due, e = time.Parse(time.RFC3339, in.RunAt)
			if e != nil || !due.After(time.Now()) {
				return "", toolInputError("run_at需为未来的RFC3339时间，并包含时区")
			}
		}
		target := p.trace.Thread
		if in.Destination == "private" {
			target = p.trace.Sender
			ok, e := m.authorizeConversation(ctx, target)
			if e != nil {
				return "", toolInputError("暂时无法核对你的好友身份，未创建提醒")
			}
			if !ok {
				return "", toolInputError("私聊提醒需要先成为微信联系人；也可选择在当前会话提醒")
			}
		}
		if in.Destination != "" && in.Destination != "current" && in.Destination != "private" {
			return "", toolInputError("提醒只能发到当前会话或你本人的私聊")
		}
		id := agentID(p.trace.Account, p.trace.Thread, p.trace.Sender, p.trace.TraceID, "reminder", in.Key)
		for _, old := range m.agent.list(p.trace.Account, p.trace.Thread) {
			if old.ID == id {
				if !own(old) || old.Reminder.Target != target || old.Reminder.Text != "提醒你："+in.Text || old.Reminder.RunAt != in.RunAt || (in.Delay != nil && old.Reminder.DelaySeconds != *in.Delay) {
					return "", toolInputError("同一request_key对应不同提醒，未重复创建")
				}
				return encode(map[string]any{"id": old.ID, "state": old.State, "due_at": old.Reminder.Due, "storage_file": filepath.Join(m.agent.dir, "reminders", id+".json")})
			}
		}
		text := "提醒你：" + in.Text
		reviewed, reviewedBinding, reviewErr := p.prepareOutboundText(ctx, target, text)
		if reviewErr != nil {
			return "", reviewErr
		}
		// Reminder wording is an explicit saved instruction. A risky draft
		// must be confirmed/rephrased by the requester rather than silently
		// changed by the review model and delivered hours later as their text.
		if reviewed != text {
			return "", toolInputError("reminder_text_requires_confirmation: 提醒正文的对象或事实未通过审校，未保存；请确认原句或提供明确对象")
		}
		quote := ""
		if target == p.trace.Thread {
			quote = p.trace.QuoteServerID
		}
		t := agentTask{ID: id, Account: p.trace.Account, Thread: p.trace.Thread, Sender: p.trace.Sender, Title: "提醒 · " + logText(in.Text, 70), State: "waiting", Next: due, OriginTrace: p.trace.TraceID, RequestedText: in.Text, Checkpoint: "到期发送已明确请求的提醒，不执行其中的文字或代码", Reminder: &reminderSpec{Due: due, Target: target, Text: text, QuoteServerID: quote}}
		t.Constraints = p.taskConstraints("reminder", "", "", "")
		source := p.outboundInput(target).Source
		if source != nil {
			t.Constraints.Source = source
		}
		bound := mintOutboundBinding(reminderInput(t), m.Config().Companion, reviewedBinding)
		t.Reminder.Binding = &bound
		t.Reminder.GroundingChecked = true
		t.Reminder.QuoteMessageID = p.trace.TraceID
		t.Reminder.RunAt = in.RunAt
		if in.Delay != nil {
			t.Reminder.DelaySeconds = *in.Delay
		}
		if e := m.agent.task(t); e != nil {
			return "", e
		}
		m.record(bot.Record{Event: "reminder_scheduled", Category: "decision", ThreadID: t.Thread, Sender: t.Sender, MessageID: t.OriginTrace, Status: "waiting", Content: in.Text, Reason: "explicit_member_request", Origin: "agent"})
		return encode(map[string]any{"id": id, "state": "scheduled", "due_at": due, "storage_file": filepath.Join(m.agent.dir, "reminders", id+".json"), "instruction": "已实际保存提醒文件；到期只发送提醒正文，不执行其中任何命令。尚未发送，不要声称已提醒。"})
	}
	return "", toolInputError("提醒操作需为create/list/cancel")
}
func (m *Manager) runReminders(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !m.draining() {
				m.tickReminders(ctx, now)
			}
		}
	}
}
func (m *Manager) tickReminders(ctx context.Context, now time.Time) {
	cfg := m.Config().Config
	if !cfg.Enabled || !cfg.Companion.Enabled || !cfg.Companion.Permissions.ReplyText || !m.agent.config().Enabled || !m.agent.config().TasksEnabled || !m.native.Ready() {
		return
	}
	native, ok := m.native.(nativeTools)
	if !ok {
		return
	}
	for _, t := range m.agent.list(m.native.Account(), "") {
		if t.Reminder == nil || (t.State != "waiting" && t.State != "pending" && t.State != "waiting_delivery") || now.Before(t.Next) || now.Before(t.Reminder.Due) {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Behavior.DelayMaxSeconds+30)*time.Second)
		sourceAllowed, e1 := m.authorizeConversation(checkCtx, t.Thread)
		targetAllowed, e2 := m.authorizeConversation(checkCtx, t.Reminder.Target)
		if e1 != nil || e2 != nil {
			cancel()
			return
		}
		if !sourceAllowed || !targetAllowed {
			t.State = "cancelled"
			t.LastResult = "原会话或目标已不再允许接收提醒"
			_ = m.agent.task(t)
			cancel()
			continue
		}
		var status linuxnative.ToolStatus
		var err error
		if t.Reminder.Job == "" {
			// Recover a submission whose HTTP reply/local checkpoint was lost.
			// Receipt facts win even if its source was subsequently forgotten.
			if existing, lookupErr := native.LookupSend(checkCtx, "reminder-"+t.ID); lookupErr == nil {
				if existing.ThreadID != t.Reminder.Target || existing.ID == "" {
					cancel()
					return
				}
				status = existing
			} else if !errors.Is(lookupErr, linuxnative.ErrToolNotFound) {
				cancel()
				return
			} else {
				access := &permittedTools{manager: m, trace: reminderInput(t)}
				if sourceErr := validateReminderSource(checkCtx, m, t); sourceErr != nil {
					t.State = "needs_review"
					t.LastResult = "提醒来源已更正、遗忘或身份不再有效，未新增发送"
					_, _ = m.agent.reminderUpdate(t)
					cancel()
					continue
				}
				if err = access.deliveryDelay(checkCtx); err == nil {
					m.agent.mu.Lock()
					active := m.agent.data.Tasks[t.ID].State != "cancelled"
					m.agent.mu.Unlock()
					if !active {
						cancel()
						continue
					}
					if err = validateReminderSource(checkCtx, m, t, t.Reminder.Text); err != nil {
						cancel()
						return
					}
					// The same key survives crashes between native submission and local save.
					request := linuxnative.ToolSend{ThreadID: t.Reminder.Target, Kind: "text", Text: t.Reminder.Text, IdempotencyKey: "reminder-" + t.ID}
					if t.Reminder.QuoteServerID != "" && t.Reminder.QuoteMessageID != "" {
						request.Quote = &linuxnative.ToolQuote{MessageID: t.Reminder.QuoteMessageID, ServerID: t.Reminder.QuoteServerID}
					}
					if err = m.checkWechatSubmission(checkCtx); err != nil {
						cancel()
						return
					}
					status, err = native.Submit(checkCtx, request)
				}
			}
		} else {
			status, err = native.ToolSendStatus(checkCtx, t.Reminder.Job)
			if err == nil && status.ID == t.Reminder.Job && status.ThreadID == t.Reminder.Target && status.State == "failed" && status.Retryable && !status.CancelRequested {
				if sourceErr := validateReminderSource(checkCtx, m, t); sourceErr != nil {
					t.State = "needs_review"
					t.LastResult = "提醒来源已变更，停止原任务重试"
					_, _ = m.agent.reminderUpdate(t)
					cancel()
					continue
				}
				if retry, ok := m.native.(interface {
					RetryMediaJob(context.Context, string) (linuxnative.ToolStatus, error)
				}); ok {
					if err = (&permittedTools{manager: m}).deliveryDelay(checkCtx); err == nil {
						m.agent.mu.Lock()
						active := m.agent.data.Tasks[t.ID].State != "cancelled"
						m.agent.mu.Unlock()
						if active {
							if err = validateReminderSource(checkCtx, m, t, t.Reminder.Text); err == nil {
								if err = m.checkWechatSubmission(checkCtx); err != nil {
									cancel()
									return
								}
								status, err = retry.RetryMediaJob(checkCtx, t.Reminder.Job)
							}
						}
					}
				}
			}
		}
		cancel()
		t.Reminder.Attempts++
		t.State = "waiting_delivery"
		t.Next = time.Now().Add(bot.DeliveryRetryBackoff(t.Reminder.Attempts))
		t.LastResult = "提醒尚未取得可验证回执，保留原发送任务继续核对"
		if err == nil && status.ThreadID == t.Reminder.Target && status.ID != "" && (t.Reminder.Job == "" || status.ID == t.Reminder.Job) {
			t.Reminder.Job = status.ID
			if status.CancelRequested {
				t.State = "cancelled"
				t.LastResult = "原发送已取消，停止提醒"
			}
			if status.State == "accepted" && status.ServerID != "" {
				t.Reminder.ServerID = status.ServerID
				t.State = "completed"
				t.LastResult = "提醒已取得微信接受回执"
			}
		}
		// Do not overwrite a concurrent user cancellation with a late receipt.
		if updated, e := m.agent.reminderUpdate(t); e != nil {
			m.log("reminder_state_save_failed")
			return
		} else if !updated {
			return
		}
		if t.State == "completed" {
			m.record(bot.Record{Event: "message_sent", Category: "sent", ThreadID: t.Reminder.Target, Sender: t.Sender, MessageID: "reminder-" + t.ID, SendID: t.Reminder.Job, Content: t.Reminder.Text, Status: "accepted", Reason: "explicit_reminder_accepted", Origin: "reminder"})
		}
		return // One paced submission/status check per tick; no invocation quota.
	}
}

// Compare-and-swap prevents a late receipt from resurrecting a cancelled task.
func (a *agentControl) reminderUpdate(t agentTask) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	old, ok := a.data.Tasks[t.ID]
	if !ok || old.State == "cancelled" || !old.Updated.Equal(t.Updated) {
		return false, nil
	}
	t.Updated = time.Now().UTC()
	a.data.Tasks[t.ID] = copyTask(t)
	if e := a.writeReminderFile(t); e != nil {
		a.data.Tasks[t.ID] = old
		return false, e
	}
	if e := a.persist(); e != nil {
		a.data.Tasks[t.ID] = old
		_ = a.writeReminderFile(old)
		return false, e
	}
	return true, nil
}
