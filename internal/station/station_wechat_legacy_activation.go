package station

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

const legacyCutoverLockFile = "legacy-cutover.lock"
const legacyCutoverClaimFile = "legacy-cutover-claim.json"

type handoverCheck uint8

const (
	handoverPreview handoverCheck = iota
	handoverClaim
	handoverActive
)

// legacyActivation is private host evidence, not a platform credential. The
// target dataset survives a restart. Transport ownership is checked anew.
type legacyActivation struct {
	ArchiveDigest      string                `json:"archive_digest"`
	ConfirmationDigest string                `json:"confirmation_digest"`
	Connection         apphost.ConnectionRef `json:"connection"`
	OldRoot            string                `json:"old_root"`
	StopDigest         string                `json:"stop_digest"`
	ProfileDigest      string                `json:"profile_digest"`
	TargetDataset      string                `json:"target_dataset"`
	ConfirmedAt        time.Time             `json:"confirmed_at"`
}

func (a legacyActivation) valid() bool {
	return legacyMemoryDigestValid(a.ArchiveDigest) && legacyMemoryDigestValid(a.ConfirmationDigest) &&
		legacyMemoryDigestValid(a.StopDigest) && legacyMemoryDigestValid(a.ProfileDigest) &&
		filepath.IsAbs(a.OldRoot) && filepath.Clean(a.OldRoot) == a.OldRoot &&
		identity.ValidPathID(a.Connection.AccountID) && identity.ValidPathID(a.TargetDataset) && !a.ConfirmedAt.IsZero()
}

type legacyActivationView struct {
	legacyCutover
	Status                    string `json:"status"`
	Digest                    string `json:"digest,omitempty"`
	Reason                    string `json:"reason,omitempty"`
	ReplyCount                int    `json:"reply_count"`
	PendingReceipts           int    `json:"pending_receipts"`
	ManagementPendingReceipts int    `json:"management_pending_receipts,omitempty"`
	ModelReady                bool   `json:"model_ready"`
}

type legacyActivationInput struct {
	Digest           string `json:"digest"`
	SameIdentity     bool   `json:"same_identity"`
	ReviewedReceipts bool   `json:"reviewed_receipts"`
}

type legacyActivationSource struct {
	*linuxnative.Sender
	check func() error
}

func (s legacyActivationSource) Events(ctx context.Context, after string) (linuxnative.EventPage, error) {
	if err := s.check(); err != nil {
		return linuxnative.EventPage{}, err
	}
	return s.Sender.Events(ctx, after)
}

func (s *stationWechat) legacyActivationRoutes(server *websession.Store, mux *http.ServeMux, path string) {
	handle := server.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var input legacyActivationInput
		if r.Method == http.MethodPost {
			if !websession.Decode(w, r, &input) {
				return
			}
			if !legacyMemoryDigestValid(input.Digest) || !input.SameIdentity {
				websession.Fail(w, http.StatusBadRequest, "请重新核对启用条件并确认实际微信身份")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		view, err := s.legacyActivation(ctx, r.PathValue("connection"), input)
		switch {
		case err != nil:
			legacyImportFailure(w, err)
		case input.Digest != "" && view.Status != "confirmed":
			websession.Fail(w, http.StatusConflict, view.Reason)
		default:
			websession.Respond(w, http.StatusOK, view)
		}
	})
	mux.HandleFunc("GET "+path, handle)
	mux.HandleFunc("POST "+path, handle)
}

func (s *stationWechat) legacyActivation(ctx context.Context, id string, input legacyActivationInput) (legacyActivationView, error) {
	view := legacyActivationView{Status: "blocked", legacyCutover: legacyCutover{
		LegacyHost: "pending", NativeProfile: "pending", TargetLogin: "pending", TargetIdentity: "owner_check_pending",
	}}
	blocked := func(reason string) (legacyActivationView, error) {
		view.Reason = reason
		return view, nil
	}
	archive, err := s.legacyReceiptArchive(id)
	if err != nil {
		return view, err
	}
	for _, receipt := range archive.Receipts {
		if legacyReceiptPending(receipt) {
			view.PendingReceipts++
		}
	}
	for _, receipt := range archive.ManagementReceipts {
		if legacyReceiptPending(receipt) {
			view.ManagementPendingReceipts++
		}
	}
	native, err := s.WorkerSender(ctx, id)
	if err != nil || native.Account() != archive.Plan.Account {
		return blocked("当前微信原生连接暂不可核对")
	}
	w, err := s.worker(id, false)
	if err != nil {
		return view, err
	}
	if w == nil {
		return blocked("请先完成旧版接入并保存回复范围")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.policy.RollbackFrozenAt.IsZero() {
		return blocked("此连接已为回退停发，请先查看回退核对")
	}
	if w.policy.LegacyArchiveDigest != "" && w.policy.LegacyArchiveDigest != archive.Plan.Digest {
		return blocked("旧接入摘要与工作器记录不一致")
	}
	_, modelErr := s.modelPersona()
	view.ModelReady, view.ReplyCount = modelErr == nil, len(w.policy.Grants)
	daemon, err := native.DaemonStatus(ctx)
	if err != nil {
		return blocked("当前微信原生实例暂不可核对")
	}
	account := native.Account()
	status, _ := native.Status(ctx)
	view.TargetLogin = targetLoginState(status, account)
	view.NativeProfile, _ = s.nativeImportEvidence(id, account, status)
	if prior := w.policy.LegacyActivation; prior != nil && s.activationMatches(*prior, id, archive.Plan, daemon.ServerID) {
		view.LegacyHost, view.TargetIdentity = "stop_record_matched", "owner_confirmed"
		if input.Digest != "" && input.Digest != prior.ConfirmationDigest {
			return blocked("启用确认记录已变化，请重新核对")
		}
		if reason := activationMissing(view); reason != "" {
			return blocked(reason)
		}
		view.Status = "confirmed"
		return view, nil
	}
	// A disabled import is already dormant. Saving its first reply scope can
	// leave Paused false; only an enabled worker must be explicitly paused.
	if w.run != nil || (w.policy.Enabled && !w.policy.Paused) {
		return blocked("请先暂停此连接并等待工作器退出")
	}
	snapshot, source, err := s.openLegacyImport()
	if errors.Is(err, errLegacySnapshotIntegrity) {
		return blocked(errLegacySnapshotIntegrity.Error())
	}
	if err != nil {
		return blocked("请保留原隔离快照及旧宿主目录，完成启用核对后再拆离快照")
	}
	defer snapshot.Close()
	if !source.matchesArchive(archive) {
		return blocked(errLegacySnapshotIntegrity.Error())
	}
	latest, err := s.previewLegacyImportFrom(ctx, id, native, daemon, snapshot, source)
	if errors.Is(err, errLegacySnapshotIntegrity) {
		return blocked(errLegacySnapshotIntegrity.Error())
	}
	cutover, evidence := s.checkLegacyCutover(ctx, id, native, snapshot, source, archive.LegacyServer)
	view.legacyCutover = *cutover
	if reason := activationMissing(view); reason != "" {
		return blocked(reason)
	}
	// The original archive stays authoritative. If the stopped source now
	// shows different delivery evidence or a different legacy policy, an
	// activation cannot silently abandon newly discovered records.
	if err != nil || !legacyMemoryCutoverReady(latest.Plan.Cutover) || !sameLegacyActivationSnapshot(archive, latest) {
		return blocked("旧投递快照或账号规则与已保存的接入摘要不同，请先核对差异")
	}
	proof := legacyActivation{
		ArchiveDigest: archive.Plan.Digest, Connection: s.connectionRef(id, account), OldRoot: cutoverPath(source.LegacyHostRoot),
		StopDigest: hex.EncodeToString(evidence[0][:]), ProfileDigest: hex.EncodeToString(evidence[1][:]), TargetDataset: daemon.ServerID,
		ConfirmedAt: time.Now().UTC(),
	}
	if reason := s.legacyHandover(proof, handoverPreview); reason != "" {
		return blocked(reason)
	}
	proof.ConfirmationDigest = localCheckpointKey("legacy-activation-v1", proof.ArchiveDigest, proof.Connection,
		proof.OldRoot, proof.StopDigest, proof.ProfileDigest, daemon, latest.Plan.Digest, w.policy.Grants, view.PendingReceipts, view.ManagementPendingReceipts)
	if input.Digest == "" {
		view.Status, view.Digest = "ready", proof.ConfirmationDigest
		return view, nil
	}
	if input.Digest != proof.ConfirmationDigest {
		return blocked("启用条件已变化，请重新核对")
	}
	if (view.PendingReceipts != 0 || view.ManagementPendingReceipts != 0) && !input.ReviewedReceipts {
		return blocked("请确认未核对的旧回执只保留归档")
	}
	fresh, daemonErr := native.DaemonStatus(ctx)
	login, loginErr := native.Status(ctx)
	profile, profileDigest := s.nativeImportReceipt(id, account)
	if daemonErr != nil || fresh != daemon || loginErr != nil || login.StorageMode != "managed" || targetLoginState(login, account) != "online" ||
		profile != "import_receipt_matched" || hex.EncodeToString(profileDigest[:]) != proof.ProfileDigest || ctx.Err() != nil || s.ctx.Err() != nil {
		return blocked("原生实例或切换证据已变化，请重新核对")
	}
	if reason := s.legacyHandover(proof, handoverClaim); reason != "" {
		return blocked(reason)
	}
	next := w.policy
	next.LegacyArchiveDigest, next.LegacyActivation = archive.Plan.Digest, &proof
	if err := w.save(next); err != nil {
		return view, err
	}
	view.Status, view.TargetIdentity = "confirmed", "owner_confirmed"
	return view, nil
}

func activationMissing(view legacyActivationView) string {
	switch {
	case view.LegacyHost != "stop_record_matched":
		return "旧宿主停机记录缺失或已变化，请核对旧桌面已正常停止"
	case view.NativeProfile != "import_receipt_matched":
		return "微信原生档案的导入凭证尚未匹配"
	case view.TargetLogin != "online":
		return "目标微信连接尚未在线"
	case !view.ModelReady:
		return "请先完成本机模型配置"
	case view.ReplyCount == 0:
		return "请先保存明确的回复范围"
	default:
		return ""
	}
}

func sameLegacyActivationSnapshot(saved, current legacyImportArchive) bool {
	return saved.LegacyServer == current.LegacyServer && saved.Plan.Account == current.Plan.Account &&
		(saved.SnapshotManifestDigest == "" || saved.SnapshotManifestDigest == current.SnapshotManifestDigest) &&
		(saved.SourceRevision == "" || saved.SourceRevision == current.SourceRevision) &&
		saved.Plan.LegacyEnabled == current.Plan.LegacyEnabled && saved.Plan.LegacyPaused == current.Plan.LegacyPaused &&
		slices.Equal(saved.Plan.SuggestedThreads, current.Plan.SuggestedThreads) && slices.Equal(saved.Plan.Omitted, current.Plan.Omitted) &&
		localCheckpointKey(saved.Receipts) == localCheckpointKey(current.Receipts) &&
		localCheckpointKey(saved.ManagementReceipts) == localCheckpointKey(current.ManagementReceipts) &&
		(len(saved.ReceiptSources) == 0 || localCheckpointKey(saved.ReceiptSources) == localCheckpointKey(current.ReceiptSources))
}

func (s *stationWechat) activationMatches(a legacyActivation, id string, plan legacyImportPlan, dataset string) bool {
	return a.valid() && a.ArchiveDigest == plan.Digest && a.Connection == s.connectionRef(id, plan.Account) && a.TargetDataset == dataset &&
		s.localActivationEvidence(id, a)
}

func (s *stationWechat) localActivationEvidence(id string, a legacyActivation) bool {
	if s.legacyHandover(a, handoverActive) != "" {
		return false
	}
	profile, digest := s.nativeImportReceipt(id, a.Connection.AccountID)
	return profile == "import_receipt_matched" && hex.EncodeToString(digest[:]) == a.ProfileDigest
}

// The legacy desktop holds the same byte-range lock from startup until its
// children have stopped. A durable claim then fences legacy startup even while
// the station is stopped or its native jobs outlive its worker. Only the same
// target may recover a claim left by an interrupted policy write.
func (s *stationWechat) legacyHandover(a legacyActivation, check handoverCheck) string {
	if legacyImportOverlap(a.OldRoot, s.dataDir) || legacyImportOverlap(a.OldRoot, s.workspaceRoot) {
		return "旧宿主目录与新站目录重叠"
	}
	root, err := workspacefs.OpenPath(a.OldRoot)
	if err != nil {
		return "无法读取旧宿主目录"
	}
	defer root.Close()
	if check == handoverClaim {
		unlock, err := hostfile.TryLock(filepath.Join(a.OldRoot, legacyCutoverLockFile))
		if err != nil {
			return "旧宿主正在运行、处理交接，或交接锁不可用"
		}
		defer unlock()
	}
	raw, err := readLegacyImportFile(root, legacyStopMarkerFile, 16<<10)
	if err != nil || hexDigest(raw) != a.StopDigest {
		return "旧宿主停机记录已变化，请重新核对"
	}
	var marker legacyStopMarker
	if json.Unmarshal(raw, &marker) != nil || marker.Version != 2 || marker.State != "stopped" || !marker.validMode() ||
		marker.AccountID != a.Connection.AccountID || !sameCutoverPath(marker.DataRoot, a.OldRoot) {
		return "旧桌面不支持互斥交接，请用支持交接的版本正常停止并更新快照"
	}
	key := legacyHandoverKey(a)
	raw, err = readLegacyImportFile(root, legacyCutoverClaimFile, 512)
	if err == nil {
		var saved string
		if json.Unmarshal(raw, &saved) == nil && saved == key {
			return ""
		}
		return "旧宿主已有其他目标的交接记录，或记录无效"
	}
	if !errors.Is(err, os.ErrNotExist) || check == handoverActive {
		return "旧宿主的交接记录缺失或无法读取"
	}
	if check == handoverClaim {
		temporary := ".legacy-cutover-" + rand.Text() + ".tmp"
		defer root.Remove(temporary)
		if err := writeLegacyStagedFile(root, temporary, key); err != nil {
			return "无法保存旧宿主的交接记录"
		}
		if err := root.Rename(temporary, legacyCutoverClaimFile); err != nil {
			return "无法启用旧宿主的交接记录"
		}
	}
	return ""
}

func legacyHandoverKey(a legacyActivation) string {
	return localCheckpointKey("legacy-cutover-claim-v1", a.ArchiveDigest, a.Connection, a.StopDigest, a.ProfileDigest, a.TargetDataset)
}

func hexDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Called while the worker is locked, before any automatic processing begins.
func (s *stationWechat) legacyActivationGuard(ctx context.Context, id string, native *linuxnative.Sender, w *stationWechatWorker) (func() bool, error) {
	root, err := workspacefs.OpenPath(w.dir)
	if err != nil {
		return nil, apphost.ErrLifecycleCutover
	}
	defer root.Close()
	return s.legacyActivationCheck(ctx, id, native, root, w.policy)
}

func (s *stationWechat) legacyActivationCheck(ctx context.Context, id string, native *linuxnative.Sender, root *os.Root, policy stationWechatPolicy) (func() bool, error) {
	if !policy.RollbackFrozenAt.IsZero() {
		return nil, apphost.ErrLifecycleCutover
	}
	archive, err := readLegacyImportArchive(root)
	if errors.Is(err, os.ErrNotExist) && policy.LegacyActivation == nil && policy.LegacyArchiveDigest == "" {
		return nil, s.checkUnarchivedNativeProfile(id)
	}
	if err != nil || policy.LegacyActivation == nil || archive.Plan.Account != native.Account() ||
		policy.LegacyArchiveDigest != archive.Plan.Digest {
		return nil, apphost.ErrLifecycleCutover
	}
	proof := *policy.LegacyActivation
	daemon, err := native.DaemonStatus(ctx)
	if err != nil || !s.activationMatches(proof, id, archive.Plan, daemon.ServerID) {
		return nil, apphost.ErrLifecycleCutover
	}
	status, err := native.Status(ctx)
	if err != nil || status.AccountID != native.Account() || status.StorageMode != "managed" {
		return nil, apphost.ErrLifecycleCutover
	}
	return func() bool { return s.localActivationEvidence(id, proof) }, nil
}

func (s *stationWechat) checkUnarchivedNativeProfile(id string) error {
	// An import receipt can exist before the worker archive is applied. Only a
	// connection without any such receipt can take the fresh-account path.
	root, err := workspacefs.OpenPath(filepath.Dir(s.root))
	if err != nil {
		return apphost.ErrLifecycleCutover
	}
	defer root.Close()
	if _, err := root.Lstat("native-import-" + id + ".json"); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return apphost.ErrLifecycleCutover
}

// Use the on-disk policy at native dispatch. Taking worker.mu here would
// deadlock shutdown, which holds it while waiting for the actor to exit.
func (s *stationWechat) admitPlatformWrite(ctx context.Context, id string, native *linuxnative.Sender) error {
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return apphost.ErrLifecycleConflict
	}
	root, err := workspacefs.OpenPath(filepath.Join(s.root, id))
	if errors.Is(err, os.ErrNotExist) {
		return s.checkUnarchivedNativeProfile(id)
	}
	if err != nil {
		return apphost.ErrLifecycleCutover
	}
	defer root.Close()
	policy, err := readStationWechatPolicy(root)
	if err != nil {
		return apphost.ErrLifecycleCutover
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = s.legacyActivationCheck(ctx, id, native, root, policy)
	return err
}

func (s *stationWechat) WorkerSender(ctx context.Context, id string) (*linuxnative.Sender, error) {
	native, err := s.stationWechatNative.WorkerSender(ctx, id)
	if err != nil {
		return nil, err
	}
	native.GuardChanges(func(ctx context.Context) (func(), error) {
		w, err := s.worker(id, false)
		if err != nil {
			return nil, err
		}
		if w == nil {
			return nil, s.admitPlatformWrite(ctx, id, native)
		}
		w.platformMu.RLock()
		if err := s.admitPlatformWrite(ctx, id, native); err != nil {
			w.platformMu.RUnlock()
			return nil, err
		}
		return w.platformMu.RUnlock, nil
	})
	return native, nil
}
