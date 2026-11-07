package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"
	wechatapp "wechat-bot/connected-apps/wechat/adapter"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/linuxnative"
)

// readAgentDelivery resolves a host-owned record before calling the native
// receipt API. The key in an HTTP request alone grants no platform lookup.
func (m *Manager) readAgentDelivery(ctx context.Context, key string) (apphost.Receipt, error) {
	batch, err := m.readCrossBatch(key)
	if err == nil {
		return m.readAppMediaBatch(ctx, batch)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return apphost.Receipt{}, err
	}
	if m.agent != nil {
		m.agent.mu.Lock()
		op, exists := m.agent.data.Operations[key]
		m.agent.mu.Unlock()
		if exists && op.ID == key && (op.Tool == "wechat_send_text" || op.Tool == "wechat_forward_file") {
			return m.readAppAgentSend(ctx, op)
		}
	}
	account := m.native.Account()
	rows, err := bot.ReadDeliveryEvidence(m.stateDir, account, []string{key})
	if err != nil {
		return apphost.Receipt{}, err
	}
	row, ok := rows[key]
	if !ok {
		return apphost.Receipt{}, apphost.ErrNotFound
	}
	status := row.Status
	result := apphost.Receipt{
		ConnectionRef: wechatConnectionRef(account), ConversationID: status.ThreadID,
		DeliveryReceipt: apphost.DeliveryReceipt{IdempotencyKey: key, Kind: status.Kind, Lookup: "stored"},
	}
	m.queryWechatDelivery(ctx, status.ThreadID, row.SourceID, status, false, &result.DeliveryReceipt)
	return result, nil
}

func (m *Manager) readAppAgentSend(ctx context.Context, op agentOperation) (apphost.Receipt, error) {
	var saved struct {
		linuxnative.ToolStatus
		NotStarted bool `json:"not_started"`
	}
	if json.Unmarshal([]byte(op.Result), &saved) != nil {
		return apphost.Receipt{}, apphost.ErrUnavailable
	}
	var thread, kind, key string
	legacyFile := op.Tool == "wechat_forward_file"
	if legacyFile {
		thread, _ = forwardReceiptDescription(op, op.Thread)
		kind = "file"
		// Old forwarded files have a recorded native job, but their native
		// submission key is not guaranteed to match the Agent ledger ID.
	} else {
		var args struct {
			Thread string `json:"thread_id"`
		}
		if json.Unmarshal(op.Arguments, &args) != nil {
			return apphost.Receipt{}, apphost.ErrUnavailable
		}
		thread, kind, key = args.Thread, "text", op.ID
	}
	if !validID(thread) || (saved.ThreadID != "" && saved.ThreadID != thread) {
		return apphost.Receipt{}, apphost.ErrUnavailable
	}
	if saved.NotStarted || saved.State == "not_sent" {
		saved.State = "prepared"
	}
	result := apphost.Receipt{
		ConnectionRef: wechatConnectionRef(m.native.Account()), ConversationID: thread,
		DeliveryReceipt: apphost.DeliveryReceipt{IdempotencyKey: key, Kind: kind, Lookup: "stored"},
	}
	m.queryWechatDelivery(ctx, thread, saved.SourceID, saved.ToolStatus, legacyFile, &result.DeliveryReceipt)
	return result, nil
}

func (m *Manager) readAppMediaBatch(ctx context.Context, batch crossMediaBatch) (apphost.Receipt, error) {
	// Read the worker's checkpoint once for the batch. A missing row is not
	// proof that the original key was never submitted.
	keys := make([]string, 0, len(batch.Parts))
	for _, part := range batch.Parts {
		if _, accepted := batch.Receipts[part.Key]; !accepted {
			keys = append(keys, part.Key)
		}
	}
	rows, err := bot.ReadDeliveryEvidence(m.stateDir, batch.Account, keys)
	if err != nil {
		return apphost.Receipt{}, err
	}
	result := apphost.Receipt{
		ConnectionRef: wechatConnectionRef(batch.Account), ConversationID: batch.Target,
		DeliveryReceipt: apphost.DeliveryReceipt{
			OperationID: batch.ID, Kind: batch.Parts[0].Media.Kind, Lookup: "per_delivery",
			CancelRequested: !batch.CancelledAt.IsZero(),
		},
		SupersededBy: batch.SupersededBy,
		Deliveries:   make([]apphost.DeliveryReceipt, 0, len(batch.Parts)),
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, part := range batch.Parts {
		child := apphost.DeliveryReceipt{IdempotencyKey: part.Key, Kind: part.Media.Kind, Lookup: "stored", CancelRequested: !batch.CancelledAt.IsZero()}
		status, accepted := batch.Receipts[part.Key]
		if !accepted {
			if row, ok := rows[part.Key]; ok {
				if row.Trace != batch.ID || row.Status.ThreadID != batch.Target || row.Status.Kind != part.Media.Kind || row.SourceID != part.SourceID {
					return apphost.Receipt{}, apphost.ErrUnavailable
				}
				status = row.Status
			}
			if child.CancelRequested && status.ID == "" && (status.State == "" || status.State == "prepared") {
				// Console cancellation was recorded only after checking that
				// none of the batch keys had been submitted.
				status.State = "cancelled"
			}
		}
		if batch.SupersededBy != "" {
			wechatapp.ProjectDeliveryReceipt(&child, status)
		} else {
			m.queryWechatDelivery(ctx, batch.Target, part.SourceID, status, false, &child)
		}
		result.Deliveries = append(result.Deliveries, child)
	}
	result.State = appMediaBatchState(result.Deliveries)
	if batch.SupersededBy != "" {
		result.State = "superseded"
	}
	for _, child := range result.Deliveries {
		result.CancelRequested = result.CancelRequested || child.CancelRequested
	}
	return result, nil
}

func appMediaBatchState(deliveries []apphost.DeliveryReceipt) string {
	var accepted int
	var unknown, pending, cancelled, failed, prepared bool
	for _, child := range deliveries {
		switch child.State {
		case "accepted":
			accepted++
		case "submitted_pending":
			pending = true
		case "cancelled":
			cancelled = true
		case "failed":
			failed = true
		case "not_submitted":
			prepared = true
		default:
			unknown = true
		}
	}
	switch {
	case accepted > 0 && accepted == len(deliveries):
		return "accepted"
	case unknown:
		return "submission_unknown"
	case pending || (prepared && (accepted > 0 || failed)):
		return "submitted_pending"
	case prepared:
		return "not_submitted"
	case cancelled:
		return "cancelled"
	case failed:
		return "failed"
	default:
		return "submission_unknown"
	}
}
