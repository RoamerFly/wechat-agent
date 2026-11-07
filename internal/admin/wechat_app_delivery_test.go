package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

type appTextReceiptNative struct {
	*fakeTools
	original linuxnative.ToolSend
	status   linuxnative.ToolStatus
	keys     []string
}

func (n *appTextReceiptNative) Submit(_ context.Context, request linuxnative.ToolSend) (linuxnative.ToolStatus, error) {
	n.submits.Add(1)
	n.original = request
	return linuxnative.ToolStatus{}, errors.New("synthetic acknowledgement lost")
}

func (n *appTextReceiptNative) LookupSend(_ context.Context, key string) (linuxnative.ToolStatus, error) {
	n.keys = append(n.keys, key)
	if key == n.original.IdempotencyKey && n.status.ID != "" {
		return n.status, nil
	}
	return linuxnative.ToolStatus{}, linuxnative.ErrToolNotFound
}

func readAppTestReceipt(t *testing.T, f *webFixture, key string) apphost.Receipt {
	t.Helper()
	code, body, response := f.request("GET", wechatAppTestPath("test")+"/receipts/"+key, nil, true, false)
	var receipt apphost.Receipt
	if code != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || json.Unmarshal(body, &receipt) != nil ||
		receipt.ConnectionRef != wechatConnectionRef("test") || strings.Contains(string(body), "synthetic-private") {
		t.Fatalf("invalid app delivery receipt: %d %s", code, body)
	}
	return receipt
}

func TestApplicationAgentTextLooksUpLostAcknowledgementByOriginalKey(t *testing.T) {
	f, p := trustedAgent(t)
	f.login()
	native := &appTextReceiptNative{fakeTools: &fakeTools{fakeNative: f.native}}
	f.manager.native = native
	settings := f.manager.agent.config()
	settings.CrossConversation = true
	if err := f.manager.agent.save(settings); err != nil {
		t.Fatal(err)
	}
	f.manager.mu.Lock()
	f.manager.config.Behavior.RandomDelay = false
	f.manager.config.Companion.Permissions.Remember = true
	f.manager.mu.Unlock()
	observeOutboundSource(t, f, p, "请向文件传输助手发送：synthetic-private-text")
	args := json.RawMessage(`{"thread_id":"filehelper","text":"synthetic-private-text","request_key":"lost-ack"}`)
	key := p.operationID("wechat_send_text", args)
	if _, err := p.Call(t.Context(), "wechat_send_text", args); err != nil || native.submits.Load() != 1 || native.original.IdempotencyKey != key {
		t.Fatalf("original Agent send not submitted exactly once: %v", err)
	}
	unknown := readAppTestReceipt(t, f, key)
	if unknown.State != "submission_unknown" || unknown.Lookup != "not_found" || unknown.OperationID != "" {
		t.Fatal("missing platform key was treated as definitive", unknown)
	}
	native.status = linuxnative.ToolStatus{ID: "original-native-job", Kind: "send", ThreadID: "wrong@chatroom", State: "accepted", ServerID: "552"}
	if got := readAppTestReceipt(t, f, key); got.Lookup != "identity_mismatch" || got.State != "submission_unknown" || got.ServerID != "" {
		t.Fatal("mismatched receipt was accepted", got)
	}
	native.status.ThreadID = "filehelper"
	if got := readAppTestReceipt(t, f, key); got.State != "accepted" || got.ServerID != "552" || got.IdempotencyKey != key {
		t.Fatal("late receipt did not bind to original send", got)
	}
	before := len(native.keys)
	for _, path := range []string{wechatAppTestPath("test") + "/receipts/unclaimed-key", wechatAppTestPath("other") + "/receipts/" + key} {
		if code, _, _ := f.request("GET", path, nil, true, false); code != http.StatusNotFound {
			t.Fatal("unclaimed or foreign receipt returned", code)
		}
	}
	if len(native.keys) != before || native.submits.Load() != 1 {
		t.Fatal("read-only or unclaimed query dispatched work", native.keys, native.submits.Load())
	}
	for _, got := range native.keys {
		if got != key {
			t.Fatal("lookup did not use original key", got)
		}
	}
}

func TestApplicationAgentVoicePreservesAcceptedPartsAfterCancellation(t *testing.T) {
	f, _, batch := crossRecoveryFixture(t, 2)
	f.login()
	batch.Receipts = map[string]linuxnative.ToolStatus{batch.Parts[0].Key: acceptedCrossPart(batch, 0)}
	saveCrossRecoveryBatch(t, f, batch)
	last := batch.Parts[1]
	native := &receiptLookupNative{forwardNative: f.manager.native.(*forwardNative), statuses: map[string]linuxnative.ToolStatus{}}
	f.manager.native = native
	chunk := linuxnative.VoiceChunkStatus{Index: 0, State: "accepted", HasReceipt: true, ServerID: "992"}
	status := linuxnative.ToolStatus{ID: "second-original-job", Kind: "voice", ThreadID: batch.Target, SourceID: last.SourceID,
		State: "running", CancelRequested: true, VoiceChunks: []linuxnative.VoiceChunkStatus{chunk, {Index: 1, State: "submitting"}}}
	native.statuses[last.Key] = status
	before, err := f.manager.readCrossBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending := readAppTestReceipt(t, f, batch.ID)
	if pending.State != "submitted_pending" || !pending.CancelRequested || len(pending.Deliveries) != 2 ||
		pending.Deliveries[0].ServerID != "800" || pending.Deliveries[1].OperationID != status.ID || len(pending.Deliveries[1].AcceptedParts) != 1 ||
		len(pending.Deliveries[1].UnconfirmedParts) != 1 || pending.Deliveries[1].UnconfirmedParts[0].State != "submitting" {
		t.Fatal("partially accepted batch lost in-flight evidence", pending)
	}
	wrong := status
	wrong.SourceID = "wrong-upload"
	native.statuses[last.Key] = wrong
	mismatch := readAppTestReceipt(t, f, batch.ID)
	if mismatch.State != "submission_unknown" || mismatch.Deliveries[1].Lookup != "identity_mismatch" ||
		len(mismatch.Deliveries[1].AcceptedParts) != 0 || mismatch.Deliveries[0].ServerID != "800" {
		t.Fatal("wrong attachment source was accepted or hid known receipts", mismatch)
	}
	status.State, status.VoiceChunks[1].State = "cancelled", "cancelled"
	native.statuses[last.Key] = status
	done := readAppTestReceipt(t, f, batch.ID)
	if done.State != "cancelled" || !done.CancelRequested || done.Deliveries[0].State != "accepted" ||
		done.Deliveries[1].State != "cancelled" || done.Deliveries[1].AcceptedParts[0].ServerID != "992" {
		t.Fatal("cancelled batch lost confirmed segments or appeared fully sent", done)
	}
	after, err := f.manager.readCrossBatch(batch.ID)
	if err != nil || len(after.Receipts) != len(before.Receipts) || native.lookups != 3 || native.submits.Load() != 0 {
		t.Fatal("app reads modified batch or attempted a send", err, native.lookups, native.submits.Load())
	}
}

func TestApplicationReadsWorkerCheckpointForAutomaticDelivery(t *testing.T) {
	f := setup(t)
	f.login()
	f.manager.native = &fakeTools{fakeNative: f.native}
	raw := `{"version":1,"account":"test","deliveries":[{"key":"worker-key","id":"worker-job","server_id":"885","thread":"filehelper","state":"accepted","text":"synthetic-private-body","input":{"account":"test","thread":"filehelper"}}]}`
	if err := os.MkdirAll(f.manager.stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(f.manager.stateDir, "messages-v2.json"), []byte(raw)); err != nil {
		t.Fatal(err)
	}
	got := readAppTestReceipt(t, f, "worker-key")
	if got.State != "accepted" || got.Lookup != "stored" || got.ServerID != "885" || got.OperationID != "worker-job" || got.Kind != "text" {
		t.Fatal("worker checkpoint lost the original receipt", got)
	}
}
