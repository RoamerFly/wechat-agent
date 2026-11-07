package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"wechat-bot/internal/fishidentity"
)

func TestLegacySettingsPreviewAndImportAreBoundedAndIdempotent(t *testing.T) {
	base := t.TempDir()
	source, target := filepath.Join(base, "legacy-admin"), filepath.Join(base, "station-data")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	writeLegacySettingsFixture(t, source, "synthetic-legacy-key-000000000000000", true)
	before := directoryNames(t, source)
	plan, err := PreviewLegacySettings(context.Background(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Digest == "" || !plan.HasIdentity || !strings.HasPrefix(plan.FishID, "fish-") || plan.Model.KeyConfigured != true || !plan.Model.PersonaSet {
		t.Fatalf("unsafe or incomplete preview: %+v", plan)
	}
	if strings.Contains(fmtPlan(plan), "synthetic-legacy-key") || strings.Contains(fmtPlan(plan), "old system prompt") {
		t.Fatalf("preview leaked private setting: %+v", plan)
	}
	if after := directoryNames(t, source); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("preview changed source: before=%v after=%v", before, after)
	}

	imported, err := ImportLegacySettings(context.Background(), source, target, plan.Digest)
	if err != nil || imported.Digest != plan.Digest {
		t.Fatalf("import=%+v err=%v", imported, err)
	}
	data, err := os.ReadFile(filepath.Join(target, "local-model.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record localModelRecord
	if err = json.Unmarshal(data, &record); err != nil || record.Revision != 1 || record.APIKey != "synthetic-legacy-key-000000000000000" {
		t.Fatalf("model record=%+v err=%v", record, err)
	}
	for _, want := range []string{"legacy companion persona", "旧系统提示：old system prompt", "相处风格：old style", "关系设定：old relationship"} {
		profile,err:=(localAppPersonaStore{dir:target}).View("wechat")
		if err!=nil || !strings.Contains(profile.Persona, want) || strings.Contains(record.Persona,want) {
			t.Fatalf("legacy character was not isolated to WeChat: %v",err)
		}
	}
	if _, err = ImportLegacySettings(context.Background(), source, target, plan.Digest); err != nil {
		t.Fatalf("idempotent import: %v", err)
	}
}

func TestLegacySettingsImportRejectsChangedSecretAndExistingTarget(t *testing.T) {
	base := t.TempDir()
	source, target := filepath.Join(base, "legacy-admin"), filepath.Join(base, "station-data")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	const oldKey = "synthetic-legacy-key-111111111111111"
	writeLegacySettingsFixture(t, source, oldKey, false)
	plan, err := PreviewLegacySettings(context.Background(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	writeLegacySettingsFixture(t, source, "synthetic-legacy-key-222222222222222", false)
	if _, err = ImportLegacySettings(context.Background(), source, target, plan.Digest); !errors.Is(err, errLegacySettingsChanged) || strings.Contains(err.Error(), oldKey) {
		t.Fatalf("changed digest err=%v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("stale apply created target: %v", statErr)
	}
	if err = os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	plan, err = PreviewLegacySettings(context.Background(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ImportLegacySettings(context.Background(), source, target, plan.Digest); !errors.Is(err, errLegacySettingsConflict) {
		t.Fatalf("existing target err=%v", err)
	}
}

func TestLegacySettingsImportConcurrentRetryAndInvalidInputs(t *testing.T) {
	base := t.TempDir()
	source, target := filepath.Join(base, "legacy-admin"), filepath.Join(base, "station-data")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	writeLegacySettingsFixture(t, source, "synthetic-legacy-key-333333333333333", true)
	plan, err := PreviewLegacySettings(context.Background(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	start, done := make(chan struct{}), make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := ImportLegacySettings(context.Background(), source, target, plan.Digest)
			done <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent import: %v", err)
		}
	}

	invalid := filepath.Join(base, "invalid-admin")
	if err := os.Mkdir(invalid, 0700); err != nil {
		t.Fatal(err)
	}
	writeLegacySettingsFixture(t, invalid, "synthetic-legacy-key-444444444444444", false)
	if err := os.WriteFile(filepath.Join(invalid, legacySettingsIdentityFile), []byte(`{"version":3,"fish_id":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewLegacySettings(context.Background(), invalid, filepath.Join(base, "invalid-target")); err == nil || strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("invalid identity err=%v", err)
	}
	if err := os.WriteFile(filepath.Join(invalid, legacySettingsConfigFile), []byte(`{"provider":"bad"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewLegacySettings(context.Background(), invalid, filepath.Join(base, "invalid-target-2")); err == nil {
		t.Fatal("invalid model was accepted")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ImportLegacySettings(cancelled, source, filepath.Join(base, "cancelled"), plan.Digest); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled import err=%v", err)
	}
}

func TestLegacySettingsPreviewAcceptsLargeLegacyConfigWithoutCopyingIt(t *testing.T) {
	base, source := t.TempDir(), filepath.Join(t.TempDir(), "legacy-admin")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	writeLegacySettingsFixture(t, source, "synthetic-legacy-key-555555555555555", false)
	path := filepath.Join(source, legacySettingsConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err = json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	groups := make([]map[string]string, 80)
	for index := range groups {
		groups[index] = map[string]string{
			"id":   "synthetic-" + strconv.Itoa(index) + "@chatroom",
			"mode": "mention", "persona": strings.Repeat("x", 2<<10),
		}
	}
	object["group_rules"] = groups
	data, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = PreviewLegacySettings(context.Background(), source, filepath.Join(base, "station-data")); err != nil {
		t.Fatalf("large legacy config preview: %v", err)
	}
}

func TestLegacySettingsImportPreservesOldDefaultsAndDeduplicatesPersona(t *testing.T) {
	base := t.TempDir()
	source, target := filepath.Join(base, "legacy-admin"), filepath.Join(base, "station")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"api_key":"synthetic-legacy-default-key","companion":{"persona":"synthetic character","style":"synthetic shared preference","relationship":"synthetic shared preference"}}`)
	if err := os.WriteFile(filepath.Join(source, legacySettingsConfigFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PreviewLegacySettings(t.Context(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Model.Model != "deepseek-flash" || plan.Model.Provider != "deepseek" || plan.Model.MaxTokens != 4096 || plan.Model.ThinkingEnabled || plan.Model.ReasoningEffort != "low" {
		t.Fatal("old effective defaults changed", plan.Model)
	}
	if _, err = ImportLegacySettings(t.Context(), source, target, plan.Digest); err != nil {
		t.Fatal(err)
	}
	settings, err := (localAppPersonaStore{dir:target}).View("wechat")
	if err != nil || strings.Count(settings.Persona, "synthetic shared preference") != 1 {
		t.Fatal("repeated character fields were not merged", err)
	}
}

func writeLegacySettingsFixture(t *testing.T, dir, key string, identity bool) {
	t.Helper()
	config := map[string]any{
		"provider": "deepseek", "base_url": "https://api.deepseek.com", "model": "deepseek-flash", "system_prompt": "old system prompt", "max_tokens": 4096,
		"thinking_enabled": true, "reasoning_effort": "low", "api_key": key,
		"companion": map[string]any{"persona": "legacy companion persona", "style": "old style", "relationship": "old relationship"},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, legacySettingsConfigFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	if identity {
		if _, err = fishidentity.New(dir).InitializeLocal(); err != nil {
			t.Fatal(err)
		}
	}
}

func directoryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	items := make([]string, 0, len(entries))
	for _, entry := range entries {
		items = append(items, entry.Name())
	}
	return items
}

func fmtPlan(plan LegacySettingsPlan) string {
	data, _ := json.Marshal(plan)
	return string(data)
}
