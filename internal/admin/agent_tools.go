package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

func agentTool(name, description, schema string) mcptools.Tool {
	return mcptools.NewTool(name, description, schema)
}
func (p *permittedTools) agentToolsAllowed() bool {
	if p.manager.agent == nil {
		return false
	}
	return p.manager.agent.config().Enabled && p.trace.Account == p.manager.native.Account() && !p.trace.Preview && !p.trace.ReportOnly && !p.trace.Surf && permittedRecipient(p.manager.Config().Config, p.trace.Thread)
}

func (p *permittedTools) agentTools() []mcptools.Tool {
	return append(p.workspaceAgentTools(), p.wechatCommunicationTools()...)
}

func (p *permittedTools) workspaceAgentTools() []mcptools.Tool {
	if p.manager.agent == nil {
		return nil
	}
	c := p.manager.agent.config()
	if !p.agentToolsAllowed() {
		return nil
	}
	tools := p.workspaceTools()
	tools = append(tools, p.artifactTools()...)
	tools = append(tools, p.documentTools()...)
	tools = append(tools, p.memoryTools()...)
	tools = append(tools, p.voiceTools()...)
	if p.privileged() {
		tools = append(tools, diagnosticsTool())
		tools = append(tools, agentTool("music_service_start", "Start the installed local managed YuE/RVC service on demand, owner only. No arbitrary unit, command or endpoint. Nonblocking: check music_service_status until ready, then resume original work. Does not create songs, alter voice defaults or send WeChat. Input {}.", `{"type":"object","properties":{},"additionalProperties":false}`))
	}
	projectTools := p.projectPluginTools(context.Background())
	tools = append(tools, projectTools...)
	projectWriteVisible := false
	for _, tool := range projectTools {
		projectWriteVisible = projectWriteVisible || tool.Name == "project_create"
	}
	if p.privileged() && p.workspaceExecutionAllowed() && (c.Host.Mode == "files" || c.Host.Mode == "full") && projectWriteVisible {
		tools = append(tools, agentTool("project_checkout_plugin", "Create or reopen an editable, digest-pinned source working copy of an installed plugin in THIS conversation's private tool-sources directory. Trusted owner with project write permission only. Copies only verified declared package files and plugin.json; never copies configuration, secrets, package-store state or mutable installed files. Same digest reopens without overwriting edits; a new installed digest creates a separate versioned directory. Input {plugin_id:string}.", `{"type":"object","properties":{"plugin_id":{"type":"string"}},"required":["plugin_id"],"additionalProperties":false}`))
	}
	for _, tool := range p.projectTools() {
		if tool.Name == "project_create" && (!p.workspaceExecutionAllowed() || (c.Host.Mode != "files" && c.Host.Mode != "full")) {
			continue
		}
		if tool.Name != "agent_global_search" || p.privileged() {
			tools = append(tools, tool)
		}
	}
	tools = append(tools, agentTool("agent_clock", "Read current server time, UTC, timezone and Unix seconds before scheduling. Use agent_reminder for harmless user reminders (available to ordinary members). agent_task persists other work/checkpoints on disk. Do not sleep in a shell for reminders. Input {}.", `{"type":"object","properties":{},"additionalProperties":false}`))
	if c.TasksEnabled {
		tools = append(tools, reminderTool())
		tools = append(tools, agentTool("agent_tasks", "Read persistent tasks and checkpoints in this conversation; includes interrupted work and prepared results. Input {}.", `{"type":"object","properties":{},"additionalProperties":false}`), agentTool("agent_task", "Persist a goal/checkpoint or update its state. Supply stable key for a NEW task, id for an existing task. state=pending|waiting schedules reevaluation after delay_seconds (nonnegative), not guaranteed delivery; real disk path and due_at are returned. source_thread_id and source_message_ids are accepted only after a successful read of those exact messages in this turn; retain their original chronological order for later requests such as ‘第二条再展开’. For an owner group request, a verified cross-conversation source defaults delivery to the requesting owner's private chat; delivery_thread_id may only name an already authorized recipient. target_task_id may cancel one of the same owner's tasks across conversations. Trusted owner requests continue under original authorization (rechecked at execution), without proactive-greeting permission. state=completed|cancelled|needs_review ends scheduling. Input {id?:string,key?:string,title?:string,checkpoint:string,state:string,delay_seconds?:integer,source_thread_id?:string,source_message_ids?:string[],delivery_thread_id?:string,target_task_id?:string}.", `{"type":"object","properties":{"id":{"type":"string"},"key":{"type":"string"},"title":{"type":"string"},"checkpoint":{"type":"string"},"state":{"type":"string","enum":["pending","waiting","completed","cancelled","needs_review"]},"delay_seconds":{"type":"integer","minimum":0},"source_thread_id":{"type":"string"},"source_message_ids":{"type":"array","items":{"type":"string"},"maxItems":50},"delivery_thread_id":{"type":"string"},"target_task_id":{"type":"string"}},"required":["checkpoint","state"],"additionalProperties":false}`))
	}
	if !p.privileged() {
		return tools
	}
	tools = append(tools, agentTool("agent_owner_report", "Privately report a technical diagnostic to the real owner of THIS account, even when the request originated in a group. Does not post in the group or grant extra privileges. Use once per new result, not repeated reminders. Input {text:string}.", `{"type":"object","properties":{"text":{"type":"string","minLength":1}},"required":["text"],"additionalProperties":false}`))

	return tools
}
func (p *permittedTools) agentToolEnabled(name string) bool {
	tools := p.workspaceAgentTools
	if wechatapp.ToolName(name) {
		tools = p.wechatCommunicationTools
	}
	for _, t := range tools() {
		if t.Name == name {
			return true
		}
	}
	return false
}
func agentToolName(name string) bool {
	if strings.HasPrefix(name, "memory_") {
		return true
	}
	if name == "agent_owner_report" {
		return true
	}
	if name == "agent_reminder" {
		return true
	}
	return strings.HasPrefix(name, "voice_library_") || strings.HasPrefix(name, "artifact_") || strings.HasPrefix(name, "document_") || name == "music_service_start" || strings.HasPrefix(name, "project_") || name == "agent_diagnostics" || name == "agent_global_search" || strings.HasPrefix(name, "workspace_") || strings.HasPrefix(name, "server_") || wechatapp.ToolName(name) || name == "agent_task" || name == "agent_tasks" || name == "agent_clock"
}
func (p *permittedTools) callAgent(ctx context.Context, name string, args json.RawMessage) (result string, callErr error) {
	defer func() {
		if name == "agent_task" {
			callErr = localAgentError(callErr)
		}
	}()
	if !p.agentToolEnabled(name) {
		return "", toolInputError("当前用户、会话或预演没有此项权限")
	}
	if strings.HasPrefix(name, "server_") || strings.HasPrefix(name, "voice_library_") || name == "agent_owner_report" {
		p.privateDiagnostics = true
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if wechatapp.SubmissionTool(name) {
		if err := p.manager.checkWechatSubmission(ctx); err != nil {
			return "", err
		}
	}
	// Canonical JSON makes object property order irrelevant to operation identity.
	var canonical any
	if json.Unmarshal(args, &canonical) != nil {
		return "", toolInputError("参数不是有效JSON")
	}
	args, _ = json.Marshal(canonical)
	if isProjectPluginTool(name) {
		return p.callProjectPlugin(ctx, name, args)
	}
	encode := func(v any, e error) (string, error) {
		if e != nil {
			return "", e
		}
		b, e := json.Marshal(v)
		return string(b), e
	}
	m := p.manager
	if strings.HasPrefix(name, "memory_") {
		return p.callMemory(ctx, name, args)
	}
	if name == "music_service_start" {
		return p.controlMusicService(ctx, "start")
	}
	if name == "project_checkout_plugin" {
		return p.checkoutPluginSource(ctx, args)
	}
	if strings.HasPrefix(name, "voice_library_") {
		return p.callVoiceLibrary(ctx, name, args)
	}
	if name == "agent_diagnostics" {
		return p.callDiagnostics(args)
	}
	if strings.HasPrefix(name, "project_") || name == "agent_global_search" {
		result, err := p.callProject(ctx, name, args)
		if err == nil && name == "agent_global_search" {
			p.routeGlobalSearchResult(result)
		}
		return result, err
	}
	if strings.HasPrefix(name, "workspace_") {
		return p.callWorkspace(ctx, name, args)
	}
	if strings.HasPrefix(name, "artifact_") {
		return p.callArtifact(ctx, name, args)
	}
	if strings.HasPrefix(name, "document_") {
		switch name {
		case "document_inspect":
			return p.callDocumentWorkshop(ctx, "document.inspect", args)
		case "document_reconstruct":
			return p.callDocumentWorkshop(ctx, "document.reconstruct", args)
		case "document_export_pdf":
			return p.callDocumentWorkshop(ctx, "document.export_pdf", args)
		case "document_verify":
			return p.callDocumentWorkshop(ctx, "document.verify", args)
		default:
			return "", toolInputError("unknown_document_tool")
		}
	}
	switch name {
	case "wechat_recall":
		return p.recallMessage(ctx, args)
	case "agent_owner_report":
		var in struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(args, &in) != nil || strings.TrimSpace(in.Text) == "" || len(in.Text) > 12000 || !p.privileged() {
			return "", toolInputError("owner_report_not_authorized_or_invalid")
		}
		if err := p.manager.queueOwnerDetail(p.trace, in.Text); err != nil {
			return "", err
		}
		return encode(map[string]any{"state": "queued_private_report", "instruction": "技术报告已安排给主人私聊；群里只自然简短回应结果，不再复述技术内容。"}, nil)
	case "wechat_file_read":
		return p.readWechatAttachment(ctx, args)
	case "agent_reminder":
		return p.callReminder(ctx, args)
	case "agent_clock":
		now := time.Now()
		zone, offset := now.Zone()
		return encode(map[string]any{"now": now.Format(time.RFC3339Nano), "utc": now.UTC().Format(time.RFC3339Nano), "unix_seconds": now.Unix(), "timezone": zone, "utc_offset_seconds": offset}, nil)
	case "wechat_delivery_status", "wechat_delivery_retry":
		result, err := p.callDelivery(ctx, name, args)
		return result, localAgentError(err)
	case "wechat_read":
		native, _ := m.native.(wechatapp.CatalogReader)
		read, err := wechatapp.ReadCatalog(ctx, native, p.trace.Thread, args, func(thread string) bool {
			return p.agentToolEnabled(name) && p.conversationReadAllowed(thread)
		})
		if errors.Is(err, wechatapp.ErrReadDenied) || errors.Is(err, wechatapp.ErrReadUnavailable) {
			return "", toolInputError(err.Error())
		}
		if err != nil {
			return "", err
		}
		result, err := encode(read.Page, nil)
		if err == nil {
			if read.Kind == "messages" {
				p.recordCrossSourceMessages(read.Thread, result)
			}
			p.routeCrossConversationThread(read.Thread)
		}
		return result, err
	case "wechat_send_media":
		return p.sendMedia(ctx, args)
	case "wechat_forward_file":
		return p.forwardFile(ctx, args)
	case "wechat_conversations":
		return p.listAccountConversations(ctx, args)
	case "wechat_operation_status":
		return p.callWechatOperation(ctx, args)
	case "agent_tasks":
		tasks := m.agent.active(p.trace.Account, p.trace.Thread)
		visible := tasks[:0]
		for _, task := range tasks {
			if task.Reminder == nil || task.Sender == p.trace.Sender {
				visible = append(visible, task)
			}
		}
		tasks = visible
		for i := range tasks {
			tasks[i] = copyTask(tasks[i])
			if tasks[i].PluginJob != nil {
				// Origin is host recovery evidence, not conversation-visible data.
				tasks[i].PluginJob.Origin = nil
				tasks[i].PluginJob.PollState = nil
			}
			if tasks[i].OwnerRequested && !p.privileged() {
				tasks[i].Actions = nil
				tasks[i].Goal = ""
			}
			if tasks[i].MusicSpec != nil {
				tasks[i].MusicSpec.Style = ""
				tasks[i].MusicSpec.Lyrics = ""
			}
		}
		return encode(tasks, nil)
	case "agent_task":
		var in struct {
			ID               string   `json:"id"`
			Key              string   `json:"key"`
			Title            string   `json:"title"`
			Checkpoint       string   `json:"checkpoint"`
			State            string   `json:"state"`
			SourceThread     string   `json:"source_thread_id"`
			SourceMessageIDs []string `json:"source_message_ids"`
			DeliveryThread   string   `json:"delivery_thread_id"`
			TargetTaskID     string   `json:"target_task_id"`
			DelaySeconds     int      `json:"delay_seconds"`
		}
		if json.Unmarshal(args, &in) != nil || len(in.Checkpoint) > 6000 || len(in.Title) > 200 || !contains([]string{"pending", "waiting", "completed", "cancelled", "needs_review"}, in.State) {
			return "", toolInputError("任务参数无效")
		}
		if (in.State == "pending" || in.State == "waiting") && (in.DelaySeconds < 0 || int64(in.DelaySeconds) > int64((1<<63-1)/time.Second)) {
			return "", toolInputError("任务等待秒数无效")
		}
		origin := p.trace.OriginTraceID
		if origin == "" {
			origin = p.trace.TraceID
		}
		if in.TargetTaskID != "" {
			if in.ID != "" || in.State != "cancelled" || !p.privileged() || !p.manager.agent.config().CrossConversation {
				return "", toolInputError("跨会话任务只能由可信主人明确取消")
			}
			target, ok := m.agent.lookupTask(in.TargetTaskID, p.trace.Account, "")
			if !ok || target.Sender != p.trace.Sender || !p.conversationReadAllowed(taskDeliveryThread(target)) {
				return "", toolInputError("指定任务不存在、不是本人任务或投递目标已撤权")
			}
			if target.State == "cancelled" {
				return encode(target, nil)
			}
			target.State = "cancelled"
			target.LastResult = "已由原请求管理员取消；未提交部分停止，已提交部分仅核对回执"
			target.Checkpoint = strings.TrimSpace(in.Checkpoint)
			if target.Checkpoint == "" {
				target.Checkpoint = "原请求者已明确取消；不再提交剩余内容，已提交部分仅核对回执。"
			}
			if err := m.agent.task(target); err != nil {
				return "", err
			}
			m.record(bot.Record{Event: "agent_task_cancelled", Category: "decision", ThreadID: taskDeliveryThread(target), Sender: p.trace.Sender, MessageID: p.trace.TraceID, Content: target.Title, Status: "cancelled", Reason: "owner_cross_conversation_task_cancelled", Origin: "agent"})
			return encode(target, nil)
		}

		sourceThread := in.SourceThread
		if sourceThread == "" {
			sourceThread = p.trace.Thread
		}
		if !p.trustedTaskSource(sourceThread) {
			return "", toolInputError("任务来源必须是本轮已成功读取的会话，不能由模型自行指定")
		}
		if !p.trustedTaskSourceMessages(sourceThread, in.SourceMessageIDs) {
			return "", toolInputError("任务消息来源必须来自本轮成功读取的原消息，不能由模型自行填写")
		}
		deliveryThread := in.DeliveryThread
		if deliveryThread == "" {
			deliveryThread = p.trace.Thread
			if sourceThread != p.trace.Thread && p.trace.IsGroup && p.privileged() {
				deliveryThread = p.trace.Sender
			}
		}
		if !p.conversationReadAllowed(deliveryThread) || sourceThread != p.trace.Thread && !p.privileged() {
			return "", toolInputError("任务投递目标未获授权")
		}
		t := agentTask{ParentID: p.trace.TaskID, OriginTrace: origin, OwnerRequested: p.privileged(), ID: in.ID, Account: p.trace.Account, Thread: deliveryThread, RequestThread: p.trace.Thread, SourceThread: sourceThread, SourceMessageIDs: append([]string(nil), in.SourceMessageIDs...), DeliveryThread: deliveryThread, Sender: p.trace.Sender, Title: in.Title}
		t.Constraints = p.taskConstraints("agent_task", "", "", "")
		t.Constraints.Thread = sourceThread
		t.Constraints.DeliveryThread = deliveryThread
		if sourceThread != p.trace.Thread {
			// Cross-session reads prove that a conversation was read, but do not
			// manufacture a user-event stamp or a group quote for a private task.
			t.Constraints.Source = nil
			t.Constraints.ReplyContext = nil
		}
		if sourceThread != deliveryThread {
			t.QuoteServerID = ""
		}
		if t.ID != "" {
			if old, ok := m.agent.lookupTask(t.ID, p.trace.Account, deliveryThread); ok && (old.State == "completed" || old.State == "cancelled") {
				return encode(old, nil)
			}
			found := false
			for _, old := range m.agent.list(p.trace.Account, deliveryThread) {
				if old.ID == t.ID {
					if old.Sender != p.trace.Sender {
						return "", toolInputError("只能调整本人发起的任务")
					}
					t = old
					found = true
					break
				}
			}
			if !found {
				return "", toolInputError("未找到本会话任务")
			}
		} else {
			if in.Key == "" || len(in.Key) > 128 || in.Title == "" {
				return "", toolInputError("新任务需要key和title")
			}
			t.ID = agentID(p.trace.Account, deliveryThread, p.trace.TraceID, in.Key)
			if old, ok := m.agent.lookupTask(t.ID, p.trace.Account, deliveryThread); ok && (old.State == "completed" || old.State == "cancelled") {
				return encode(old, nil)
			}
			for _, old := range m.agent.list(p.trace.Account, deliveryThread) {
				if old.ID == t.ID {
					t = old
					break
				}
			}
		}
		if t.PluginJob != nil {
			if in.State == "cancelled" {
				updated, err := m.cancelPluginTask(ctx, t)
				return encode(updated, err)
			}
			return "", toolInputError("工具任务由其持久生命周期管理，请使用plugin_job_status或plugin_job_cancel；不改写为已完成")
		}
		if t.Reminder != nil {
			return "", toolInputError("这是独立提醒任务；请用agent_reminder查询或取消，不能改写为其他程序任务")
		}
		t.Checkpoint = in.Checkpoint
		t.State = in.State
		if t.DeliveryJob != "" && t.State == "completed" {
			native, ok := m.native.(nativeTools)
			if !ok {
				return "", toolInputError("请先核对原发送回执，未标记完成")
			}
			status, e := native.ToolSendStatus(ctx, t.DeliveryJob)
			if e != nil || status.ThreadID != t.ReportSourceThread || status.State != "accepted" {
				t.State = "waiting"
				if in.DelaySeconds < 30 {
					in.DelaySeconds = 120
				}
				t.Checkpoint += "\n原发送仍未确认完成，继续核对同群聊天、修复和重试；不能以已查询状态代替完成交付。"
			}
		}
		t.Next = time.Now().UTC().Add(time.Duration(in.DelaySeconds) * time.Second)
		if in.Title != "" {
			t.Title = in.Title
		}
		if err := m.agent.task(t); err != nil {
			return "", err
		}
		m.record(bot.Record{Event: "agent_task_saved", Category: "decision", ThreadID: p.trace.Thread, Sender: p.trace.Sender, MessageID: p.trace.TraceID, Content: t.Title, Status: t.State, Reason: "persistent_checkpoint_saved", Origin: "agent"})
		requirements := "proactive permission, selected target, quiet hours and daily budget; pending is not delivery"
		if t.OwnerRequested {
			requirements = "continuation of an authenticated owner request; current owner and conversation authorization rechecked; pending is not delivery"
		}
		return encode(map[string]any{"task": t, "background_requires": requirements, "storage_file": m.agent.dir + "/agent-state.json", "due_at": t.Next}, nil)
	case "wechat_send_text":
		return wechatapp.SendText(ctx, args, &wechatTextHost{p: p, raw: args})
	case "wechat_account_action":
		return wechatapp.AccountAction(ctx, args, &wechatActionHost{p: p, tool: name, raw: args})
	}
	return "", toolInputError("未知Agent工具")
}

func (p *permittedTools) rememberAgentSend(thread, text, key string) {
	c := p.manager.Config().Companion
	if !c.Permissions.Remember {
		return
	}
	in := companion.Input{Channel: "wechat", Account: p.trace.Account, Thread: thread}
	without := c
	without.Permissions.Remember = false
	if err := p.manager.memory.Observe(in, without, time.Now()); err != nil {
		p.manager.log("agent_send_memory_failed")
		return
	}
	if err := p.manager.memory.AcceptedDelivery(in, text, key, c, time.Now()); err != nil {
		p.manager.log("agent_send_memory_failed")
	}
}
