package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"wechat-bot/internal/companion"
)

func TestLocalChatReadsOnlyRequestedApplicationPersonaOnDemand(t *testing.T) {
	f := syntheticObservationFixture(t, newSyntheticObservationApp())
	if _, err := (localAppPersonaStore{dir: f.dir}).Save("offline-library", localAppPersona{Persona: "合成应用专属：只在书库聊海洋文学"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(_ context.Context, system string, messages []map[string]string) (string, error) {
			calls++
			if strings.Contains(system, "只在书库聊海洋文学") {
				t.Error("app role leaked into the main persona")
			}
			if calls == 1 {
				return `{"action":"tool","tool":"app_observe","arguments":{"action":"persona","app_id":"offline-library"}}`, nil
			}
			last := messages[len(messages)-1]["content"]
			if !strings.Contains(last, "只在书库聊海洋文学") || !strings.Contains(last, "application_only") {
				t.Error("model did not receive requested app role")
			}
			return `{"action":"reply","messages":["已读到书库的专属喜好。"]}`, nil
		}), nil
	}
	if code, row := localChatSend(f, "read-app-persona", "查看书库应用的子角色"); code != 200 || row["status"] != "completed" || calls != 2 {
		t.Fatal(code, row, calls)
	}
}

func TestApplicationPersonasAreScopedVersionedAndReadAtCallTime(t *testing.T) {
	dir := t.TempDir()
	store := localAppPersonaStore{dir: dir}
	wechat, err := store.Save("wechat", localAppPersona{Persona: "合成微信喜好"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save("douyin", localAppPersona{Persona: "合成抖音喜好"}); err != nil {
		t.Fatal(err)
	}
	if _, err = (localAppPersonaStore{dir: dir}).Save("wechat", localAppPersona{Persona: "过时覆盖"}); !errors.Is(err, errAppPersonaRevision) {
		t.Fatal(err)
	}
	models := newLocalModelStore(dir)
	settings := defaultLocalModelSettings()
	settings.Persona = "合成主角色"
	if _, err = models.Save(LocalModelInput{LocalModelSettings: settings, APIKey: "synthetic-persona-key-not-real"}); err != nil {
		t.Fatal(err)
	}
	model := localApplicationModel{store: models, app: "wechat"}
	_, policy, err := model.prepare("工具协议原文")
	if err != nil || !strings.Contains(policy, "合成主角色") || !strings.Contains(policy, "合成微信喜好") || strings.Contains(policy, "合成抖音喜好") || strings.Contains(policy, "synthetic-persona-key") {
		t.Fatal("application policy lost scope", err)
	}
	if _, err = store.Save("wechat", localAppPersona{Revision: wechat.Revision, Persona: "新微信喜好"}); err != nil {
		t.Fatal(err)
	}
	_, policy, err = model.prepare("工具协议原文")
	if err != nil || strings.Contains(policy, "合成微信喜好") || !strings.Contains(policy, "新微信喜好") {
		t.Fatal("running adapter retained stale persona", err)
	}
	main, err := models.View()
	if err != nil || main.Persona != "合成主角色" {
		t.Fatal("app setting changed main role", err)
	}
	if _, err = store.Save("../escape", localAppPersona{}); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err = store.Save("wechat", localAppPersona{Revision: 2, Persona: strings.Repeat("字", 4001)}); err == nil {
		t.Fatal("oversized role accepted")
	}
	if err = os.WriteFile(filepath.Join(dir, localAppPersonasFile), []byte(`{"version":1,"apps":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save("wechat", localAppPersona{}); err == nil {
		t.Fatal("corrupt profile overwritten")
	}
}

func TestApplicationPersonaHTTPRequiresOwnerAndCSRF(t *testing.T) {
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "data"), filepath.Join(base, "work"))
	path := "/api/local/apps/wechat/persona"
	if code, _ := f.request("GET", path, nil, false); code != 401 {
		t.Fatal(code)
	}
	f.login()
	if code, _ := f.request("GET", path, nil, false); code != 409 {
		t.Fatal(code)
	}
	if code, _ := f.request("POST", "/api/local/home", map[string]any{"action": "initialize"}, true); code != 200 {
		t.Fatal(code)
	}
	if code, _ := f.request("PUT", path, map[string]any{"revision": 0, "persona": "合成微信角色"}, false); code != 403 {
		t.Fatal(code)
	}
	if code, _ := f.request("PUT", path, map[string]any{"revision": 0, "persona": "合成微信角色"}, true); code != 200 {
		t.Fatal(code)
	}
	if code, _ := f.request("PUT", path, map[string]any{"revision": 0, "persona": "过期保存"}, true); code != 409 {
		t.Fatal(code)
	}
	if code, row := f.request("GET", "/api/local/apps/douyin/persona", nil, false); code != 200 || row["persona"] != "" {
		t.Fatal(code, row)
	}
	if code, _ := f.request("GET", "/api/local/apps/unknown-fixture/persona", nil, false); code != 404 {
		t.Fatal(code)
	}
}

func TestLegacyGlobalRoleMovesToWeChatOnlyWhenUnchanged(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "owner-edited"}[edited], func(t *testing.T) {
			base := t.TempDir()
			source, target := filepath.Join(base, "old"), filepath.Join(base, "new")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			writeLegacySettingsFixture(t, source, "synthetic-role-migration-key", false)
			loaded, err := loadLegacySettings(t.Context(), source, target)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			record := loaded.record
			record.Persona = loaded.appPersona
			if edited {
				record.Revision = 2
				record.Persona = "主人的新设定"
			}
			model, _ := json.Marshal(record)
			receipt, _ := json.Marshal(legacySettingsReceipt{Version: 1, Source: loaded.plan.Source, ConfigSHA256: loaded.configHash})
			for name, data := range map[string][]byte{"local-model.json": model, legacySettingsReceiptFile: receipt} {
				if err = os.WriteFile(filepath.Join(target, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err = migrateLegacyAppPersona(target); err != nil {
					t.Fatal(err)
				}
			}
			main, err := newLocalModelStore(target).View()
			if err != nil {
				t.Fatal(err)
			}
			app, err := (localAppPersonaStore{dir: target}).View("wechat")
			if err != nil {
				t.Fatal(err)
			}
			if edited {
				if main.Persona != "主人的新设定" || app.Revision != 0 {
					t.Fatal("owner edit changed")
				}
			} else if main.Persona != defaultLocalModelSettings().Persona || app.Persona != loaded.appPersona || app.Revision != 1 {
				t.Fatal("legacy migration lost role or was repeated")
			}
		})
	}
}
