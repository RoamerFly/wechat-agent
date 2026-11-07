package admin

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

// The two Agent tools must use the same explicit grant for the same mutation.
func (p *permittedTools) wechatActionAllowed(action linuxnative.AdminAction, allowCross bool, settings agentSettings) bool {
	if !p.privileged() || p.trace.TraceID == "" || !validID(action.ThreadID) ||
		!contains(settings.AccountActions, action.Kind) || !p.manager.agentTaskActive(p.trace) {
		return false
	}
	if allowCross {
		return p.conversationReadAllowed(action.ThreadID)
	}
	return permittedRecipient(p.manager.Config().Config, action.ThreadID)
}

func (p *permittedTools) submitWechatAction(ctx context.Context, tool string, raw json.RawMessage, action linuxnative.AdminAction, key string, settings agentSettings, allowCross bool) (string, error) {
	m := p.manager
	// Saving policy can hold op while waiting for this worker to stop. An old
	// worker must yield immediately if a management operation owns that lock.
	if !m.op.TryLock() {
		return "", toolInputError("账号状态正在更新，尚未提交操作")
	}
	defer m.op.Unlock()
	if err := m.checkWechatSubmission(ctx); err != nil {
		return "", err
	}
	current := m.agent.config()
	if current.Revision != settings.Revision || !p.wechatActionAllowed(action, allowCross, current) {
		return "", toolInputError("账号动作授权已变化，尚未提交操作")
	}
	native, ok := m.native.(nativeActions)
	if !ok {
		return "", toolInputError("后台不支持账号动作")
	}
	requestID := p.trace.OriginTraceID
	if requestID == "" {
		requestID = p.trace.TraceID
	}
	intent := apphost.ActionIntent{
		ConnectionRef: wechatConnectionRef(m.native.Account()), Action: action.Kind, RequesterID: p.trace.Sender,
		Source: "wechat", SourceThread: p.trace.Thread, SourceRequestID: requestID, Target: action.ThreadID,
		Purpose: "agent_account_action", GrantVersion: "agent:" + strconv.Itoa(current.Revision), IdempotencyKey: key,
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return p.claimPrivilegedCall(ctx, tool, raw, &intent, func() (any, error) {
		return native.ExecuteAction(ctx, action, intent.IdempotencyKey)
	})
}

func ownerActionIntent(p wechatapp.ActionProposal) apphost.ActionIntent {
	return apphost.ActionIntent{
		ConnectionRef: wechatConnectionRef(p.Account), Action: p.Request.Kind, RequesterID: "station_owner",
		Source: "local_admin", SourceRequestID: p.ID, Target: p.Request.ThreadID,
		Purpose: "owner_approved_account_action", GrantVersion: "owner_approval:" + p.ID, IdempotencyKey: p.ID,
	}
}

func matchesOwnWechatMessage(msg linuxnative.Message, account, thread, message string) bool {
	return wechatapp.OwnMessage(msg, account, thread, message)
}

func matchesWechatAction(previous, latest linuxnative.ActionStatus) bool {
	return previous.ID != "" && latest.ID == previous.ID && (previous.Kind == "" || latest.Kind == "" || latest.Kind == previous.Kind)
}

func (w wechatCompatibility) ReadAction(ctx context.Context, connection, id string) (apphost.ActionReceipt, error) {
	item, release, err := w.acquire(connection)
	if err != nil {
		return apphost.ActionReceipt{}, err
	}
	defer release()
	if !validID(id) {
		return apphost.ActionReceipt{}, apphost.ErrNotFound
	}
	m := item.manager
	if m == nil || m.actions == nil || m.agent == nil || m.native.Account() != item.record.ID {
		return apphost.ActionReceipt{}, apphost.ErrUnavailable
	}
	out := apphost.ActionReceipt{ConnectionRef: wechatConnectionRef(item.record.ID), OperationID: id, State: "submission_unknown", Lookup: "stored"}
	var status linuxnative.ActionStatus
	if proposal, ok := m.actions.Read(id, item.record.ID); ok {
		out.Action, out.Target = proposal.Request.Kind, proposal.Request.ThreadID
		switch proposal.State {
		case "awaiting_approval", "expired":
			out.State = "not_submitted"
		case "cancelled":
			out.State = "cancelled"
		}
		status = proposal.Result
	} else {
		m.agent.mu.Lock()
		op, ok := m.agent.data.Operations[id]
		m.agent.mu.Unlock()
		if !ok || op.AppAction == nil || op.AppAction.ConnectionRef != out.ConnectionRef {
			return apphost.ActionReceipt{}, apphost.ErrNotFound
		}
		out.Action, out.Target = op.AppAction.Action, op.AppAction.Target
		if err := json.Unmarshal([]byte(op.Result), &status); err != nil {
			return apphost.ActionReceipt{}, apphost.ErrUnavailable
		}
	}
	if status.ID == "" {
		return out, nil
	}
	wechatapp.ProjectActionReceipt(&out, status)
	if out.State == "succeeded" || out.State == "failed" || out.State == "cancelled" {
		return out, nil
	}
	native, ok := m.native.(nativeActions)
	if !ok {
		out.Lookup = "unavailable"
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	latest, err := native.InspectAction(ctx, status.ID)
	switch {
	case err != nil:
		out.Lookup = "unavailable"
	case !matchesWechatAction(status, latest):
		out.Lookup = "identity_mismatch"
	default:
		out.Lookup = "platform"
		wechatapp.ProjectActionReceipt(&out, latest)
	}
	return out, nil
}
