package admin

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type wechatDeliveryHost struct {
	p    *permittedTools
	name string
	raw  json.RawMessage
}

func (h *wechatDeliveryHost) CurrentThread() string        { return h.p.trace.Thread }
func (h *wechatDeliveryHost) ValidID(id string) bool       { return validID(id) }
func (h *wechatDeliveryHost) Invalid(message string) error { return toolInputError(message) }
func (h *wechatDeliveryHost) ReadDelivery(ctx context.Context, in wechatapp.DeliveryRequest) (wechatapp.DeliverySession, error) {
	var expected wechatapp.DeliveryRequest
	if json.Unmarshal(h.raw, &expected) != nil {
		return nil, h.Invalid("需要原发送任务编号")
	}
	if expected.Thread == "" {
		expected.Thread = h.CurrentThread()
	}
	if in != expected || !validID(in.Job) {
		return nil, h.Invalid("发送任务不属于原请求")
	}
	session := &wechatDeliverySession{host: h, in: in}
	if err := session.Check(ctx); err != nil {
		return nil, err
	}
	native, ok := h.p.manager.native.(nativeTools)
	if !ok {
		return nil, h.Invalid("原生发送回执不可用")
	}
	status, err := native.ToolSendStatus(ctx, in.Job)
	if err != nil {
		return nil, h.Invalid("暂未取得原发送任务状态；未重复提交")
	}
	if status.ID != in.Job || status.ThreadID != in.Thread {
		return nil, h.Invalid("发送任务不属于当前会话")
	}
	session.status = status
	if err := session.Check(ctx); err != nil {
		return nil, err
	}
	h.p.routeCrossConversationThread(in.Thread)
	return session, nil
}

type wechatDeliverySession struct {
	host   *wechatDeliveryHost
	in     wechatapp.DeliveryRequest
	status linuxnative.ToolStatus
}

func (s *wechatDeliverySession) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := s.host.p
	if !p.agentToolEnabled(s.host.name) || !p.conversationReadAllowed(s.in.Thread) {
		return toolInputError("未授权跨会话核对发送任务")
	}
	return nil
}
func (s *wechatDeliverySession) Status() linuxnative.ToolStatus {
	out := s.status
	out.VoiceChunks = slices.Clone(out.VoiceChunks)
	return out
}
func (s *wechatDeliverySession) CanReconcileChat() bool {
	_, ok := s.host.p.manager.native.(nativeCatalog)
	return ok
}
func (s *wechatDeliverySession) ReconcileChat(ctx context.Context) (linuxnative.DeliveryReconciliation, error) {
	if err := s.Check(ctx); err != nil {
		return linuxnative.DeliveryReconciliation{}, err
	}
	reader, ok := s.host.p.manager.native.(nativeCatalog)
	if !ok {
		return linuxnative.DeliveryReconciliation{}, errors.New("后台不支持会话读取")
	}
	proof, err := linuxnative.ReconcileMedia(ctx, reader, s.status)
	if denied := s.Check(ctx); denied != nil {
		return linuxnative.DeliveryReconciliation{}, denied
	}
	return proof, err
}

func (s *wechatDeliverySession) retryAllowed(ctx context.Context, status linuxnative.ToolStatus) error {
	if err := s.Check(ctx); err != nil {
		return err
	}
	p := s.host.p
	if !p.privileged() || s.in.Key == "" || len(s.in.Key) > 128 {
		return toolInputError("仅可信主人可续传，需提供稳定请求编号")
	}
	if status.ID != s.in.Job || status.ThreadID != s.in.Thread || status.SourceID != s.status.SourceID {
		return toolInputError("原发送任务或媒体来源已变化，未续传")
	}
	if status.CancelRequested || status.State == "cancelled" || !p.manager.agentTaskActive(p.trace) {
		return toolInputError("用户已取消原发送，停止补发")
	}
	for _, kind := range []string{"voice", "file", "image", "video"} {
		if _, exists := p.manager.tools.source(status.SourceID, p.trace.Account, kind); exists && companion.MediaPermitted(p.manager.Config().Companion.Permissions, kind) {
			return p.manager.checkWechatSubmission(ctx)
		}
	}
	return toolInputError("原媒体来源未获当前发送权限；未续传")
}

func (s *wechatDeliverySession) Retry(ctx context.Context) (string, error) {
	p := s.host.p
	if s.host.name != "wechat_delivery_retry" {
		return "", toolInputError("本次请求只允许查询回执")
	}
	if err := s.retryAllowed(ctx, s.status); err != nil {
		return "", err
	}
	retry, ok := p.manager.native.(interface {
		RetryMediaJob(context.Context, string) (linuxnative.ToolStatus, error)
	})
	if !ok {
		return "", toolInputError("原生后台不支持原任务续传")
	}
	return p.privilegedCall(ctx, s.host.name, s.host.raw, func() (any, error) {
		if err := s.retryAllowed(ctx, s.status); err != nil {
			return nil, err
		}
		if err := p.deliveryDelay(ctx); err != nil {
			return nil, err
		}
		reader := p.manager.native.(nativeTools)
		current, err := reader.ToolSendStatus(ctx, s.in.Job)
		if err != nil {
			return nil, toolInputError("等待后未取得原发送状态，保留原任务，尚未续传")
		}
		if err = s.retryAllowed(ctx, current); err != nil {
			return nil, err
		}
		current = current.WithVoiceReceipts()
		var proof apphost.DeliveryReceipt
		wechatapp.ProjectDeliveryReceipt(&proof, current)
		if proof.State == "accepted" {
			return current, nil
		}
		if !current.Retryable {
			if reconciler, ok := p.manager.native.(interface {
				ReconcileMediaJob(context.Context, string) (linuxnative.ToolStatus, error)
			}); ok {
				return reconciler.ReconcileMediaJob(ctx, s.in.Job)
			}
			return nil, errors.New("当前原生版本缺少聊天核对后恢复接口，需要修复服务；保存任务继续处理")
		}
		return retry.RetryMediaJob(ctx, s.in.Job)
	})
}
