package station

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/stationruntime"
)

// localProcessTools is deliberately independent from an application Manager.
// The local owner owns a stable process namespace, while an active home grant
// is required only to start new work.  This lets a paused owner inspect and
// stop an already-created process without granting the home back.
type localProcessTools struct {
	access     *localWorkspaceAccess
	jobs       *agenthost.Jobs
	cfg        *agenthost.WindowsConfig
	privateDir string
}

type localProcessInput struct {
	Action  string   `json:"action"`
	ID      string   `json:"id"`
	Path    string   `json:"path"`
	Argv    []string `json:"argv"`
	Key     string   `json:"request_key"`
	Seconds int      `json:"timeout_seconds"`
}

// newLocalProcessTools persists only process metadata under the host-private
// data directory.  The broker spool remains host-private as well; neither is
// added to an Agent mount.
func newLocalProcessTools(access *localWorkspaceAccess, dataDir string, cfg *agenthost.WindowsConfig) (*localProcessTools, error) {
	if access == nil || access.home == nil || access.home.identity == nil || strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("本机程序工具配置无效")
	}
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	jobs, err := agenthost.OpenJobs(filepath.Join(dataDir, "local-processes"))
	if err != nil {
		return nil, err
	}
	t := &localProcessTools{access: access, jobs: jobs, privateDir: dataDir}
	if cfg != nil {
		copy := *cfg
		// The caller owns runtime discovery.  Keep an invalid/incomplete runtime
		// observable through List, then return a concrete not-started result on
		// start instead of falling back to an unrestricted host shell.
		t.cfg = &copy
	}
	return t, nil
}

func (t *localProcessTools) Close() error {
	if t != nil && t.jobs != nil {
		t.jobs.Close()
	}
	return nil
}

func (t *localProcessTools) BeginShutdown() {
	if t != nil && t.jobs != nil {
		t.jobs.BeginShutdown()
	}
}

func (t *localProcessTools) List(context.Context) ([]mcptools.Tool, error) {
	return []mcptools.Tool{mcptools.NewTool("server_windows_process", "Run, inspect, read or cancel a persistent Windows LPAC process in Dafeiyu's own home. Start accepts only an absolute executable argv and never falls back to the host shell. request_key is durable: reuse it only to query the same process.", `{"type":"object","properties":{"action":{"type":"string","enum":["start","list","status","read","cancel"]},"id":{"type":"string"},"path":{"type":"string"},"argv":{"type":"array","items":{"type":"string"},"minItems":1},"request_key":{"type":"string","minLength":1,"maxLength":128},"timeout_seconds":{"type":"integer","minimum":0}},"required":["action"],"additionalProperties":false}`)}, nil
}

func (t *localProcessTools) Call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if t == nil || t.access == nil || t.jobs == nil {
		return "", errors.New("本机程序工具不可用")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if name != "server_windows_process" {
		return "", fmt.Errorf("未知本机程序工具: %s", name)
	}
	var in localProcessInput
	if err := strictLocalMemoryDecode(raw, &in); err != nil {
		return "", errors.New("本机程序参数无效")
	}
	switch in.Action {
	case "start":
		grant, err := t.access.resolve()
		if err != nil {
			return "", err
		}
		return t.start(ctx, in, grant)
	case "list":
		return t.list()
	case "status", "read":
		return t.status(in.ID, false)
	case "cancel":
		return t.status(in.ID, true)
	default:
		return "", errors.New("程序操作须为start、list、status、read或cancel")
	}
}

func (t *localProcessTools) start(ctx context.Context, in localProcessInput, grant localWorkspaceGrant) (string, error) {
	cfg, known, err := t.preflight(in, grant)
	if err != nil {
		return "", err
	}
	if known != "" {
		return known, nil
	}
	return t.startPrepared(ctx, in, grant, cfg)
}

// preflight contains only checks whose failure proves no launch was attempted.
// It is deliberately shared by direct calls and checkpointed calls: known bad
// arguments must not reserve a durable operation and turn the remainder of a
// normal chat into inspect-only recovery.
func (t *localProcessTools) preflight(in localProcessInput, grant localWorkspaceGrant) (agenthost.Config, string, error) {
	if strings.TrimSpace(in.Key) == "" {
		return agenthost.Config{}, "", mcptools.ArgumentError("$.request_key", "请提供 1 到 128 个字符的 request_key")
	}
	if len(in.Key) > 128 {
		return agenthost.Config{}, "", mcptools.ArgumentError("$.request_key", "长度不能超过 128 个字符")
	}
	if strings.TrimSpace(in.Path) == "" {
		return agenthost.Config{}, "", mcptools.ArgumentError("$.path", "请提供大肥鱼家目录内的绝对工作目录")
	}
	if err := localProcessPath(grant.Directory, in.Path); err != nil {
		return agenthost.Config{}, "", mcptools.ArgumentError("$.path", err.Error())
	}
	if len(in.Argv) == 0 {
		return agenthost.Config{}, "", mcptools.ArgumentError("$.argv", "请提供以已授权绝对 exe 文件开头的 argv")
	}
	if in.Seconds < 0 {
		return agenthost.Config{}, "", mcptools.ArgumentError("$.timeout_seconds", "不能小于 0")
	}
	if t.cfg == nil || t.cfg.Validate() != nil {
		known, err := localProcessJSON(map[string]any{
			"status": "not_started", "code": "windows_runtime_not_configured",
			"message":      "本机 Windows 隔离运行器尚未配置；程序未启动",
			"continuation": "由宿主配置受管 pythonw.exe、windows_agent_host.pyw 和私有暂存目录后重试；不要改用宿主 shell。",
		})
		return agenthost.Config{}, known, err
	}
	cfg, err := t.config(grant.Directory)
	if err != nil {
		return agenthost.Config{}, "", err
	}
	if err = localProcessArgv(cfg, in.Argv); err != nil {
		return agenthost.Config{}, "", mcptools.ArgumentError("$.argv[0]", err.Error())
	}
	return cfg, "", nil
}

func (t *localProcessTools) startPrepared(ctx context.Context, in localProcessInput, grant localWorkspaceGrant, cfg agenthost.Config) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, err := t.jobs.StartWindowsArgv(cfg, localProcessScope(grant.FishID, grant.OwnerID), in.Key, in.Path, in.Argv, in.Seconds, func() bool {
		return t.access.current(grant)
	})
	if err != nil {
		return "", err
	}
	return localProcessJSON(value)
}

func (t *localProcessTools) list() (string, error) {
	scope, err := t.ownerScope()
	if err != nil {
		return "", err
	}
	return localProcessJSON(t.jobs.List(scope))
}

func (t *localProcessTools) status(id string, cancel bool) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", errors.New("需要程序id")
	}
	scope, err := t.ownerScope()
	if err != nil {
		return "", err
	}
	value, err := t.jobs.Status(scope, id, cancel)
	if err != nil {
		return "", err
	}
	return localProcessJSON(value)
}

func (t *localProcessTools) ownerScope() (string, error) {
	record, err := t.access.home.identity.Read()
	if err != nil || !fishidentity.ValidFishID(record.FishID) || record.LocalOwner == nil || !fishidentity.ValidLocalOwner(*record.LocalOwner) {
		return "", errors.New("本机鱼身份不可用")
	}
	if !t.access.matches(record.FishID, record.LocalOwner.ID) {
		return "", errors.New("本机身份已变化，请重新开启会话")
	}
	return localProcessScope(record.FishID, record.LocalOwner.ID), nil
}

func localProcessScope(fishID, ownerID string) string {
	return companion.ScopeID("local-process", fishID, ownerID)
}

func (t *localProcessTools) config(home string) (agenthost.Config, error) {
	if t.cfg == nil || t.cfg.Validate() != nil {
		return agenthost.Config{}, errors.New("本机 Windows 隔离运行器尚未配置")
	}
	// Runtime roots are host-selected package children only.  They are never
	// writable and never include the home, private data directory, or spool.
	readonly := []string{}
	for _, candidate := range stationruntime.PackagedCommandRuntimeRoots(t.cfg) {
		if localProcessContains(home, candidate) || localProcessContains(t.privateDir, candidate) || localProcessContains(t.cfg.Spool, candidate) || localProcessContains(candidate, home) || localProcessContains(candidate, t.privateDir) || localProcessContains(candidate, t.cfg.Spool) {
			continue
		}
		readonly = append(readonly, candidate)
	}
	return agenthost.Config{
		Mode: "full", Roots: []string{home}, OutputBytes: 16000, TimeoutSeconds: 60,
		Windows: t.cfg, Isolation: &agenthost.Isolation{ReadOnly: readonly},
	}, nil
}

func localProcessPath(home, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !localProcessContains(home, path) {
		return errors.New("程序工作目录必须是大肥鱼家目录内的绝对目录")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return errors.New("程序工作目录不可用")
	}
	return nil
}

func localProcessArgv(cfg agenthost.Config, argv []string) error {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) || filepath.Clean(argv[0]) != argv[0] {
		return errors.New("程序必须是已授权目录内的绝对exe文件")
	}
	for _, root := range append(append([]string{}, cfg.Roots...), cfg.Isolation.ReadOnly...) {
		if localProcessContains(root, argv[0]) {
			return nil
		}
	}
	return errors.New("程序不在大肥鱼家目录或受管运行时目录内")
}

func localProcessContains(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func localProcessJSON(value any) (string, error) {
	b, err := json.Marshal(value)
	return string(b), err
}

var _ companion.ToolAccess = (*localProcessTools)(nil)
