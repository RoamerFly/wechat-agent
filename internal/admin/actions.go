package admin

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
)

type nativeActions interface {
	ExecuteAction(context.Context, linuxnative.AdminAction, string) (linuxnative.ActionStatus, error)
	InspectAction(context.Context, string) (linuxnative.ActionStatus, error)
}

func (s *Server) actionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/actions", s.protected(func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"items": s.manager.actions.List(s.manager.native.Account())})
	}))
	mux.HandleFunc("POST /api/actions/prepare", s.protected(s.prepareAction))
	mux.HandleFunc("POST /api/actions/{id}/approve", s.protected(s.approveAction))
	mux.HandleFunc("POST /api/actions/{id}/cancel", s.protected(s.cancelAction))
	mux.HandleFunc("GET /api/actions/{id}", s.protected(s.inspectAction))
}
func (s *Server) prepareAction(w http.ResponseWriter, r *http.Request) {
	var input linuxnative.AdminAction
	if !decode(w, r, &input) {
		return
	}
	if _, ok := s.manager.native.(nativeActions); !ok {
		fail(w, 501, "当前后台未接入账户操作")
		return
	}
	m := s.manager
	m.op.Lock()
	defer m.op.Unlock()
	c := m.Config().Config
	labels := map[string]string{"rename": "修改群名", "my_name": "修改我的群昵称", "mute": "设置群免打扰", "add_members": "添加群成员", "remove_members": "移出群成员", "leave": "退出群聊", "clear_announcement": "清空群公告", "create": "创建群聊", "recall": "撤回自己的消息"}
	label, ok := labels[input.Kind]
	if !ok || !utf8.ValidString(input.Text) || len(input.Text) > 256 || strings.ContainsRune(input.Text, '\x00') {
		fail(w, 400, "操作类型或文字无效")
		return
	}
	if input.Kind != "create" && (!permittedRecipient(c, input.ThreadID) || !validID(input.ThreadID)) {
		fail(w, 403, "操作目标必须是已授权会话")
		return
	}
	if input.Kind != "create" && input.Kind != "recall" && !strings.HasSuffix(input.ThreadID, "@chatroom") {
		fail(w, 400, "这个操作需要群聊目标")
		return
	}
	if input.Kind == "rename" || input.Kind == "my_name" {
		if strings.TrimSpace(input.Text) == "" {
			fail(w, 400, "请填写新名称")
			return
		}
	}
	if input.Kind == "create" || input.Kind == "add_members" || input.Kind == "remove_members" {
		min := 1
		if input.Kind == "create" {
			min = 2
		}
		if len(input.Members) < min || len(input.Members) > 40 {
			fail(w, 400, "成员数量不符合限制")
			return
		}
		seen := map[string]bool{}
		for _, id := range input.Members {
			if !validID(id) || strings.HasSuffix(id, "@chatroom") || seen[id] {
				fail(w, 400, "成员 ID 无效或重复")
				return
			}
			seen[id] = true
		}
	}
	if input.Kind == "recall" {
		msg, err := m.native.Message(r.Context(), input.MessageID)
		if err != nil || !matchesOwnWechatMessage(msg, m.native.Account(), input.ThreadID, input.MessageID) || msg.ServerID != input.ServerID {
			fail(w, 400, "只能撤回所选会话中具有准确服务端编号的本人消息")
			return
		}
	}
	id := randomToken()
	summary := label + " · " + input.ThreadID
	if input.Kind == "create" {
		summary = label + " · " + input.Text
	}
	proposal := wechatapp.ActionProposal{ID: id, Account: m.native.Account(), Request: input, Summary: summary, State: "awaiting_approval", Created: time.Now().UTC()}
	if err := m.actions.Save(proposal); err != nil {
		fail(w, 500, err.Error())
		return
	}
	m.log("admin_action_prepared")
	respond(w, 201, proposal)
}
func (s *Server) approveAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirm string `json:"confirm"`
	}
	if !decode(w, r, &input) {
		return
	}
	m := s.manager
	m.op.Lock()
	defer m.op.Unlock()
	p, ok := m.actions.Read(r.PathValue("id"), m.native.Account())
	if !ok {
		fail(w, 404, "未找到待审批操作")
		return
	}
	if p.State != "awaiting_approval" {
		respond(w, 200, p)
		return
	}
	if m.connectionPaused() {
		fail(w, 409, "微信连接已暂停，未提交操作")
		return
	}
	if input.Confirm != p.ID {
		fail(w, 400, "需要确认当前操作编号")
		return
	}
	if time.Since(p.Created) > 15*time.Minute {
		p.State = "expired"
		_ = m.actions.Save(p)
		fail(w, 409, "审批已过期，请重新准备")
		return
	}
	if p.Request.Kind != "create" && !permittedRecipient(m.Config().Config, p.Request.ThreadID) {
		fail(w, 403, "会话授权已取消")
		return
	}
	native, ok := m.native.(nativeActions)
	if !ok {
		fail(w, 501, "后台操作不可用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if p.Request.Kind == "recall" {
		msg, err := m.native.Message(ctx, p.Request.MessageID)
		if err != nil || !matchesOwnWechatMessage(msg, p.Account, p.Request.ThreadID, p.Request.MessageID) || msg.ServerID != p.Request.ServerID {
			fail(w, 409, "原撤回目标已变化，尚未提交")
			return
		}
	}
	intent := ownerActionIntent(p)
	p.AppAction = &intent
	p.State = "unknown"
	if err := m.actions.Save(p); err != nil {
		fail(w, 500, "无法保存操作领取记录，未执行")
		return
	}
	status, err := native.ExecuteAction(ctx, p.Request, intent.IdempotencyKey)
	if err == nil {
		p.Result = status
		p.State = status.State
	}
	if saveErr := m.actions.Save(p); saveErr != nil {
		fail(w, 500, "操作可能已提交，记录保存失败；不要重复审批")
		return
	}
	m.log("admin_action_submitted_no_retry")
	respond(w, 202, p)
}
func (s *Server) cancelAction(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if !decode(w, r, &input) {
		return
	}
	m := s.manager
	m.op.Lock()
	defer m.op.Unlock()
	p, ok := m.actions.Read(r.PathValue("id"), m.native.Account())
	if !ok {
		fail(w, 404, "未找到操作")
		return
	}
	if p.State != "awaiting_approval" {
		fail(w, 409, "只允许取消尚未审批的草稿")
		return
	}
	p.State = "cancelled"
	if err := m.actions.Save(p); err != nil {
		fail(w, 500, err.Error())
		return
	}
	respond(w, 200, p)
}
func (s *Server) inspectAction(w http.ResponseWriter, r *http.Request) {
	m := s.manager
	m.op.Lock()
	defer m.op.Unlock()
	p, ok := m.actions.Read(r.PathValue("id"), m.native.Account())
	if !ok {
		fail(w, 404, "未找到操作")
		return
	}
	if p.Result.ID != "" {
		native, ok := m.native.(nativeActions)
		if !ok {
			fail(w, 501, "后台操作不可用")
			return
		}
		status, err := native.InspectAction(r.Context(), p.Result.ID)
		if err != nil || !matchesWechatAction(p.Result, status) {
			fail(w, 502, "操作查询失败，不要重复执行")
			return
		}
		p.Result = status
		p.State = status.State
		if err = m.actions.Save(p); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	respond(w, 200, p)
}
