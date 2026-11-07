package adapter

import (
	"context"
	"encoding/json"

	"wechat-bot/internal/linuxnative"
)

type DeliveryRequest struct {
	Job    string `json:"job_id"`
	Key    string `json:"request_key"`
	Thread string `json:"thread_id"`
}

type DeliverySession interface {
	Check(context.Context) error
	Status() linuxnative.ToolStatus
	CanReconcileChat() bool
	ReconcileChat(context.Context) (linuxnative.DeliveryReconciliation, error)
	Retry(context.Context) (string, error)
}
type DeliveryHost interface {
	CurrentThread() string
	ValidID(string) bool
	Invalid(string) error
	ReadDelivery(context.Context, DeliveryRequest) (DeliverySession, error)
}

func Delivery(ctx context.Context, name string, raw json.RawMessage, host DeliveryHost) (string, error) {
	var in DeliveryRequest
	if json.Unmarshal(raw, &in) != nil || !host.ValidID(in.Job) {
		return "", host.Invalid("需要原发送任务编号")
	}
	if in.Thread == "" {
		in.Thread = host.CurrentThread()
	}
	session, err := host.ReadDelivery(ctx, in)
	if err != nil {
		return "", err
	}
	status := session.Status()
	if name == "wechat_delivery_status" {
		b, _ := json.Marshal(status)
		var result map[string]any
		_ = json.Unmarshal(b, &result)
		result["retryable"] = status.Retryable
		if session.CanReconcileChat() && (status.State == "needs_review" || status.State == "unknown") {
			proof, scanErr := session.ReconcileChat(ctx)
			result["chat_reconciliation"] = proof
			if scanErr != nil {
				result["next_action"] = "聊天读取失败，保存检查点稍后核对；不能据此断言未发送"
			} else {
				result["next_action"] = "已核对同群真实聊天；用wechat_delivery_retry续办原任务，工具会保留已接受和同步确认的分段。retryable=false只是底层原来的自动续传标记，不是停止恢复的理由"
			}
		}
		if err := session.Check(ctx); err != nil {
			return "", err
		}
		b, err = json.Marshal(result)
		return string(b), err
	}
	if name != "wechat_delivery_retry" {
		return "", host.Invalid("未知微信投递工具")
	}
	if status.CancelRequested {
		return "", host.Invalid("用户已取消原发送，停止补发")
	}
	return session.Retry(ctx)
}
