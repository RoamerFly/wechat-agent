package station

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	appcatalog "wechat-bot/connected-apps"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

const localAppPersonasFile = "app-personas.json"

type localAppPersona struct {
	Revision uint64 `json:"revision"`
	Persona  string `json:"persona"`
}

type localAppPersonaRecord struct {
	Version int                        `json:"version"`
	Apps    map[string]localAppPersona `json:"apps"`
}

type localAppPersonaStore struct{ dir string }

var errAppPersonaRevision = errors.New("应用子角色已更新，请重新读取后保存；当前草稿保留")

func validAppPersona(value localAppPersona) bool {
	return value.Revision != 0 && len(value.Persona) <= 16000 && utf8.RuneCountInString(value.Persona) <= 4000 && utf8.ValidString(value.Persona) && !strings.ContainsRune(value.Persona, 0)
}

func (s localAppPersonaStore) read() (localAppPersonaRecord, error) {
	record := localAppPersonaRecord{Version: 1, Apps: map[string]localAppPersona{}}
	root, err := workspacefs.OpenPath(s.dir)
	if err != nil {
		return record, err
	}
	defer root.Close()
	f, err := workspacefs.OpenRegular(root, localAppPersonasFile)
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return record, errors.New("应用子角色配置不可读，原文件保留")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &record) != nil || record.Version != 1 || record.Apps == nil || len(record.Apps) > 100 {
		return record, errors.New("应用子角色配置损坏，原文件保留")
	}
	for id, value := range record.Apps {
		if !validLocalAppID(id) || !validAppPersona(value) {
			return record, errors.New("应用子角色配置损坏，原文件保留")
		}
	}
	return record, nil
}

func validLocalAppID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (s localAppPersonaStore) View(id string) (localAppPersona, error) {
	if !validLocalAppID(id) {
		return localAppPersona{}, errors.New("应用标识无效")
	}
	record, err := s.read()
	return record.Apps[id], err
}

func (s localAppPersonaStore) Save(id string, input localAppPersona) (localAppPersona, error) {
	if !validLocalAppID(id) || input.Revision == ^uint64(0) {
		return localAppPersona{}, errors.New("应用子角色版本或标识无效")
	}
	next := localAppPersona{Revision: input.Revision + 1, Persona: strings.TrimSpace(input.Persona)}
	if !validAppPersona(next) {
		return localAppPersona{}, errors.New("应用子角色最多 4000 字，不能包含无效字符")
	}
	release, err := hostfile.Lock(filepath.Join(s.dir, localAppPersonasFile+".lock"))
	if err != nil {
		return localAppPersona{}, err
	}
	defer release()
	record, err := s.read()
	if err != nil {
		return localAppPersona{}, err
	}
	if record.Apps[id].Revision != input.Revision {
		return localAppPersona{}, errAppPersonaRevision
	}
	if len(record.Apps) >= 100 && record.Apps[id].Revision == 0 {
		return localAppPersona{}, errors.New("应用子角色记录已满")
	}
	record.Apps[id] = next
	raw, err := json.Marshal(record)
	if err != nil {
		return localAppPersona{}, err
	}
	if len(raw) > 1<<20 {
		return localAppPersona{}, errors.New("应用子角色配置容量已满，请精简后再保存")
	}
	if err = hostfile.WritePrivate(filepath.Join(s.dir, localAppPersonasFile), raw); err != nil {
		return localAppPersona{}, err
	}
	return next, nil
}

func (h *localHomeService) appPersonaRoutes(s *websession.Store, mux *http.ServeMux, registry apphost.Registry) {
	handle := s.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id := r.PathValue("app")
		_, registered := registry.Lookup(id)
		known := registered
		for _, candidate := range appcatalog.IDs() {
			known = known || candidate == id
		}
		if !known {
			websession.Fail(w, 404, "未找到此应用")
			return
		}
		if _, err := h.applicationOwner(); err != nil {
			websession.Fail(w, 409, "请先准备本机主人身份")
			return
		}
		store := localAppPersonaStore{dir: h.dataDir}
		var value localAppPersona
		var err error
		if r.Method == http.MethodPut {
			r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
			var input localAppPersona
			if !websession.Decode(w, r, &input) {
				return
			}
			value, err = store.Save(id, input)
		} else {
			value, err = store.View(id)
		}
		if errors.Is(err, errAppPersonaRevision) {
			websession.Fail(w, 409, err.Error())
			return
		}
		if err != nil {
			websession.Fail(w, 400, err.Error())
			return
		}
		websession.Respond(w, 200, value)
	})
	mux.HandleFunc("GET /api/local/apps/{app}/persona", handle)
	mux.HandleFunc("PUT /api/local/apps/{app}/persona", handle)
}

func applicationPersonaPolicy(main string, app localAppPersona) string {
	text := "\n主角色设定：\n" + main
	if app.Persona != "" {
		text += "\n当前应用的子角色补充（只影响本应用的表达、喜好与相处方式，不改变身份、工具权限或资料可见范围）：\n" + app.Persona
	}
	return text
}

// Older station imports put the WeChat character into the global role. Move
// only an unchanged, source-verified import. Owner edits always take precedence.
// Write the app copy first so interruption cannot lose the existing character.
func migrateLegacyAppPersona(dir string) error {
	release, err := hostfile.Lock(filepath.Join(dir, ".legacy-app-persona-migration.lock"))
	if err != nil {
		return err
	}
	defer release()
	root, err := workspacefs.OpenPath(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	raw, err := readLegacySettingsFile(root, legacySettingsReceiptFile, false, legacySettingsTargetLimit)
	if err != nil || len(raw) == 0 {
		return nil
	}
	var receipt legacySettingsReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.Version != 1 {
		return nil
	}
	models := newLocalModelStore(dir)
	main, err := models.View()
	if err != nil {
		return err
	}
	if main.Revision != 1 {
		return nil
	}
	source, err := workspacefs.OpenPath(receipt.Source)
	if err != nil {
		return nil
	}
	defer source.Close()
	config, err := readLegacySettingsFile(source, legacySettingsConfigFile, true, legacySettingsSourceLimit)
	if err != nil || legacySettingsHash(config) != receipt.ConfigSHA256 {
		return nil
	}
	defaults, character := defaultLocalModelSettings(), companion.Defaults()
	old := legacySettingsConfig{SystemPrompt: deepseek.DefaultSystemPrompt}
	old.Companion.Persona, old.Companion.Style, old.Companion.Relationship = character.Persona, character.Style, character.Relationship
	if json.Unmarshal(config, &old) != nil {
		return nil
	}
	if old.SystemPrompt == deepseek.LegacySystemPrompt {
		old.SystemPrompt = deepseek.DefaultSystemPrompt
	}
	legacy := mergedLegacyPersona(old)
	if main.Persona != legacy {
		return nil
	}
	store := localAppPersonaStore{dir: dir}
	profile, err := store.View("wechat")
	if err != nil {
		return err
	}
	if profile.Revision == 0 {
		profile, err = store.Save("wechat", localAppPersona{Persona: legacy})
		if err != nil {
			return err
		}
	}
	if profile.Persona != legacy {
		return nil
	}
	main.Persona = defaults.Persona
	_, err = models.Save(LocalModelInput{LocalModelSettings: main})
	return err
}
