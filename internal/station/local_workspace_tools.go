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
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/workspacefs"
)

// localWorkspaceGrant is minted from the station identity, never from a
// connected application account.  Revision makes a paused home grant
// immediately unusable by a tool access object retained by an old turn.
type localWorkspaceGrant struct {
	FishID    string
	OwnerID   string
	Directory string
	Revision  uint64
}

// localWorkspaceAccess exposes the fish's own home only.  It deliberately has
// no account, conversation, or platform identity: applications are connected
// resources, not the owner of the fish's private working directory.
type localWorkspaceAccess struct {
	home            *localHomeService
	fishID, ownerID string
}

func (a *localWorkspaceAccess) matches(fishID, ownerID string) bool {
	return (a.fishID == "" || a.fishID == fishID) && (a.ownerID == "" || a.ownerID == ownerID)
}

func (a *localWorkspaceAccess) resolve() (localWorkspaceGrant, error) {
	if a == nil || a.home == nil || a.home.identity == nil {
		return localWorkspaceGrant{}, errors.New("本机家目录不可用")
	}
	record, err := a.home.identity.Read()
	if err != nil {
		return localWorkspaceGrant{}, errors.New("本机身份记录不可读，原文件保留")
	}
	if record.LocalOwner == nil || !record.LocalOwner.HomeAccess || !fishidentity.ValidFishID(record.FishID) || !fishidentity.ValidLocalOwner(*record.LocalOwner) {
		return localWorkspaceGrant{}, errors.New("本机家目录授权已暂停")
	}
	if !a.matches(record.FishID, record.LocalOwner.ID) {
		return localWorkspaceGrant{}, errors.New("本机身份已变化，请重新开启会话")
	}
	if err := a.home.ensureDirectory(record); err != nil {
		return localWorkspaceGrant{}, err
	}
	directory, err := a.home.directory(record)
	if err != nil {
		return localWorkspaceGrant{}, err
	}
	// EnsurePath creates and pins ancestors without following a path string
	// through a reparse point.  Check the final mount too before passing it to
	// agenthost, which then uses os.Root/workspacefs for every child operation.
	root, err := workspacefs.EnsurePath(directory)
	if err != nil {
		return localWorkspaceGrant{}, errors.New("家目录暂不可用，身份与已有文件保留")
	}
	if err := root.Close(); err != nil {
		return localWorkspaceGrant{}, errors.New("家目录暂不可用，身份与已有文件保留")
	}
	resolved, resolveErr := filepath.EvalSymlinks(directory)
	info, statErr := os.Lstat(directory)
	rel, relErr := filepath.Rel(directory, resolved)
	if resolveErr != nil || statErr != nil || relErr != nil || rel != "." || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return localWorkspaceGrant{}, errors.New("家目录暂不可用，身份与已有文件保留")
	}
	return localWorkspaceGrant{FishID: record.FishID, OwnerID: record.LocalOwner.ID, Directory: directory, Revision: record.LocalOwner.Revision}, nil
}

// current re-reads the durable identity on every operation boundary.  A
// revision change (including pause) invalidates an already discovered tool.
func (a *localWorkspaceAccess) current(grant localWorkspaceGrant) bool {
	latest, err := a.resolve()
	return err == nil && latest.FishID == grant.FishID && latest.OwnerID == grant.OwnerID && latest.Directory == grant.Directory && latest.Revision == grant.Revision
}

// withCurrentWriteGrant serializes a file mutation with a pause/resume.  A
// second unlocked boolean check would still leave a TOCTOU gap between the
// check and agenthost opening the destination.  Keep identity's registry and
// sidecar lock through the write instead: either pause wins before this call,
// or this one complete file operation finishes before pause returns.
func (a *localWorkspaceAccess) withCurrentWriteGrant(grant localWorkspaceGrant, write func() error) error {
	if a == nil || a.home == nil || a.home.identity == nil {
		return errors.New("本机家目录文件工具不可用")
	}
	err := a.home.identity.GuardHome(grant.FishID, grant.OwnerID, grant.Revision, func(record fishidentity.Record) error {
		directory, err := a.home.directory(record)
		if err != nil || directory != grant.Directory {
			return fishidentity.ErrHomeRevisionConflict
		}
		return write()
	})
	switch {
	case errors.Is(err, fishidentity.ErrRegistryBusy):
		return errors.New("本机身份绑定正在被其他进程使用")
	case errors.Is(err, fishidentity.ErrHomeRevisionConflict):
		return errors.New("本机家目录授权已变更，本次写入未执行")
	default:
		return err
	}
}

type localWorkspaceFileTools struct{ access *localWorkspaceAccess }

func newLocalFileTools(access *localWorkspaceAccess) *localWorkspaceFileTools {
	return &localWorkspaceFileTools{access: access}
}

func (t *localWorkspaceFileTools) List(context.Context) ([]mcptools.Tool, error) {
	return []mcptools.Tool{
		mcptools.NewTool("server_status", "查看大肥鱼家目录的文件工具状态。文件能力在此处提供；程序运行请使用 server_windows_process，并以宿主实际配置为准。Input {}.", `{"type":"object","properties":{},"additionalProperties":false}`),
		mcptools.NewTool("server_list", "列出大肥鱼家目录中的文件或目录。path 可为相对家目录路径或家目录内绝对路径。", `{"type":"object","properties":{"path":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`),
		mcptools.NewTool("server_read", "读取大肥鱼家目录中的一个 UTF-8 文本文件。返回 sha256 和续读位置；path 可为相对路径。", `{"type":"object","properties":{"path":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":0},"length":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`),
		mcptools.NewTool("server_search", "在大肥鱼家目录内搜索文本。content 是要搜索的文字；按返回 cursor 继续，不要自行编造 cursor。", `{"type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1},"cursor":{"type":"string"},"max_files":{"type":"integer","minimum":0},"timeout_seconds":{"type":"number","minimum":0},"exclude_dirs":{"type":"array","items":{"type":"string"}}},"required":["path","content"],"additionalProperties":false}`),
		mcptools.NewTool("server_write", "在大肥鱼家目录中新建一个 UTF-8 文件。不会覆盖已有文件；需要修改时先读再用 server_edit。", `{"type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string","maxLength":64000}},"required":["path","content"],"additionalProperties":false}`),
		mcptools.NewTool("server_edit", "按刚读取到的 sha256 修改大肥鱼家目录内一个 UTF-8 文本文件。", `{"type":"object","properties":{"path":{"type":"string","minLength":1},"expected_sha256":{"type":"string","minLength":64,"maxLength":64},"old_text":{"type":"string","minLength":1},"new_text":{"type":"string","maxLength":64000}},"required":["path","expected_sha256","old_text","new_text"],"additionalProperties":false}`),
	}, nil
}

// localWorkspaceFileInput intentionally omits command and all execution
// controls. Process execution uses its own explicit authorization surface.
type localWorkspaceFileInput struct {
	Path           string    `json:"path"`
	Content        string    `json:"content"`
	Expected       string    `json:"expected_sha256"`
	Old            string    `json:"old_text"`
	New            string    `json:"new_text"`
	Offset         int       `json:"offset"`
	Limit          int       `json:"limit"`
	Length         int       `json:"length"`
	Cursor         string    `json:"cursor"`
	MaxFiles       *int      `json:"max_files"`
	TimeoutSeconds *float64  `json:"timeout_seconds"`
	ExcludeDirs    *[]string `json:"exclude_dirs"`
}

func (in localWorkspaceFileInput) hostInput(path string) agenthost.Input {
	return agenthost.Input{Path: path, Content: in.Content, Expected: in.Expected, Old: in.Old, New: in.New, Offset: in.Offset, Limit: in.Limit, Length: in.Length, Cursor: in.Cursor, MaxFiles: in.MaxFiles, TimeoutSeconds: in.TimeoutSeconds, ExcludeDirs: in.ExcludeDirs}
}

func localWorkspacePath(grant localWorkspaceGrant, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || strings.ContainsRune(raw, 0) {
		return "", errors.New("请提供家目录内的文件路径")
	}
	path := filepath.Clean(raw)
	if !filepath.IsAbs(path) {
		if path == ".." || strings.HasPrefix(path, ".."+string(os.PathSeparator)) {
			return "", errors.New("路径不能离开大肥鱼家目录")
		}
		path = filepath.Join(grant.Directory, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(grant.Directory, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", errors.New("路径不在大肥鱼家目录内")
	}
	return path, nil
}

func localWorkspaceFileHost(grant localWorkspaceGrant) agenthost.Host {
	return agenthost.Host{Config: agenthost.Config{Mode: "files", Roots: []string{grant.Directory}, TimeoutSeconds: 60, OutputBytes: 16000}}
}

func (t *localWorkspaceFileTools) Call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if t == nil || t.access == nil {
		return "", errors.New("本机家目录文件工具不可用")
	}
	grant, err := t.access.resolve()
	if err != nil {
		return "", err
	}
	var in localWorkspaceFileInput
	if err := strictLocalMemoryDecode(raw, &in); err != nil {
		return "", errors.New("本机家目录文件参数无效")
	}
	return t.call(ctx, name, in, grant)
}

func (t *localWorkspaceFileTools) call(ctx context.Context, name string, in localWorkspaceFileInput, grant localWorkspaceGrant) (string, error) {
	if name != "server_status" && name != "server_list" && name != "server_read" && name != "server_search" && name != "server_write" && name != "server_edit" {
		return "", fmt.Errorf("未知本机家目录文件工具: %s", name)
	}
	var err error
	path := ""
	if name != "server_status" {
		path, err = localWorkspacePath(grant, in.Path)
		if err != nil {
			return "", err
		}
	}
	// Check after parsing too: callers cannot reuse an old grant while a turn
	// is waiting for model/tool scheduling.  For writes this is repeated at the
	// closest available boundary immediately before agenthost opens the file.
	if (name != "server_write" && name != "server_edit") && !t.access.current(grant) {
		return "", errors.New("本机家目录授权已变更，本次操作未执行")
	}
	encoded, err := json.Marshal(in.hostInput(path))
	if err != nil {
		return "", errors.New("本机家目录文件参数无效")
	}
	host := localWorkspaceFileHost(grant)
	var result any
	if name == "server_write" || name == "server_edit" {
		err = t.access.withCurrentWriteGrant(grant, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			result, err = host.Call(ctx, name, encoded)
			return err
		})
	} else {
		result, err = host.Call(ctx, name, encoded)
	}
	if err != nil {
		return "", err
	}
	if name == "server_status" {
		// Host's GPU fields describe Linux device probes, not the Windows
		// station's configured runner. Do not turn their absence into a false
		// claim that no GPU is available. Execution capability is separate.
		if status, ok := result.(map[string]any); ok {
			delete(status, "gpu_devices_available")
			delete(status, "gpu_mounted")
			delete(status, "gpu_instruction")
			status["capabilities"] = []string{"list", "read", "search", "write", "edit"}
			status["execution_tool"] = "server_windows_process"
			status["execution_scope"] = "host-configured isolated runner"
		}
	}
	encoded, err = json.Marshal(result)
	if err != nil {
		return "", errors.New("本机家目录文件结果不可用")
	}
	return string(encoded), nil
}
