package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wechat-bot/internal/workspacefs"
)

// Called with the lifecycle lock. Do not hold the directory lock across a
// native operation or while waiting for this account's active requests.
func (a *accountRegistry) removeAccount(w http.ResponseWriter, r *http.Request, purge bool) {
	id := r.PathValue("id")
	item := a.lookup(id)
	if item == nil {
		fail(w, 404, "账号不存在")
		return
	}
	if id == a.root.manager.native.Account() {
		fail(w, 409, "主账号是本机服务入口，无法移除")
		return
	}
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed || a.root.manager.draining() {
		fail(w, 409, "服务正在收尾，请稍后重试")
		return
	}
	record := item.record
	if record.Purged || (record.Removed && item.manager == nil && !purge && !record.Deleting) {
		respond(w, 200, map[string]bool{"removed": true})
		return
	}
	if record.Deleting && !purge {
		fail(w, 409, "此账号的数据删除尚未完成")
		return
	}
	if item.access != nil {
		item.access.mu.Lock()
		defer item.access.mu.Unlock()
	}
	if item.manager != nil && item.manager.Config().Enabled {
		if err := item.manager.Toggle(false); err != nil {
			fail(w, 409, "账号 Agent 未能停止，请重试")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	factory, ok := a.root.manager.native.(accountFactory)
	if !ok {
		fail(w, 409, "原生服务不支持账号管理")
		return
	}
	native, err := factory.ForAccount(id)
	if err == nil {
		err = native.EndAccountSession(ctx, purge, a.root.manager.native.Account())
	}
	if err != nil {
		fail(w, 502, "未能确认此账号退出微信："+err.Error())
		return
	}
	record.Removed = true
	record.Deleting = purge
	if err = a.saveRemoval(item, record); err != nil {
		fail(w, 500, "账号目录保存失败，请重试")
		return
	}
	if item.manager != nil {
		if err = item.manager.Close(); err != nil {
			fail(w, 500, "账号已停用，后台资源尚未完全关闭："+err.Error())
			return
		}
		item.manager = nil
		item.handler = nil
		a.mu.Lock()
		a.items[id].manager = nil
		a.items[id].handler = nil
		a.mu.Unlock()
	}
	if purge {
		// A purge is irreversible for this account. Remove only its binding;
		// the service-level fish identity and any other platform binding remain.
		// Do this before deleting account storage so a registry failure cannot be
		// reported as a successful purge.
		if err = a.fish.Unbind(id); err != nil {
			fail(w, 500, "账号已停用，鱼身份解绑未完成："+err.Error())
			return
		}
		if err = a.eraseAccountFiles(id); err != nil {
			fail(w, 500, "账号已停用，删除未完成："+err.Error())
			return
		}
		record.Label = ""
		record.Purged = true
		record.Deleting = false
		if err = a.saveRemoval(item, record); err != nil {
			fail(w, 500, "数据已清理，请重试以完成登记")
			return
		}
	}
	respond(w, 200, map[string]any{"id": id, "removed": true, "history_preserved": !purge})
}

func (a *accountRegistry) saveRemoval(item *accountRuntime, record accountRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	next := append([]accountRecord{}, a.order...)
	for i := range next {
		if next[i].ID == record.ID {
			next[i] = record
		}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err = writePrivate(filepath.Join(a.root.manager.dir, "accounts.json"), data); err != nil {
		return err
	}
	a.order = next
	a.items[record.ID].record = record
	if item.access != nil {
		item.access.removed = true
	}
	return nil
}

func (a *accountRegistry) eraseAccountFiles(id string) error {
	m := a.root.manager
	digest := accountDirectory(id)
	// Roots and child names are host-derived. Never follow an imported workspace
	// pointer to a different account or to an attached external project.
	config := filepath.Join(m.dir, "accounts", digest)
	marker, err := os.ReadFile(filepath.Join(config, "workspace-directory"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("私有工作区标记不可读")
	}
	name := strings.TrimSpace(string(marker))
	if name != "" && name != readableWorkspaceDirectory(id) && name != digest {
		return errors.New("私有工作区归属无法核验")
	}
	for _, dir := range []string{digest, readableWorkspaceDirectory(id)} {
		marker := filepath.Join(m.managedRoot(), "pri_workspace", dir, "account.json")
		data, err := os.ReadFile(marker)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		var identity struct {
			Account string `json:"account_id"`
			Scope   string `json:"scope"`
		}
		if err != nil || json.Unmarshal(data, &identity) != nil || identity.Account != id || identity.Scope != digest {
			return errors.New("私有工作区身份不匹配，已保留资料")
		}
	}
	fishID, _, err := a.fish.Bindings()
	if err != nil {
		return err
	}
	if fishID != "" {
		parent := fishPrivateRoot(m.managedRoot(), fishID)
		if _, err := os.Lstat(parent); err == nil {
			private, err := workspacefs.EnsurePath(parent)
			if err != nil {
				return fmt.Errorf("打开鱼的私有目录失败：%w", err)
			}
			err = private.RemoveAll(fishAccountDir(id))
			private.Close()
			if err != nil {
				return fmt.Errorf("清理此账号的鱼家目录失败：%w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, target := range []struct{ base, parent, name string }{
		{m.managedRoot(), "pri_workspace", readableWorkspaceDirectory(id)},
		{m.managedRoot(), "pri_workspace", digest},
		{m.stateDir, "accounts", digest}, {m.dir, "accounts", digest},
	} {
		base, err := os.OpenRoot(target.base)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		err = eraseAccountChild(base, target.parent, target.name)
		base.Close()
		if err != nil {
			return fmt.Errorf("清理 %s 失败：%w", target.parent, err)
		}
	}
	return nil
}

func eraseAccountChild(base *os.Root, parent, name string) error {
	info, err := base.Lstat(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("目录不是独立的本地目录")
	}
	root, err := base.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(name)
}
