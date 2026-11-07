package adapter

import (
	"context"
	"encoding/json"
	"strings"

	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

const StationGroupMuteToolName = "wechat_group_mute"

func StationGroupMuteTool() mcptools.Tool {
	return tool(StationGroupMuteToolName, "Manage notifications for groups specifically authorized by the owner on this local WeChat connection. In the owner's local chat, use action=list to find authorized connection and group IDs. Use action=set with connection_id, group_id and muted=true or false only when carrying out the owner's request. Use action=status with those two IDs to check the most recent operation without resubmitting. The host binds the request key. Pending and unknown receipts are not success. No message is sent.", `{"type":"object","properties":{"action":{"type":"string","enum":["list","set","status"]},"connection_id":{"type":"string","maxLength":128},"group_id":{"type":"string","maxLength":128},"muted":{"type":"boolean"}},"required":["action"],"additionalProperties":false}`)
}

// ActionSubmission is bound by the host to the original request and policy
// revision. Calling it must recheck that grant and use the existing ledger.
type ActionSubmission func(context.Context, linuxnative.AdminAction) (string, error)

type ActionHost interface {
	Account() string
	Thread() string
	Owner() bool
	ValidID(string) bool
	Invalid(string) error
	AuthorizeAction(context.Context, linuxnative.AdminAction, bool) (ActionSubmission, error)
	ReadMessage(context.Context, string) (linuxnative.Message, error)
}

func OwnMessage(msg linuxnative.Message, account, thread, message string) bool {
	return msg.AccountID == account && msg.ID == message && msg.ThreadID == thread && msg.Direction == "outgoing" && msg.ServerID != ""
}

// MatchGroupActionReceipt accepts only evidence from this account's exact
// operation and group. Native success also requires a remote acceptance.
func MatchGroupActionReceipt(status linuxnative.ActionStatus, account, action, group, operation string) bool {
	return status.AccountID == account && status.Kind == "group_mutation" && status.Action == action &&
		status.Target == group && status.ID != "" && (operation == "" || operation == status.ID) &&
		(status.State != "succeeded" || status.RemoteOutcome == "accepted")
}

// MatchRecallReceipt requires the same original outgoing message and remote confirmation.
func MatchRecallReceipt(status linuxnative.ActionStatus, account string, request linuxnative.AdminAction, operation string) bool {
	return status.AccountID == account && status.Kind == "message_recall" && status.ID != "" &&
		(operation == "" || status.ID == operation) && status.ThreadID == request.ThreadID &&
		status.MessageID == request.MessageID && status.ServerID == request.ServerID &&
		(status.State != "succeeded" || status.RemoteOutcome == "accepted" || status.RemoteOutcome == "confirmed")
}

func AccountAction(ctx context.Context, raw json.RawMessage, host ActionHost) (string, error) {
	var action linuxnative.AdminAction
	if json.Unmarshal(raw, &action) != nil ||
		(action.Kind != "recall" && action.Kind != "mute" && !strings.HasSuffix(action.ThreadID, "@chatroom")) ||
		len(action.Text) > 512 || len(action.Members) > 40 {
		return "", host.Invalid("账号动作未授权或参数无效")
	}
	submit, err := host.AuthorizeAction(ctx, action, false)
	if err != nil {
		return "", err
	}
	if action.Kind == "add_members" || action.Kind == "remove_members" {
		if len(action.Members) == 0 {
			return "", host.Invalid("成员不能为空")
		}
		for _, id := range action.Members {
			if !host.ValidID(id) || strings.HasSuffix(id, "@chatroom") {
				return "", host.Invalid("成员ID无效")
			}
		}
	}
	if action.Kind == "recall" {
		msg, err := host.ReadMessage(ctx, action.MessageID)
		if err != nil || !OwnMessage(msg, host.Account(), action.ThreadID, action.MessageID) || msg.ServerID != action.ServerID {
			return "", host.Invalid("只能撤回当前目标中准确匹配的本人消息")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return submit(ctx, action)
}

func Recall(ctx context.Context, raw json.RawMessage, host ActionHost) (string, error) {
	var in struct {
		Thread  string `json:"thread_id"`
		Message string `json:"message_id"`
		Key     string `json:"request_key"`
	}
	if json.Unmarshal(raw, &in) != nil || !host.Owner() || in.Message == "" || !host.ValidID(in.Key) {
		return "", host.Invalid("recall_requires_verified_owner_and_message")
	}
	if in.Thread == "" {
		in.Thread = host.Thread()
	}
	action := linuxnative.AdminAction{Kind: "recall", ThreadID: in.Thread, MessageID: in.Message}
	submit, err := host.AuthorizeAction(ctx, action, true)
	if err != nil {
		return "", err
	}
	msg, err := host.ReadMessage(ctx, in.Message)
	if err != nil {
		return "", err
	}
	if !OwnMessage(msg, host.Account(), in.Thread, in.Message) {
		return "", host.Invalid("recall_requires_exact_own_outgoing_message")
	}
	action.ServerID = msg.ServerID
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return submit(ctx, action)
}
