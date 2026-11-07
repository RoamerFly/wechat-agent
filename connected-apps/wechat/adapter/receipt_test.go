package adapter

import (
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

func TestApplicationReceiptRetainsAcceptedPartsWhenRemainingSendIsCancelled(t *testing.T) {
	part := linuxnative.VoiceChunkStatus{Index: 0, State: "accepted", HasReceipt: true, ServerID: "111"}
	status := linuxnative.ToolStatus{ID: "same-job", State: "needs_review", ServerID: "111", VoiceChunks: []linuxnative.VoiceChunkStatus{part}}
	var result apphost.DeliveryReceipt
	ProjectDeliveryReceipt(&result, status)
	if result.State != "submission_unknown" || result.ServerID != "" || len(result.AcceptedParts) != 1 {
		t.Fatal("one accepted part was mistaken for a complete delivery", result)
	}
	status.State, status.VoiceChunks = "cancelled", nil
	ProjectDeliveryReceipt(&result, status)
	if result.State != "cancelled" || result.ServerID != "" || len(result.AcceptedParts) != 1 || result.AcceptedParts[0].ServerID != "111" {
		t.Fatal("cancel discarded an existing receipt", result)
	}
	status.State, status.VoiceChunks = "accepted", []linuxnative.VoiceChunkStatus{part, {Index: 1, State: "submitting"}}
	ProjectDeliveryReceipt(&result, status)
	if result.State == "accepted" || result.ServerID != "" {
		t.Fatal("in-flight second segment was treated as fully accepted", result)
	}
}
