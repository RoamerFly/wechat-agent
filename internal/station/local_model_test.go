package station

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalModelStoreConcurrentInstancesKeepCAS(t *testing.T) {
	dir := t.TempDir()
	first, second := newLocalModelStore(dir), newLocalModelStore(dir)
	settings, err := first.View()
	if err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for _, store := range []*localModelStore{first, second} {
		go func(store *localModelStore) {
			<-start
			_, err := store.Save(LocalModelInput{LocalModelSettings: settings, APIKey: "synthetic-concurrent-model-key"})
			results <- err
		}(store)
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, errLocalModelRevision) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent CAS: success=%d conflict=%d", success, conflict)
	}
	view, err := first.View()
	if err != nil || view.Revision != 1 || !view.KeyConfigured {
		t.Fatalf("persisted config: %#v %v", view, err)
	}
}

func TestLocalModelStoreDefaultsAndPrivateKey(t *testing.T) {
	store := newLocalModelStore(t.TempDir())
	defaults, err := store.View()
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Revision != 0 || defaults.Provider != "deepseek" || defaults.Model != "deepseek-flash" || defaults.BaseURL != "https://api.deepseek.com" || defaults.MaxTokens != 4096 || !defaults.ThinkingEnabled || defaults.ReasoningEffort != "low" || defaults.KeyConfigured || defaults.Persona == "" {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}
	first, err := store.Save(LocalModelInput{LocalModelSettings: defaults, APIKey: "local-test-key-000000000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || !first.KeyConfigured {
		t.Fatalf("unexpected saved view: %+v", first)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(store.path), "local-model.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "local-test-key-000000000000000000") {
		t.Fatal("private record did not retain synthetic key")
	}
	view, err := store.View()
	if err != nil || view.KeyConfigured != true || strings.Contains(view.Persona, "local-test-key") {
		t.Fatalf("view leaked or failed: %+v err=%v", view, err)
	}
	view.Model = "another-model"
	second, err := store.Save(LocalModelInput{LocalModelSettings: view})
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != 2 || !second.KeyConfigured || second.Model != "another-model" {
		t.Fatalf("key was not retained: %+v", second)
	}
	if _, err := store.Client(); err != nil {
		t.Fatalf("configured client should construct without a request: %v", err)
	}
	_, snapshot, err := store.ClientSnapshot()
	if err != nil || snapshot.Revision != second.Revision || snapshot.Model != second.Model || !snapshot.KeyConfigured {
		t.Fatalf("client snapshot=%+v err=%v", snapshot, err)
	}
}

func TestLocalModelStoreClearAndCAS(t *testing.T) {
	store := newLocalModelStore(t.TempDir())
	view, err := store.View()
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Save(LocalModelInput{LocalModelSettings: view, APIKey: "local-test-key-000000000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(LocalModelInput{LocalModelSettings: first, APIKey: "another-test-key-000000000000000", ClearKey: true}); err == nil {
		t.Fatal("key and clear key were accepted together")
	}
	stale := first
	cleared, err := store.Save(LocalModelInput{LocalModelSettings: first, ClearKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.KeyConfigured || cleared.Revision != 2 {
		t.Fatalf("key not cleared: %+v", cleared)
	}
	if _, err := store.Save(LocalModelInput{LocalModelSettings: stale}); err == nil || !strings.Contains(err.Error(), "已更新") {
		t.Fatalf("stale write error=%v", err)
	}
	if _, err := store.Client(); err == nil || !strings.Contains(err.Error(), "尚未配置") {
		t.Fatalf("missing-key client error=%v", err)
	}
}

func TestLocalModelStoreCorruptFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	store := newLocalModelStore(dir)
	path := filepath.Join(dir, "local-model.json")
	broken := []byte(`{"revision":`)
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	settings := defaultLocalModelSettings()
	if _, err := store.Save(LocalModelInput{LocalModelSettings: settings, APIKey: "local-test-key-000000000000000000"}); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("corrupt file error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(broken) {
		t.Fatalf("broken file was overwritten: %q err=%v", after, err)
	}
}

func TestLocalModelStoreRejectsStoredZeroAndRevisionOverflow(t *testing.T) {
	dir := t.TempDir()
	store := newLocalModelStore(dir)
	if err := os.WriteFile(store.path, []byte(`{"revision":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.View(); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("stored zero revision error=%v", err)
	}
	record := localModelRecord{LocalModelSettings: defaultLocalModelSettings(), APIKey: "local-test-key-000000000000000000"}
	record.Revision, record.KeyConfigured = math.MaxUint64, true
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(LocalModelInput{LocalModelSettings: localModelView(record)}); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("revision overflow error=%v", err)
	}
}
