package station

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
)

func TestStationListsResumeLineageWithoutNativeService(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "queue"), 0700); err != nil {
		t.Fatal(err)
	}
	value := map[string]any{"version": 2, "account": "fixture", "media": []any{map[string]any{
		"key": "resume-key", "trace": "explicit-continuation", "kind": "voice", "state": "resume_prepared", "resumed_from": "original-key",
		"input": map[string]any{"account": "fixture", "thread": "room"},
	}}}
	raw, _ := json.Marshal(value)
	if err := os.WriteFile(filepath.Join(dir, "queue", "messages-v2.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	id := wechatapp.ConnectionID("fixture")
	s := &stationWechat{ctx: context.Background(), workers: map[string]*stationWechatWorker{id: {dir: dir}}}
	page, err := s.ListReceipts(t.Context(), id, "", 20)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	row := page.Items[0]
	if row.ResumedFrom != "original-key" || row.IdempotencyKey != "resume-key" || row.State != "not_submitted" || row.ConversationID != "room" {
		t.Fatal("continuation lineage or preparation state lost", row)
	}
}

func TestStationContinuationReceiptCannotBorrowOriginalJob(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("synthetic-continuation-receipt-token"), 0600); err != nil {
		t.Fatal(err)
	}
	native, err := linuxnative.New(linuxnative.Config{URL: "http://127.0.0.1:1", TokenFile: token, Account: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	reader := stationMediaReceiptReader{native: native, source: "source", kind: "voice", origin: "original-job"}
	value := linuxnative.ToolStatus{ID: "original-job", AccountID: "fixture", SourceID: "source", Kind: "send", State: "accepted", ServerID: "100"}
	if _, err := reader.validate(value, nil); err == nil {
		t.Fatal("original accepted receipt assigned to continuation")
	}
	value.ID = "new-job"
	if _, err := reader.validate(value, nil); err != nil {
		t.Fatal("valid independent continuation rejected", err)
	}
}
