package station

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/fishidentity"
)

func localProcessTestHome(t *testing.T) (*localHomeService, localWorkspaceGrant, string) {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	home := &localHomeService{
		identity: fishidentity.New(filepath.Join(base, "private")),
		root:     filepath.Join(base, "workspace"),
		dataDir:  filepath.Join(base, "private"),
	}
	record, err := home.identity.InitializeLocal()
	if err != nil {
		t.Fatal(err)
	}
	if err = home.ensureDirectory(record); err != nil {
		t.Fatal(err)
	}
	directory, err := home.directory(record)
	if err != nil {
		t.Fatal(err)
	}
	return home, localWorkspaceGrant{FishID: record.FishID, OwnerID: record.LocalOwner.ID, Directory: directory, Revision: record.LocalOwner.Revision}, filepath.Join(base, "private")
}

func TestLocalProcessToolsAdvertiseWithoutWindowsRuntime(t *testing.T) {
	home, grant, dataDir := localProcessTestHome(t)
	tools, err := newLocalProcessTools(&localWorkspaceAccess{home: home}, dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tools.Close() })
	listed, err := tools.List(t.Context())
	if err != nil || len(listed) != 1 || listed[0].Name != "server_windows_process" {
		t.Fatalf("List = %#v, %v", listed, err)
	}
	result, err := tools.Call(t.Context(), "server_windows_process", localProcessInputJSON(t, map[string]any{"action": "start", "path": grant.Directory, "argv": []string{filepath.Join(grant.Directory, "tool.exe")}, "request_key": "missing-runtime"}))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal([]byte(result), &got); err != nil || got["status"] != "not_started" || got["code"] != "windows_runtime_not_configured" {
		t.Fatalf("start without runtime = %s, %v", result, err)
	}
	if entries, err := os.ReadDir(filepath.Join(dataDir, "local-processes")); err != nil || len(entries) != 0 {
		t.Fatalf("missing runtime started a process: %#v, %v", entries, err)
	}
}

func TestLocalProcessToolsKeepOwnerJobsVisibleAfterPauseAndInterruptOnRestart(t *testing.T) {
	home, grant, dataDir := localProcessTestHome(t)
	processDir := filepath.Join(dataDir, "local-processes")
	if err := os.MkdirAll(processDir, 0700); err != nil {
		t.Fatal(err)
	}
	owned := "process-owned"
	writeLocalProcessRecord(t, processDir, map[string]any{
		"id": owned, "scope": localProcessScope(grant.FishID, grant.OwnerID), "request_key": "one", "request_hash": "hash", "state": "running", "started": time.Now().UTC(), "updated": time.Now().UTC(),
	})
	writeLocalProcessRecord(t, processDir, map[string]any{
		"id": "process-other", "scope": localProcessScope("fish-other", grant.OwnerID), "request_key": "other", "request_hash": "hash", "state": "completed", "started": time.Now().UTC(), "updated": time.Now().UTC(),
	})
	tools, err := newLocalProcessTools(&localWorkspaceAccess{home: home}, dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tools.Close() })

	paused, err := home.identity.SetHomeAccess(grant.Revision, false)
	if err != nil || paused.LocalOwner.HomeAccess {
		t.Fatalf("pause = %#v, %v", paused, err)
	}
	result, err := tools.Call(context.Background(), "server_windows_process", json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err = json.Unmarshal([]byte(result), &rows); err != nil || len(rows) != 1 || rows[0]["id"] != owned || rows[0]["state"] != "interrupted" || rows[0]["reason"] != "management_restarted_no_replay" {
		t.Fatalf("paused owner list = %s, %v", result, err)
	}
	result, err = tools.Call(context.Background(), "server_windows_process", json.RawMessage(`{"action":"status","id":"`+owned+`"}`))
	if err != nil || !containsJSON(result, `"id":"`+owned+`"`) {
		t.Fatalf("paused status = %s, %v", result, err)
	}
	if _, err = tools.Call(context.Background(), "server_windows_process", localProcessInputJSON(t, map[string]any{"action": "start", "path": grant.Directory, "argv": []string{filepath.Join(grant.Directory, "tool.exe")}, "request_key": "paused"})); err == nil || !strings.Contains(err.Error(), "授权已暂停") {
		t.Fatalf("paused start = %v, want authorization denial", err)
	}
}

func TestLocalProcessPathsCannotEscapeHome(t *testing.T) {
	home, grant, _ := localProcessTestHome(t)
	outside := filepath.Join(filepath.Dir(filepath.Dir(grant.Directory)), "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := localProcessPath(grant.Directory, outside); err == nil {
		t.Fatal("outside directory admitted")
	}
	if err := localProcessPath(grant.Directory, filepath.Join(grant.Directory, "..")); err == nil {
		t.Fatal("non-canonical parent admitted")
	}
	if _, err := home.identity.SetHomeAccess(grant.Revision, false); err != nil {
		t.Fatal(err)
	}
}

func writeLocalProcessRecord(t *testing.T, dir string, value map[string]any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := value["id"].(string)
	if err = os.WriteFile(filepath.Join(dir, id+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func containsJSON(text, fragment string) bool { return strings.Contains(text, fragment) }

func localProcessInputJSON(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
