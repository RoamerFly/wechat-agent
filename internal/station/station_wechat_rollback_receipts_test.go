package station

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/linuxnative"
)

func TestProjectRollbackReceiptSeparatesActiveFromUnknownAndRetainsParts(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("synthetic-rollback-token"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-rollback-token" || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/accounts/one/sends/voice-job" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "voice-job", "accountId": "one", "threadId": "room", "kind": "voice", "state": "running", "sourceId": "source-one",
			"result": map[string]any{"chunks": []map[string]any{
				{"index": 0, "state": "accepted", "serverMessageId": "71", "durationMs": 1000},
				{"index": 1, "state": "submitting", "durationMs": 1000},
			}},
		})
	}))
	defer server.Close()
	native, err := linuxnative.New(linuxnative.Config{URL: server.URL, TokenFile: token, Account: "one"})
	if err != nil {
		t.Fatal(err)
	}
	row := bot.DeliveryEvidence{SourceID: "source-one", StrictSource: true, Status: linuxnative.ToolStatus{
		ID: "voice-job", Kind: "voice", ThreadID: "room", State: "unknown",
		VoiceChunks: []linuxnative.VoiceChunkStatus{
			{Index: 0, State: "accepted", HasReceipt: true, ServerID: "71"},
			{Index: 2, State: "submitting"},
		},
	}}
	ref := apphost.ConnectionRef{AppID: "wechat", InstallationID: "local", ConnectionID: "wechat-one", AccountID: "one"}
	receipt, active := projectRollbackReceipt(context.Background(), native, ref, "delivery-one", row)
	if !active || receipt.Lookup != "platform" || receipt.PlatformState != "running" || receipt.State != "submitted_pending" {
		t.Fatalf("running native operation was not projected as active: %#v active=%v", receipt, active)
	}
	if len(receipt.AcceptedParts) != 1 || receipt.AcceptedParts[0].Index != 0 || receipt.AcceptedParts[0].ServerID != "71" {
		t.Fatalf("accepted prefix was lost: %#v", receipt)
	}
	if len(receipt.UnconfirmedParts) != 2 || receipt.UnconfirmedParts[0].Index != 1 || receipt.UnconfirmedParts[1].Index != 2 {
		t.Fatalf("a native response that omitted a pending position erased evidence: %#v", receipt)
	}

	unknown, active := projectRollbackReceipt(context.Background(), nil, ref, "delivery-one", row)
	if active || unknown.Lookup != "stored" || unknown.State != "submission_unknown" {
		t.Fatalf("offline checkpoint must remain unknown rather than active: %#v active=%v", unknown, active)
	}
	if len(unknown.AcceptedParts) != 1 || unknown.AcceptedParts[0].ServerID != "71" {
		t.Fatalf("offline projection lost accepted prefix: %#v", unknown)
	}
}
