package bot

import (
	"context"
	"errors"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

// ErrDeliveryCancelled means the native call was never entered. It must never
// be used to classify a timeout from a native submission as safe to retry.
var ErrDeliveryCancelled = errors.New("delivery_cancelled_before_native_submission")

var errMediaAuthorityChanged = errors.New("media_source_or_authorization_changed")

func (w *Worker) guardedDeliverySubmit(ctx context.Context, in companion.Input, run func() error) error {
	if w.config.WithDeliverySubmit != nil {
		return w.config.WithDeliverySubmit(ctx, in, run)
	}
	return run()
}

// The actor owns queue changes; a host control journal never edits the queue.
func (w *Worker) applyDeliveryCancellations() {
	if w.runtime == nil || w.config.DeliveryCancelled == nil {
		return
	}
	for _, d := range w.runtime.Deliveries {
		if d.State != "accepted" && !d.Cancelled && !w.taskActive(d.Input) {
			if w.persistErr != nil {
				return
			}
			_, _ = w.cancelledDelivery(d)
		}
	}
	if native, ok := w.sender.(mediaNative); ok {
		for _, d := range w.runtime.Media {
			if d.State != "accepted" && !d.UserCancelled && !w.taskActive(d.Input) {
				if w.persistErr != nil {
					return
				}
				d.Paused = false
				w.cancelMedia(native, d, true, "task_cancelled_before_delivery")
			}
		}
	}
}

func (w *Worker) submitMedia(ctx context.Context, native mediaNative, d *mediaDelivery) (linuxnative.ToolStatus, error) {
	var status linuxnative.ToolStatus
	in := d.Input
	in.TraceID = d.Trace
	// Remote source lookup must not hold the cancellation fence. Recheck
	// local authority inside the fence immediately before native submission.
	if w.config.AuthorizeMediaSubmit != nil && !w.config.AuthorizeMediaSubmit(ctx, in, d.Kind, d.AssetID) {
		return status, errMediaAuthorityChanged
	}
	err := w.guardedDeliverySubmit(ctx, in, func() error {
		if w.config.AuthorizeMediaSubmit != nil && w.config.AuthorizeMediaAsset != nil && !w.config.AuthorizeMediaAsset(in, d.Kind, d.AssetID) {
			return errMediaAuthorityChanged
		}
		var err error
		status, err = native.Submit(ctx, linuxnative.ToolSend{URL: d.URL, Title: d.Title, Description: d.Description, ThreadID: d.Input.Thread, Kind: d.Kind, SourceID: d.SourceID, IdempotencyKey: d.Key})
		return err
	})
	return status, err
}
