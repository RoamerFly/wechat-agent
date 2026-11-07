package station

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

func TestRollbackActionEvidenceKeepsLocalSnapshotAfterNativeAudit(t *testing.T) {
	dir := t.TempDir()
	worker := &stationWechatWorker{dir: dir}
	ref := apphost.ConnectionRef{AppID: wechatapp.ID, InstallationID: "local", ConnectionID: wechatapp.ConnectionID("one"), AccountID: "one"}
	worker.mu.Lock()
	ledger, err := worker.actionLedgerLocked()
	worker.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	proposal := wechatapp.ActionProposal{
		ID: "action-one", Account: "one", State: "unknown",
		Request:   linuxnative.AdminAction{Kind: "mute", ThreadID: "room@chatroom"},
		AppAction: &apphost.ActionIntent{ConnectionRef: ref, Action: "mute", Target: "room@chatroom", IdempotencyKey: "action-one"},
	}
	if err := ledger.Save(proposal); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "native-token")
	if err := os.WriteFile(token, []byte("synthetic-rollback-action-token"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-rollback-action-token" || r.Method != http.MethodGet || r.URL.Path != "/v1/accounts/one/group-mutations/by-idempotency-key" || r.URL.Query().Get("key") != "action-one" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "native-action-one", "accountId": "one", "state": "running", "kind": "group_mutation",
			"result": map[string]any{"action": "mute", "targetId": "room@chatroom"},
		})
	}))
	defer server.Close()
	native, err := linuxnative.New(linuxnative.Config{URL: server.URL, TokenFile: token, Account: "one"})
	if err != nil {
		t.Fatal(err)
	}
	s := &stationWechat{}
	audited, digest, err := s.rollbackActionEvidence(context.Background(), worker, ref, native)
	if err != nil || digest == "" || len(audited) != 1 {
		t.Fatalf("unexpected audit result: receipts=%#v digest=%q err=%v", audited, digest, err)
	}
	if audited[0].Lookup != "platform" || audited[0].State != "submitted_pending" || audited[0].PlatformOperationID != "native-action-one" {
		t.Fatalf("native action was not projected read-only: %#v", audited[0])
	}

	// A second local-only read observes the same unchanged ledger snapshot and
	// does not mistake the prior platform query for a durable ledger mutation.
	local, laterDigest, err := s.rollbackActionEvidence(context.Background(), worker, ref, nil)
	if err != nil || laterDigest != digest || len(local) != 1 {
		t.Fatalf("local recheck changed durable audit evidence: receipts=%#v before=%q after=%q err=%v", local, digest, laterDigest, err)
	}
	if local[0].Lookup != "stored" || local[0].State != "submission_unknown" {
		t.Fatalf("local recheck must retain only stored unknown reservation: %#v", local[0])
	}
}
