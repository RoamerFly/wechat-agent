package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
)

// This is a stopped-dataset audit, not a recovery test.  It makes no network
// call and must neither wake a queue nor expose the identifiers, text, or file
// names deliberately included below.
func TestAuditLegacyWechatQueueWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the stopped legacy WeChat queue audit is Windows-only")
	}
	root := filepath.Join(t.TempDir(), "legacy")
	fixture := writeLegacyWechatQueueAuditFixture(t, root)
	before := digestLegacyQueueAuditTree(t, root)

	audit, err := AuditLegacyWechatQueue(context.Background(), root, fixture.primary)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if after := digestLegacyQueueAuditTree(t, root); after != before {
		t.Fatalf("read-only audit changed legacy data: before=%x after=%x", before, after)
	}
	public, err := json.Marshal(audit)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-message-id", "private-sender", "private-thread", "private old body", "private-file-name.wav", "private-task", "private-operation"} {
		if bytes.Contains(public, []byte(secret)) {
			t.Fatalf("audit exposed private value %q: %s", secret, public)
		}
	}
	var view map[string]any
	if err := json.Unmarshal(public, &view); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"account_count", "worker_delivery", "waiting_wakes", "inbox_pending", "plan_pending", "media_reply_pending",
		"admin_tasks_pending", "admin_operations_open", "admin_delivery", "stop_marker_present", "claim_present",
	} {
		if _, ok := view[key]; !ok {
			t.Fatalf("audit omitted %q: %s", key, public)
		}
	}
	if got := intJSON(t, view, "account_count"); got != 2 {
		t.Fatalf("account count=%d, want 2: %s", got, public)
	}
	if got := intJSON(t, view, "worker_delivery_open"); got != 1 {
		t.Fatalf("open worker deliveries=%d, want 1: %s", got, public)
	}
	if got := intJSON(t, view, "waiting_wakes"); got != 1 {
		t.Fatalf("waiting wakes=%d, want 1: %s", got, public)
	}
	if got := intJSON(t, view, "inbox_pending"); got != 1 {
		t.Fatalf("inbox pending=%d, want 1: %s", got, public)
	}
	if got := intJSON(t, view, "plan_pending"); got != 1 {
		t.Fatalf("plan pending=%d, want 1: %s", got, public)
	}
	if got := intJSON(t, view, "media_reply_pending"); got != 1 {
		t.Fatalf("media reply pending=%d, want 1: %s", got, public)
	}
	if got := intJSON(t, view, "admin_tasks_pending"); got != 1 {
		t.Fatalf("admin pending tasks=%d, want 1: %s", got, public)
	}
	if got := intJSON(t, view, "admin_operations_open"); got != 1 {
		t.Fatalf("open admin operations=%d, want 1: %s", got, public)
	}
	if view["stop_marker_present"] != true || view["claim_present"] != false {
		t.Fatalf("wrong stop/claim indicators: %s", public)
	}
	assertLegacyAuditReceiptCounts(t, view, "worker_delivery", map[string]map[string]int{
		"text":  {"accepted": 1, "submission_unknown": 1, "cancelled": 1},
		"media": {"accepted": 1},
	})
	assertLegacyAuditFlatReceiptCounts(t, view, "admin_delivery", map[string]int{"accepted": 1, "not_submitted": 1})
}

func TestAuditLegacyWechatQueueRejectsScopeMismatch(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the stopped legacy WeChat queue audit is Windows-only")
	}
	root := filepath.Join(t.TempDir(), "legacy")
	fixture := writeLegacyWechatQueueAuditFixture(t, root)
	queue := filepath.Join(root, "bot-state", "messages-v2.json")
	raw := mustReadSnapshotFixture(t, queue)
	mustWriteSnapshotFixture(t, queue, bytes.Replace(raw, []byte(`"account":"one"`), []byte(`"account":"wrong"`), 1))
	before := digestLegacyQueueAuditTree(t, root)
	if _, err := AuditLegacyWechatQueue(context.Background(), root, fixture.primary); err == nil {
		t.Fatal("audit accepted a primary queue with the wrong account")
	}
	if after := digestLegacyQueueAuditTree(t, root); after != before {
		t.Fatalf("rejected audit changed legacy data: before=%x after=%x", before, after)
	}
}

type legacyWechatQueueAuditFixture struct{ primary string }

func writeLegacyWechatQueueAuditFixture(t *testing.T, root string) legacyWechatQueueAuditFixture {
	t.Helper()
	const primary, secondary = "one", "two"
	writeJSON := func(path string, value any) {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		mustWriteSnapshotFixture(t, path, b)
	}
	writeJSON(filepath.Join(root, "admin", "accounts.json"), []wechatapp.LegacyAccountRecord{{ID: secondary, Label: "synthetic secondary"}})
	writeJSON(filepath.Join(root, legacyStopMarkerFile), legacyStopMarker{
		Version: 2, State: "stopped", Epoch: strings.Repeat("a", 32), AccountID: primary, DataRoot: root,
		NativeServerID: "old-dataset", At: time.Now().UTC().Add(-time.Minute),
	})

	primaryQueue := map[string]any{
		"version": 1, "server": "old-dataset", "account": primary,
		"inbox": []any{
			map[string]any{"phase": "received", "event": map[string]any{"data": map[string]any{"message_id": "private-message-id", "sender": "private-sender", "content": "private old body"}}},
			map[string]any{"phase": "completed", "event": map[string]any{"data": map[string]any{"message_id": "completed-message"}}},
		},
		"conversation_wakes": []any{map[string]any{"state": "waiting", "purpose": "private old body"}},
		"utterance_plans":    []any{map[string]any{"state": "waiting", "messages": []string{"private old body"}}},
		"media_replies":      []any{map[string]any{"state": "waiting", "source_id": "private-file-name.wav"}},
		"deliveries": []any{
			map[string]any{"key": "accepted", "id": "native-accepted", "thread": "private-thread", "state": "accepted", "server_id": "101", "text": "private old body", "input": map[string]any{"account": primary, "thread": "private-thread"}},
			map[string]any{"key": "unknown", "id": "", "thread": "private-thread", "state": "prepared", "submitted_text": "private old body", "input": map[string]any{"account": primary, "thread": "private-thread"}},
			map[string]any{"key": "cancelled", "id": "", "thread": "private-thread", "state": "stopped", "cancelled": true, "input": map[string]any{"account": primary, "thread": "private-thread"}},
		},
		"media": []any{map[string]any{
			"key": "voice-accepted", "id": "native-voice", "kind": "voice", "state": "accepted", "input": map[string]any{"account": primary, "thread": "private-thread"},
			"status": map[string]any{"state": "accepted", "thread_id": "private-thread", "kind": "voice", "id": "native-voice", "server_id": "102"},
		}},
	}
	writeJSON(filepath.Join(root, "bot-state", "messages-v2.json"), primaryQueue)
	writeJSON(filepath.Join(root, "bot-state", "checkpoint.json"), map[string]any{"version": 1, "server_id": "old-dataset", "account": primary, "cursor": "private-message-id"})
	writeJSON(filepath.Join(root, "bot-state", "accounts", wechatapp.AccountDirectory(secondary), "messages-v2.json"), map[string]any{"version": 1, "server": "old-dataset", "account": secondary, "inbox": []any{}, "deliveries": []any{}})
	writeJSON(filepath.Join(root, "bot-state", "accounts", wechatapp.AccountDirectory(secondary), "checkpoint.json"), map[string]any{"version": 1, "server_id": "old-dataset", "account": secondary, "cursor": "secondary"})

	writeJSON(filepath.Join(root, "admin", "agent-state.json"), map[string]any{
		"tasks":      map[string]any{"private-task": map[string]any{"id": "private-task", "account": primary, "thread": "private-thread", "sender": "private-sender", "state": "waiting", "title": "private old body"}},
		"operations": map[string]any{"private-operation": map[string]any{"id": "private-operation", "tool": "private-tool", "thread": "private-thread", "state": "running", "result": "private old body"}},
	})
	writeJSON(filepath.Join(root, "admin", "tool-state.json"), map[string]any{
		"version": 1,
		"sends": map[string]any{
			"accepted": map[string]any{"key": "admin-accepted", "hash": "private-hash", "account": primary, "thread": "private-thread", "kind": "text", "status": map[string]any{"state": "accepted", "kind": "text", "thread_id": "private-thread", "id": "private-native", "server_id": "103"}},
			"draft":    map[string]any{"key": "admin-draft", "hash": "private-hash", "account": primary, "thread": "private-thread", "kind": "text", "status": map[string]any{"state": "prepared", "kind": "text", "thread_id": "private-thread"}},
		}, "uploads": map[string]any{},
	})
	return legacyWechatQueueAuditFixture{primary: primary}
}

func digestLegacyQueueAuditTree(t *testing.T, root string) [32]byte {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The test only needs a stable whole-tree read witness; the implementation
	// itself is responsible for rejecting links and scope mismatches.
	h := sha256.New()
	for _, path := range paths {
		b := mustReadSnapshotFixture(t, path)
		_, _ = h.Write([]byte(filepath.ToSlash(strings.TrimPrefix(path, root))))
		_, _ = h.Write(b)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func intJSON(t *testing.T, value map[string]any, key string) int {
	t.Helper()
	n, ok := value[key].(float64)
	if !ok {
		t.Fatalf("%s is not a JSON number: %#v", key, value[key])
	}
	return int(n)
}

func assertLegacyAuditReceiptCounts(t *testing.T, view map[string]any, key string, want map[string]map[string]int) {
	t.Helper()
	raw, ok := view[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not a map: %#v", key, view[key])
	}
	for kind, states := range want {
		gotKind, ok := raw[kind].(map[string]any)
		if !ok {
			t.Fatalf("%s omitted kind %s: %#v", key, kind, raw)
		}
		for state, count := range states {
			got, ok := gotKind[state].(float64)
			if !ok || int(got) != count {
				t.Fatalf("%s.%s.%s=%#v, want %d", key, kind, state, gotKind[state], count)
			}
		}
	}
}

func assertLegacyAuditFlatReceiptCounts(t *testing.T, view map[string]any, key string, want map[string]int) {
	t.Helper()
	raw, ok := view[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not a map: %#v", key, view[key])
	}
	for state, count := range want {
		got, ok := raw[state].(float64)
		if !ok || int(got) != count {
			t.Fatalf("%s.%s=%#v, want %d", key, state, raw[state], count)
		}
	}
}
