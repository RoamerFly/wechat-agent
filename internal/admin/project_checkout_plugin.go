package admin

// This is a host-owned convenience for maintaining a plugin, not a package
// installation path.  It copies a verified immutable package snapshot into
// the requesting owner's normal conversation workspace; edits there can never
// change the installed package, its configuration, or its credentials.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"wechat-bot/internal/packagecheckout"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/workspacefs"
)

type projectCheckoutPluginInput struct {
	PluginID string `json:"plugin_id"`
}

// projectCheckoutPluginAllowed deliberately depends on the installed project
// capability's *write* grant.  A trusted owner with a read-only Agent host
// cannot turn a package inspection into a writable source tree.
func (p *permittedTools) projectCheckoutPluginAllowed(ctx context.Context) bool {
	if !p.privileged() || !p.workspaceExecutionAllowed() {
		return false
	}
	rows, _, installed, err := p.projectPluginCatalog(ctx)
	if err != nil {
		return false
	}
	for _, row := range rows {
		if row.Name == "project_create" && p.projectPluginToolAllowed(row, installed.Permissions) {
			return true
		}
	}
	return false
}

func checkoutPluginDirectory(id, digest string) (string, error) {
	if id == "" || len(digest) != 64 || strings.ToLower(digest) != digest {
		return "", errors.New("plugin_checkout_invalid_identity")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", errors.New("plugin_checkout_invalid_identity")
	}
	name := id + "-" + digest
	if filepath.Base(name) != name || strings.ContainsAny(name, `\\/:`+"\x00") {
		return "", errors.New("plugin_checkout_invalid_identity")
	}
	return name, nil
}

func (p *permittedTools) checkoutPluginSource(ctx context.Context, raw json.RawMessage) (string, error) {
	if !p.projectCheckoutPluginAllowed(ctx) {
		return "", toolInputError("project_checkout_requires_owner_workspace_write")
	}
	var in projectCheckoutPluginInput
	if musicStrictJSON(raw, &in) != nil || in.PluginID == "" {
		return "", toolInputError("project_checkout_invalid_request")
	}
	store, err := p.manager.packageStore()
	if err != nil {
		return "", err
	}
	installed, err := store.Installed(ctx, in.PluginID)
	if err != nil || installed.Removed || installed.Package.ID != in.PluginID {
		return "", toolInputError("plugin_checkout_plugin_not_installed")
	}
	sourceDir, err := store.PackageDirectory(installed.Digest)
	if err != nil {
		return "", err
	}
	verified, err := pluginhost.Verify(sourceDir)
	if err != nil || !reflect.DeepEqual(verified, installed.Package) {
		return "", errors.New("plugin_checkout_source_unverified")
	}
	source, err := os.OpenRoot(sourceDir)
	if err != nil {
		return "", err
	}
	defer source.Close()
	manifestFile, err := workspacefs.OpenRegular(source, "plugin.json")
	if err != nil {
		return "", errors.New("plugin_checkout_source_unverified")
	}
	manifest, readErr := io.ReadAll(io.LimitReader(manifestFile, (1<<20)+1))
	closeErr := manifestFile.Close()
	if readErr != nil || closeErr != nil || len(manifest) > 1<<20 || namedSpeechSHA(manifest) != installed.Digest {
		return "", errors.New("plugin_checkout_source_unverified")
	}
	x := p.executionContext()
	conversation, err := workspacefs.EnsurePath(x.Conversation)
	if err != nil {
		return "", err
	}
	defer conversation.Close()
	root, err := workspacefs.EnsureDirectory(conversation, "tool-sources")
	if err != nil {
		return "", err
	}
	defer root.Close()
	destinationName, err := checkoutPluginDirectory(installed.Package.ID, installed.Digest)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(x.Conversation, "tool-sources", destinationName)
	if exists, err := packagecheckout.ExistingDirectory(root, destinationName); err != nil {
		return "", err
	} else if exists {
		return projectCheckoutResult(destination, installed, "existing", 0, 0)
	}
	stageName, stage, err := packagecheckout.NewStage(root)
	if err != nil {
		return "", err
	}
	defer func() {
		if stageName != "" {
			_ = root.RemoveAll(stageName)
		}
	}()
	defer stage.Close()
	total, files := int64(0), 0
	if err = packagecheckout.CopyCheckedFile(ctx, source, stage, "plugin.json", installed.Digest, &total); err != nil {
		return "", err
	}
	files++
	names := make([]string, 0, len(installed.Package.Files))
	for name := range installed.Package.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err = packagecheckout.CopyCheckedFile(ctx, source, stage, name, installed.Package.Files[name], &total); err != nil {
			return "", err
		}
		files++
	}
	// Rename is the only publish step.  If another authorized turn won the
	// race, preserve its working copy verbatim rather than repairing/clobbering
	// files that may already contain edits.
	if err = stage.Close(); err != nil {
		return "", err
	}
	current, err := store.Installed(ctx, in.PluginID)
	if err != nil || current.Removed || current.Revision != installed.Revision || current.Digest != installed.Digest || !p.projectCheckoutPluginAllowed(ctx) {
		return "", toolInputError("plugin_checkout_authorization_or_package_changed")
	}
	if err = root.Rename(stageName, destinationName); err != nil {
		if exists, checkErr := packagecheckout.ExistingDirectory(root, destinationName); checkErr == nil && exists {
			return projectCheckoutResult(destination, installed, "existing", 0, 0)
		}
		return "", err
	}
	stageName = ""
	return projectCheckoutResult(destination, installed, "created", files, total)
}

func projectCheckoutResult(path string, installed pluginhost.InstalledPackage, state string, files int, bytes int64) (string, error) {
	result := map[string]any{
		"state": state, "path": path, "plugin_id": installed.Package.ID,
		"version": installed.Package.Version, "digest": installed.Digest,
		"source": "verified_installed_package", "editable": true,
		"instruction": "此目录是当前会话私有工作副本；可用 server_read/server_edit 修改并自行验证。不会修改已安装工具、配置或凭据。相同工具摘要会复用已有副本，新版本会使用新的摘要目录。",
	}
	if state == "created" {
		result["files"], result["bytes"] = files, bytes
	}
	b, err := json.Marshal(result)
	return string(b), err
}
