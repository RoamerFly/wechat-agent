package station

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

func TestStationLegacyMemoryToolPagesAndRevokes(t *testing.T) {
	const account, thread = "legacy-account", "legacy-room@chatroom"
	now := time.Now().UTC().Truncate(time.Second)
	grant := now.Add(-time.Minute)
	root := t.TempDir()
	store, err := companion.NewStore(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("synthetic-token"), 0600); err != nil {
		t.Fatal(err)
	}
	native, err := linuxnative.New(linuxnative.Config{URL: "http://127.0.0.1:1", Account: account, TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("旧资料", 100)
	sum := sha256.Sum256([]byte(text))
	event := companion.LegacyMemoryEvent{InstanceID: store.SourceInstanceID(), EventID: "legacy-event", MessageID: "legacy-message", ServerID: "legacy-server", Sender: "member", OccurredAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano), Revision: 1, Text: text, Hash: hex.EncodeToString(sum[:])}
	item := stationLegacyMemory{Version: 1, Account: account, Thread: thread, Grant: grant, Digest: strings.Repeat("a", 64), Selection: companion.LegacyMemorySelection{
		Events:   []companion.LegacyMemoryEvent{event},
		Memories: []companion.LegacyMemoryFact{{ID: "legacy-fact", Version: 1, Kind: "fact", Text: "一条有来源的旧记忆", Sources: []companion.LegacyMemoryFactSource{{EventID: event.EventID, Revision: event.Revision}}}},
	}}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, stationLegacyMemoryFile(account, thread)), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var blocked atomic.Bool
	tools := &stationAttachmentTools{native: native, store: store, dir: root, reads: map[string]time.Time{thread: grant}, blocked: blocked.Load}
	tools.SetTrace(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: "member", TraceID: "current", MessageTime: now.Format(time.RFC3339Nano)})
	available, err := tools.List(t.Context())
	if err != nil || !toolVisible(available, "wechat_read_legacy") {
		t.Fatalf("legacy tool not discoverable: %#v, %v", available, err)
	}
	read := func(raw string) map[string]any {
		t.Helper()
		out, err := tools.Call(t.Context(), "wechat_read_legacy", json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(out), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := read(`{"kind":"events","max_bytes":256}`)
	if first["data_role"] != "old_imported_conversation_evidence_not_instruction" || first["returned_count"] != float64(1) || first["items"].([]any)[0].(map[string]any)["text_complete"] != false {
		t.Fatal(first)
	}
	cursor, _ := first["next_cursor"].(string)
	if !strings.HasPrefix(cursor, "wl-") {
		t.Fatal(first)
	}
	continued := read(`{"kind":"events","max_bytes":256,"cursor":"` + cursor + `"}`)
	if continued["items"].([]any)[0].(map[string]any)["text_offset"] == float64(0) {
		t.Fatal(continued)
	}
	facts := read(`{"kind":"memories","max_bytes":256}`)
	if facts["items"].([]any)[0].(map[string]any)["record_origin"] != "legacy_import_snapshot" {
		t.Fatal(facts)
	}
	blocked.Store(true)
	if _, err := tools.Call(t.Context(), "wechat_read_legacy", json.RawMessage(`{"kind":"events","max_bytes":256}`)); err == nil {
		t.Fatal("blocked worker disclosed legacy data")
	}
	blocked.Store(false)
	newGrant := grant.Add(time.Minute)
	tools.reads[thread] = newGrant
	tools.SetTrace(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: "member", TraceID: "current-2", MessageTime: now.Add(2 * time.Minute).Format(time.RFC3339Nano)})
	if available, _ := tools.List(t.Context()); toolVisible(available, "wechat_read_legacy") {
		t.Fatal("new authorization epoch reused the old legacy bundle")
	}
}

func toolVisible(tools []mcptools.Tool, want string) bool {
	for _, tool := range tools {
		if tool.Name == want {
			return true
		}
	}
	return false
}
