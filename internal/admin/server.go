package admin

import (
	"context"
	"embed"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/apphttp"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/station"
	"wechat-bot/internal/stationui"
	"wechat-bot/internal/websession"
)

//go:embed static/*
var assets embed.FS

const legacyAdminCookie = "wechat_admin_session"

type Server struct {
	auth         *websession.Store
	accounts     *accountRegistry
	catalogMu    sync.Mutex
	catalogJob   *catalogRefresh
	manager      *Manager
	loginMu      sync.Mutex
	operation    string
	loginUnknown bool
}

func randomToken() string { return identity.RandomToken() }

func NewHandler(manager *Manager, password, origin string) (http.Handler, error) {
	if manager == nil {
		return nil, errors.New("invalid admin authentication configuration")
	}
	auth, err := websession.New(password, origin, legacyAdminCookie)
	if errors.Is(err, websession.ErrOrigin) {
		return nil, errors.New("invalid public admin origin")
	}
	if err != nil {
		return nil, errors.New("invalid admin authentication configuration")
	}
	return newHandler(manager, auth, true)
}

func newHandler(manager *Manager, auth *websession.Store, root bool) (http.Handler, error) {
	s := &Server{manager: manager, auth: auth}
	if root {
		var err error
		s.accounts, err = newAccountRegistry(s)
		if err != nil {
			return nil, err
		}
		manager.accounts = s.accounts
	}
	mux := http.NewServeMux()
	if s.accounts != nil {
		registry, err := apphost.New(wechatCompatibility{accounts: s.accounts}.registration())
		if err != nil {
			s.accounts.close()
			return nil, err
		}
		apphttp.Register(mux, s.auth, registry)
		s.accounts.routes(mux)
		station.RegisterLegacyHomeRoutes(s.auth, mux, manager.fish, manager.managedRoot(), manager.dir)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]bool{"ok": true}) })
	if root {
		mux.HandleFunc("POST /api/login", auth.Login)
		mux.HandleFunc("POST /api/logout", auth.Protect(auth.Logout))
		mux.HandleFunc("POST /api/password", auth.Protect(s.changePassword))
		mux.HandleFunc("GET /api/session", auth.Protect(auth.Status))
	}
	mux.HandleFunc("GET /api/config", s.protected(func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.manager.Config()) }))
	mux.HandleFunc("POST /api/config", s.protected(s.saveConfig))
	mux.HandleFunc("GET /api/status", s.protected(s.status))
	mux.HandleFunc("GET /api/logs", s.protected(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("date") {
			s.logsByDate(w, r)
			return
		}
		respond(w, 200, map[string]any{"items": s.manager.displayLogs(r.Context())})
	}))
	mux.HandleFunc("POST /api/bot/start", s.protected(func(w http.ResponseWriter, r *http.Request) { s.toggle(w, r, true) }))
	mux.HandleFunc("POST /api/bot/stop", s.protected(func(w http.ResponseWriter, r *http.Request) { s.toggle(w, r, false) }))
	mux.HandleFunc("POST /api/model/test", s.protected(s.testModel))
	mux.HandleFunc("GET /api/model/models", s.protected(func(w http.ResponseWriter, r *http.Request) {
		s.manager.mu.Lock()
		c := cloneConfig(s.manager.config)
		s.manager.mu.Unlock()
		client, err := s.manager.makeModel(c)
		if err != nil {
			fail(w, 400, "请先保存服务商与密钥配置")
			return
		}
		ids, err := client.Models(r.Context())
		if err != nil {
			fail(w, 502, "暂时无法获取模型列表，请检查服务商配置")
			return
		}
		respond(w, 200, map[string]any{"items": ids, "generation_calls": 0})
	}))
	mux.HandleFunc("GET /api/groups", s.protected(s.groups))
	s.companionRoutes(mux)
	s.billingRoutes(mux)
	s.agentRoutes(mux)
	s.ownerRequestRoutes(mux)
	s.mobileRoutes(mux)
	s.storageRoutes(mux)
	s.speechRoutes(mux)
	s.channelsRoutes(mux)
	s.contextRoutes(mux)
	s.memoryServicesRoutes(mux)
	s.memoryRoutes(mux)
	s.replyAuditRoutes(mux)
	s.sourceRepairRoutes(mux)
	s.memoryRebuildRoutes(mux)
	s.canvasRoutes(mux)
	s.toolRoutes(mux)
	s.libraryRoutes(mux)
	s.voiceLibraryRoutes(mux)
	s.catalogRoutes(mux)
	s.catalogSyncRoutes(mux)
	s.actionRoutes(mux)
	s.mcpRoutes(mux)
	s.pluginRoutes(mux)
	s.pluginPackageRoutes(mux)
	s.surfRoutes(mux)
	s.mediaRuntimeRoutes(mux)
	mux.HandleFunc("POST /api/wechat/login", s.protected(s.beginLogin))
	mux.HandleFunc("GET /api/wechat/login/{id}", s.protected(s.loginStatus))
	mux.HandleFunc("GET /api/wechat/login/{id}/qr", s.protected(s.qr))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.NotFound(w, r)
			return
		}
		name := r.URL.Path
		if stationui.ServeLegacy(w, name) {
			return
		}
		switch name {
		case "/":
			name = "index.html"
		case "/app.js", "/assets/app.js":
			name = "app.js"
		case "/assets/voices.js":
			name = "voices.js"
		case "/assets/theme.js":
			name = "theme.js"
		case "/assets/tools.js":
			name = "tools.js"
		case "/assets/catalog.js":
			name = "catalog.js"
		case "/assets/accounts.js":
			name = "accounts.js"
		case "/assets/catalog-sync.js":
			name = "catalog-sync.js"
		case "/assets/actions.js":
			name = "actions.js"
		case "/assets/mcp.js":
			name = "mcp.js"
		case "/assets/surf.js":
			name = "surf.js"
		case "/assets/proactive.js":
			name = "proactive.js"
		case "/assets/billing.js":
			name = "billing.js"
		case "/assets/storage.js":
			name = "storage.js"
		case "/assets/workbench.js":
			name = "workbench.js"
		case "/assets/flow-model.js":
			name = "flow-model.js"
		case "/assets/flow-default.json":
			name = "flow-default.json"
		case "/assets/workbench.css":
			name = "workbench.css"
		case "/assets/refresh.js":
			name = "refresh.js"
		case "/assets/controls.js":
			name = "controls.js"
		case "/assets/dialogs.js":
			name = "dialogs.js"
		case "/assets/memory.js":
			name = "memory.js"
		case "/assets/life-journal.js":
			name = "life-journal.js"
		case "/assets/palace.js":
			name = "palace.js"
		case "/assets/palace-graph.js":
			name = "palace-graph.js"
		case "/assets/palace-graph.css":
			name = "palace-graph.css"
		case "/assets/conversations.js":
			name = "conversations.js"
		case "/assets/rounds.js":
			name = "rounds.js"
		case "/assets/console.css":
			name = "console.css"
		case "/assets/console.js":
			name = "console.js"
		case "/assets/logs.js":
			name = "logs.js"
		case "/assets/save-actions.js":
			name = "save-actions.js"
		case "/assets/ui.js":
			name = "ui.js"
		case "/assets/channels.js":
			name = "channels.js"
		case "/assets/speech.js":
			name = "speech.js"
		case "/assets/agent.js":
			name = "agent.js"
		case "/assets/agent-workspace.js":
			name = "agent-workspace.js"
		case "/assets/plugin-packages.js":
			name = "plugin-packages.js"
		case "/assets/plugins.js":
			name = "plugins.js"
		case "/assets/whale-girl.png":
			name = "whale-girl.png"
		case "/app.css", "/assets/app.css":
			name = "app.css"
		default:
			http.NotFound(w, r)
			return
		}
		data, err := assets.ReadFile("static/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch filepath.Ext(name) {
		case ".html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case ".js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case ".json":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		case ".png":
			w.Header().Set("Content-Type", "image/png")
		case ".webp":
			w.Header().Set("Content-Type", "image/webp")
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		_, _ = w.Write(data)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if s.accounts != nil {
			s.accounts.selectAccount(w, r, mux)
		} else {
			mux.ServeHTTP(w, r)
		}
	}), nil
}

func respond(w http.ResponseWriter, status int, value any)   { websession.Respond(w, status, value) }
func fail(w http.ResponseWriter, status int, message string) { websession.Fail(w, status, message) }
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	return websession.Decode(w, r, value)
}

func (s *Server) session(r *http.Request) (string, websession.Session, bool) { return s.auth.Lookup(r) }
func (s *Server) protected(next http.HandlerFunc) http.HandlerFunc           { return s.auth.Protect(next) }

func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	var input ConfigInput
	if !decode(w, r, &input) {
		return
	}
	view, err := s.manager.Save(input)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	respond(w, 200, view)
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	native, err := s.manager.native.Status(r.Context())
	if err != nil {
		native = linuxnative.Status{Connection: "unavailable"}
	}
	name := ""
	if resolver, ok := s.manager.native.(interface {
		DisplayName(context.Context) (string, error)
	}); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		name, _ = resolver.DisplayName(ctx)
		cancel()
	}
	respond(w, 200, map[string]any{"native": native, "bot": s.manager.BotStatus(), "key_configured": s.manager.Config().KeyConfigured, "account": s.manager.native.Account(), "account_name": name, "device_control_retired": s.manager.mobileRetired, "version": "0.35.0-managed-workspaces-local"})
}
func (s *Server) toggle(w http.ResponseWriter, r *http.Request, enabled bool) {
	if err := s.manager.Toggle(enabled); err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.status(w, r)
}
func (s *Server) testModel(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.TestModel(r.Context()); err != nil {
		fail(w, 400, err.Error())
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	items, err := s.manager.native.Groups(r.Context(), r.URL.Query().Get("search"))
	if err != nil {
		fail(w, 502, "群目录暂不可用，请检查登录与同步状态，或缩小搜索范围")
		return
	}
	respond(w, 200, map[string]any{"items": items})
}
func terminal(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled", "expired", "rejected", "needs_login", "challenge_required", "approval_required", "needs_review":
		return true
	}
	return false
}
func (s *Server) beginLogin(w http.ResponseWriter, r *http.Request) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginUnknown {
		fail(w, 409, "上一次登录提交结果未知，请核对手机和后台状态后再处理，未自动重试")
		return
	}
	if s.operation != "" {
		op, err := s.manager.native.LoginStatus(r.Context(), s.operation)
		if err != nil {
			fail(w, 502, "暂时无法核对原登录操作，未重复提交，请稍后查询")
			return
		}
		if !terminal(op.State) {
			respond(w, 200, op)
			return
		}
	}
	op, err := s.manager.native.BeginLogin(r.Context(), randomToken())
	if err != nil {
		s.loginUnknown = errors.Is(err, linuxnative.ErrLoginOutcomeUnknown)
		fail(w, 502, "登录请求未取得确定结果，未自动重试；请检查后台状态")
		return
	}
	s.operation = op.ID
	s.manager.log("wechat_qr_login_requested")
	respond(w, 200, op)
}
func (s *Server) loginStatus(w http.ResponseWriter, r *http.Request) {
	op, err := s.manager.native.LoginStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, 502, "暂时无法读取登录状态")
		return
	}
	respond(w, 200, op)
}
func (s *Server) qr(w http.ResponseWriter, r *http.Request) {
	generation, err := strconv.Atoi(r.URL.Query().Get("generation"))
	if err != nil {
		fail(w, 400, "二维码代次无效")
		return
	}
	data, err := s.manager.native.LoginQR(r.Context(), r.PathValue("id"), generation)
	if err != nil {
		fail(w, 502, "二维码尚未生成或已经过期")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(data)
}
