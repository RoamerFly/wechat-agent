package station

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/fishidentity"
)

func localWorkspaceToolsFixture(t *testing.T) (*localHomeService, *localWorkspaceAccess, fishidentity.Record, string) {
	t.Helper()
	dataDir := t.TempDir()
	workspace := t.TempDir()
	identity := fishidentity.New(dataDir)
	record, err := identity.InitializeLocal()
	if err != nil {
		t.Fatal(err)
	}
	home := &localHomeService{identity: identity, root: workspace, dataDir: dataDir, chatAvailable: true}
	if err := home.ensureDirectory(record); err != nil {
		t.Fatal(err)
	}
	directory, err := home.directory(record)
	if err != nil {
		t.Fatal(err)
	}
	return home, &localWorkspaceAccess{home: home}, record, directory
}

func localWorkspaceToolCall(t *testing.T, tools interface {
	Call(context.Context, string, json.RawMessage) (string, error)
}, name string, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tools.Call(t.Context(), name, raw)
	if err != nil {
		t.Fatalf("%s rejected: %v", name, err)
	}
	return out
}

func TestLocalWorkspaceFileToolsCreateRestartReadAndEdit(t *testing.T) {
	home, access, record, directory := localWorkspaceToolsFixture(t)
	tools := newLocalFileTools(access)
	path := filepath.Join("Project Tools", "daily-note.py")
	localWorkspaceToolCall(t, tools, "server_write", map[string]any{"path": path, "content": "print('first')\n"})

	var read struct {
		Content string `json:"content"`
		SHA256  string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(localWorkspaceToolCall(t, tools, "server_read", map[string]any{"path": path, "length": 1024})), &read); err != nil || read.Content != "print('first')\n" || len(read.SHA256) != 64 {
		t.Fatalf("created source was not readable: %#v (%v)", read, err)
	}

	// A restart reopens only durable identity and home state; it must not retain
	// a grant object or any in-memory path capability from the prior service.
	restartedHome := &localHomeService{identity: fishidentity.New(home.dataDir), root: home.root, dataDir: home.dataDir, chatAvailable: true}
	restartedTools := newLocalFileTools(&localWorkspaceAccess{home: restartedHome})
	var afterRestart struct {
		Content string `json:"content"`
		SHA256  string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(localWorkspaceToolCall(t, restartedTools, "server_read", map[string]any{"path": filepath.Join(directory, path), "length": 1024})), &afterRestart); err != nil || afterRestart.Content != "print('first')\n" {
		t.Fatalf("restart lost created source: %#v (%v)", afterRestart, err)
	}
	localWorkspaceToolCall(t, restartedTools, "server_edit", map[string]any{"path": path, "expected_sha256": afterRestart.SHA256, "old_text": "first", "new_text": "second"})
	if body, err := os.ReadFile(filepath.Join(directory, path)); err != nil || string(body) != "print('second')\n" {
		t.Fatalf("restart edit did not persist: %q (%v)", body, err)
	}

	status := localWorkspaceToolCall(t, restartedTools, "server_status", map[string]any{})
	if !strings.Contains(status, `"execution_tool":"server_windows_process"`) || !strings.Contains(status, `"mode":"files"`) || strings.Contains(status, "gpu_devices_available") {
		t.Fatalf("file-only status is inaccurate: %s", status)
	}
	if record.LocalOwner == nil {
		t.Fatal("fixture did not initialize local owner")
	}
}

func TestLocalWorkspaceFileToolsPauseAndPathBoundaries(t *testing.T) {
	home, access, record, directory := localWorkspaceToolsFixture(t)
	tools := newLocalFileTools(access)
	secret := filepath.Join(home.dataDir, "host-secret.txt")
	if err := os.WriteFile(secret, []byte("host-private-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"path": secret, "length": 1024},
		{"path": filepath.Join("..", "host-secret.txt"), "length": 1024},
		{"path": filepath.Join(directory, "..", "outside.txt"), "content": "no"},
	} {
		name := "server_read"
		if _, ok := args["content"]; ok {
			name = "server_write"
		}
		raw, _ := json.Marshal(args)
		if out, err := tools.Call(t.Context(), name, raw); err == nil {
			t.Fatalf("%s unexpectedly admitted outside path: %s", name, out)
		}
	}
	if record.LocalOwner == nil {
		t.Fatal("fixture did not initialize local owner")
	}
	if _, err := home.identity.SetHomeAccess(record.LocalOwner.Revision, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"path": filepath.Join("Project Tools", "after-pause.py"), "content": "blocked"})
	if out, err := tools.Call(t.Context(), "server_write", raw); err == nil {
		t.Fatalf("paused home unexpectedly wrote: %s", out)
	}
	if _, err := os.Stat(filepath.Join(directory, "Project Tools", "after-pause.py")); !os.IsNotExist(err) {
		t.Fatalf("paused home created a file: %v", err)
	}
}

func TestLocalWorkspaceFileToolsRejectUnknownFields(t *testing.T) {
	_, access, _, _ := localWorkspaceToolsFixture(t)
	tools := newLocalFileTools(access)
	if out, err := tools.Call(t.Context(), "server_read", json.RawMessage(`{"path":"Project Tools/x.py","command":"whoami"}`)); err == nil {
		t.Fatalf("unexpected command field accepted: %s", out)
	}
}
