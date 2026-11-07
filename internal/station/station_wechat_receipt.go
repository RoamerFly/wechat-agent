package station

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
)

func (s *stationWechat) ListReceipts(ctx context.Context, id, after string, limit int) (apphost.ReceiptPage, error) {
	page := apphost.ReceiptPage{Items: []apphost.Receipt{}, Coverage: "current_worker_checkpoint"}
	if !artifactID(id, "wechat-") || after != "" && !identity.ValidPathID(after) || limit < 1 {
		return page, apphost.ErrNotFound
	}
	w, account, err := s.deliveryCheckpoint(id)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, apphost.ErrNotFound) {
		return page, nil
	}
	if err != nil {
		return page, err
	}
	if w == nil {
		return page, nil
	}
	rows, err := bot.ReadDeliveryEvidencePage(filepath.Join(w.dir, "queue"), account, after, limit)
	if err != nil {
		return page, apphost.ErrUnavailable
	}
	page.Snapshot, page.NextAfter = rows.Snapshot, rows.NextAfter
	w.deliveryMu.Lock()
	stops, err := w.readDeliveryStops()
	controls, controlErr := w.readResumeControls()
	w.deliveryMu.Unlock()
	if err != nil || controlErr != nil {
		return page, apphost.ErrUnavailable
	}
	page.Snapshot = localCheckpointKey(rows.Snapshot, stops, controls)
	for _, key := range rows.Keys {
		row := rows.Records[key]
		receipt := apphost.Receipt{ConnectionRef: s.connectionRef(id, account), ConversationID: row.Status.ThreadID,
			DeliveryReceipt: apphost.DeliveryReceipt{IdempotencyKey: key, Kind: row.Status.Kind, Lookup: "stored"}}
		wechatapp.ProjectDeliveryReceipt(&receipt.DeliveryReceipt, row.Status)
		receipt.ResumedFrom = row.ResumedFrom
		if control, exists := controls[key]; exists {
			receipt.Continuation = resumeControlView(control)
		}
		_, cancelled := stops[deliveryStopID(companion.Input{Account: account, Thread: row.Status.ThreadID, TraceID: row.Trace})]
		receipt.CancelRequested = receipt.CancelRequested || cancelled
		page.Items = append(page.Items, receipt)
	}
	return page, nil
}

func (s *stationWechat) ReadReceipt(ctx context.Context, id, key string) (apphost.Receipt, error) {
	if !identity.ValidPathID(key) {
		return apphost.Receipt{}, apphost.ErrNotFound
	}
	native, err := s.WorkerSender(ctx, id)
	if err != nil {
		return apphost.Receipt{}, err
	}
	w, err := s.worker(id, false)
	if err != nil {
		return apphost.Receipt{}, err
	}
	if w == nil {
		return apphost.Receipt{}, apphost.ErrNotFound
	}
	evidence, err := bot.ReadDeliveryEvidence(filepath.Join(w.dir, "queue"), native.Account(), []string{key})
	if err != nil {
		return apphost.Receipt{}, apphost.ErrUnavailable
	}
	row, found := evidence[key]
	if !found {
		return s.readLegacyReceipt(ctx, id, key, native)
	}
	result := apphost.Receipt{
		ResumedFrom:     row.ResumedFrom,
		ConnectionRef:   s.connectionRef(id, native.Account()),
		ConversationID:  row.Status.ThreadID,
		DeliveryReceipt: apphost.DeliveryReceipt{IdempotencyKey: key, Kind: row.Status.Kind, Lookup: "stored"},
	}
	var reader any = native
	if row.StrictSource || row.ResumeOriginID != "" {
		reader = stationMediaReceiptReader{native: native, source: row.SourceID, kind: row.Status.Kind, origin: row.ResumeOriginID}
	}
	wechatapp.LookupDeliveryReceipt(ctx, reader, row.Status.ThreadID, row.SourceID, row.Status, row.StrictSource && row.Status.ID != "", &result.DeliveryReceipt)
	cancelled, err := w.deliveryCancelled(companion.Input{Account: native.Account(), Thread: row.Status.ThreadID, TraceID: row.Trace})
	if err != nil {
		return apphost.Receipt{}, apphost.ErrUnavailable
	}
	result.CancelRequested = result.CancelRequested || cancelled
	w.deliveryMu.Lock()
	controls, err := w.readResumeControls()
	w.deliveryMu.Unlock()
	if err != nil {
		return apphost.Receipt{}, apphost.ErrUnavailable
	}
	if control, exists := controls[key]; exists {
		result.Continuation = resumeControlView(control)
	}
	return result, nil
}

type stationMediaReceiptReader struct {
	native               *linuxnative.Sender
	source, kind, origin string
}

func (r stationMediaReceiptReader) validate(status linuxnative.ToolStatus, err error) (linuxnative.ToolStatus, error) {
	if err == nil && (r.source == "" || status.SourceID != r.source || status.AccountID != r.native.Account() || status.Kind != "send" && status.Kind != r.kind || r.origin != "" && status.ID == r.origin) {
		return linuxnative.ToolStatus{}, errors.New("station_media_receipt_identity_mismatch")
	}
	return status, err
}

func (r stationMediaReceiptReader) LookupSend(ctx context.Context, key string) (linuxnative.ToolStatus, error) {
	return r.validate(r.native.LookupSend(ctx, key))
}

func (r stationMediaReceiptReader) ToolSendStatus(ctx context.Context, id string) (linuxnative.ToolStatus, error) {
	return r.validate(r.native.ToolSendStatus(ctx, id))
}
