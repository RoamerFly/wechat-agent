package station

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/websession"
)

type rollbackReceipt struct {
	apphost.Receipt
	Disposition string `json:"disposition"`
}

type legacyRollbackView struct {
	State     string            `json:"state"`
	Eligible  bool              `json:"eligible"`
	FrozenAt  time.Time         `json:"frozen_at,omitzero"`
	Snapshot  string            `json:"snapshot,omitempty"`
	NextAfter string            `json:"next_after,omitempty"`
	Native    string            `json:"native,omitempty"`
	Items     []rollbackReceipt `json:"items"`
}

type legacyRollbackTarget struct {
	legacyRollbackView
	worker  *stationWechatWorker
	ref     apphost.ConnectionRef
	dataset string
}

func (s *stationWechat) legacyRollbackRoutes(owner *websession.Store, mux *http.ServeMux, path string) {
	handle := owner.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		freeze := r.Method == http.MethodPost
		if freeze && !websession.Decode(w, r, &struct{}{}) {
			return
		}
		q := r.URL.Query()
		limit := 20
		if raw := q.Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > 200 {
				websession.Fail(w, http.StatusBadRequest, "每页请输入 1 到 200 条")
				return
			}
		}
		cursor := q.Get("after")
		revision, after, _ := strings.Cut(cursor, ":")
		if cursor != "" && (!legacyMemoryDigestValid(revision) || !identity.ValidPathID(after)) {
			websession.Fail(w, http.StatusBadRequest, "回退核对游标无效，请从第一页读取")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		view, err := s.legacyRollback(ctx, r.PathValue("connection"), freeze, revision, after, limit)
		switch {
		case errors.Is(err, apphost.ErrLifecycleCutover):
			websession.Fail(w, http.StatusConflict, "尚未确认该连接的接续，无法冻结回退")
		case errors.Is(err, errLegacyImportChanged):
			websession.Fail(w, http.StatusConflict, "本地投递记录已变化，请从第一页重新核对")
		case err != nil:
			legacyImportFailure(w, err)
		case freeze && view.State == "draining":
			websession.Respond(w, http.StatusAccepted, view)
		default:
			websession.Respond(w, http.StatusOK, view)
		}
	})
	mux.HandleFunc("GET "+path, handle)
	mux.HandleFunc("POST "+path, handle)
}

func (s *stationWechat) legacyRollback(ctx context.Context, id string, freeze bool, revision, after string, limit int) (legacyRollbackView, error) {
	target, err := s.rollbackTarget(ctx, id, freeze)
	if err != nil || freeze || target.State != "frozen" {
		return target.legacyRollbackView, err
	}
	return s.rollbackReceiptPage(ctx, target, revision, after, limit)
}

func (s *stationWechat) rollbackTarget(ctx context.Context, id string, freeze bool) (legacyRollbackTarget, error) {
	view := legacyRollbackTarget{legacyRollbackView: legacyRollbackView{State: "not_frozen", Items: []rollbackReceipt{}}}
	archive, err := s.legacyReceiptArchive(id)
	if err != nil {
		return view, err
	}
	w, err := s.worker(id, false)
	if err != nil {
		return view, err
	}
	if w == nil {
		return view, apphost.ErrNotFound
	}
	ref := s.connectionRef(id, archive.Plan.Account)
	view.worker, view.ref = w, ref
	w.mu.Lock()
	proof := w.policy.LegacyActivation
	view.Eligible = proof != nil && proof.Connection == ref && proof.ArchiveDigest == archive.Plan.Digest && w.policy.LegacyArchiveDigest == archive.Plan.Digest
	if !view.Eligible {
		w.mu.Unlock()
		if freeze {
			return view, apphost.ErrLifecycleCutover
		}
		return view, nil
	}
	if freeze && w.policy.RollbackFrozenAt.IsZero() {
		if ctx.Err() != nil || s.ctx.Err() != nil {
			w.mu.Unlock()
			return view, apphost.ErrLifecycleConflict
		}
		next := w.policy
		next.Paused, next.RollbackFrozenAt = true, time.Now().UTC()
		if err := w.changePolicy(next); err != nil {
			w.mu.Unlock()
			return view, err
		}
	}
	view.FrozenAt = w.policy.RollbackFrozenAt
	if view.FrozenAt.IsZero() {
		w.mu.Unlock()
		return view, nil
	}
	// The pause is durable even if the caller disconnects while the worker is
	// exiting. A subsequent read can observe the drain without changing state.
	run := w.run
	if freeze && run != nil {
		run.block.Store(true)
		run.cancel()
	}
	view.dataset = proof.TargetDataset
	w.mu.Unlock()
	workerStopped := run == nil
	if run != nil {
		select {
		case <-run.done:
			workerStopped = true
		default:
		}
	}
	view.State = "draining"
	if !workerStopped || !w.platformMu.TryLock() {
		return view, nil
	}
	// The persisted policy denies new platform requests. Acquiring this lock
	// proves previously admitted requests have returned and closed their bodies.
	w.platformMu.Unlock()
	view.State = "frozen"
	return view, nil
}

type rollbackCheckpoint struct {
	rows     bot.DeliveryEvidencePage
	stops    map[string]stationDeliveryStop
	controls map[string]stationResumeControl
	digest   string
}

func (target legacyRollbackTarget) checkpoint(after string, limit int) (rollbackCheckpoint, error) {
	w := target.worker
	w.deliveryMu.Lock()
	defer w.deliveryMu.Unlock()
	rows, err := bot.ReadDeliveryEvidencePage(filepath.Join(w.dir, "queue"), target.ref.AccountID, after, limit)
	stops, stopErr := w.readDeliveryStops()
	controls, controlErr := w.readResumeControls()
	if err != nil || stopErr != nil || controlErr != nil {
		return rollbackCheckpoint{}, apphost.ErrUnavailable
	}
	digest := localCheckpointKey("wechat-rollback-v1", target.ref, target.FrozenAt, target.dataset, rows.Snapshot, stops, controls)
	return rollbackCheckpoint{rows, stops, controls, digest}, nil
}

func (s *stationWechat) rollbackReceiptPage(ctx context.Context, target legacyRollbackTarget, revision, after string, limit int) (legacyRollbackView, error) {
	view := target.legacyRollbackView
	checkpoint, err := target.checkpoint(after, limit)
	if err != nil {
		return view, err
	}
	view.Snapshot = checkpoint.digest
	if revision != "" && revision != view.Snapshot {
		return view, errLegacyImportChanged
	}
	if len(checkpoint.rows.Keys) == 0 {
		return view, nil
	}
	if checkpoint.rows.ServerID != target.dataset {
		view.Native = "dataset_mismatch"
		view.Items, err = checkpoint.project(ctx, target.ref, nil)
	} else {
		native, _, state := s.rollbackNative(ctx, target)
		view.Native = state
		view.Items, err = checkpoint.project(ctx, target.ref, native)
	}
	if checkpoint.rows.NextAfter != "" {
		view.NextAfter = view.Snapshot + ":" + checkpoint.rows.NextAfter
	}
	return view, err
}

func (s *stationWechat) rollbackNative(ctx context.Context, target legacyRollbackTarget) (*linuxnative.Sender, linuxnative.DaemonStatus, string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	native, err := s.WorkerSender(ctx, target.ref.ConnectionID)
	if err != nil || native.Account() != target.ref.AccountID {
		return nil, linuxnative.DaemonStatus{}, "unavailable"
	}
	daemon, err := native.DaemonStatus(ctx)
	if err != nil {
		return nil, daemon, "unavailable"
	}
	if daemon.ServerID != target.dataset {
		return nil, daemon, "dataset_mismatch"
	}
	return native, daemon, "available"
}

func (c rollbackCheckpoint) project(ctx context.Context, ref apphost.ConnectionRef, native *linuxnative.Sender) ([]rollbackReceipt, error) {
	items := make([]rollbackReceipt, 0, len(c.rows.Keys))
	for _, key := range c.rows.Keys {
		if !identity.ValidPathID(key) {
			return nil, apphost.ErrUnavailable
		}
		// Keep the requested local page intact if the optional platform lookups
		// exhaust their deadline. The remaining records retain stored evidence.
		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		row := c.rows.Records[key]
		receipt, active := projectRollbackReceipt(lookupCtx, native, ref, key, row)
		cancel()
		_, stopped := c.stops[deliveryStopID(companion.Input{Account: ref.AccountID, Thread: row.Status.ThreadID, TraceID: row.Trace})]
		receipt.CancelRequested = receipt.CancelRequested || stopped
		if control, exists := c.controls[key]; exists {
			receipt.Continuation = resumeControlView(control)
		}
		items = append(items, rollbackReceipt{Receipt: receipt, Disposition: rollbackDisposition(receipt, active)})
	}
	return items, nil
}

func rollbackDisposition(receipt apphost.Receipt, active bool) string {
	switch {
	case active:
		return "active"
	case receipt.State == "accepted":
		return "accepted"
	case receipt.PartsIncomplete || len(receipt.UnconfirmedParts) != 0:
		return "unknown"
	case receipt.State == "failed" || receipt.State == "cancelled":
		return "terminal"
	case receipt.State == "not_submitted" && receipt.OperationID == "":
		return "not_submitted"
	default:
		return "unknown"
	}
}
