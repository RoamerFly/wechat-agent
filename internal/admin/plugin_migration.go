package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"sort"

	"wechat-bot/internal/pluginhost"
	catalog "wechat-bot/plugins"
)

func (s *Server) pluginMigrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/plugin-migrations/preview", s.protected(func(w http.ResponseWriter, r *http.Request) {
		preview, err := s.manager.pluginMigrationPreview(r.Context())
		if err != nil {
			fail(w, 500, "迁移预演读取失败")
			return
		}
		respond(w, 200, preview)
	}))
	mux.HandleFunc("POST /api/plugin-migrations/apply", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Account               string   `json:"account"`
			Plugin                string   `json:"plugin"`
			ApplySourceHash       string   `json:"apply_source_hash"`
			PreparedPackageDigest string   `json:"prepared_package_digest"`
			Permissions           []string `json:"permissions"`
			Confirm               bool     `json:"confirm"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !in.Confirm || in.Account == "" || in.Plugin == "" || in.ApplySourceHash == "" || in.PreparedPackageDigest == "" {
			fail(w, 400, "请确认迁移计划、账号、包摘要和权限")
			return
		}
		if s.manager.mobileRetired && in.Plugin == mobileDevicePluginID {
			fail(w, http.StatusGone, "手机控制已停用；不能迁移或重新启用历史手机工具")
			return
		}
		if s.manager.draining() {
			fail(w, 409, "服务正在收尾，请稍后重试")
			return
		}
		installed, config, record, err := s.manager.applyPluginMigration(r.Context(), in.Account, in.Plugin, in.ApplySourceHash, in.PreparedPackageDigest, in.Permissions)
		if err != nil {
			fail(w, 409, "迁移未应用：预演来源、包、权限或目标状态已变化，请刷新预演后重试")
			return
		}
		respond(w, 201, map[string]any{"installed": installed, "configuration": config, "migration": record, "enabled": false})
	}))
}

// officialMigrationDirectory derives the source only from trusted deployment
// options and a fixed catalog ID; apply never accepts an arbitrary path.
func officialMigrationDirectory(id string) (string, error) {
	if id == "" || filepath.Base(id) != id {
		return "", errors.New("invalid official plugin")
	}
	options, err := runtimeDeploymentOptions()
	if err != nil || options.CatalogDirectory == "" {
		return "", errors.New("official_package_source_unavailable")
	}
	root, err := filepath.Abs(options.CatalogDirectory)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, id)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", errors.New("official_package_source_unavailable")
	}
	p, err := pluginhost.Verify(dir)
	if err != nil || p.ID != id {
		return "", errors.New("official package unavailable")
	}
	return dir, nil
}

func (m *Manager) migrationManager(account string) *Manager {
	if account == "" || account == m.native.Account() {
		return m
	}
	if m.accounts == nil {
		return nil
	}
	m.accounts.mu.Lock()
	defer m.accounts.mu.Unlock()
	if item := m.accounts.items[account]; item != nil && !item.record.Removed {
		return item.manager
	}
	return nil
}

func (m *Manager) applyPluginMigration(ctx context.Context, account, id, sourceHash, digest string, permissions []string) (pluginhost.InstalledPackage, pluginhost.Configuration, pluginhost.MigrationRecord, error) {
	target := m.migrationManager(account)
	if target == nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, errors.New("migration account unavailable")
	}
	if target.mobileRetired && id == mobileDevicePluginID {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, errDeviceServiceRetired
	}
	// Activating external host state replaces the legacy JSON source bytes with
	// canonical SQLite bytes. A browser retry after the successful POST can
	// therefore carry the just-completed legacy digest. Accept only that exact,
	// disabled and empty-config migration record; never re-import or mutate it.
	if id == externalHTTPPluginID {
		if installed, configuration, record, ok, err := target.completedExternalMigrationReplay(ctx, account, sourceHash, digest, permissions); err != nil {
			return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, err
		} else if ok {
			return installed, configuration, record, nil
		}
	}
	preview, err := target.pluginMigrationPreviewFor(ctx, migrationCatalog())
	if err != nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, err
	}
	var step *pluginMigrationStep
	for i := range preview.Plugins {
		if preview.Plugins[i].Plugin == id {
			step = &preview.Plugins[i]
			break
		}
	}
	if step == nil || step.Status != "ready_for_administrator_review" || len(step.RequiredMissing) != 0 || step.ApplySourceHash != sourceHash {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, errors.New("migration source changed or blocked")
	}
	items, err := catalog.List()
	if err != nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, err
	}
	valid := false
	for _, p := range items {
		if p.ID == id && p.Version == step.Package.Version && p.Status == "ready" {
			valid = true
		}
	}
	if !valid {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, errors.New("official migration package changed")
	}
	store, err := target.packageStore()
	if err != nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, err
	}
	migrationID := "legacy-" + account + "-" + id + "-" + sourceHash
	if current, e := store.Installed(ctx, id); e == nil {
		record, recordErr := store.Migration(ctx, migrationID)
		resuming := recordErr == nil && record.Plugin == id && record.Digest == digest && record.SourceHash == sourceHash && record.InstalledRevision == current.Revision
		if current.Enabled || current.Removed || (current.Revision != step.ExpectedRevision && !resuming) {
			return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, errors.New("migration target changed")
		}
	} else if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, e
	}
	dir, err := officialMigrationDirectory(id)
	if err != nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, err
	}
	prepared, err := store.Prepare(ctx, dir)
	if err != nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, err
	}
	if prepared.Digest != digest || prepared.Package.ID != id || prepared.Package.Version != step.Package.Version {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, errors.New("prepared package changed")
	}
	installed, configuration, record, err := store.ApplyMigration(ctx, pluginhost.MigrationApply{ID: migrationID, SourceHash: sourceHash, PreparedToken: prepared.Token, PreparedDigest: prepared.Digest, ExpectedPackage: id, Permissions: permissions, Configuration: step.update})
	if err != nil {
		return installed, configuration, record, err
	}
	if id == externalHTTPPluginID {
		// The external package is only an adapter. Its legacy MCP and HTTP
		// declarations stay host-owned, so activation happens after the
		// disabled install and is retried on the same apply request if it was
		// interrupted. No tool is enabled or executed here.
		if err := target.migrateExternalConfiguration(ctx); err != nil {
			return installed, configuration, record, err
		}
	}
	return installed, configuration, record, nil
}

func (m *Manager) completedExternalMigrationReplay(ctx context.Context, account, sourceHash, digest string, permissions []string) (pluginhost.InstalledPackage, pluginhost.Configuration, pluginhost.MigrationRecord, bool, error) {
	store, err := m.packageStore()
	if err != nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, false, err
	}
	migrationID := "legacy-" + account + "-" + externalHTTPPluginID + "-" + sourceHash
	record, err := store.Migration(ctx, migrationID)
	if errors.Is(err, sql.ErrNoRows) {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, false, nil
	}
	if err != nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, false, err
	}
	installed, err := store.Installed(ctx, externalHTTPPluginID)
	if err != nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, false, err
	}
	configuration, err := store.Configuration(ctx, externalHTTPPluginID)
	if err != nil {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, false, err
	}
	requested := append([]string(nil), permissions...)
	actual := append([]string(nil), installed.Permissions...)
	sort.Strings(requested)
	sort.Strings(actual)
	if record.State != "complete" || record.Plugin != externalHTTPPluginID || record.Digest != digest || record.SourceHash != sourceHash || record.InstalledRevision != installed.Revision || installed.Digest != digest || installed.Enabled || installed.Removed || len(configuration.Values) != 0 || len(configuration.ConfiguredSecrets) != 0 || len(requested) != len(actual) {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, false, nil
	}
	for i := range requested {
		if requested[i] != actual[i] {
			return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, false, nil
		}
	}
	if _, active := m.externalConfigurationSources(); !active {
		return pluginhost.InstalledPackage{}, pluginhost.Configuration{}, pluginhost.MigrationRecord{}, false, nil
	}
	return installed, configuration, record, true, nil
}
