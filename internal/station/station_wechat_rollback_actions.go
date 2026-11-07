package station

import (
	"context"
	"errors"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

// rollbackActionEvidence snapshots every connected group mutation and recall
// reservation for one connection.  It only reads the ledger and, when a
// native sender is provided, queries the original operation or idempotency
// key.  It never records refreshed results, retries, or submits an action.
//
// The returned digest is the durable-ledger snapshot before native lookups.
// Thus callers can read it again with native=nil after a drain and prove that
// no local action reservation changed while the platform checks ran.
func (s *stationWechat) rollbackActionEvidence(ctx context.Context, w *stationWechatWorker, ref apphost.ConnectionRef, native *linuxnative.Sender) ([]apphost.ActionReceipt, string, error) {
	if s == nil || w == nil || ref.AppID != wechatapp.ID || ref.ConnectionID != wechatapp.ConnectionID(ref.AccountID) {
		return nil, "", apphost.ErrNotFound
	}
	w.mu.Lock()
	ledger, err := w.actionLedgerLocked()
	if err != nil {
		w.mu.Unlock()
		return nil, "", err
	}
	proposals, snapshot, err := ledger.SnapshotConnected(ref)
	w.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	out := make([]apphost.ActionReceipt, 0, len(proposals))
	for _, proposal := range proposals {
		out = append(out, projectRollbackActionReceipt(ctx, native, proposal))
	}
	return out, snapshot, nil
}

func projectRollbackActionReceipt(ctx context.Context, native *linuxnative.Sender, proposal wechatapp.ActionProposal) apphost.ActionReceipt {
	out := proposal.Receipt()
	// A terminal durable action is authoritative. Do not permit a stale native
	// response to downgrade it during a read-only rollback audit.
	if native == nil || actionReceiptTerminal(out.State) {
		return out
	}
	if native.Account() != proposal.Account || ctx.Err() != nil {
		out.Lookup = "unavailable"
		return out
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var status linuxnative.ActionStatus
	var err error
	if proposal.Result.ID != "" {
		status, err = native.InspectAction(lookupCtx, proposal.Result.ID)
	} else if proposal.Request.Kind != "recall" {
		status, err = native.InspectGroupActionKey(lookupCtx, proposal.ID)
	} else {
		// The native API has no recall lookup by request key. Preserve this
		// durable unknown reservation rather than creating any new action.
		return out
	}
	switch {
	case errors.Is(err, linuxnative.ErrToolNotFound):
		out.Lookup = "not_found"
	case err != nil:
		out.Lookup = "unavailable"
	case !proposal.MatchesReceipt(status):
		out.Lookup = "identity_mismatch"
	default:
		out.Lookup = "platform"
		wechatapp.ProjectActionReceipt(&out, status)
	}
	return out
}

func actionReceiptTerminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled" || state == "rejected"
}
