package station

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

func TestLegacyReceiptRejectsMismatchedArchivedMediaSource(t *testing.T) {
	base := t.TempDir()
	tokenPath := filepath.Join(base, "native-token")
	if err := os.WriteFile(tokenPath, []byte("synthetic-legacy-receipt-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var matchingSource atomic.Bool
	var requests atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-legacy-receipt-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"pid": 44, "dataDir": base, "serverId": "legacy-media-dataset", "ownershipGeneration": "synthetic-generation"})
		case "/v1/accounts/one/sends/old-media-operation":
			requests.Add(1)
			returnedSource := "other-archived-source"
			if matchingSource.Load() {
				returnedSource = "expected-archived-source"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "old-media-operation", "accountId": "one", "threadId": "filehelper", "kind": "voice", "state": "accepted", "serverId": "77123", "sourceId": returnedSource})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer daemon.Close()
	native, err := linuxnative.New(linuxnative.Config{URL: daemon.URL, TokenFile: tokenPath, Account: "one"})
	if err != nil {
		t.Fatal(err)
	}
	archive := legacyImportArchive{LegacyServer: "legacy-media-dataset", ReceiptSources: map[string]string{"legacy-media": "expected-archived-source"}}
	original := apphost.Receipt{
		ConnectionRef:  apphost.ConnectionRef{AccountID: "one"},
		ConversationID: "filehelper",
		DeliveryReceipt: apphost.DeliveryReceipt{
			IdempotencyKey: "legacy-media", OperationID: "old-media-operation", Kind: "voice", State: "submission_unknown", Lookup: "legacy_snapshot",
		},
	}
	checked := checkLegacyReceipt(t.Context(), native, archive, original)
	if checked.Lookup != "identity_mismatch" || checked.State != "submission_unknown" || checked.OperationID != original.OperationID || checked.ServerID != "" {
		t.Fatalf("mismatched source changed the archived receipt: %#v", checked)
	}
	if count := requests.Load(); count != 1 {
		t.Fatalf("unexpected lookup count after mismatch: %d", count)
	}
	matchingSource.Store(true)
	checked = checkLegacyReceipt(t.Context(), native, archive, original)
	if checked.Lookup != "platform" || checked.State != "accepted" || checked.ServerID != "77123" {
		t.Fatalf("matching source did not produce a bounded platform receipt: %#v", checked)
	}
	if count := requests.Load(); count != 2 {
		t.Fatalf("unexpected lookup count after matching source: %d", count)
	}
}
