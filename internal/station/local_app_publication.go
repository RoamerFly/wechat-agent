package station

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/websession"
)

const localPublicationToolName = "app_publish"

func localPublicationTool() mcptools.Tool {
	return mcptools.NewTool(localPublicationToolName, "管理创作草稿与应用发布。targets 列已授权的应用连接。draft 保存已登记 artifact_ids，可选 project_artifact_id 保留可编辑源。不选应用时保存在本机。B站 publish 接受单个视频，platform_options 提供分区编号、标签、原创或转载。抖音 web-page-text 经普通账号创作者中心发布单个 MP4 或 WebM，title 与 body 合成不超过 1000 字，platform_options.visibility 必须为 public、private 或 friends；无需开放平台。submit 使用原 job_id 提交，status 核对原进度与回执，resume 续办原任务，cancel 停止未提交操作。投稿结果未知只查原任务，不能另建任务重发。list 按 app_id 查看，默认 local。",
		`{"type":"object","properties":{"action":{"type":"string","enum":["targets","draft","list","submit","resume","status","cancel"]},"app_id":{"type":"string"},"connection_id":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"project_artifact_id":{"type":"string"},"artifact_ids":{"type":"array","items":{"type":"string"}},"platform_options":{"oneOf":[{"type":"object","properties":{"category_id":{"type":"integer","minimum":1},"tags":{"type":"array","items":{"type":"string"}},"copyright":{"type":"integer","enum":[1,2],"description":"1 为原创，2 为转载"},"no_reprint":{"type":"boolean"},"source":{"type":"string","description":"转载出处"}},"required":["category_id","tags","copyright"],"additionalProperties":false},{"type":"object","properties":{"visibility":{"type":"string","enum":["public","private","friends"]}},"required":["visibility"],"additionalProperties":false}]},"job_id":{"type":"string"},"after":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1}},"required":["action"],"additionalProperties":false}`)
}

type localPublicationInput struct {
	Action       string          `json:"action"`
	AppID        string          `json:"app_id"`
	ConnectionID string          `json:"connection_id"`
	Title        string          `json:"title"`
	Body         string          `json:"body"`
	ProjectID    string          `json:"project_artifact_id"`
	ArtifactIDs  []string        `json:"artifact_ids"`
	Options      json.RawMessage `json:"platform_options"`
	JobID        string          `json:"job_id"`
	After        int64           `json:"after"`
	Limit        int             `json:"limit"`
}

func (h *localChatService) publicationHost() (*apphost.Publisher, pluginhost.Invocation, error) {
	identity, err := h.home.identity.Read()
	if err != nil || identity.LocalOwner == nil || h.stopping.Load() {
		return nil, pluginhost.Invocation{}, errors.New("本机身份暂不可用")
	}
	actor := pluginhost.Invocation{Account: identity.FishID, Thread: identity.LocalOwner.ID, Requester: identity.LocalOwner.ID, Purpose: "local publication"}
	host, err := h.projectTools.forFish(identity.FishID)
	if err != nil {
		return nil, actor, err
	}
	return apphost.NewPublisher(h.applications, host.store, h.validatePublication, h.openPublicationArtifact), actor, nil
}

func (t *localToolSet) callPublication(ctx context.Context, raw json.RawMessage) (string, error) {
	if t.publications == nil || t.run == nil {
		return "", errors.New("本轮没有本地创作任务权限")
	}
	if err := t.run.CheckSource(ctx); err != nil {
		return "", err
	}
	var in localPublicationInput
	if strictLocalMemoryDecode(raw, &in) != nil {
		return "", errors.New("创作任务参数无效")
	}
	publisher, actor, err := t.publications.publicationHost()
	if err != nil {
		return "", err
	}
	var out any
	switch in.Action {
	case "targets":
		out = map[string]any{"items": publisher.Discover(ctx)}
	case "list":
		if in.AppID == "" {
			in.AppID = "local"
		}
		if in.Limit == 0 {
			in.Limit = 20
		}
		out, err = publisher.List(ctx, actor, in.AppID, in.After, in.Limit)
	case "status", "cancel", "submit", "resume":
		if !artifactID("sha-"+in.JobID, "sha-") || in.AppID != "" || in.ConnectionID != "" || in.ProjectID != "" || len(in.ArtifactIDs) != 0 || len(in.Options) != 0 || in.Title != "" || in.Body != "" {
			return "", errors.New("请只提供原发布任务编号及操作。目标和成品以该任务记录为准")
		}
		if (in.Action == "submit" || in.Action == "resume") && t.inspectOnly {
			return "", errors.New("续办请先查询原 job_id，不能新增发布提交")
		}
		out, err = publisher.Action(ctx, actor, in.JobID, in.Action)
	case "draft":
		if in.JobID != "" || (in.AppID == "") != (in.ConnectionID == "") {
			return "", errors.New("保存新草稿时，请同时提供应用和连接编号，或都留空保存到本机")
		}
		if t.inspectOnly {
			return "", errors.New("续办只能查询原草稿，新草稿需要新的请求")
		}
		grant, resolveErr := t.access.resolve()
		if resolveErr != nil {
			return "", resolveErr
		}
		source, ok := t.artifactSource(grant)
		if !ok || source == nil {
			return "", errors.New("当前请求来源不可用")
		}
		origin := localPublicationOrigin{Request: t.traceID, Source: source}
		for _, read := range t.appReads {
			ref := localPublicationObservation{ConnectionRef: read.ConnectionRef, ContentID: read.ContentID, Field: read.Field, GrantVersion: read.GrantVersion}
			if !slices.Contains(origin.Observations, ref) {
				origin.Observations = append(origin.Observations, ref)
			}
		}
		encodedOrigin, _ := json.Marshal(origin)
		draft := apphost.PublicationDraft{Title: in.Title, Body: in.Body, Options: in.Options, Origin: encodedOrigin}
		if in.AppID != "" || in.ConnectionID != "" {
			draft.Target, err = publisher.ResolveTarget(ctx, in.AppID, in.ConnectionID)
			if err != nil {
				return "", err
			}
		}
		for _, id := range in.ArtifactIDs {
			a, readErr := t.access.home.artifacts.read(id)
			if readErr != nil {
				return "", errors.New("指定成品不可用，请先登记并核对来源")
			}
			draft.Artifacts = append(draft.Artifacts, localPublicationArtifact(a))
		}
		if in.ProjectID != "" {
			a, readErr := t.access.home.artifacts.read(in.ProjectID)
			if readErr != nil {
				return "", errors.New("可编辑源文件不可用")
			}
			ref := localPublicationArtifact(a)
			draft.Project = &ref
		}
		actor.Authorization = localCheckpointKey(grant)
		actor.SourceVersions = map[string]int{source.EventID: source.Revision}
		actor.RequestKey = localCheckpointKey(localPublicationToolName, source.EventID, source.Revision, draft)
		out, err = publisher.Draft(ctx, actor, draft)
	default:
		return "", errors.New("请选择草稿、发布、查询或取消操作")
	}
	if err != nil {
		_, message := localPublicationFailure(err)
		return "", errors.New(message)
	}
	if err := t.run.CheckSource(ctx); err != nil {
		return "", err
	}
	return localProcessJSON(out)
}

func (h *localChatService) publicationRoutes(s *websession.Store, mux *http.ServeMux) {
	handler := s.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		publisher, actor, err := h.publicationHost()
		if err != nil {
			status, message := localPublicationFailure(err)
			websession.Fail(w, status, message)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		stop := context.AfterFunc(h.ctx, cancel)
		defer stop()
		var out any
		if r.Method == http.MethodGet {
			appID := r.URL.Query().Get("app_id")
			if appID == "" {
				appID = "local"
			}
			limit := 20
			if raw := r.URL.Query().Get("limit"); raw != "" {
				limit, err = strconv.Atoi(raw)
			}
			var after int64
			if raw := r.URL.Query().Get("after"); raw != "" {
				var parseErr error
				after, parseErr = strconv.ParseInt(raw, 10, 64)
				if parseErr != nil {
					err = parseErr
				}
			}
			if err != nil || limit < 1 || after < 0 {
				websession.Fail(w, http.StatusBadRequest, "页大小或续读位置无效")
				return
			}
			var tasks apphost.PublicationPage
			tasks, err = publisher.ListForOwner(ctx, actor, appID, after, limit)
			out = map[string]any{"tasks": tasks, "targets": publisher.Discover(ctx)}
		} else {
			var in struct {
				Action string `json:"action"`
				JobID  string `json:"job_id"`
			}
			if !websession.Decode(w, r, &in) {
				return
			}
			if !artifactID("sha-"+in.JobID, "sha-") || (in.Action != "submit" && in.Action != "resume" && in.Action != "status" && in.Action != "cancel") {
				websession.Fail(w, http.StatusBadRequest, "请提供原发布任务编号，并选择发布、查询或取消")
				return
			}
			out, err = publisher.Action(ctx, actor, in.JobID, in.Action)
		}
		if err != nil {
			status, message := localPublicationFailure(err)
			websession.Fail(w, status, message)
			return
		}
		websession.Respond(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/local/publications", handler)
	mux.HandleFunc("POST /api/local/publications", handler)
	mux.HandleFunc("POST /api/local/publications/reconcile", s.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var in struct {
			JobID string `json:"job_id"`
			apphost.PublicationReconciliationQuery
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if !websession.Decode(w, r, &in) {
			return
		}
		if !artifactID("sha-"+in.JobID, "sha-") {
			websession.Fail(w, http.StatusBadRequest, "请提供原发布任务编号")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		stop := context.AfterFunc(h.ctx, cancel)
		defer stop()
		publisher, actor, err := h.publicationHost()
		if err == nil {
			var out apphost.PublicationReconciliation
			out, err = publisher.ReconcileForOwner(ctx, actor, in.JobID, in.PublicationReconciliationQuery)
			if err == nil {
				websession.Respond(w, http.StatusOK, out)
				return
			}
		}
		status, message := localPublicationFailure(err)
		if errors.Is(err, apphost.ErrPublicationDenied) {
			message = "请先核验并启用原 B 站账号的发布授权"
		}
		websession.Fail(w, status, message)
	}))
}

func localPublicationFailure(err error) (int, string) {
	switch {
	case errors.Is(err, apphost.ErrPublicationReconciliationInput):
		return http.StatusBadRequest, "请检查稿件编号、页码或页大小"
	case errors.Is(err, apphost.ErrPublicationReconciliationRead):
		return http.StatusBadGateway, "B 站未返回可核对的稿件信息，原任务仍待核对"
	case errors.Is(err, apphost.ErrPublicationDenied):
		return http.StatusForbidden, "目标应用未授予本任务发布权限，草稿保留在本机"
	case errors.Is(err, apphost.ErrPublicationInvalidDraft):
		return http.StatusBadRequest, "草稿来源、家目录授权或成品内容无法核对，未提交发布"
	case errors.Is(err, apphost.ErrPublicationOwnership):
		return http.StatusForbidden, "该发布任务不属于当前本机主人"
	case errors.Is(err, apphost.ErrNotFound):
		return http.StatusNotFound, "没有找到原发布任务或目标连接"
	case errors.Is(err, apphost.ErrPublicationUnavailable):
		return http.StatusServiceUnavailable, "目标应用尚未提供可用的发布接口，原任务保留"
	case errors.Is(err, apphost.ErrPublicationReceipt):
		return http.StatusConflict, "应用返回的作品回执与原任务不一致，请查询原任务，不能重新发布"
	case errors.Is(err, pluginhost.ErrRequestConflict):
		return http.StatusConflict, "原请求已绑定其他草稿，不能覆盖"
	case errors.Is(err, apphost.ErrPublicationConflict):
		return http.StatusConflict, "原任务状态已变化，请查询原任务"
	default:
		return http.StatusConflict, "创作任务暂不可用，原记录保留，请查询原任务后续办"
	}
}
