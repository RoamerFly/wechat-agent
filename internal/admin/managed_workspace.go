package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/stationruntime"
	"wechat-bot/internal/workspacefs"
)

// ExecutionContext is minted from the authenticated native packet, never from
// tool arguments, nicknames, quoted text, summaries or downloaded documents.
type ExecutionContext struct {
	Account      string `json:"account"`
	OriginThread string `json:"origin_thread"`
	Requester    string `json:"requester"`
	Owner        bool   `json:"owner"`
	TaskID       string `json:"task_id,omitempty"`
	Private      string `json:"private_workspace"`
	Public       string `json:"public_workspace"`
	Conversation string `json:"conversation_workspace"`
}

func (m *Manager) managedRoot() string {
	if base := os.Getenv("WECHAT_WORKSPACE_ROOT"); base != "" && filepath.IsAbs(base) {
		return filepath.Clean(base)
	}
	if m.accounts != nil && m.accounts.root.manager != m {
		return m.accounts.root.manager.managedRoot()
	}
	if filepath.Base(filepath.Dir(m.dir)) == "accounts" {
		return filepath.Join(filepath.Dir(filepath.Dir(m.dir)), "managed-workspaces")
	}
	return filepath.Join(m.dir, "managed-workspaces")
}
func (p *permittedTools) executionContext() ExecutionContext {
	root := p.manager.managedRoot()
	private := p.manager.privateWorkspaceFor(p.trace.Account)
	return ExecutionContext{Account: p.trace.Account, OriginThread: p.trace.Thread, Requester: p.trace.Sender, Owner: p.privileged(), TaskID: p.trace.TaskID, Private: private, Public: filepath.Join(root, "public_workspace"), Conversation: filepath.Join(private, "conversations", companion.ScopeID("wechat", p.trace.Account, p.trace.Thread))}
}
func ensureManaged(paths ...string) error {
	for _, path := range paths {
		root, e := workspacefs.EnsurePath(path)
		if e != nil {
			return e
		}
		if e = root.Close(); e != nil {
			return e
		}
	}
	return nil
}
func (p *permittedTools) hostConfig() (agenthost.Config, error) {
	c := p.manager.agent.config().Host
	if c.Windows != nil {
		windows := *c.Windows
		windows.MusicRegistration = filepath.Join(p.manager.dir, "windows-music.json")
		c.Windows = &windows
	}
	x := p.executionContext()
	publicProjects := filepath.Join(x.Public, "projects")
	models, runtimes := filepath.Join(x.Public, "models"), filepath.Join(x.Public, "runtimes")
	if err := ensureManaged(x.Conversation, x.Private, publicProjects, models, runtimes, filepath.Join(x.Public, "manifests")); err != nil {
		return c, err
	}
	for _, sub := range []string{"memory", "inbox", "output", "tasks", "scratch"} {
		if e := ensureManaged(filepath.Join(x.Conversation, sub)); e != nil {
			return c, e
		}
	}
	c.Roots = []string{x.Conversation}
	c.Isolation = &agenthost.Isolation{ReadOnly: []string{publicProjects, models, runtimes, filepath.Join(x.Public, "manifests")}}
	// The desktop package owns lightweight command-line runtimes once. Mount
	// only audited non-control children needed by generic workspace work; never
	// expose the desktop executable, broker, device tools or the package root.
	c.Isolation.ReadOnly = append(c.Isolation.ReadOnly, stationruntime.PackagedCommandRuntimeRoots(c.Windows)...)
	if x.Owner {
		// An owner may maintain public projects and the current account's private
		// workspace, but the public workspace root also holds runtime definitions,
		// model registrations and manifests consumed by trusted host components.
		// Never turn that parent into a writable mount: it would let a workspace
		// edit indirectly alter a host control plane. Keep only projects writable;
		// package/runtime/model/manifests stay explicit read-only mounts.
		// Account-wide private access remains an explicit global=true elevation;
		// the normal owner mount retains only this conversation plus public code.
		c.Roots = []string{x.Conversation, publicProjects}
	}
	// Windows uses its own mandatory LPAC backend; never the legacy user shell.
	return c, nil
}

// A global mount is requested for one tool invocation, never stored on a session.
func (p *permittedTools) scopedHostConfig(raw json.RawMessage) (agenthost.Config, error) {
	var in struct {
		Global   bool `json:"global"`
		GPU      bool `json:"gpu"`
		FishHome bool `json:"fish_home"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return agenthost.Config{}, toolInputError("invalid execution scope")
	}
	if in.Global && in.FishHome {
		return agenthost.Config{}, toolInputError("fish_home cannot be combined with global scope")
	}
	c, e := p.hostConfig()
	if e != nil {
		return c, e
	}
	c.Isolation.GPU = in.GPU
	if in.FishHome {
		home, _, err := p.boundFishHome()
		if err != nil {
			return c, toolInputError(err.Error())
		}
		// Do not expose the fish parent, conversation, account-private or public
		// project writable roots. Retain only hostConfig's read-only dependencies.
		c.Roots = []string{home}
		return c, nil
	}
	if in.Global {
		if !p.privileged() || !p.manager.agent.config().CrossConversation {
			return c, toolInputError("account global workspace requires the trusted owner")
		}
		x := p.executionContext()
		// Match the owner default above. global widens only this account's private
		// data, not the public runtime/control parent or another account.
		c.Roots = []string{x.Private, filepath.Join(x.Public, "projects")}
	}
	return c, nil
}
func (p *permittedTools) workspaceExecutionAllowed() bool {
	return p.trace.Sender != "" && (strings.HasSuffix(p.trace.Thread, "@chatroom") || p.trace.Thread == p.trace.Sender) && !p.trace.Preview && !p.trace.ReportOnly && !p.trace.Surf && p.trace.Account == p.manager.native.Account() && p.trace.Thread != "" && permittedRecipient(p.manager.Config().Config, p.trace.Thread) && p.manager.agent.config().Enabled && p.manager.agent.config().Host.Mode != "disabled"
}
func (p *permittedTools) projectTools() []mcptools.Tool {
	return []mcptools.Tool{
		agentTool("agent_global_search", "Trusted owner only: search THIS account's stored conversation summaries, messages and assets, newest original time first. Asset at refers to its first source request, not later cache/format updates; time_basis labels legacy fallback and summary times. Results retain source conversation, message IDs and sender names. since is inclusive, before exclusive; use RFC3339 with timezone. Does not switch identity or authorize other accounts. Optional archives=true includes retained indexed originals; coverage reports partial indexing. order=relevance uses lexical/metadata ranking. Pagination accepts cursor or offset; a source change invalidates its cursor. Input {query?,thread_id?,since?,before?,offset?,limit?,cursor?,archives?,order?}.", `{"type":"object","properties":{"query":{"type":"string"},"thread_id":{"type":"string"},"since":{"type":"string","description":"Inclusive original time, RFC3339 with timezone"},"before":{"type":"string","description":"Exclusive original time, RFC3339 with timezone"},"cursor":{"type":"string"},"order":{"type":"string","enum":["time","relevance"]},"archives":{"type":"boolean"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1}},"additionalProperties":false}`),
	}
}
func (p *permittedTools) callProject(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if isProjectPluginTool(name) {
		return p.callProjectPlugin(ctx, name, raw)
	}
	var in struct {
		ID, Query     string
		Thread        string `json:"thread_id"`
		Since, Before string
		Cursor, Order string
		Archives      bool
		Offset, Limit int
	}
	if json.Unmarshal(raw, &in) != nil || in.Offset < 0 || in.Limit < 0 {
		return "", toolInputError("invalid search arguments")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if name != "agent_global_search" {
		return "", toolInputError("unknown memory tool")
	}
	return p.searchAccountMemory(ctx, accountMemorySearch{Query: in.Query, Thread: in.Thread, Since: in.Since, Before: in.Before, Offset: in.Offset, Limit: in.Limit, Cursor: in.Cursor, Order: in.Order, Archives: in.Archives})
}

// Notes are mapped into the same physical session directories as file tools.
// Legacy notes are copied once, never overwritten or deleted.
func (m *Manager) configureManagedNotes() error {
	base := m.privateWorkspaceFor(m.native.Account())
	if err := ensureManaged(base, filepath.Join(base, "global", "memory"), filepath.Join(base, "global", "tasks"), filepath.Join(base, "assets", "objects")); err != nil {
		return err
	}
	if err := m.writeWorkspaceIdentity(); err != nil {
		return err
	}
	identity := filepath.Join(base, "account.md")
	if _, err := os.Lstat(identity); os.IsNotExist(err) {
		content := "# Account workspace\n\nStable account: " + m.native.Account() + "\n\nConversation directories belong only to this account. Global access requires the real trusted owner for the current request. Use project_list/project_read for shared projects and agent_global_search for source-labelled account memory. Credentials and executor control files are outside this workspace.\n"
		f, e := os.OpenFile(identity, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = f.WriteString(content)
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	} else if err != nil {
		return err
	}
	resolve := func(scope string) (string, error) {
		if scope == "shared" {
			return filepath.Join(base, "guides"), nil
		}
		if scope == agentID(m.native.Account(), "global-notes") {
			return filepath.Join(base, "global", "memory"), nil
		}
		if len(scope) != 64 || strings.Trim(scope, "0123456789abcdef") != "" {
			return "", errors.New("invalid_note_scope")
		}
		return filepath.Join(base, "conversations", scope, "memory"), nil
	}
	old := filepath.Join(m.dir, "agent-workspace")
	entries, e := os.ReadDir(old)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		dest, e := resolve(entry.Name())
		if e != nil {
			continue
		}
		if e = ensureManaged(dest); e != nil {
			return e
		}
		files, e := os.ReadDir(filepath.Join(old, entry.Name()))
		if e != nil {
			return e
		}
		for _, file := range files {
			if file.IsDir() || file.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(file.Name(), ".md") {
				continue
			}
			b, e := os.ReadFile(filepath.Join(old, entry.Name(), file.Name()))
			if e != nil {
				return e
			}
			target := filepath.Join(dest, file.Name())
			if _, e = os.Stat(target); os.IsNotExist(e) {
				if e = writePrivate(target, b); e != nil {
					return e
				}
			}
		}
	}
	m.workspace.ResolveDirectory = resolve
	m.workspace.BoundaryRoot = base
	return ensureManaged(filepath.Join(base, "global", "memory"), filepath.Join(base, "assets", "objects"))
}

func (m *Manager) managedLayoutStatus() map[string]any {
	backend := "当前平台尚未适配隔离执行器"
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err == nil {
			backend = "Linux · Bubblewrap 已安装"
		} else {
			backend = "Linux · 待安装 Bubblewrap"
		}
	} else if runtime.GOOS == "windows" {
		backend = "Windows · LPAC CMD"
	}
	return map[string]any{"account": m.native.Account(), "private_workspace": m.privateWorkspaceFor(m.native.Account()), "private_voice_library": m.privateVoiceLibrary(), "public_workspace": filepath.Join(m.managedRoot(), "public_workspace"), "backend": backend, "platform": runtime.GOOS, "isolation_required": true}
}

func (s *Server) managedWorkspaceRoutes(mux *http.ServeMux) {
	s.crossMediaRoutes(mux)
	s.accountConversationConsoleRoutes(mux)
	mux.HandleFunc("POST /api/agent/file-read", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Message string `json:"message_id"`
			Thread  string `json:"thread_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		m := s.manager
		if !s.consoleConversationAllowed(in.Thread) {
			fail(w, 403, "来源会话未授权")
			return
		}
		msg, e := m.native.Message(r.Context(), in.Message)
		if e != nil || msg.AccountID != m.native.Account() || msg.ThreadID != in.Thread || msg.FileAttachmentName() == "" {
			fail(w, 400, "不是当前账号来源会话中的文件")
			return
		}
		result, e := (mediaUnderstanding{manager: m}).receiveFile(r.Context(), msg, msg.FileAttachmentName())
		if e != nil {
			fail(w, 502, e.Error())
			return
		}
		respond(w, 200, map[string]any{"observation": result, "real_wechat_sends": 0})
	}))

	mux.HandleFunc("GET /api/agent/global-search", s.protected(func(w http.ResponseWriter, r *http.Request) {
		p, accessErr := s.consoleGlobalAccess()
		if accessErr != nil {
			fail(w, 403, accessErr.Error())
			return
		}
		offset, e := strconv.Atoi(r.URL.Query().Get("offset"))
		if e != nil && r.URL.Query().Has("offset") {
			fail(w, 400, "分页参数无效")
			return
		}
		limit, e := strconv.Atoi(r.URL.Query().Get("limit"))
		if e != nil && r.URL.Query().Has("limit") {
			fail(w, 400, "分页参数无效")
			return
		}
		// Explicit authenticated console inspection, not a fabricated WeChat packet.
		raw, _ := json.Marshal(map[string]any{"query": r.URL.Query().Get("query"), "thread_id": r.URL.Query().Get("thread_id"), "since": r.URL.Query().Get("since"), "before": r.URL.Query().Get("before"), "offset": offset, "limit": limit, "cursor": r.URL.Query().Get("cursor"), "order": r.URL.Query().Get("order"), "archives": r.URL.Query().Get("archives") == "true"})
		result, err := p.callProject(r.Context(), "agent_global_search", raw)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(result))
	}))
	mux.HandleFunc("GET /api/agent/projects", s.protected(func(w http.ResponseWriter, r *http.Request) {
		p := &permittedTools{manager: s.manager, trace: companion.Input{Account: s.manager.native.Account()}}
		result, err := p.callProject(s.speechPluginTestContext(r), "project_list", json.RawMessage(`{}`))
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(result))
	}))
}

// Only a current, compact public registration index enters each round. Detailed
// manuals and private model files are still retrieved on demand by tools.
func (p *permittedTools) currentProjectIndex(ctx context.Context) []map[string]string {
	rows := []map[string]string{}
	// Optional project discovery is package-owned. Core note context remains
	// available when this package or its isolated runtime is absent.
	if len(p.projectPluginTools(ctx)) == 0 {
		return rows
	}
	result, err := p.callProjectPlugin(ctx, "project_list", json.RawMessage(`{}`))
	if err != nil {
		return rows
	}
	var out struct {
		Projects []struct {
			ID, Name string
			Status   string `json:"runtime_status"`
		} `json:"projects"`
	}
	if json.Unmarshal([]byte(result), &out) != nil {
		return rows
	}
	for _, row := range out.Projects {
		if row.ID != "" {
			rows = append(rows, map[string]string{"id": row.ID, "name": row.Name, "registered_runtime_status": row.Status, "details_tool": "project_read"})
		}
	}
	return rows
}
