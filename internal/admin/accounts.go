package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphttp"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/linuxnative"
)

type accountRecord = wechatapp.LegacyAccountRecord
type accountAccess struct {
	mu      sync.RWMutex
	removed bool
}
type accountRuntime struct {
	access    *accountAccess
	record    accountRecord
	manager   *Manager
	handler   http.Handler
	loadError string
}
type accountRegistry struct {
	lifecycle sync.Mutex
	mu        sync.Mutex
	root      *Server
	fish      *fishidentity.Registry
	items     map[string]*accountRuntime
	order     []accountRecord
	closed    bool
}
type accountFactory interface {
	ForAccount(string) (*linuxnative.Sender, error)
	Accounts(context.Context) ([]linuxnative.AccountInfo, error)
}

func accountDirectory(id string) string { return wechatapp.AccountDirectory(id) }

func newAccountRegistry(root *Server) (*accountRegistry, error) {
	fish := root.manager.fish
	if fish == nil {
		fish = fishidentity.New(root.manager.dir)
		root.manager.fish = fish
	}
	a := &accountRegistry{root: root, fish: fish, items: map[string]*accountRuntime{}}
	primary := root.manager.native.Account()
	a.items[primary] = &accountRuntime{record: accountRecord{ID: primary}, manager: root.manager}
	data, err := os.ReadFile(filepath.Join(root.manager.dir, "accounts.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("账号目录不可读")
	}
	if err == nil {
		if len(data) > 1<<20 || json.Unmarshal(data, &a.order) != nil {
			return nil, errors.New("账号目录无效，未覆盖原文件")
		}
		for _, record := range a.order {
			if !validID(record.ID) || record.ID == primary || a.items[record.ID] != nil {
				a.close()
				return nil, errors.New("账号目录身份重复或无效")
			}
			if record.Removed {
				a.items[record.ID] = &accountRuntime{record: record}
				continue
			}
			if _, err := a.open(record, false); err != nil {
				// A damaged secondary configuration must not take the existing
				// login offline, nor be silently replaced with empty defaults.
				a.items[record.ID] = &accountRuntime{record: record, loadError: "账号配置未能载入；原文件保留，请修复配置后重启管理端"}
			}
		}
	}
	return a, nil
}

func (a *accountRegistry) open(record accountRecord, initialize bool) (*accountRuntime, error) {
	factory, ok := a.root.manager.native.(accountFactory)
	if !ok {
		return nil, errors.New("当前原生服务不支持多账号")
	}
	native, err := factory.ForAccount(record.ID)
	if err != nil {
		return nil, err
	}
	// Native identifiers never become filesystem paths: each account has a stable
	// digest directory, including imported aliases and the original primary login.
	dir := filepath.Join(a.root.manager.dir, "accounts", accountDirectory(record.ID))
	if !initialize {
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
			return nil, errors.New("已保存账号的配置文件不可读")
		}
	}
	if initialize {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if _, err = os.Stat(filepath.Join(dir, "config.json")); errors.Is(err, os.ErrNotExist) {
			a.root.manager.mu.Lock()
			c := cloneConfig(a.root.manager.config)
			a.root.manager.mu.Unlock()
			// Reuse model/persona defaults, not other accounts' chat rules, memories,
			// scheduled work or host/owner privileges. Starting is an explicit action.
			c.Enabled = false
			c.DirectIDs = []string{}
			c.GroupRules = []GroupRule{}
			c.friendIDs = nil
			c.Companion.ProactiveThreads = nil
			c.Companion.ProactiveRules = nil
			c.Companion.Permissions.Proactive = false
			b, e := json.Marshal(c)
			if e != nil {
				return nil, e
			}
			if e = writePrivate(filepath.Join(dir, "config.json"), b); e != nil {
				return nil, e
			}
		} else if err != nil {
			return nil, err
		}
	}
	m, err := newManager(dir, filepath.Join(a.root.manager.stateDir, "accounts", accountDirectory(record.ID)), native, a.fish, a.root.manager.mobileRetired, a.root.manager.deviceIO)
	if err != nil {
		return nil, err
	}
	h, err := newHandler(m, a.root.auth, false)
	if err != nil {
		m.Close()
		return nil, err
	}
	item := &accountRuntime{record: record, manager: m, handler: h, access: &accountAccess{}}
	a.items[record.ID] = item
	return item, nil
}

func (a *accountRegistry) add(record accountRecord) (*accountRuntime, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, errors.New("账号服务正在停止")
	}
	if a.root.manager.draining() {
		return nil, errors.New("管理服务更新收尾中，请稍后添加账号")
	}
	if old := a.items[record.ID]; old != nil {
		return old, nil
	}
	item, err := a.open(record, true)
	if err != nil {
		return nil, err
	}
	next := append(append([]accountRecord{}, a.order...), record)
	data, err := json.Marshal(next)
	if err == nil {
		err = writePrivate(filepath.Join(a.root.manager.dir, "accounts.json"), data)
	}
	if err != nil {
		delete(a.items, record.ID)
		item.manager.Close()
		return nil, err
	}
	a.order = next
	return item, nil
}

func (a *accountRegistry) close() {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	items := make([]*Manager, 0, len(a.items))
	for _, item := range a.items {
		if item.manager != nil && item.manager != a.root.manager {
			items = append(items, item.manager)
		}
	}
	a.mu.Unlock()
	for _, m := range items {
		m.Close()
	}
}
func (a *accountRegistry) lookup(id string) *accountRuntime {
	a.mu.Lock()
	defer a.mu.Unlock()
	item := a.items[id]
	if item == nil {
		return nil
	}
	copy := *item
	return &copy
}

func (a *accountRegistry) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/accounts", a.root.protected(a.list))
	mux.HandleFunc("POST /api/accounts/{id}/manage", a.root.protected(a.manage))
	mux.HandleFunc("POST /api/accounts", a.root.protected(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Label string `json:"label"`
		}
		if !decode(w, r, &in) {
			return
		}
		in.Label = strings.TrimSpace(in.Label)
		if len(in.Label) > 256 || strings.ContainsAny(in.Label, "\x00\r\n") {
			fail(w, 400, "账号备注需要为单行文字")
			return
		}
		record := accountRecord{ID: "account-" + randomToken()[:24], Label: in.Label}
		item, err := a.add(record)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 201, map[string]any{"id": item.record.ID, "name": in.Label, "created": true})
	}))
}

// Keep a tombstone so native directory refresh cannot undo removal. Private
// history is retained only for restorable accounts.
func (a *accountRegistry) manage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
		Label  string `json:"label"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Label = strings.TrimSpace(in.Label)
	if len(in.Label) > 256 || strings.ContainsAny(in.Label, "\x00\r\n") {
		fail(w, 400, "账号备注需要为单行文字，最多 256 字节")
		return
	}
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	if in.Action == "remove" || in.Action == "purge" {
		a.removeAccount(w, r, in.Action == "purge")
		return
	}
	id := r.PathValue("id")
	snapshot := a.lookup(id)
	if snapshot != nil && snapshot.access != nil {
		snapshot.access.mu.Lock()
		defer snapshot.access.mu.Unlock()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	item := a.items[id]
	if a.closed || a.root.manager.draining() {
		fail(w, 409, "服务正在收尾，请稍后重试")
		return
	}
	if item == nil || item.record.Purged || item.record.Deleting {
		fail(w, 404, "账号不存在或正在删除")
		return
	}
	if item.record.Removed && (in.Action == "bind_fish" || in.Action == "unbind_fish") {
		fail(w, 404, "账号已移除，请先恢复后再管理鱼身份")
		return
	}
	if id == a.root.manager.native.Account() && in.Action != "bind_fish" && in.Action != "unbind_fish" {
		fail(w, 409, "主账号是本机服务入口，可暂停 Agent 或退出微信登录，不能移除入口")
		return
	}

	record := item.record
	switch in.Action {
	case "rename":
		record.Label = in.Label
	case "bind_fish":
		fishID, err := a.fish.Bind(id)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]any{"id": id, "fish_id": fishID})
		return
	case "unbind_fish":
		if err := a.fish.Unbind(id); err != nil {
			fail(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]any{"id": id, "fish_id": ""})
		return
	case "restore":
		if record.Removed && item.manager != nil {
			fail(w, 409, "上次停止账号未完成，请重启服务后恢复")
			return
		}
		if !record.Removed {
			respond(w, 200, map[string]any{"id": id})
			return
		}
		record.Removed = false
	default:
		fail(w, 400, "不支持的账号操作")
		return
	}
	next := append([]accountRecord{}, a.order...)
	for i := range next {
		if next[i].ID == id {
			next[i] = record
		}
	}
	// Open before committing restoration; keep it inaccessible until persistence.
	oldItem := item
	if in.Action == "restore" && item.manager == nil {
		opened, err := a.open(record, false)
		if err != nil {
			fail(w, 409, "账号配置未能恢复，原资料仍保留")
			return
		}
		opened.record = item.record
		item = opened
	}
	data, err := json.Marshal(next)
	if err == nil {
		err = writePrivate(filepath.Join(a.root.manager.dir, "accounts.json"), data)
	}
	if err != nil {
		if item != oldItem {
			a.items[id] = oldItem
			item.manager.Close()
		}
		fail(w, 500, "账号目录保存失败，原记录保留")
		return
	}
	copy := *item
	copy.record = record
	if copy.access != nil {
		copy.access.removed = record.Removed
	}
	a.items[id] = &copy
	a.order = next
	respond(w, 200, map[string]any{"id": id, "removed": record.Removed, "label": record.Label, "history_preserved": true})
}

func (a *accountRegistry) list(w http.ResponseWriter, r *http.Request) {
	directoryError := ""
	_, supported := a.root.manager.native.(accountFactory)
	if factory, ok := a.root.manager.native.(accountFactory); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		native, err := factory.Accounts(ctx)
		cancel()
		if err != nil {
			directoryError = "原生账号目录暂不可用，仍显示已保存账号"
		} else {
			for _, n := range native {
				// Match both identifiers to avoid importing the current login twice.
				if a.lookup(n.ID) != nil || a.lookup(n.Alias) != nil {
					continue
				}
				// Events and messages carry the canonical native ID, not its
				// editable alias. Keep their account identity checks aligned.
				if _, err = a.add(accountRecord{ID: n.ID}); err != nil {
					directoryError = "部分原生账号未能载入，请稍后刷新"
				}
			}
		}
	}
	a.mu.Lock()
	// Snapshot bindings while the account set is locked. Manage operations take
	// the same account-then-fish lock order, so a response cannot combine an
	// earlier identity view with a later account registry view.
	fishID, fishBindings, fishErr := a.fish.Bindings()
	if fishErr != nil {
		// An unreadable identity registry must never be represented as a clean
		// unbound state. Keep ordinary account status available, but make the
		// identity fault explicit and reject every binding mutation.
		fishID, fishBindings = "", map[string]bool{}
	}
	items := make([]*accountRuntime, 0, len(a.items))
	for _, item := range a.items {
		if item.record.Purged {
			continue
		}
		copy := *item
		items = append(items, &copy)
	}
	a.mu.Unlock()
	primary := a.root.manager.native.Account()
	sort.Slice(items, func(i, j int) bool {
		if (items[i].record.ID == primary) != (items[j].record.ID == primary) {
			return items[i].record.ID == primary
		}
		return items[i].record.ID < items[j].record.ID
	})
	rows := make([]map[string]any, len(items))
	var wg sync.WaitGroup
	// Slow/offline profiles do not serially block every other account's status.
	for i, item := range items {
		wg.Add(1)
		go func(i int, item *accountRuntime) {
			defer wg.Done()
			if item.record.Removed {
				rows[i] = map[string]any{"id": item.record.ID, "name": item.record.Label, "label": item.record.Label, "removed": true, "deleting": item.record.Deleting, "fish_id": ""}
				return
			}
			if item.manager == nil {
				name := item.record.Label
				if name == "" {
					name = "配置异常的小号"
				}
				rows[i] = map[string]any{"id": item.record.ID, "name": name, "load_error": item.loadError, "native": linuxnative.Status{Connection: "unavailable"}, "bot": BotStatus{State: "error"}, "fish_id": fishIDForAccount(fishID, fishBindings, item.record.ID)}
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			native, err := item.manager.native.Status(ctx)
			if err != nil {
				native.Connection = "unavailable"
			}
			name := item.record.Label
			nickname := ""
			if resolver, ok := item.manager.native.(interface {
				DisplayName(context.Context) (string, error)
			}); ok {
				nickname, _ = resolver.DisplayName(ctx)
			}
			if nickname != "" && item.record.Label == "" {
				name = nickname
			}
			if name == "" {
				if item.record.ID == primary {
					name = "当前微信账号"
				} else {
					name = "未登录的小号"
				}
			}
			rows[i] = map[string]any{"id": item.record.ID, "name": name, "nickname": nickname, "label": item.record.Label, "primary": item.record.ID == primary, "native": native, "bot": item.manager.BotStatus(), "fish_id": fishIDForAccount(fishID, fishBindings, item.record.ID)}
		}(i, item)
	}
	wg.Wait()
	response := map[string]any{"items": rows, "primary": primary, "supported": supported, "directory_error": directoryError}
	if fishErr != nil {
		response["fish_error"] = fishErr.Error()
	}
	respond(w, 200, response)
}

func fishIDForAccount(fishID string, bindings map[string]bool, account string) string {
	if bindings[account] {
		return fishID
	}
	return ""
}

// selectAccount never falls back to primary for an unknown or conflicting ID.
func (a *accountRegistry) selectAccount(w http.ResponseWriter, r *http.Request, primary http.Handler) {
	if !strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/api/local/") || apphttp.OwnsPath(r.URL.Path) || (r.URL.Path == "/api/accounts" || strings.HasPrefix(r.URL.Path, "/api/accounts/")) || r.URL.Path == "/api/login" || r.URL.Path == "/api/logout" || r.URL.Path == "/api/password" || r.URL.Path == "/api/session" {
		primary.ServeHTTP(w, r)
		return
	}
	if _, _, ok := a.root.session(r); !ok {
		fail(w, 401, "请先登录管理页面")
		return
	}
	header, query := r.Header.Get("X-Wechat-Account"), r.URL.Query().Get("account")
	if header != "" && query != "" && header != query {
		fail(w, 400, "账号上下文不一致，请重新选择账号")
		return
	}
	id := header
	if id == "" {
		id = query
	}
	if id == "" {
		id = a.root.manager.native.Account()
	}
	item := a.lookup(id)
	if item == nil || item.record.Removed {
		fail(w, 404, "账号不存在，请从账号列表重新选择")
		return
	}
	if item.manager == nil {
		fail(w, 503, item.loadError)
		return
	}
	if item.access != nil {
		item.access.mu.RLock()
		defer item.access.mu.RUnlock()
		if item.access.removed {
			fail(w, 404, "账号已移除，请重新选择")
			return
		}
	}
	w.Header().Set("X-Wechat-Account", id)
	if item.handler == nil {
		primary.ServeHTTP(w, r)
	} else {
		item.handler.ServeHTTP(w, r)
	}
}
