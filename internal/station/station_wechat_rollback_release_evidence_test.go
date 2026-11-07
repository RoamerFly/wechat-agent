package station

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
)

func TestValidateRollbackReleaseEvidence(t *testing.T) {
	now := time.Now().UTC().Add(-time.Second)
	root := t.TempDir()
	ref := apphost.ConnectionRef{AppID: wechatapp.ID, InstallationID: "local", ConnectionID: wechatapp.ConnectionID("one"), AccountID: "one"}
	target := legacyRollbackTarget{legacyRollbackView: legacyRollbackView{State: "frozen", Eligible: true, FrozenAt: now.Add(-time.Second)}, ref: ref, dataset: "dataset-one"}
	audit := rollbackAudit{Version: 1, Connection: ref, FrozenAt: target.FrozenAt, CapturedAt: now, Checkpoint: strings.Repeat("a", 64), ActionSnapshot: strings.Repeat("b", 64), TargetDataset: target.dataset, NativeState: "available", DesktopEpoch: strings.Repeat("c", 32), NativeGeneration: "native-generation"}
	marker := stationNativeStop{Version: 1, State: "stopped", Epoch: audit.DesktopEpoch, DataRoot: filepath.Clean(root), NativeServerID: target.dataset, NativeOwnershipGeneration: audit.NativeGeneration, At: now.Add(time.Millisecond)}
	if err := validateRollbackReleaseEvidence(target, audit, marker, root, false); err != nil {
		t.Fatalf("clean stopped audit should release: %v", err)
	}

	unknown := audit
	unknown.DeliveryDataset = target.dataset
	unknown.Deliveries = []rollbackReceipt{{Receipt: apphost.Receipt{ConnectionRef: ref, DeliveryReceipt: apphost.DeliveryReceipt{IdempotencyKey: "delivery-one", Kind: "text", State: "submission_unknown", Lookup: "stored"}}, Disposition: "unknown"}}
	if err := validateRollbackReleaseEvidence(target, unknown, marker, root, false); err == nil {
		t.Fatal("unknown delivery released without explicit acknowledgement")
	}
	if err := validateRollbackReleaseEvidence(target, unknown, marker, root, true); err != nil {
		t.Fatalf("explicit acknowledgement should allow parked unknown: %v", err)
	}

	active := audit
	active.Actions = []apphost.ActionReceipt{{ConnectionRef: ref, OperationID: "action-one", State: "submitted_pending", Lookup: "platform"}}
	if err := validateRollbackReleaseEvidence(target, active, marker, root, true); err == nil {
		t.Fatal("active native action released")
	}
	activeDelivery := audit
	activeDelivery.DeliveryDataset = target.dataset
	activeDelivery.Deliveries = []rollbackReceipt{{Receipt: apphost.Receipt{ConnectionRef: ref, DeliveryReceipt: apphost.DeliveryReceipt{IdempotencyKey: "delivery-active", Kind: "voice", State: "submitted_pending", Lookup: "platform", PlatformState: "running"}}, Disposition: "active"}}
	if err := validateRollbackReleaseEvidence(target, activeDelivery, marker, root, true); err == nil {
		t.Fatal("active native delivery released")
	}

	wrongDataset := audit
	wrongDataset.Deliveries = []rollbackReceipt{{Receipt: apphost.Receipt{ConnectionRef: ref, DeliveryReceipt: apphost.DeliveryReceipt{IdempotencyKey: "delivery-two", Kind: "text", State: "not_submitted", Lookup: "stored"}}, Disposition: "not_submitted"}}
	wrongDataset.DeliveryDataset = "other-dataset"
	if err := validateRollbackReleaseEvidence(target, wrongDataset, marker, root, false); err == nil {
		t.Fatal("observed checkpoint from another native dataset released")
	}

	oldMarker := marker
	oldMarker.At = audit.CapturedAt.Add(-time.Nanosecond)
	if err := validateRollbackReleaseEvidence(target, audit, oldMarker, root, false); err == nil {
		t.Fatal("stop marker older than audit released")
	}
}
