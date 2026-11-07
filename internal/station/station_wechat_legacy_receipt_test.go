package station

import (
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

func TestLegacyReceiptDoesNotLoseSegmentEvidence(t *testing.T) {
	receipt := apphost.DeliveryReceipt{
		OperationID: "old-job", State: "submission_unknown", Lookup: "legacy_snapshot", PartsIncomplete: true,
		AcceptedParts:    []apphost.ReceiptPart{{Index: 0, ServerID: "101"}},
		UnconfirmedParts: []apphost.ReceiptPart{{Index: 1, State: "unknown"}},
	}
	status := linuxnative.ToolStatus{ID: "old-job", State: "accepted", ServerID: "102", VoiceChunks: []linuxnative.VoiceChunkStatus{
		{Index: 0, State: "accepted", ServerID: "999", HasReceipt: true},
	}}
	if projectLegacyReceipt(&receipt, status) || receipt.AcceptedParts[0].ServerID != "101" || receipt.State != "submission_unknown" {
		t.Fatalf("conflicting platform part replaced archived evidence: %#v", receipt)
	}
	status.VoiceChunks[0].ServerID = "101"
	if !projectLegacyReceipt(&receipt, status) || receipt.State == "accepted" || !receipt.PartsIncomplete || len(receipt.UnconfirmedParts) != 1 {
		t.Fatalf("omitted segment was treated as accepted: %#v", receipt)
	}
	status.VoiceChunks = append(status.VoiceChunks, linuxnative.VoiceChunkStatus{Index: 1, State: "accepted", ServerID: "102", HasReceipt: true})
	if !projectLegacyReceipt(&receipt, status) || receipt.State != "accepted" || receipt.PartsIncomplete || len(receipt.AcceptedParts) != 2 || len(receipt.UnconfirmedParts) != 0 {
		t.Fatalf("complete platform evidence did not resolve the receipt: %#v", receipt)
	}
}
