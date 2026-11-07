package station

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	appcatalog "wechat-bot/connected-apps"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/packagecheckout"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/workspacefs"

	"wechat-bot/internal/websession"
)

func (h *localHomeService) configureAppCatalog() {
	if executable, err := os.Executable(); err == nil {
		h.appCatalog = filepath.Join(filepath.Dir(executable), "connected-apps")
	}
}

func (h *localHomeService) appInstallRoutes(s *websession.Store, mux *http.ServeMux) {
	mux.HandleFunc("GET /api/local/apps/catalog", s.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ids := appcatalog.IDs()
		rows := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			contract, _, sourceErr := h.verifyAppSource(id)
			if contract.Package.ID == "" {
				websession.Fail(w, http.StatusServiceUnavailable, "应用发行目录不可读")
				return
			}
			installState, installedVersion := h.appInstallState(contract)
			if id != "wechat" && sourceErr != nil && installState != "installed" && installState != "conflict" {
				continue
			}
			row := map[string]any{
				"id": id, "name": contract.Package.Name, "version": contract.Package.Version,
				"digest": contract.Digest, "source_available": sourceErr == nil, "state": "source_only",
				"install_state": installState,
			}
			if installedVersion != "" {
				row["installed_version"] = installedVersion
			}
			rows = append(rows, row)
		}
		websession.Respond(w, http.StatusOK, map[string]any{"applications": rows})
	}))
	mux.HandleFunc("POST /api/local/apps/{app}/install", s.Protect(h.installAppSource))
}

// appInstallState reports the checked-out source separately from the bundled
// source.  A normal checkout is deliberately editable: only its descriptor is
// immutable.  In particular, workspace files may change without making the
// application look untrusted or inviting an automatic overwrite.
func (h *localHomeService) appInstallState(contract appcatalog.Contract) (state, version string) {
	record, err := h.identity.Read()
	if err != nil || record.LocalOwner == nil || !fishidentity.ValidFishID(record.FishID) {
		return "unavailable", ""
	}
	homePath, err := h.directory(record)
	if err != nil {
		return "unavailable", ""
	}
	// A paused home remains observable by its authenticated owner.  The POST
	// path still requires HomeAccess, but hiding the existing checkout here
	// would make a pause appear as data loss.
	home, err := workspacefs.OpenPath(homePath)
	if err != nil {
		return "unavailable", ""
	}
	defer home.Close()
	parent, err := workspacefs.OpenExistingDirectory(home, "Connected Apps")
	if errors.Is(err, os.ErrNotExist) {
		return "not_installed", ""
	}
	if err != nil {
		return "conflict", ""
	}
	defer parent.Close()
	installed, err := workspacefs.OpenExistingDirectory(parent, contract.Package.ID)
	if errors.Is(err, os.ErrNotExist) {
		return "not_installed", ""
	}
	if err != nil {
		return "conflict", ""
	}
	defer installed.Close()
	digest, descriptor, err := installedAppDescriptor(installed)
	if err != nil || descriptor.ID != contract.Package.ID {
		return "conflict", ""
	}
	version = descriptor.Version
	if digest != contract.Digest {
		return "conflict", version
	}
	return "installed", version
}

// installedAppDescriptor performs the only inspection needed for an editable
// checkout.  os.Root plus OpenRegular prevents a foreign link from being read;
// it intentionally does not walk workspace files owned by the companion.
func installedAppDescriptor(root *os.Root) (string, pluginhost.Package, error) {
	var descriptor pluginhost.Package
	info, err := root.Lstat("app.json")
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", descriptor, errors.New("应用安装声明不可读")
	}
	file, err := workspacefs.OpenRegular(root, "app.json")
	if err != nil {
		return "", descriptor, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return "", descriptor, errors.New("应用安装声明不可读")
	}
	if err := json.Unmarshal(data, &descriptor); err != nil || descriptor.ID == "" || descriptor.Version == "" {
		return "", descriptor, errors.New("应用安装声明无效")
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), descriptor, nil
}

func appDescriptorDigest(root *os.Root) (string, error) {
	file, err := workspacefs.OpenRegular(root, "app.json")
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return "", errors.New("应用发行声明不可读")
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func (h *localHomeService) verifyAppSource(id string) (appcatalog.Contract, string, error) {
	contract, err := appcatalog.Lookup(id)
	if err != nil {
		return contract, "", err
	}
	directory := filepath.Join(h.appCatalog, id)
	if h.appCatalog == "" {
		return contract, "", errors.New("本机未提供应用发行源")
	}
	if _, err := pluginhost.VerifyDescriptor(directory, "app.json"); err != nil {
		return contract, "", errors.New("应用发行源未通过完整性校验")
	}
	root, err := workspacefs.OpenPath(directory)
	if err != nil {
		return contract, "", err
	}
	defer root.Close()
	digest, err := appDescriptorDigest(root)
	if err != nil || digest != contract.Digest {
		return contract, "", errors.New("应用发行源与当前主程序版本不匹配")
	}
	return contract, directory, nil
}

// Installation prepares editable application source only. Account connection,
// credentials and runtime permissions stay in their existing host registries.
func (h *localHomeService) installAppSource(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Digest   string `json:"digest"`
		Revision uint64 `json:"revision"`
	}
	if !websession.Decode(w, r, &in) {
		return
	}
	h.appInstallMu.Lock()
	defer h.appInstallMu.Unlock()
	record, err := h.identity.Read()
	if err != nil || record.LocalOwner == nil || !record.LocalOwner.HomeAccess || record.LocalOwner.Revision != in.Revision {
		websession.Fail(w, http.StatusConflict, fishidentity.ErrHomeRevisionConflict.Error())
		return
	}
	id := r.PathValue("app")
	contract, sourcePath, err := h.verifyAppSource(id)
	if err != nil || contract.Digest != in.Digest {
		websession.Fail(w, http.StatusConflict, "请刷新应用发行目录后重试")
		return
	}
	homePath, err := h.directory(record)
	if err != nil {
		websession.Fail(w, http.StatusConflict, err.Error())
		return
	}
	home, err := workspacefs.EnsurePath(homePath)
	if err != nil {
		websession.Fail(w, http.StatusConflict, err.Error())
		return
	}
	defer home.Close()
	parent, err := workspacefs.EnsureDirectory(home, "Connected Apps")
	if err != nil {
		websession.Fail(w, http.StatusConflict, err.Error())
		return
	}
	defer parent.Close()
	result := func(state string) {
		websession.Respond(w, http.StatusOK, map[string]any{"id": id, "digest": contract.Digest, "result": state,
			"state": "source_only", "connected": false, "directory": filepath.Join(homePath, "Connected Apps", id)})
	}
	if exists, err := packagecheckout.ExistingDirectory(parent, id); err != nil {
		websession.Fail(w, http.StatusConflict, err.Error())
		return
	} else if exists {
		existing, err := parent.OpenRoot(id)
		if err != nil {
			websession.Fail(w, http.StatusConflict, err.Error())
			return
		}
		digest, err := appDescriptorDigest(existing)
		_ = existing.Close()
		if err != nil || digest != contract.Digest {
			websession.Fail(w, http.StatusConflict, "应用目录已存在不同版本，请先保留并整理已有修改")
			return
		}
		result("existing")
		return
	}
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		websession.Fail(w, http.StatusConflict, err.Error())
		return
	}
	defer source.Close()
	stageName, stage, err := packagecheckout.NewStage(parent)
	if err != nil {
		websession.Fail(w, http.StatusConflict, err.Error())
		return
	}
	defer func() {
		_ = stage.Close()
		if stageName != "" {
			_ = parent.RemoveAll(stageName)
		}
	}()
	total := int64(0)
	err = packagecheckout.CopyCheckedFile(r.Context(), source, stage, "app.json", contract.Digest, &total)
	names := make([]string, 0, len(contract.Package.Files))
	for name := range contract.Package.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err != nil {
			break
		}
		err = packagecheckout.CopyCheckedFile(r.Context(), source, stage, name, contract.Package.Files[name], &total)
	}
	if err == nil {
		err = stage.Close()
	}
	if err != nil {
		websession.Fail(w, http.StatusConflict, "接入版本安装未完成，请重新检查应用目录")
		return
	}
	// Keep revocation and publishing in the same identity critical section.
	err = h.identity.GuardHome(record.FishID, record.LocalOwner.ID, record.LocalOwner.Revision, func(fishidentity.Record) error {
		return parent.Rename(stageName, id)
	})
	if errors.Is(err, fishidentity.ErrHomeRevisionConflict) || errors.Is(err, fishidentity.ErrRegistryBusy) {
		websession.Fail(w, http.StatusConflict, fishidentity.ErrHomeRevisionConflict.Error())
		return
	}
	if err != nil {
		websession.Fail(w, http.StatusConflict, "应用目录发布失败，请检查现有目录后重试")
		return
	}
	stageName = ""
	result("created")
}
