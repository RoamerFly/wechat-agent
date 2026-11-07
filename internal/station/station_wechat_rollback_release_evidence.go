package station

import (
	"errors"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/identity"
)

// validateRollbackReleaseEvidence validates a frozen connection's previously
// captured local audit against the desktop's later clean-stop marker.  It is a
// pure gate: callers remain responsible for file locks, current-snapshot
// rechecks, journal writes, and the actual release.
//
// Unknown platform outcomes are intentionally not made safe by this function;
// they require the explicit acceptUnknown acknowledgement.  Active work is
// never releasable, regardless of acknowledgement.
func validateRollbackReleaseEvidence(target legacyRollbackTarget, audit rollbackAudit, marker stationNativeStop, stationRoot string, acceptUnknown bool) error {
	if target.State != "frozen" || !target.Eligible || !audit.matchesTarget(target) || !identity.ValidPathID(target.dataset) {
		return apphost.ErrLifecycleConflict
	}
	if audit.NativeState != "available" || !validRollbackEpoch(audit.DesktopEpoch) || !identity.ValidPathID(audit.NativeGeneration) {
		return apphost.ErrLifecycleConflict
	}
	if marker.Version != 1 || marker.State != "stopped" || !validRollbackEpoch(marker.Epoch) ||
		!sameCutoverPath(stationRoot, marker.DataRoot) || marker.Epoch != audit.DesktopEpoch ||
		marker.NativeServerID != target.dataset || marker.NativeOwnershipGeneration != audit.NativeGeneration ||
		!identity.ValidPathID(marker.NativeServerID) || !identity.ValidPathID(marker.NativeOwnershipGeneration) ||
		marker.At.IsZero() || marker.At.Before(audit.CapturedAt) || marker.At.After(time.Now().Add(time.Minute)) {
		return apphost.ErrLifecycleConflict
	}
	if len(audit.Deliveries) != 0 && audit.DeliveryDataset != target.dataset {
		return apphost.ErrLifecycleConflict
	}
	unknown := false
	for _, row := range audit.Deliveries {
		if row.ConnectionRef != target.ref || !identity.ValidPathID(row.IdempotencyKey) {
			return apphost.ErrLifecycleConflict
		}
		switch row.Disposition {
		case "accepted":
			if row.State != "accepted" || !wechatapp.ValidServerReceiptID(row.ServerID) || row.PartsIncomplete || len(row.UnconfirmedParts) != 0 {
				return apphost.ErrLifecycleConflict
			}
		case "terminal":
			if row.State != "failed" && row.State != "cancelled" {
				return apphost.ErrLifecycleConflict
			}
		case "not_submitted":
			if row.State != "not_submitted" || row.OperationID != "" {
				return apphost.ErrLifecycleConflict
			}
		case "unknown":
			unknown = true
		case "active":
			return apphost.ErrLifecycleConflict
		default:
			return apphost.ErrLifecycleConflict
		}
	}
	for _, action := range audit.Actions {
		if action.ConnectionRef != target.ref || !identity.ValidPathID(action.OperationID) {
			return apphost.ErrLifecycleConflict
		}
		switch action.State {
		case "succeeded", "failed", "cancelled", "rejected":
			// Durable terminal action evidence is releasable.
		case "submitted_pending":
			if action.Lookup == "platform" {
				return apphost.ErrLifecycleConflict
			}
			unknown = true
		default:
			unknown = true
		}
	}
	if unknown && !acceptUnknown {
		return errors.New("rollback release has unverified delivery or action outcomes")
	}
	return nil
}

func validRollbackEpoch(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == ""
}
