package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"time"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

// Console requests are authenticated management actions, not forged incoming
// WeChat messages. They reuse the same owner authorization and durable queue as
// Agent tool calls and never create a conversational reply as a side effect.
func (s *Server) crossMediaConsoleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/agent/media-batches/{id}/cancel", s.protected(func(w http.ResponseWriter, r *http.Request) {
		m := s.manager
		m.op.Lock()
		defer m.op.Unlock()
		m.crossSendMu.Lock()
		defer m.crossSendMu.Unlock()
		m.mu.Lock()
		stopped := m.status.State == "disabled" || (m.status.State == "paused" && m.done == nil)
		m.mu.Unlock()
		if !stopped {
			fail(w, 409, "请先停止 Agent，避免与正在提交的发送竞态")
			return
		}
		batch, err := m.readCrossBatch(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "未找到当前账号的有效投递批次")
			return
		}
		p := &permittedTools{manager: m, trace: companion.Input{Account: batch.Account, Thread: batch.Origin, Sender: batch.Sender}}
		if !p.crossAllowed(batch.Source, batch.Target) {
			fail(w, 403, "原投递权限已失效")
			return
		}
		if !batch.CancelledAt.IsZero() {
			respond(w, 200, map[string]any{"operation_id": batch.ID, "state": "cancelled"})
			return
		}
		if crossBatchAccepted(batch) {
			fail(w, 409, "原批次已有已接受的附件，不能取消")
			return
		}
		native, ok := m.native.(nativeTools)
		if !ok {
			fail(w, 501, "附件后台不可用")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		for _, part := range batch.Parts {
			_, err := native.LookupSend(ctx, part.Key)
			if !errors.Is(err, linuxnative.ErrToolNotFound) {
				fail(w, 409, "原发送可能已提交，请先核对微信回执")
				return
			}
		}
		batch.CancelledAt = time.Now().UTC()
		data, _ := json.Marshal(batch)
		if err := writePrivate(filepath.Join(m.dir, "cross-media", batch.ID+".json"), data); err != nil {
			fail(w, 500, "取消记录保存失败")
			return
		}
		if err := m.queueCrossBatch(batch); err != nil {
			fail(w, 500, "原批次已标记取消，队列清理待恢复")
			return
		}
		respond(w, 200, map[string]any{"operation_id": batch.ID, "state": "cancelled", "real_wechat_sends": 0})
	}))
	mux.HandleFunc("POST /api/agent/media-batches", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Owner string `json:"owner_id"`
			crossMediaInput
		}
		if !decode(w, r, &in) {
			return
		}
		m := s.manager
		actionID := "authenticated-admin-media-" + agentID(in.Key)
		p := &permittedTools{manager: m, trace: companion.Input{Channel: "wechat", Account: m.native.Account(), Thread: in.Owner, Sender: in.Owner, TraceID: actionID, OriginTraceID: actionID}}
		if in.Owner == "" || !p.privileged() || !m.agent.config().CrossConversation {
			fail(w, 403, "需要当前账号已授权的主人及跨会话权限")
			return
		}
		// Both ends must be explicit in a management action; never default the
		// destination to the owner's private chat when the console omits it.
		if in.Source == "" || in.Target == "" {
			fail(w, 400, "请明确来源会话和目标会话")
			return
		}
		if !p.crossAllowed(in.Source, in.Target) {
			fail(w, 403, "来源或目标会话未授权")
			return
		}
		// This mode is minted after authenticated/CSRF/owner/route admission.
		// The common sender still verifies the selected asset before queuing.
		p.trace.Mode = "admin_action"
		args, _ := json.Marshal(in.crossMediaInput)
		result, err := p.sendMedia(r.Context(), args)
		if err != nil {
			fail(w, 400, "附件投递参数或授权无效")
			return
		}
		m.record(bot.Record{Event: "admin_cross_media_request", Category: "tool", ThreadID: in.Target, Sender: in.Owner, MessageID: p.trace.TraceID, Origin: "admin", Reason: "synthetic_authenticated_admin_action"})
		respond(w, 202, map[string]any{"result": json.RawMessage(result), "origin": "synthetic_authenticated_admin_action", "conversational_replies": 0})
	}))
	mux.HandleFunc("POST /api/agent/media-batches/{id}/reconcile", s.protected(func(w http.ResponseWriter, r *http.Request) {
		m := s.manager
		batch, err := m.readCrossBatch(r.PathValue("id"))
		if err != nil {
			fail(w, 404, "未找到当前账号的有效投递批次")
			return
		}
		p := &permittedTools{manager: m, trace: companion.Input{Account: batch.Account, Thread: batch.Origin, Sender: batch.Sender}}
		if !p.crossAllowed(batch.Source, batch.Target) {
			fail(w, 403, "原投递权限已失效")
			return
		}
		result, err := p.crossMediaStatus(r.Context(), batch.ID)
		if err != nil {
			fail(w, 502, "原批次回执暂不可核对，请继续查询同一批次")
			return
		}
		respond(w, 200, map[string]any{"result": json.RawMessage(result), "real_wechat_sends": 0})
	}))
}
