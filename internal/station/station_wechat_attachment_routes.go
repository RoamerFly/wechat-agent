package station

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/workspacefs"

	"wechat-bot/internal/websession"
)

func (s *stationWechat) attachmentRoutes(owner *websession.Store, mux *http.ServeMux) {
	mux.HandleFunc("POST /api/apps/wechat/connections/{connection}/attachments", owner.Protect(s.acquireAttachment))
	mux.HandleFunc("GET /api/apps/wechat/connections/{connection}/attachments/{asset}/content", owner.Protect(s.downloadAttachment))
}

func (s *stationWechat) ownerAttachmentAccess(ctx context.Context, id string, w *stationWechatWorker, in wechatapp.AttachmentRequest) (stationAttachmentAccess, error) {
	w.mu.Lock()
	grant := w.policy.AttachmentGrants[in.Thread]
	w.mu.Unlock()
	allowed := func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return s.ctx.Err() == nil && !grant.IsZero() && w.policy.AttachmentGrants[in.Thread].Equal(grant)
	}
	if !allowed() {
		return stationAttachmentAccess{}, wechatapp.ErrAttachmentDenied
	}
	native, err := s.WorkerSender(ctx, id)
	if err != nil {
		return stationAttachmentAccess{}, err
	}
	return stationAttachmentAccess{native: native, dir: w.dir, thread: in.Thread, message: in.Message, grant: grant, allowed: allowed}, nil
}

func (s *stationWechat) acquireAttachment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("connection")
	var in wechatapp.AttachmentRequest
	if !websession.Decode(w, r, &in) {
		return
	}
	if !identity.ValidPathID(id) || !identity.ValidPathID(in.Thread) || strings.TrimSpace(in.Message) != in.Message || len(in.Message) == 0 || len(in.Message) > 8192 {
		websession.Fail(w, http.StatusBadRequest, "连接、会话或原消息编号无效")
		return
	}
	worker, err := s.worker(id, false)
	if err != nil || worker == nil {
		websession.Fail(w, http.StatusNotFound, "连接未开放附件读取")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	access, err := s.ownerAttachmentAccess(ctx, id, worker, in)
	if err == nil {
		raw, _ := json.Marshal(in)
		var result map[string]any
		result, err = wechatapp.ReadAttachment(ctx, raw, access)
		if err == nil {
			websession.Respond(w, http.StatusOK, result)
			return
		}
	}
	if errors.Is(err, wechatapp.ErrAttachmentDenied) {
		websession.Fail(w, http.StatusNotFound, "未找到当前连接获准读取的附件")
		return
	}
	websession.Fail(w, http.StatusBadGateway, "无法取得已核验的原件，请检查微信连接、原消息及素材状态后重试")
}

func (s *stationWechat) downloadAttachment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, asset := r.PathValue("connection"), r.PathValue("asset")
	if !identity.ValidPathID(id) || !stationAttachmentID(asset) {
		http.NotFound(w, r)
		return
	}
	worker, err := s.worker(id, false)
	if err != nil || worker == nil {
		http.NotFound(w, r)
		return
	}
	root, err := workspacefs.OpenPath(filepath.Join(worker.dir, "attachments", asset))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	record, err := readStationAttachmentRecord(root)
	root.Close()
	if err != nil || record.ID != asset {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	in := wechatapp.AttachmentRequest{Thread: record.Source.Thread, Message: record.Source.Message}
	access, err := s.ownerAttachmentAccess(ctx, id, worker, in)
	if err != nil || !access.grant.Equal(record.Source.Grant) {
		http.NotFound(w, r)
		return
	}
	source, err := access.source(ctx, in.Thread, in.Message)
	if err != nil || source != record.Source {
		http.NotFound(w, r)
		return
	}
	verified, file, err := access.open(ctx, source)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	check := func() bool { return access.Allowed(ctx, in.Thread) }
	if !check() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": source.Filename}))
	w.Header().Set("Content-Length", strconv.FormatInt(verified.Bytes, 10))
	_, _ = io.Copy(attachmentGrantWriter{allow: check, out: w}, io.LimitReader(file, verified.Bytes))
}
