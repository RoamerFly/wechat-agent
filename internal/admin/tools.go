package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/bot"
	"wechat-bot/internal/linuxnative"
)

type nativeTools interface {
	Capabilities(context.Context) (linuxnative.Capabilities, error)
	Upload(context.Context, string, string, io.Reader, int64) (linuxnative.UploadSource, error)
	Submit(context.Context, linuxnative.ToolSend) (linuxnative.ToolStatus, error)
	LookupSend(context.Context, string) (linuxnative.ToolStatus, error)
	ToolSendStatus(context.Context, string) (linuxnative.ToolStatus, error)
}

func toolsLimit(kind string) int64 {
	switch kind {
	case "image", "voice":
		return 25*1024*1024 - 17
	case "video":
		return 50 * 1024 * 1024
	case "file":
		return 512 * 1024 * 1024
	}
	return 0
}
func (s *Server) toolsNative(w http.ResponseWriter) (nativeTools, bool) {
	native, ok := s.manager.native.(nativeTools)
	if !ok {
		fail(w, 501, "当前后台不支持附件工具")
	}
	return native, ok
}
func permittedRecipient(c Config, id string) bool {
	for _, thread := range c.allowed() {
		if id == thread {
			return true
		}
	}
	return false
}
func (s *Server) toolRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tools/state", s.protected(s.toolsState))
	mux.HandleFunc("POST /api/tools/upload", s.protected(s.toolsUpload))
	mux.HandleFunc("POST /api/tools/send", s.protected(s.toolsSend))
	mux.HandleFunc("GET /api/tools/sends/{id}", s.protected(s.toolsStatus))
	mux.HandleFunc("GET /api/tools/lookup", s.protected(s.toolsLookup))
}
func (s *Server) toolsState(w http.ResponseWriter, r *http.Request) {
	native, ok := s.toolsNative(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	capabilities, err := native.Capabilities(ctx)
	if err != nil {
		fail(w, 502, "无法读取微信后台附件能力")
		return
	}
	c := s.manager.Config()
	recipients := []map[string]string{}
	for _, id := range c.allowed() {
		name := s.manager.memoryLabel(ctx, id)
		recipients = append(recipients, map[string]string{"id": id, "name": name})
	}
	limits := map[string]int64{}
	for _, kind := range capabilities.MediaKinds {
		limit := toolsLimit(kind)
		if upstream := capabilities.MediaSendMaxBytes[kind]; upstream > 0 && upstream < limit {
			limit = upstream
		}
		if limit > 0 {
			limits[kind] = limit
		}
	}
	respond(w, 200, map[string]any{"capabilities": capabilities, "recipients": recipients, "limits": limits, "recent_sends": s.manager.tools.recent(s.manager.native.Account())})
}
func (s *Server) toolsUpload(w http.ResponseWriter, r *http.Request) {
	native, ok := s.toolsNative(w)
	if !ok {
		return
	}
	ctx, cancel, err := manualUploadContext(w, r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	defer cancel()
	capabilities, err := native.Capabilities(ctx)
	if err != nil {
		fail(w, 502, "无法读取微信后台附件能力")
		return
	}
	var maximum int64
	for kind, upstream := range capabilities.MediaSendMaxBytes {
		if upstream > 0 {
			maximum = max(maximum, min(toolsLimit(kind), upstream))
		}
	}
	if maximum <= 0 {
		fail(w, 400, "微信后台未提供附件上传能力")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maximum+(64<<10))
	upload, err := stageManualUpload(ctx, r, filepath.Join(s.manager.dir, "manual-uploads"), maximum, availableStorageBytes)
	if err != nil {
		var parse *uploadFailure
		if errors.As(err, &parse) {
			fail(w, parse.status, parse.message)
		} else {
			fail(w, 400, "附件暂存失败")
		}
		return
	}
	defer upload.close()
	kind := upload.kind
	if upload.size > toolsLimit(kind) || capabilities.MediaSendMaxBytes[kind] <= 0 || upload.size > capabilities.MediaSendMaxBytes[kind] {
		fail(w, 400, "微信后台不支持这个类型或附件超过后台限制")
		return
	}
	source, err := native.Upload(ctx, upload.name, upload.contentType, upload.file, upload.size)
	if err != nil {
		fail(w, 502, "附件上传失败；未发送微信消息")
		return
	}
	if err = s.manager.tools.upload(stagedUpload{UploadSource: source, Account: s.manager.native.Account(), Kind: kind}); err != nil {
		fail(w, 500, "附件已暂存，但网页记录保存失败；未发送微信消息")
		return
	}
	s.manager.log("manual_attachment_staged")
	respond(w, 201, source)
}
func (s *Server) toolsSend(w http.ResponseWriter, r *http.Request) {
	native, ok := s.toolsNative(w)
	if !ok {
		return
	}
	var input linuxnative.ToolSend
	if !decode(w, r, &input) {
		return
	}
	if !validID(input.IdempotencyKey) || len(input.IdempotencyKey) > 128 || !validID(input.ThreadID) || strings.TrimSpace(input.Text) == "" && input.Kind == "text" || len(input.Text) > 4096 || !utf8.ValidString(input.Text) {
		fail(w, 400, "发送内容或操作编号无效")
		return
	}
	s.manager.op.Lock()
	defer s.manager.op.Unlock()
	if s.manager.connectionPaused() {
		fail(w, 409, "微信连接已暂停，未提交发送")
		return
	}
	c := s.manager.Config().Config
	if !permittedRecipient(c, input.ThreadID) {
		fail(w, 403, "这个接收方不在已授权回复会话中")
		return
	}
	if input.Kind != "text" {
		if toolsLimit(input.Kind) == 0 {
			fail(w, 400, "不支持这个附件类型")
			return
		}
		if _, exists := s.manager.tools.source(input.SourceID, s.manager.native.Account(), input.Kind); !exists {
			fail(w, 400, "附件不是本账号已上传的对应类型")
			return
		}
	}
	if input.Quote != nil {
		if input.Kind != "text" {
			fail(w, 400, "仅文字支持引用")
			return
		}
		msg, err := s.manager.native.Message(r.Context(), input.Quote.MessageID)
		if err != nil || msg.ThreadID != input.ThreadID || msg.ServerID == "" || msg.ServerID != input.Quote.ServerID {
			fail(w, 400, "引用消息不属于这个会话")
			return
		}
	}
	raw, _ := json.Marshal(input)
	hash := sha256.Sum256(raw)
	initial := linuxnative.ToolStatus{State: "unknown", Kind: input.Kind, ThreadID: input.ThreadID, Reason: "submission_result_pending"}
	record, isNew, err := s.manager.tools.claim(toolRecord{Key: input.IdempotencyKey, Hash: hex.EncodeToString(hash[:]), Account: s.manager.native.Account(), Thread: input.ThreadID, Kind: input.Kind, Status: initial, Created: time.Now().UTC()})
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if !isNew {
		respond(w, 202, record.Status)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	status, err := s.manager.submitWechat(ctx, native, input)
	if err != nil {
		initial.Reason = "submission_result_unknown_check_same_key"
		_ = s.manager.tools.update(input.IdempotencyKey, initial)
		s.manager.log("manual_send_outcome_unknown_not_retried")
		s.manager.recordManualSend(input, "unknown", initial.Reason, "")
		respond(w, 202, initial)
		return
	}
	s.manager.recordManualSend(input, status.State, status.Reason, status.ID)
	if err = s.manager.tools.update(input.IdempotencyKey, status); err != nil {
		s.manager.log("manual_send_receipt_persistence_failed")
		fail(w, 500, "发送已提交，但记录保存失败；请用原操作编号查询，不要重发")
		return
	}
	s.manager.log("manual_send_submitted")

	respond(w, 202, status)
}
func (s *Server) toolsStatus(w http.ResponseWriter, r *http.Request) {
	native, ok := s.toolsNative(w)
	if !ok {
		return
	}
	record, exists, err := s.manager.tools.byID(r.PathValue("id"), s.manager.native.Account())
	if err != nil {
		fail(w, 503, "历史发送记录不可核验，请检查存储；不要重新发送")
		return
	}
	if !exists {
		fail(w, 404, "未找到本网页创建的发送任务")
		return
	}
	if record.archived {
		respond(w, 200, record.Status)
		return
	}
	status, err := native.ToolSendStatus(r.Context(), record.Status.ID)
	if err != nil {
		fail(w, 502, "任务查询失败；请稍后查询，不要重发")
		return
	}
	status.Kind = record.Kind
	if record.Status.State != status.State {
		s.manager.record(bot.Record{Event: "manual_send_status", Category: sendCategory(status.State), ThreadID: record.Thread, Content: s.manager.manualContent(record.Key), Status: status.State, MessageID: record.Key, Origin: "manual", Reason: status.Reason, SendID: status.ID})
	}
	if err = s.manager.tools.update(record.Key, status); err != nil {
		fail(w, 500, "任务状态保存失败")
		return
	}
	respond(w, 200, status)
}
func (s *Server) toolsLookup(w http.ResponseWriter, r *http.Request) {
	native, ok := s.toolsNative(w)
	if !ok {
		return
	}
	key := r.URL.Query().Get("key")
	record, exists, err := s.manager.tools.record(key, s.manager.native.Account())
	if err != nil {
		fail(w, 503, "历史发送记录不可核验，请检查存储；不要重新发送")
		return
	}
	if !exists {
		fail(w, 404, "未找到本网页创建的操作编号")
		return
	}
	if record.archived {
		respond(w, 200, record.Status)
		return
	}
	status, err := native.LookupSend(r.Context(), key)
	if errors.Is(err, linuxnative.ErrToolNotFound) {
		status = record.Status
		status.Reason = "native_record_not_found_do_not_auto_resend"
		respond(w, 200, status)
		return
	}
	if err != nil {
		fail(w, 502, "任务查询失败；请稍后查询，不要重发")
		return
	}
	status.Kind = record.Kind
	if record.Status.State != status.State {
		s.manager.record(bot.Record{Event: "manual_send_status", Category: sendCategory(status.State), ThreadID: record.Thread, Content: s.manager.manualContent(record.Key), Status: status.State, MessageID: record.Key, Origin: "manual", Reason: status.Reason, SendID: status.ID})
	}
	if err = s.manager.tools.update(key, status); err != nil {
		fail(w, 500, "任务状态保存失败")
		return
	}
	respond(w, 200, status)
}
