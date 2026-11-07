package station

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

const rollbackAuditFile = "rollback-audit.json"
const stationNativeStopFile = "station-native-stop.json"

type stationNativeStop struct {
	Version                   int       `json:"version"`
	State                     string    `json:"state"`
	Epoch                     string    `json:"epoch"`
	DataRoot                  string    `json:"data_root"`
	NativeServerID            string    `json:"native_server_id"`
	NativeOwnershipGeneration string    `json:"native_ownership_generation"`
	At                        time.Time `json:"at"`
}

type rollbackAudit struct {
	Version          int                     `json:"version"`
	Connection       apphost.ConnectionRef   `json:"connection"`
	FrozenAt         time.Time               `json:"frozen_at"`
	CapturedAt       time.Time               `json:"captured_at"`
	Checkpoint       string                  `json:"checkpoint"`
	ActionSnapshot   string                  `json:"action_snapshot"`
	TargetDataset    string                  `json:"target_dataset"`
	NativeState      string                  `json:"native_state"`
	DeliveryDataset  string                  `json:"delivery_dataset,omitempty"`
	DesktopEpoch     string                  `json:"desktop_epoch,omitempty"`
	NativeGeneration string                  `json:"native_generation,omitempty"`
	Deliveries       []rollbackReceipt       `json:"deliveries"`
	Actions          []apphost.ActionReceipt `json:"actions"`
}

type rollbackAuditView struct {
	Digest       string    `json:"digest"`
	CapturedAt   time.Time `json:"captured_at"`
	Deliveries   int       `json:"delivery_count"`
	Actions      int       `json:"action_count"`
	Active       int       `json:"active_count"`
	Unknown      int       `json:"unknown_count"`
	NativeState  string    `json:"native_state"`
	DesktopBound bool      `json:"desktop_bound"`
	LocalCurrent bool      `json:"local_current"`
}

func (a rollbackAudit) summary(digest string) rollbackAuditView {
	view := rollbackAuditView{Digest: digest, CapturedAt: a.CapturedAt, Deliveries: len(a.Deliveries), Actions: len(a.Actions),
		NativeState: a.NativeState, DesktopBound: a.DesktopEpoch != "" && a.NativeGeneration != "", LocalCurrent: true}
	for _, row := range a.Deliveries {
		switch row.Disposition {
		case "active":
			view.Active++
		case "unknown":
			view.Unknown++
		}
	}
	for _, row := range a.Actions {
		switch row.State {
		case "succeeded", "failed", "rejected", "cancelled":
		case "submitted_pending":
			if row.Lookup == "platform" {
				view.Active++
			} else {
				view.Unknown++
			}
		default:
			view.Unknown++
		}
	}
	return view
}

func (s *stationWechat) legacyRollbackAuditRoutes(owner *websession.Store, mux *http.ServeMux, path string) {
	handle := owner.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		capture := r.Method == http.MethodPost
		if capture && !websession.Decode(w, r, &struct{}{}) {
			return
		}
		digest := r.URL.Query().Get("digest")
		if digest != "" && (capture || !legacyMemoryDigestValid(digest)) {
			websession.Fail(w, http.StatusBadRequest, "回退核对摘要无效")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		view, err := s.legacyRollbackAudit(ctx, r.PathValue("connection"), capture, digest)
		switch {
		case errors.Is(err, apphost.ErrNotFound):
			websession.Fail(w, http.StatusNotFound, "此连接没有保存的回退核对记录")
		case errors.Is(err, apphost.ErrLifecycleConflict), errors.Is(err, errLegacyImportChanged):
			websession.Fail(w, http.StatusConflict, "请等待停发排空并重新核对。采集期间本地记录变化时需重新保存")
		case err != nil:
			websession.Fail(w, http.StatusServiceUnavailable, "暂时无法完整保存或读取回退核对记录")
		default:
			websession.Respond(w, http.StatusOK, view)
		}
	})
	mux.HandleFunc("GET "+path, handle)
	mux.HandleFunc("POST "+path, handle)
}

func (s *stationWechat) legacyRollbackAudit(ctx context.Context, id string, capture bool, digest string) (rollbackAuditView, error) {
	var view rollbackAuditView
	target, err := s.rollbackTarget(ctx, id, false)
	if err != nil {
		return view, err
	}
	if target.State != "frozen" || !target.worker.rollbackAuditMu.TryLock() {
		return view, apphost.ErrLifecycleConflict
	}
	defer target.worker.rollbackAuditMu.Unlock()
	root, err := workspacefs.OpenPath(target.worker.dir)
	if err != nil {
		return view, err
	}
	defer root.Close()
	if !capture {
		raw, err := readRollbackAudit(root, digest)
		if errors.Is(err, os.ErrNotExist) {
			return view, apphost.ErrNotFound
		}
		var audit rollbackAudit
		if err != nil || json.Unmarshal(raw, &audit) != nil || !audit.matchesTarget(target) {
			return view, apphost.ErrUnavailable
		}
		view = audit.summary(hexDigest(raw))
		view.LocalCurrent, err = s.rollbackAuditCurrent(ctx, target, audit)
		return view, err
	}
	audit, err := s.captureRollbackAudit(ctx, target)
	if err != nil {
		return view, err
	}
	if current, err := s.rollbackAuditCurrent(ctx, target, audit); err != nil {
		return view, err
	} else if !current || ctx.Err() != nil || s.ctx.Err() != nil {
		return view, errLegacyImportChanged
	}
	raw, err := json.Marshal(audit)
	if err != nil || len(raw) > legacyImportArchiveLimit {
		return view, apphost.ErrUnavailable
	}
	if err := saveRollbackAudit(root, raw); err != nil {
		return view, err
	}
	return audit.summary(hexDigest(raw)), nil
}

func readRollbackAudit(root *os.Root, digest string) ([]byte, error) {
	if digest != "" && !legacyMemoryDigestValid(digest) {
		return nil, apphost.ErrNotFound
	}
	raw, err := readLegacyImportFile(root, rollbackAuditFile, legacyImportArchiveLimit)
	if digest == "" || err == nil && hexDigest(raw) == digest {
		return raw, err
	}
	raw, err = readLegacyImportFile(root, "rollback-audit-"+digest+".json", legacyImportArchiveLimit)
	if err == nil && hexDigest(raw) != digest {
		return nil, apphost.ErrUnavailable
	}
	return raw, err
}

func saveRollbackAudit(root *os.Root, raw []byte) error {
	prior, err := readRollbackAudit(root, "")
	if err == nil {
		// Preserve earlier verified server receipts if a later offline capture
		// cannot reproduce the same platform observations.
		archive := "rollback-audit-" + hexDigest(prior) + ".json"
		found, readErr := readLegacyImportFile(root, archive, legacyImportArchiveLimit)
		switch {
		case errors.Is(readErr, os.ErrNotExist):
			if err := replaceRollbackAudit(root, archive, prior); err != nil {
				return err
			}
		case readErr != nil || !bytes.Equal(prior, found):
			return apphost.ErrUnavailable
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return replaceRollbackAudit(root, rollbackAuditFile, raw)
}

func replaceRollbackAudit(root *os.Root, name string, raw []byte) error {
	temporary := ".rollback-audit-" + identity.RandomToken()
	defer root.Remove(temporary)
	if err := writeLegacyStagedFile(root, temporary, json.RawMessage(raw)); err != nil {
		return err
	}
	return root.Rename(temporary, name)
}

func (s *stationWechat) captureRollbackAudit(ctx context.Context, target legacyRollbackTarget) (rollbackAudit, error) {
	var audit rollbackAudit
	checkpoint, err := target.checkpoint("", math.MaxInt)
	if err != nil {
		return audit, err
	}
	native, daemon, state := s.rollbackNative(ctx, target)
	epoch := s.runningDesktopEpoch()
	deliveryNative := native
	if checkpoint.rows.ServerID != target.dataset {
		deliveryNative = nil
	}
	lookup, cancel := context.WithTimeout(ctx, 8*time.Second)
	deliveries, err := checkpoint.project(lookup, target.ref, deliveryNative)
	cancel()
	if err != nil {
		return audit, err
	}
	lookup, cancel = context.WithTimeout(ctx, 8*time.Second)
	actions, actionSnapshot, err := s.rollbackActionEvidence(lookup, target.worker, target.ref, native)
	cancel()
	if err != nil {
		return audit, err
	}
	audit = rollbackAudit{Version: 1, Connection: target.ref, FrozenAt: target.FrozenAt, CapturedAt: time.Now().UTC(),
		Checkpoint: checkpoint.digest, ActionSnapshot: actionSnapshot, TargetDataset: target.dataset,
		NativeState: state, DeliveryDataset: checkpoint.rows.ServerID, Deliveries: deliveries, Actions: actions}
	if epoch != "" && native != nil {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		latest, err := native.DaemonStatus(probe)
		cancel()
		if err == nil && latest == daemon && sameStationNativeRoot(s.dataDir, daemon) && s.runningDesktopEpoch() == epoch {
			audit.DesktopEpoch, audit.NativeGeneration = epoch, daemon.OwnershipGeneration
		}
	}
	return audit, nil
}

func (a rollbackAudit) matchesTarget(target legacyRollbackTarget) bool {
	if a.Version != 1 || a.Connection != target.ref || !a.FrozenAt.Equal(target.FrozenAt) || a.TargetDataset != target.dataset ||
		!legacyMemoryDigestValid(a.Checkpoint) || !legacyMemoryDigestValid(a.ActionSnapshot) || a.CapturedAt.Before(a.FrozenAt) {
		return false
	}
	for _, row := range a.Deliveries {
		if row.ConnectionRef != target.ref || !identity.ValidPathID(row.IdempotencyKey) {
			return false
		}
	}
	for _, row := range a.Actions {
		if row.ConnectionRef != target.ref || !identity.ValidPathID(row.OperationID) {
			return false
		}
	}
	return true
}

func (s *stationWechat) rollbackAuditCurrent(ctx context.Context, target legacyRollbackTarget, audit rollbackAudit) (bool, error) {
	checkpoint, err := target.checkpoint("", 1)
	if err != nil {
		return false, err
	}
	_, actions, err := s.rollbackActionEvidence(ctx, target.worker, target.ref, nil)
	return checkpoint.digest == audit.Checkpoint && actions == audit.ActionSnapshot, err
}

func (s *stationWechat) runningDesktopEpoch() string {
	root, err := workspacefs.OpenPath(s.dataDir)
	if err != nil {
		return ""
	}
	defer root.Close()
	raw, err := readLegacyImportFile(root, stationNativeStopFile, 16<<10)
	var marker stationNativeStop
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Version != 1 || marker.State != "starting" ||
		len(marker.Epoch) != 32 || strings.Trim(marker.Epoch, "0123456789abcdef") != "" || !sameCutoverPath(s.dataDir, marker.DataRoot) ||
		marker.At.IsZero() || marker.At.After(time.Now().Add(time.Minute)) {
		return ""
	}
	return marker.Epoch
}

func sameStationNativeRoot(root string, daemon linuxnative.DaemonStatus) bool {
	return sameCutoverPath(filepath.Join(root, "apps", "wechat", "native"), daemon.DataDirectory)
}
