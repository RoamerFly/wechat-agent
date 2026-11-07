package station

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/stationruntime"
	"wechat-bot/internal/websession"
)

type localChatService struct {
	home         *localHomeService
	models       *localModelStore
	mu           sync.Mutex
	turn         sync.Mutex
	busy         atomic.Bool
	store        *companion.Store
	fishID       string
	ownerID      string
	closed       bool
	stopping     atomic.Bool
	closeOnce    sync.Once
	closeErr     error
	ctx          context.Context
	cancel       context.CancelFunc
	windows      *agenthost.WindowsConfig
	processes    *localProcessTools
	projectTools *localProjectTools
	wechat       *stationWechat
	observations *apphost.Observer
	applications apphost.Registry
	// The output registration belongs to the host and cannot be set by a
	// browser request or connected account.
	live         *apphost.LiveController
	liveFish     string
	liveOutput   apphost.LiveOutput
	liveSource   apphost.LiveSourceOutput
	liveEncoder  string
	liveRunDone  chan struct{}
	livePrograms *localLivePrograms
	obsInstallMu sync.Mutex
	// Test transport only; production uses the pinned offline installer.
	obsInstaller func(context.Context, string, string, string) error
	// liveStopDone is created only when shutdown has claimed an existing live
	// controller. It lets Close wait for the same bounded stop operation rather
	// than issue a second remote stop while the first is in flight.
	liveStopOnce sync.Once
	liveStopDone chan struct{}
	liveStopErr  error
	// Tests replace only the model transport, retaining the normal engine,
	// source handling, SQLite and HTTP boundary.
	modelFactory func() (companion.LanguageModel, error)
	followups    *localTaskFollowupWorker
}

func newLocalChatService(home *localHomeService) *localChatService {
	ctx, cancel := context.WithCancel(context.Background())
	h := &localChatService{home: home, models: newLocalModelStore(home.dataDir), ctx: ctx, cancel: cancel,
		livePrograms: &localLivePrograms{home: home},
		projectTools: &localProjectTools{
			home: home, options: stationruntime.PackagedToolOptions(),
			rvc: newLocalRVCService(home.dataDir, home.root), comfy: newLocalComfyService(home.dataDir, home.root),
			yue: newLocalYuEService(home.dataDir, home.root),
			tts: newLocalTTSBackend(home.dataDir, home.root),
		}}
	h.followups = &localTaskFollowupWorker{service: h, wake: make(chan struct{}, 1), done: make(chan struct{})}
	return h
}

func (h *localChatService) BeginShutdown() {
	if h == nil {
		return
	}
	h.stopping.Store(true)
	// Live shutdown has to begin before a chat turn is drained: an in-flight
	// model must never keep an owned output running. The bounded stop continues
	// independently of the HTTP request that initiated desktop shutdown.
	h.closeLiveAsync()
	if h.projectTools != nil {
		h.projectTools.obs.close()
	}
	// Cancel is idempotent. It is intentionally outside turn/mu: Close must
	// be able to interrupt the model that currently owns turn before waiting
	// to close SQLite or process records.
	if h.cancel != nil {
		h.cancel()
	}
	h.mu.Lock()
	processes := h.processes
	h.mu.Unlock()
	// Do not wait for child process bookkeeping here. Jobs closes admission and
	// cancels existing child contexts while holding its own registry mutex.
	if processes != nil {
		processes.BeginShutdown()
	}
}

func (h *localChatService) closeLiveAsync() {
	if h == nil {
		return
	}
	h.mu.Lock()
	live, liveRunDone := h.live, h.liveRunDone
	h.mu.Unlock()
	if live == nil {
		return
	}
	// Seal admission synchronously before the background stop owns any provider
	// work. A shutdown racing a browser Start must not create a new session.
	live.BeginShutdown()
	h.liveStopOnce.Do(func() {
		done := make(chan struct{})
		h.mu.Lock()
		h.liveStopDone = done
		h.mu.Unlock()
		go func() {
			// BeginShutdown cancels the recovery loop. Let it release the
			// shared ledger before reconciling the final state.
			if liveRunDone != nil {
				<-liveRunDone
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := live.Close(ctx)
			cancel()
			h.mu.Lock()
			h.liveStopErr = err
			close(done)
			h.mu.Unlock()
		}()
	})
}

func (h *localChatService) CloseContext(ctx context.Context) error {
	h.BeginShutdown()
	if h == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		h.closeOnce.Do(func() {
			// An active chat owns turn. Its context was cancelled above, so this
			// wait cannot keep a desktop stop blocked on a provider timeout.
			h.turn.Lock()
			defer h.turn.Unlock()
			h.mu.Lock()
			h.closed = true
			processes, store, liveStopDone := h.processes, h.store, h.liveStopDone
			h.mu.Unlock()
			// beginShutdown starts this before waiting for turn. The shared wait
			// avoids racing two StopLive calls against the same durable session.
			if liveStopDone != nil {
				// Do not let an outer desktop timeout close SQLite while the
				// bounded live stop is still persisting its final state. The outer
				// CloseContext caller regains control through its own select below;
				// this background closer remains responsible for ordering.
				<-liveStopDone
				h.mu.Lock()
				h.closeErr = errors.Join(h.closeErr, h.liveStopErr)
				h.mu.Unlock()
			}
			// Jobs owns cancellation and waits for the isolated child result before
			// its durable record is closed. Do not replay any interrupted request.
			if processes != nil {
				h.closeErr = errors.Join(h.closeErr, processes.Close())
			}
			if h.projectTools != nil {
				h.closeErr = errors.Join(h.closeErr, h.projectTools.close())
			}
			if store != nil {
				if h.followups != nil && h.followups.started.Load() {
					<-h.followups.done
				}
				h.closeErr = errors.Join(h.closeErr, store.Close())
			}
		})
		close(done)
	}()
	select {
	case <-done:
		return h.closeErr
	case <-ctx.Done():
		return fmt.Errorf("本机服务停止超过等待时限: %w", ctx.Err())
	}
}

func (h *localChatService) conversation() (*companion.Store, companion.Input, error) {
	if h == nil || h.stopping.Load() {
		return nil, companion.Input{}, errors.New("本机服务正在停止")
	}
	record, err := h.home.identity.Read()
	if err != nil || record.LocalOwner == nil {
		return nil, companion.Input{}, errors.New("请先准备本机身份；身份损坏时请保留原文件")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, companion.Input{}, errors.New("本机服务正在停止")
	}
	if h.store == nil {
		store, err := companion.NewStore(filepath.Join(h.home.dataDir, "local-companion", record.FishID))
		if err != nil {
			return nil, companion.Input{}, errors.New("本地聊天存储不可用，原记录保留")
		}
		h.store, h.fishID, h.ownerID = store, record.FishID, record.LocalOwner.ID
	}
	if h.fishID != record.FishID || h.ownerID != record.LocalOwner.ID {
		return nil, companion.Input{}, errors.New("本机身份已变化，请重启独立服务后继续")
	}
	return h.store, companion.Input{Channel: "local", Account: record.FishID, Thread: record.LocalOwner.ID, Sender: record.LocalOwner.ID, SenderName: "主人", SenderNameSource: "local_owner", Mode: "direct", Directed: true}, nil
}

func (h *localChatService) routes(s *websession.Store, mux *http.ServeMux) {
	mux.HandleFunc("GET /api/local/chat", s.Protect(h.history))
	mux.HandleFunc("POST /api/local/chat", s.Protect(h.send))
	mux.HandleFunc("GET /api/local/model", s.Protect(h.modelView))
	mux.HandleFunc("PUT /api/local/model", s.Protect(h.modelSave))
	h.appObservationRoutes(s, mux)
	h.publicationRoutes(s, mux)
	h.interactionRoutes(s, mux)
	h.liveRoutes(s, mux)
	h.localFollowupRoutes(s, mux)
}

func (h *localChatService) modelView(w http.ResponseWriter, r *http.Request) {
	view, err := h.models.View()
	if err != nil {
		websession.Fail(w, 409, "本机模型配置不可读，原文件保留")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	websession.Respond(w, 200, view)
}

func (h *localChatService) modelSave(w http.ResponseWriter, r *http.Request) {
	var in LocalModelInput
	if !websession.Decode(w, r, &in) {
		return
	}
	view, err := h.models.Save(in)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errLocalModelRevision) {
			status = http.StatusConflict
		}
		websession.Fail(w, status, err.Error())
		return
	}
	websession.Respond(w, 200, view)
}

func (h *localChatService) history(w http.ResponseWriter, r *http.Request) {
	store, in, err := h.conversation()
	if err != nil {
		websession.Fail(w, 409, err.Error())
		return
	}
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			websession.Fail(w, 400, "每页可读取 1–100 条；更早记录可按游标续读")
			return
		}
	}
	page, err := store.LocalConversationPage(r.Context(), in, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		websession.Fail(w, 409, "聊天记录或游标不可用，请从最新记录重新读取")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	pending, err := store.HasPendingLocalTaskFollowups(r.Context(), in)
	if err != nil {
		websession.Fail(w, 503, "后台任务状态不可读，请稍后刷新")
		return
	}
	websession.Respond(w, 200, map[string]any{"items": page.Items, "next_cursor": page.NextCursor, "has_more": page.HasMore, "busy": h.busy.Load(), "background_pending": pending})
}

func localConversationConfig(persona string) companion.Config {
	c := companion.Defaults()
	c.Enabled, c.Name, c.Persona = true, "大肥鱼", persona
	c.Permissions = companion.Permissions{ReplyText: true, Remember: true}
	c.Context = companion.ContextDefaults()
	// Maintenance is not silently launched by a newly created local chat.
	// Original events remain on disk and older text can be read on demand.
	c.Context.AutoCompress = false
	c.MemoryDays = 365
	return c
}

func (h *localChatService) send(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.stopping.Load() {
		websession.Fail(w, http.StatusServiceUnavailable, "本机服务正在停止，未启动新的聊天或工具")
		return
	}
	var request struct {
		RequestID string `json:"request_id"`
		Text      string `json:"text"`
	}
	if !websession.Decode(w, r, &request) {
		return
	}
	if !identity.ValidPathID(request.RequestID) || strings.TrimSpace(request.Text) == "" || !utf8.ValidString(request.Text) || len(request.Text) > 64<<10 || strings.ContainsRune(request.Text, 0) {
		websession.Fail(w, 400, "请提供稳定请求编号及 1–65536 字节的有效正文；长资料可分批继续")
		return
	}
	if done := h.followups.interrupt(); done != nil {
		select {
		case <-done:
		case <-r.Context().Done():
			websession.Fail(w, 409, "本次请求已取消")
			return
		case <-time.After(3 * time.Second):
		}
	}
	if !h.turn.TryLock() {
		websession.Fail(w, 409, "当前对话正在生成，请等待后查看记录；不会重复提交")
		return
	}
	defer h.turn.Unlock()
	h.busy.Store(true)
	defer h.busy.Store(false)
	store, in, err := h.conversation()
	if err != nil {
		websession.Fail(w, 409, err.Error())
		return
	}
	prior, err := store.LocalConversationRequest(r.Context(), in, request.RequestID)
	if err != nil {
		websession.Fail(w, 409, "该请求来源不可用或已被遗忘，请使用新请求继续")
		return
	}
	if prior != nil && prior.InputText != request.Text {
		websession.Fail(w, 409, "同一请求编号的正文已变化，请使用新的请求编号")
		return
	}
	if prior != nil && prior.Completed {
		websession.Respond(w, 200, map[string]any{"request_id": request.RequestID, "messages": prior.Messages, "status": "completed", "reused": true})
		return
	}
	in.Text, in.OriginalText, in.TraceID = request.Text, &request.Text, request.RequestID
	in.MessageTime = time.Now().UTC().Format(time.RFC3339Nano)
	if prior != nil {
		in.MessageTime = prior.OccurredAt
	}
	in.Source = &companion.SourceRef{MessageID: request.RequestID}
	// Save the actual user request before consulting model settings. This uses
	// the normal event/source identity and makes an unavailable model a retryable
	// unfinished turn rather than a lost message or a second event stream.
	if err = store.Observe(in, localConversationConfig(""), time.Now().UTC()); err != nil {
		websession.Fail(w, 503, "原消息未能写入本地记录，请先刷新核对")
		return
	}
	run, err := store.OpenLocalToolRun(r.Context(), in, prior == nil)
	if err != nil {
		websession.Fail(w, 503, "工具检查点无法核验，未执行本轮工具")
		return
	}
	response, err := h.generateLocalTurn(r.Context(), store, in, run, "")
	writeLocalTurnResponse(w, response, err)
}
