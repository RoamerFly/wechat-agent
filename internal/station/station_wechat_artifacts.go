package station

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/stationservices"
	"wechat-bot/internal/workspacefs"

	"wechat-bot/internal/websession"
)

type stationWorkGrant struct {
	ID, Artifact, Account, Thread, Requester, RequestKey string
	MediaGrant                                           time.Time
	Enabled                                              bool
	HomeRevision                                         uint64
}

func (g stationWorkGrant) id() string {
	return "export-" + localCheckpointKey(g.Artifact, g.Account, g.Thread, g.Requester, g.MediaGrant, g.RequestKey, g.HomeRevision)
}

type stationWorkAccess struct {
	artifacts stationservices.Artifacts
	dir       string
	grants    map[string]time.Time
}

func (a *stationWorkAccess) read(id string) (stationWorkGrant, error) {
	var g stationWorkGrant
	if a == nil || !artifactID(id, "export-") {
		return g, errors.New("work_grant_unavailable")
	}
	root, err := workspacefs.OpenPath(filepath.Join(a.dir, "exports"))
	if err != nil {
		return g, err
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, id+".json")
	if err != nil {
		return g, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 16<<10))
	if err != nil || len(raw) >= 16<<10 || json.Unmarshal(raw, &g) != nil || g.ID != id || g.id() != id {
		return stationWorkGrant{}, errors.New("work_grant_invalid")
	}
	return g, nil
}

func (a *stationWorkAccess) authorized(in companion.Input, id string) (stationservices.Artifact, error) {
	g, err := a.read(id)
	if err != nil || !g.Enabled || g.Account != in.Account || g.Thread != in.Thread || g.Requester != in.Sender {
		return stationservices.Artifact{}, wechatapp.ErrAttachmentDenied
	}
	current := a.grants[in.Thread]
	if current.IsZero() || !current.Equal(g.MediaGrant) {
		return stationservices.Artifact{}, wechatapp.ErrAttachmentDenied
	}
	homeRevision, err := a.artifacts.HomeRevision()
	if err != nil || homeRevision != g.HomeRevision {
		return stationservices.Artifact{}, wechatapp.ErrAttachmentDenied
	}
	artifact, err := a.artifacts.ReadArtifact(g.Artifact)
	if err != nil {
		return stationservices.Artifact{}, wechatapp.ErrAttachmentDenied
	}
	return artifact, nil
}

func (a *stationWorkAccess) list(in companion.Input, after string, limit int) ([]map[string]any, string, error) {
	items := []map[string]any{}
	if a == nil {
		return items, "", nil
	}
	root, err := workspacefs.OpenPath(filepath.Join(a.dir, "exports"))
	if err != nil {
		if os.IsNotExist(err) {
			return items, "", nil
		}
		return nil, "", err
	}
	defer root.Close()
	dir, err := workspacefs.OpenDirectory(root, ".")
	if err != nil {
		return nil, "", err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, "", err
	}
	sort.Strings(names)
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
		if id <= after {
			continue
		}
		artifact, e := a.authorized(in, id)
		if e == nil {
			if len(items) == limit {
				last, _ := items[len(items)-1]["delivery_id"].(string)
				return items, last, nil
			}
			view := artifact.View()
			view["delivery_id"] = id
			items = append(items, view)
		}
	}
	return items, "", nil
}

func (s *stationWechat) artifactRoutes(owner *websession.Store, mux *http.ServeMux) {
	mux.HandleFunc("GET /api/apps/wechat/connections/{connection}/work-grants", owner.Protect(s.workGrants))
	mux.HandleFunc("POST /api/apps/wechat/connections/{connection}/work-grants/{delivery}/revoke", owner.Protect(s.revokeWorkGrant))
	mux.HandleFunc("POST /api/apps/wechat/connections/{connection}/work-grants", owner.Protect(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Artifact  string `json:"artifact_id"`
			Thread    string `json:"thread_id"`
			Requester string `json:"requester_id"`
			Enabled   *bool  `json:"enabled"`
			Key       string `json:"request_key"`
		}
		if !websession.Decode(w, r, &req) {
			return
		}
		if !identity.ValidPathID(req.Thread) || !identity.ValidPathID(req.Requester) || !identity.ValidPathID(req.Key) || req.Enabled == nil {
			websession.Fail(w, 400, "请提供会话、请求者、请求编号和授权状态")
			return
		}
		native, err := s.WorkerSender(r.Context(), r.PathValue("connection"))
		if err != nil {
			websession.Fail(w, 404, "微信连接不可用")
			return
		}
		worker, err := s.worker(r.PathValue("connection"), false)
		if err != nil || worker == nil {
			websession.Fail(w, 404, "请先配置此连接的附件回复范围")
			return
		}
		artifact, err := s.artifacts.ReadArtifact(req.Artifact)
		if err != nil {
			websession.Fail(w, 404, "成品不存在或来源已变化")
			return
		}
		homeRevision, err := s.artifacts.HomeRevision()
		if err != nil {
			websession.Fail(w, 409, "请先恢复家目录授权")
			return
		}
		worker.mu.Lock()
		defer worker.mu.Unlock()
		grant := worker.policy.MediaReplyGrants[req.Thread]
		if grant.IsZero() {
			websession.Fail(w, 409, "请先在此会话开启附件回复")
			return
		}
		g := stationWorkGrant{Artifact: artifact.ID, Account: native.Account(), Thread: req.Thread, Requester: req.Requester, MediaGrant: grant, Enabled: *req.Enabled, RequestKey: req.Key}
		g.HomeRevision = homeRevision
		g.ID = g.id()
		root, err := workspacefs.EnsurePath(filepath.Join(worker.dir, "exports"))
		if err != nil {
			websession.Fail(w, 503, "交付授权存储不可用")
			return
		}
		defer root.Close()
		if g.Enabled {
			prior, priorErr := (&stationWorkAccess{artifacts: s.artifacts, dir: worker.dir}).read(g.ID)
			if priorErr == nil && !prior.Enabled {
				websession.Fail(w, 409, "此交付授权已撤销，新授权请使用新的请求编号")
				return
			}
		}
		stage := ".grant-" + identity.RandomToken()
		defer root.Remove(stage)
		if err = writeLegacyStagedFile(root, stage, g); err == nil {
			err = root.Rename(stage, g.ID+".json")
		}
		if err != nil {
			websession.Fail(w, 503, "交付授权未保存")
			return
		}
		if !g.Enabled && worker.run != nil {
			next := worker.policy
			next.Paused = true
			if err := worker.changePolicy(next); err != nil {
				websession.Fail(w, 503, "授权已撤销，暂停连接失败，请重试暂停")
				return
			}
			if err := worker.stop(r.Context()); err != nil {
				websession.Fail(w, 409, "授权已撤销，连接正在停止")
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		websession.Respond(w, 200, map[string]any{"delivery_id": g.ID, "artifact_id": g.Artifact, "enabled": g.Enabled, "status": "authorization_saved"})
	}))
}

func (s *stationWechat) revokeWorkGrant(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	native, err := s.WorkerSender(r.Context(), r.PathValue("connection"))
	if err != nil {
		websession.Fail(w, 404, "微信连接不可用")
		return
	}
	worker, err := s.worker(r.PathValue("connection"), false)
	if err != nil || worker == nil {
		websession.Fail(w, 404, "交付授权不存在")
		return
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	g, err := (&stationWorkAccess{dir: worker.dir}).read(r.PathValue("delivery"))
	if err != nil || g.Account != native.Account() {
		websession.Fail(w, 404, "交付授权不存在")
		return
	}
	root, err := workspacefs.OpenPath(filepath.Join(worker.dir, "exports"))
	if err != nil {
		websession.Fail(w, 503, "交付授权暂不可读")
		return
	}
	defer root.Close()
	g.Enabled = false
	stage := ".grant-" + identity.RandomToken()
	defer root.Remove(stage)
	if err = writeLegacyStagedFile(root, stage, g); err == nil {
		err = root.Rename(stage, g.ID+".json")
	}
	if err != nil {
		websession.Fail(w, 503, "撤销未保存")
		return
	}
	if worker.run != nil {
		next := worker.policy
		next.Paused = true
		if err := worker.changePolicy(next); err != nil {
			websession.Fail(w, 503, "授权已撤销，暂停连接失败")
			return
		}
		if err := worker.stop(r.Context()); err != nil {
			websession.Fail(w, 409, "授权已撤销，连接正在停止")
			return
		}
	}
	websession.Respond(w, 200, map[string]any{"delivery_id": g.ID, "enabled": false})
}

func (s *stationWechat) workGrants(w http.ResponseWriter, r *http.Request) {
	native, err := s.WorkerSender(r.Context(), r.PathValue("connection"))
	if err != nil {
		websession.Fail(w, 404, "微信连接不可用")
		return
	}
	worker, err := s.worker(r.PathValue("connection"), false)
	if err != nil || worker == nil {
		websession.Fail(w, 404, "微信连接尚未配置")
		return
	}
	limit := 20
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
	}
	after := r.URL.Query().Get("after")
	if err != nil || limit < 1 || after != "" && !artifactID(after, "export-") {
		websession.Fail(w, 400, "分页参数无效")
		return
	}
	root, err := workspacefs.OpenPath(filepath.Join(worker.dir, "exports"))
	items := []map[string]any{}
	if os.IsNotExist(err) {
		websession.Respond(w, 200, map[string]any{"items": items})
		return
	}
	if err != nil {
		websession.Fail(w, 503, "交付授权暂不可读")
		return
	}
	defer root.Close()
	dir, err := workspacefs.OpenDirectory(root, ".")
	if err != nil {
		websession.Fail(w, 503, "交付授权暂不可读")
		return
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		websession.Fail(w, 503, "交付授权暂不可读")
		return
	}
	sort.Strings(names)
	worker.mu.Lock()
	grants := worker.policy.MediaReplyGrants
	worker.mu.Unlock()
	access := &stationWorkAccess{dir: worker.dir, artifacts: s.artifacts, grants: grants}
	next := ""
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
		if id <= after {
			continue
		}
		g, e := access.read(id)
		if e != nil || g.Account != native.Account() {
			continue
		}
		if len(items) == limit {
			next, _ = items[len(items)-1]["delivery_id"].(string)
			break
		}
		artifact, sourceErr := access.authorized(companion.Input{Account: g.Account, Thread: g.Thread, Sender: g.Requester}, id)
		items = append(items, map[string]any{"delivery_id": id, "artifact_id": g.Artifact, "thread_id": g.Thread, "requester_id": g.Requester, "request_key": g.RequestKey, "enabled": g.Enabled, "usable": sourceErr == nil, "filename": artifact.Name})
	}
	w.Header().Set("Cache-Control", "no-store")
	websession.Respond(w, 200, map[string]any{"items": items, "next_after": next})
}

func (t *stationAttachmentTools) workReply(ctx context.Context, raw json.RawMessage) (string, error) {
	var req struct {
		Action string `json:"action"`
		ID     string `json:"delivery_id"`
		After  string `json:"after"`
		Limit  int    `json:"limit"`
	}
	if !t.replyAllowed(t.trace) || t.work == nil || strictLocalMemoryDecode(raw, &req) != nil {
		return "", wechatapp.ErrAttachmentDenied
	}
	if req.Action == "list" {
		if req.Limit == 0 {
			req.Limit = 20
		}
		if req.Limit < 1 || req.After != "" && !artifactID(req.After, "export-") {
			return "", errors.New("invalid_work_page")
		}
		items, next, err := t.work.list(t.trace, req.After, req.Limit)
		if err != nil {
			return "", errors.New("作品目录暂不可读")
		}
		return localProcessJSON(map[string]any{"items": items, "next_after": next})
	}
	if req.Action != "prepare" {
		return "", errors.New("work_reply_action_invalid")
	}
	a, err := t.work.authorized(t.trace, req.ID)
	if err != nil {
		return "", err
	}
	check := func() bool {
		_, err := t.work.authorized(t.trace, req.ID)
		return t.replyAllowed(t.trace) && err == nil
	}
	file, err := a.OpenVerified(ctx, check)
	if err != nil {
		return "", err
	}
	file.Close()
	media := companion.GeneratedMedia{ID: "reply-" + req.ID, AssetID: req.ID, Kind: "file", Title: a.Name}
	if t.prepared == nil {
		t.prepared = map[string]companion.GeneratedMedia{}
	}
	t.prepared[media.ID] = media
	return localProcessJSON(map[string]any{"media": []companion.GeneratedMedia{media}, "status": "prepared"})
}

func (t *stationAttachmentTools) workAllowed(in companion.Input, kind, id string) bool {
	if stationDocumentReplyID(id) {
		r, err := t.readDocumentReply(id)
		if kind != "file" || err != nil {
			return false
		}
		_, err = t.documentReplyAccess(context.Background(), in, r)
		return err == nil
	}
	if !strings.HasPrefix(id, "export-") {
		return true
	}
	if !t.replyAllowed(in) || t.work == nil || kind != "file" {
		return false
	}
	_, err := t.work.authorized(in, id)
	return err == nil
}

func (t *stationAttachmentTools) stageWork(ctx context.Context, in companion.Input, kind, id string) (string, error) {
	if !t.workAllowed(in, kind, id) {
		return "", wechatapp.ErrAttachmentDenied
	}
	a, err := t.work.authorized(in, id)
	if err != nil {
		return "", err
	}
	check := func() bool { return t.workAllowed(in, kind, id) }
	file, err := a.OpenVerified(ctx, check)
	if err != nil {
		return "", err
	}
	defer file.Close()
	upload, err := t.native.UploadVerified(ctx, a.Name, "application/octet-stream", file, a.Bytes, a.SHA256)
	if err != nil {
		return "", err
	}
	if !check() {
		return "", wechatapp.ErrAttachmentDenied
	}
	return upload.ID, nil
}
