package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/plugins"
)

func copyTask(t agentTask) agentTask {
	b, _ := json.Marshal(t)
	var out agentTask
	_ = json.Unmarshal(b, &out)
	return out
}
func (m *Manager) agentTaskActive(in companion.Input) bool {
	if in.TaskID == "" {
		return true
	}
	if t, ok := m.agent.lookupTask(in.TaskID, in.Account, in.Thread); ok {
		return (!m.mobileRetired || t.DeviceTask == "") && t.State != "cancelled" && t.State != "stopped"
	}
	return false
}
func (m *Manager) pluginRuntime() (*plugins.Runtime, error) {
	return plugins.New(m.plugins.installed())
}
func (p *permittedTools) pluginTaskAllowed() bool {
	a := p.manager.agent.config()
	return a.Enabled && a.TasksEnabled && !p.trace.Preview && !p.trace.Surf && p.trace.Account == p.manager.native.Account() && p.trace.Thread != "" && permittedRecipient(p.manager.Config().Config, p.trace.Thread)
}
func (p *permittedTools) pluginTaskTools() []mcptools.Tool {
	if !p.pluginTaskAllowed() {
		return nil
	}
	return []mcptools.Tool{
		agentTool("plugin_job_status", "Read or prepare an existing background plugin job in THIS conversation; does not create a new job. Use task_id returned by the original plugin. Completed output may be staged into media IDs for final reply. Input {task_id:string}.", `{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false}`),
		agentTool("plugin_job_cancel", "Stop this requester's existing background plugin job and future delivery. Calls the owner-configured remote cancel endpoint once when supported. An uncertain cancel is never automatically replayed. Input {task_id:string}.", `{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false}`),
	}
}
func pluginJobSummary(t agentTask) map[string]any {
	return map[string]any{"task_id": t.ID, "job_id": t.PluginJob.Ticket.RemoteID, "state": t.State, "remote_state": t.PluginJob.Ticket.State, "automatic_followup": t.State == "waiting" || t.State == "pending", "cancel_state": t.PluginJob.CancelState, "instruction": "这是同一条已保存外部任务的状态，不代表微信已发送。等待完成后自动交付原会话，不重新创建；也可以查询plugin_job_status或取消plugin_job_cancel。不要向群友展示内部编号。"}
}
func (p *permittedTools) startPluginJob(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if !p.pluginTaskAllowed() {
		return "", toolInputError("后台工具任务需要启用持续任务并选择已授权会话；预演不创建远端任务")
	}
	var arg map[string]any
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.UseNumber()
	if decoder.Decode(&arg) != nil || arg == nil || len(args) > 8192 {
		return "", toolInputError("工具参数无效")
	}
	canonical, _ := json.Marshal(arg)
	pollOnly := p.externalPollOnly(name)
	if err := p.runtime.CheckJobArgs(name, canonical); !pollOnly && err != nil {
		return "", err
	}
	id := "plugin-" + agentID(p.trace.Account, p.trace.Thread, p.trace.TraceID, name)[:48]
	for _, old := range p.manager.agent.list(p.trace.Account, p.trace.Thread) {
		if old.PluginJob != nil && (old.ID == id || p.trace.TaskID == old.ID) {
			b, _ := json.Marshal(pluginJobSummary(old))
			return string(b), nil
		}
	}
	bridge, legacyHash, err := p.manager.externalHTTPBridge()
	if err != nil {
		return "", err
	}
	capability := externalJobCapability
	var ticket plugins.JobTicket
	var interval, deadline, polls int
	var pollState *externalPollState
	if pollOnly {
		capability = externalPollCapability
		protocol, e := bridge.PollProtocol(name)
		if e != nil {
			return "", e
		}
		ticket = plugins.JobTicket{Tool: name, Fingerprint: protocol.Tool.Fingerprint, State: "submitting"}
		var timeout int
		interval, polls, timeout, err = bridge.PollLimits(name)
		if err != nil {
			return "", err
		}
		deadline = timeout + polls*(interval+timeout)
		pollState = &externalPollState{Arguments: append(json.RawMessage(nil), canonical...)}
	} else {
		ticket, err = p.runtime.NewJobTicket(name)
		if err != nil {
			return "", err
		}
		interval, deadline, polls = p.runtime.JobLimits(name)
		protocol, e := bridge.JobProtocol(name)
		if e != nil || protocol.Tool.Fingerprint != ticket.Fingerprint {
			return "", plugins.ErrJobConfigChanged
		}
	}
	if _, _, _, err = p.manager.externalPackageCapability(ctx, capability); err != nil {
		return "", err
	}
	origin, err := cloneProjectPluginTrace(p.trace)
	if err != nil {
		return "", err
	}
	t := agentTask{ID: id, Account: p.trace.Account, Thread: p.trace.Thread, Sender: p.trace.Sender, Title: "工具任务 · " + name, OriginTrace: p.trace.TraceID, QuoteServerID: p.trace.QuoteServerID, RequestedText: logText(companion.UserRequestText(p.trace.Text), 2000), State: "waiting", Next: time.Now().Add(time.Duration(interval) * time.Second), OwnerRequested: p.privileged(), PluginJob: &pluginJobState{PollState: pollState, Origin: &origin, LegacyConfigurationSHA256: legacyHash, Ticket: ticket, Deadline: time.Now().Add(time.Duration(deadline) * time.Second), IntervalSeconds: interval, MaxPolls: polls}}
	t.Constraints = (&permittedTools{manager: p.manager, trace: origin}).taskConstraints("external-job", "", "", "")
	// Claim the task before the POST. The claim itself is scoped and atomic.
	a := p.manager.agent
	a.mu.Lock()
	if _, exists := a.data.Tasks[id]; exists {
		old := a.data.Tasks[id]
		a.mu.Unlock()
		b, _ := json.Marshal(pluginJobSummary(old))
		return string(b), nil
	}
	t.Updated = time.Now().UTC()
	a.data.Tasks[id] = t
	if err = a.persist(); err != nil {
		delete(a.data.Tasks, id)
		a.mu.Unlock()
		return "", err
	}
	a.mu.Unlock()
	// The in-memory ledger must own a distinct copy from the network caller.
	t = copyTask(t)
	var created plugins.JobTicket
	var callErr error
	if pollOnly {
		created, t.PluginJob.PollState.Initial, callErr = p.manager.callExternalPollPhase(ctx, t, "start")
	} else {
		created, callErr = p.manager.callExternalJobPhase(ctx, t, "start", canonical)
	}
	if callErr != nil && created.RemoteID != "" {
		created.State = "waiting"
		callErr = nil
	}
	t.PluginJob.Ticket = created
	if pollOnly && callErr == nil {
		t.Next = time.Now().Add(time.Duration(interval) * time.Second)
	}
	if callErr != nil {
		t.State = "needs_review"
		t.LastResult = "工具创建结果无法确认，未自动重试；请核对服务商记录"
	} else {
		t.LastResult = "外部任务已领取，后台查询同一任务，不重新创建"
	}
	saved, saveErr := a.updateSnapshot(t)
	if saveErr != nil {
		return "", saveErr
	}
	if !saved {
		for _, old := range a.list(t.Account, t.Thread) {
			if old.ID == t.ID {
				b, _ := json.Marshal(pluginJobSummary(old))
				return string(b), nil
			}
		}
		return "", toolInputError("工具任务状态已改变")
	}
	p.manager.record(bot.Record{Event: "plugin_job_submitted", Category: "tool", ThreadID: t.Thread, MessageID: t.OriginTrace, Tool: name, Status: t.State, Reason: "plugin_job_durable_claim", Content: t.LastResult, Origin: "agent"})
	if callErr != nil {
		return "", toolInputError("plugin_job_submission_unknown")
	}
	b, _ := json.Marshal(pluginJobSummary(t))
	return string(b), nil
}
func (p *permittedTools) callPluginJob(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var in struct {
		TaskID string `json:"task_id"`
	}
	if json.Unmarshal(args, &in) != nil || len(in.TaskID) > 128 {
		return "", toolInputError("工具任务参数无效")
	}
	var task *agentTask
	for _, old := range p.manager.agent.list(p.trace.Account, p.trace.Thread) {
		if old.ID == in.TaskID && old.PluginJob != nil {
			copy := copyTask(old)
			task = &copy
		}
	}
	if task == nil || task.Sender != p.trace.Sender && !p.privileged() {
		return "", toolInputError("只能查询或取消本人在本会话发起的工具任务")
	}
	if name == "plugin_job_cancel" {
		out, err := p.manager.cancelPluginTask(ctx, *task)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(pluginJobSummary(out))
		return string(b), nil
	}
	if task.State == "cancelled" || task.State == "completed" || task.PluginJob.Ticket.State != "completed" {
		b, _ := json.Marshal(pluginJobSummary(*task))
		return string(b), nil
	}
	if p.trace.TaskID != task.ID || task.State != "running" {
		b, _ := json.Marshal(pluginJobSummary(*task))
		return string(b), nil
	}
	if task.PluginJob.LegacyConfigurationSHA256 == "" || task.PluginJob.Origin == nil {
		return "", toolInputError("legacy_configuration_or_source_proof_missing")
	}
	if _, _, _, err := p.manager.externalPackageCapability(ctx, externalTaskCapability(*task)); err != nil {
		return "", err
	}
	_, configurationHash, err := p.manager.externalHTTPBridge()
	if err != nil || configurationHash != task.PluginJob.LegacyConfigurationSHA256 {
		return "", plugins.ErrJobConfigChanged
	}
	if err = p.manager.validateTaskSource(*task); err != nil {
		return "", err
	}
	if task.Delivery != nil || task.PluginJob.Prepared {
		return "", toolInputError("任务已进入微信交付，请核对原回执，不重复暂存或发送")
	}
	if !p.enabled(task.PluginJob.Ticket.Tool) {
		return "", toolInputError("原工具或媒体权限已关闭")
	}
	if task.PluginJob.PollState != nil {
		bridge, _, e := p.manager.externalHTTPBridge()
		if e != nil {
			return "", e
		}
		current, e := bridge.PollProtocol(task.PluginJob.Ticket.Tool)
		if e != nil || current.Tool.Fingerprint != task.PluginJob.Ticket.Fingerprint {
			return "", plugins.ErrJobConfigChanged
		}
	} else {
		current, e := p.runtime.NewJobTicket(task.PluginJob.Ticket.Tool)
		if e != nil {
			return "", e
		}
		if current.Fingerprint != task.PluginJob.Ticket.Fingerprint {
			return "", toolInputError("工具声明已改变，旧任务未交给新接口")
		}
	}
	output := task.PluginJob.Ticket.Output
	if task.PluginJob.PollState != nil {
		var readPolicy *externalHTTPPolicy
		output, readPolicy, err = p.manager.externalPollCachedOutput(ctx, *task)
		if err != nil {
			return "", err
		}
		ctx = withOutputAuthority(ctx, func(check context.Context) error {
			return readPolicy.Check(check, pluginhost.Job{Plugin: externalHTTPPluginID, Tool: externalPollCapability, Context: readPolicy.invocation, Arguments: readPolicy.arguments}, pluginhost.Capability{})
		})
	}
	result, err := p.stageOutput(ctx, output)
	if err != nil {
		return "", err
	}
	task.PluginJob.Prepared = true
	if saved, err := p.manager.agent.updateSnapshot(*task); err != nil {
		return "", err
	} else if !saved {
		return "", toolInputError("任务状态已改变，未交付过期结果")
	}
	var staged struct {
		Media []companion.GeneratedMedia `json:"media"`
	}
	if json.Unmarshal([]byte(result), &staged) == nil && len(staged.Media) > 0 {
		var body map[string]any
		_ = json.Unmarshal([]byte(result), &body)
		ids := []string{}
		for _, item := range staged.Media {
			ids = append(ids, item.ID)
		}
		body["required_media_ids"] = ids
		b, _ := json.Marshal(body)
		result = string(b)
	}
	return result, nil
}
func (m *Manager) claimPluginTask(task agentTask, now time.Time) (*companion.Input, error) {
	task = copyTask(task)
	job := task.PluginJob
	if job.LegacyConfigurationSHA256 == "" || job.Origin == nil {
		task.State = "needs_review"
		task.LastResult = "legacy_configuration_proof_missing：保留原任务及远端编号，缺少可验证的原配置或来源，已停止续办，不重提或改绑"
		if job.LegacyConfigurationSHA256 != "" {
			task.LastResult = "legacy_source_proof_missing：原来源证明不可追溯，保留原任务待核验，不推断新的来源授权"
		}
		_, err := m.agent.updateSnapshot(task)
		return nil, err
	}
	if job.Ticket.State == "submitting" {
		task.State = "needs_review"
		task.LastResult = "创建结果未知，不自动重新创建"
		_, err := m.agent.updateSnapshot(task)
		return nil, err
	}
	p, _, err := m.toolAccess()
	if err != nil {
		return nil, err
	}
	defer p.hub.Close()
	trace, traceErr := m.externalJobTrace(task)
	if traceErr != nil {
		return nil, traceErr
	}
	p.SetTrace(trace)
	if !p.enabled(job.Ticket.Tool) {
		task.LastResult = "工具或任务权限已关闭，保留任务，不创建替代任务"
		task.Next = now.Add(time.Minute)
		_, err := m.agent.updateSnapshot(task)
		return nil, err
	}
	if _, _, _, readyErr := m.externalPackageCapability(context.Background(), externalTaskCapability(task)); readyErr != nil {
		task.LastResult = "外部任务工具未安装、停用或不可用；保留同一任务及原远端编号，不创建替代任务"
		task.Next = now.Add(time.Minute)
		_, err := m.agent.updateSnapshot(task)
		return nil, err
	}
	if job.Ticket.State == "waiting" {
		if !now.Before(job.Deadline) || job.Polls >= job.MaxPolls {
			task.State = "needs_review"
			task.LastResult = "工具任务超过本地查询期限或次数，未自动重新生成；远端可能仍在运行"
			_, err := m.agent.updateSnapshot(task)
			return nil, err
		}
		timeout := 4
		if job.PollState != nil {
			bridge, _, e := m.externalHTTPBridge()
			if e != nil {
				return nil, e
			}
			_, _, timeout, e = bridge.PollLimits(job.Ticket.Tool)
			if e != nil {
				return nil, e
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		var ticket plugins.JobTicket
		var pollErr error
		if job.PollState != nil {
			ticket, _, pollErr = m.callExternalPollPhase(ctx, task, "query")
		} else {
			ticket, pollErr = m.callExternalJobPhase(ctx, task, "poll", json.RawMessage(`{}`))
		}
		cancel()
		job.Polls++
		if pollErr != nil {
			if errors.Is(pollErr, plugins.ErrJobConfigChanged) {
				task.State = "needs_review"
				task.LastResult = "工具声明已改变，旧任务未交给新接口；请核对原服务商任务"
				_, err := m.agent.updateSnapshot(task)
				return nil, err
			}
			task.LastResult = "工具任务状态暂时不可读，稍后查询同一任务，不重新创建"
			task.Next = now.Add(time.Duration(job.IntervalSeconds) * time.Second)
			_, err := m.agent.updateSnapshot(task)
			return nil, err
		}
		job.Ticket = ticket
		if ticket.State == "waiting" {
			task.LastResult = "外部任务排队或执行中，尚未取得完成结果"
			task.Next = now.Add(time.Duration(job.IntervalSeconds) * time.Second)
			_, err := m.agent.updateSnapshot(task)
			return nil, err
		}
	}
	task.LastResult = "工具结果已就绪，交回主模型处理"
	if job.Ticket.State == "failed" {
		job.Failure = true
		job.Prepared = true
		task.LastResult = "工具报告失败，交回主模型处理"
	}
	facts := map[string]any{"plugin_task": task.ID, "remote_state": job.Ticket.State, "failed": job.Failure}
	in, err := m.claimTaskResult(task, now, facts, "调用plugin_job_status取得同一任务真实输出；不要重新调用创建工具。失败时根据具体反馈诊断并验证。")
	if err != nil || in == nil {
		return nil, err
	}
	// Preserve the original named-voice expectations on the continuation. They
	// remain evidence requirements, never claims that an external API met them.
	if job.Origin != nil {
		in.ExpectedVoiceID = job.Origin.ExpectedVoiceID
		in.ExpectedVoiceProfile = job.Origin.ExpectedVoiceProfile
	}
	m.record(bot.Record{Event: "plugin_job_result_ready", Category: "tool", ThreadID: task.Thread, MessageID: task.OriginTrace, Tool: job.Ticket.Tool, Status: job.Ticket.State, Reason: "plugin_job_same_task_result", Origin: "agent"})
	return in, nil
}
func (m *Manager) cancelPluginTask(ctx context.Context, task agentTask) (agentTask, error) {
	if task.State == "cancelled" || task.State == "completed" {
		return task, nil
	}
	task = copyTask(task)
	task.State = "cancelled"
	task.LastResult = "已停止本地跟进和后续交付；正在核对远端取消能力"
	task.PluginJob.CancelState = "claimed"
	if task.PluginJob.PollState != nil {
		task.PluginJob.CancelState = "local_only"
		task.LastResult = "已停止本地查询及后续交付；此声明没有远端取消端点，不代表远端已停止"
	}
	a := m.agent
	a.mu.Lock()
	current, ok := a.data.Tasks[task.ID]
	if !ok {
		a.mu.Unlock()
		return task, errors.New("任务不存在")
	}
	if current.State == "cancelled" {
		a.mu.Unlock()
		return current, nil
	}
	if !current.Updated.Equal(task.Updated) {
		a.mu.Unlock()
		return task, errors.New("任务状态已改变，请刷新")
	}
	task.Updated = time.Now().UTC()
	a.data.Tasks[task.ID] = copyTask(task)
	if err := a.persist(); err != nil {
		a.data.Tasks[task.ID] = current
		a.mu.Unlock()
		return task, err
	}
	a.mu.Unlock()
	if task.PluginJob.PollState != nil {
		return task, nil
	}
	bound, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := m.callExternalJobPhase(bound, task, "cancel", json.RawMessage(`{}`))
	cancel()
	task.PluginJob.CancelState = "requested"
	task.LastResult = "已停止本地交付，远端已接受取消请求；不代表远端已确认终止"
	if err != nil {
		task.PluginJob.CancelState = "unconfirmed"
		task.LastResult = "已停止本地交付；远端取消不支持、不可用或结果未知，未自动重试"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current = a.data.Tasks[task.ID]
	if current.State == "cancelled" && current.PluginJob.CancelState == "claimed" {
		a.data.Tasks[task.ID] = copyTask(task)
		if err := a.persist(); err != nil {
			a.data.Tasks[task.ID] = current
			return task, err
		}
	}
	return task, nil
}
