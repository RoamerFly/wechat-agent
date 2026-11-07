package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

func TestActionLedgerReadsLegacyRecordsAndIsolatesAccounts(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"legacy":{"id":"legacy","account":"account-one","request":{"kind":"add_members","thread_id":"room@chatroom","members":["member-one"]},"summary":"legacy draft","state":"awaiting_approval","created":"2026-11-06T00:00:00Z","result":{"id":"","state":"","kind":""}}}`)
	if err := os.WriteFile(filepath.Join(dir, "action-proposals.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	ledger, err := OpenActionLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := ledger.Read("legacy", "account-one")
	if !ok || first.State != "awaiting_approval" || first.AppAction != nil || first.Request.Members[0] != "member-one" {
		t.Fatal("legacy action was changed or hidden", first, ok)
	}
	if leaked, ok := ledger.Read("legacy", "account-two"); ok || leaked.ID != "" || len(ledger.List("account-two")) != 0 {
		t.Fatal("a foreign account could read the record")
	}
	first.Request.Members[0] = "replacement"
	items := ledger.List("account-one")
	if len(items) != 1 || items[0].Request.Members[0] != "member-one" {
		t.Fatal("returned proposal could modify the ledger")
	}
	first.Account = "account-two"
	if err := ledger.Save(first); !errors.Is(err, apphost.ErrLifecycleConflict) {
		t.Fatal("a foreign account could replace the original operation", err)
	}
}

func TestConnectedActionReceiptBindsAndKeepsTerminalResult(t *testing.T) {
	dir := t.TempDir()
	ledger, err := OpenActionLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	const account, installation, group = "account-one", "install-one", "room@chatroom"
	connection := ConnectionID(account)
	proposal := ActionProposal{
		ID: "operation-one", Account: account, State: "unknown", Created: time.Now().UTC(),
		Request: linuxnative.AdminAction{Kind: "mute", ThreadID: group, Muted: true},
		AppAction: &apphost.ActionIntent{
			ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installation, ConnectionID: connection, AccountID: account},
			Action:        "mute", Target: group, IdempotencyKey: "operation-one", GrantVersion: "grant-one",
		},
	}
	if err := ledger.Save(proposal); err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger.FindConnected("other-install", connection, proposal.ID, "", ""); ok {
		t.Fatal("wrong installation could find the operation")
	}
	if _, ok := ledger.FindConnected(installation, ConnectionID("account-two"), proposal.ID, "", ""); ok {
		t.Fatal("wrong connection could find the operation")
	}
	pending, ok := ledger.FindConnected(installation, connection, "", group, "mute")
	if !ok || pending.Receipt().State != "submission_unknown" {
		t.Fatal("missing recoverable reservation", pending, ok)
	}
	pending.AppAction.GrantVersion = "other-grant"
	status := linuxnative.ActionStatus{ID: "native-one", Kind: "group_mutation", AccountID: account, Action: "mute", Target: group, State: "running"}
	if _, err := ledger.RecordConnectedReceipt(pending, status); !errors.Is(err, apphost.ErrNotFound) {
		t.Fatal("modified intent could record a platform receipt", err)
	}
	wrong := status
	wrong.Target = "other@chatroom"
	if got, err := ledger.RecordConnectedReceipt(proposal, wrong); err != nil || got.Lookup != "identity_mismatch" {
		t.Fatal("wrong target became authoritative", got, err)
	}
	if got, err := ledger.RecordConnectedReceipt(proposal, status); err != nil || got.State != "submitted_pending" {
		t.Fatal("matching progress was not recorded", got, err)
	}
	duplicate := cloneActionProposal(proposal)
	duplicate.ID = "operation-two"
	duplicate.AppAction.IdempotencyKey = duplicate.ID
	if err := ledger.Save(duplicate); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger.RecordConnectedReceipt(duplicate, status); err != nil || got.Lookup != "identity_mismatch" {
		t.Fatal("one native receipt was assigned to two operations", got, err)
	}
	if err := ledger.RejectUnsubmitted(proposal); !errors.Is(err, apphost.ErrLifecycleConflict) {
		t.Fatal("an admitted operation must not become unsubmitted", err)
	}
	if err := ledger.RejectUnsubmitted(duplicate); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger.RecordConnectedReceipt(duplicate, status); err != nil || got.State != "rejected" || got.PlatformOperationID != "" {
		t.Fatal("a locally rejected operation must not acquire a platform receipt", got, err)
	}
	final := status
	final.State, final.RemoteOutcome = "succeeded", "accepted"
	var wg sync.WaitGroup
	for _, candidate := range []linuxnative.ActionStatus{final, status} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ledger.RecordConnectedReceipt(proposal, candidate); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	restored, err := OpenActionLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := restored.FindConnected(installation, connection, proposal.ID, "", "mute")
	if !ok || got.Result != final || got.Receipt().State != "succeeded" {
		t.Fatal("terminal receipt did not survive stale progress and reopen", got, ok)
	}
	if got, ok := restored.FindConnected(installation, connection, duplicate.ID, "", "mute"); !ok || got.Receipt().State != "rejected" || got.Result.ID != "" {
		t.Fatal("local rejection did not survive reopen", got, ok)
	}
}
