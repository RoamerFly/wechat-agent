package station

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/workspacefs"
)

const stationNativeRunLockFile = "station-native-run.lock"
const stationNativeReleaseFile = "station-native-release.json"

type LegacyWechatReleaseRequest struct {
	DataDir, ConnectionID, AuditDigest, ApplyDigest string
	AcceptUnknown                                   bool
}

type LegacyWechatReleaseResult struct {
	State        string `json:"state"`
	ConnectionID string `json:"connection"`
	AuditDigest  string `json:"audit_digest"`
	ApplyDigest  string `json:"apply_digest"`
	Unknown      int    `json:"unknown_count"`
	NativeFenced bool   `json:"native_runtime_fenced"`
}

type legacyReleaseIntent struct {
	Version       int    `json:"version"`
	ConnectionID  string `json:"connection"`
	AuditDigest   string `json:"audit_digest"`
	ApplyDigest   string `json:"apply_digest"`
	ClaimDigest   string `json:"claim_digest"`
	AcceptUnknown bool   `json:"accept_unknown"`
}

// ReleaseLegacyWechat performs an offline handoff. Both desktop lifetime
// leases stay held until the new native fence and old claim agree.
func ReleaseLegacyWechat(ctx context.Context, req LegacyWechatReleaseRequest) (LegacyWechatReleaseResult, error) {
	var out LegacyWechatReleaseResult
	if runtime.GOOS != "windows" {
		return out, errors.New("微信交接解除仅支持 Windows 桌面")
	}
	dataDir := cutoverPath(req.DataDir)
	if !sameCutoverPath(dataDir, dataDir) || !identity.ValidPathID(req.ConnectionID) || !legacyMemoryDigestValid(req.AuditDigest) ||
		req.ApplyDigest != "" && !legacyMemoryDigestValid(req.ApplyDigest) {
		return out, errors.New("数据目录、连接或核对摘要无效")
	}
	stationRoot, err := workspacefs.OpenPath(dataDir)
	if err != nil {
		return out, errors.New("无法打开成长站数据目录")
	}
	defer stationRoot.Close()
	unlockStation, err := lockReleaseRoot(stationRoot, dataDir, stationNativeRunLockFile)
	if err != nil {
		return out, errors.New("成长站微信原生服务仍在使用，或停机锁不可用")
	}
	defer unlockStation()
	workerDir := filepath.Join("apps", wechatapp.ID, "workers")
	worker, err := workspacefs.OpenExistingDirectory(stationRoot, filepath.Join(workerDir, req.ConnectionID))
	if err != nil {
		return out, errors.New("找不到此微信连接的本地交接资料")
	}
	defer worker.Close()
	host := stationWechat{root: filepath.Join(dataDir, workerDir), dataDir: dataDir, installation: "wechat-station-native"}
	policy, policyErr := readStationWechatPolicy(worker)
	archive, archiveErr := readLegacyImportArchive(worker)
	proof := policy.LegacyActivation
	ref := host.connectionRef(req.ConnectionID, archive.Plan.Account)
	if policyErr != nil || archiveErr != nil || proof == nil || !proof.valid() || !policy.Paused || policy.RollbackFrozenAt.IsZero() ||
		req.ConnectionID != wechatapp.ConnectionID(ref.AccountID) || proof.Connection != ref || proof.ArchiveDigest != archive.Plan.Digest ||
		policy.LegacyArchiveDigest != archive.Plan.Digest || legacyImportOverlap(dataDir, proof.OldRoot) {
		return out, errors.New("此连接没有已冻结且身份一致的微信交接")
	}
	oldRoot, err := workspacefs.OpenPath(proof.OldRoot)
	if err != nil {
		return out, errors.New("无法打开旧微信桌面数据目录")
	}
	defer oldRoot.Close()
	unlockOld, err := lockReleaseRoot(oldRoot, proof.OldRoot, legacyCutoverLockFile)
	if err != nil {
		return out, errors.New("旧微信桌面仍在使用，或交接锁不可用")
	}
	defer unlockOld()

	target := legacyRollbackTarget{
		legacyRollbackView: legacyRollbackView{State: "frozen", Eligible: true, FrozenAt: policy.RollbackFrozenAt},
		worker:             &stationWechatWorker{dir: filepath.Join(host.root, req.ConnectionID)}, ref: ref, dataset: proof.TargetDataset,
	}
	auditRaw, auditErr := readRollbackAudit(worker, req.AuditDigest)
	var audit rollbackAudit
	if auditErr != nil || json.Unmarshal(auditRaw, &audit) != nil || !audit.matchesTarget(target) {
		return out, errors.New("找不到与此冻结连接匹配的核对报告")
	}
	markerRaw, markerErr := readLegacyImportFile(stationRoot, stationNativeStopFile, 16<<10)
	var marker stationNativeStop
	if markerErr != nil || json.Unmarshal(markerRaw, &marker) != nil {
		return out, errors.New("缺少成长站微信原生服务的正常停机记录")
	}
	counts := audit.summary(req.AuditDigest)
	if counts.Active != 0 {
		return out, errors.New("还有在途投递或群操作，须重新核对至终态后才能解除")
	}
	if counts.Unknown != 0 && !req.AcceptUnknown {
		return out, errors.New("仍有结果未知的投递或群操作，请核对报告后明确确认")
	}
	if validateRollbackReleaseEvidence(target, audit, marker, dataDir, req.AcceptUnknown) != nil {
		return out, errors.New("核对报告与成长站正常停机记录不匹配")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	current, err := host.rollbackAuditCurrent(ctx, target, audit)
	if err != nil || !current {
		return out, errors.New("保存报告后的本地投递或群操作记录发生变化，请重新核对")
	}
	profile, digest := host.nativeImportReceipt(req.ConnectionID, ref.AccountID)
	if profile != "import_receipt_matched" || hex.EncodeToString(digest[:]) != proof.ProfileDigest {
		return out, errors.New("成长站的原生登录导入记录已变化")
	}
	return releaseLegacyClaim(ctx, req, stationRoot, oldRoot, *proof, archive.LegacyServer, hexDigest(markerRaw), counts.Unknown)
}

func lockReleaseRoot(root *os.Root, path, name string) (func(), error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("missing or invalid desktop lease")
	}
	unlock, err := hostfile.TryLock(filepath.Join(path, name))
	if err != nil {
		return nil, err
	}
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(info, current) {
		unlock()
		return nil, errors.New("desktop lease changed")
	}
	return unlock, nil
}

func releaseLegacyClaim(ctx context.Context, req LegacyWechatReleaseRequest, stationRoot, oldRoot *os.Root, proof legacyActivation, oldServer, stopDigest string, unknown int) (LegacyWechatReleaseResult, error) {
	var out LegacyWechatReleaseResult
	key := legacyHandoverKey(proof)
	claimRaw, err := json.Marshal(key)
	if err != nil {
		return out, err
	}
	claimDigest := hexDigest(claimRaw)
	applyDigest := localCheckpointKey("wechat-rollback-release-v1", proof, req.AuditDigest, stopDigest, claimDigest, req.AcceptUnknown)
	if req.ApplyDigest != "" && req.ApplyDigest != applyDigest {
		return out, errors.New("执行令牌已变化，请重新预演")
	}
	intent := legacyReleaseIntent{Version: 1, ConnectionID: req.ConnectionID, AuditDigest: req.AuditDigest, ApplyDigest: applyDigest, ClaimDigest: claimDigest, AcceptUnknown: req.AcceptUnknown}
	intentRaw, intentErr := readLegacyImportFile(stationRoot, stationNativeReleaseFile, 4096)
	hasIntent := intentErr == nil
	var saved legacyReleaseIntent
	if hasIntent && (json.Unmarshal(intentRaw, &saved) != nil || saved != intent) || !hasIntent && !errors.Is(intentErr, os.ErrNotExist) {
		return out, errors.New("成长站已有不同或无法核对的微信解除记录")
	}
	archiveName := "legacy-cutover-released-" + applyDigest + ".json"
	archived, archiveErr := readLegacyImportFile(oldRoot, archiveName, 512)
	claim, claimErr := readLegacyImportFile(oldRoot, legacyCutoverClaimFile, 512)
	out = LegacyWechatReleaseResult{State: "ready", ConnectionID: req.ConnectionID, AuditDigest: req.AuditDigest, ApplyDigest: applyDigest, Unknown: unknown, NativeFenced: hasIntent}
	if archiveErr == nil {
		if !hasIntent || hexDigest(archived) != claimDigest || !errors.Is(claimErr, os.ErrNotExist) {
			return LegacyWechatReleaseResult{}, errors.New("旧微信桌面的解除存档与交接凭证不一致")
		}
		out.State = "released"
		return out, nil
	}
	if !errors.Is(archiveErr, os.ErrNotExist) || claimErr != nil || hexDigest(claim) != claimDigest {
		return LegacyWechatReleaseResult{}, errors.New("旧微信桌面的交接凭证缺失、变化或不可读")
	}
	oldStop, stopErr := readLegacyImportFile(oldRoot, legacyStopMarkerFile, 16<<10)
	var oldMarker legacyStopMarker
	if stopErr != nil || hexDigest(oldStop) != proof.StopDigest || json.Unmarshal(oldStop, &oldMarker) != nil ||
		oldMarker.Version != 2 || oldMarker.State != "stopped" || !oldMarker.validMode() || !validRollbackEpoch(oldMarker.Epoch) || oldMarker.At.IsZero() ||
		oldMarker.At.After(time.Now().Add(time.Minute)) || !sameCutoverPath(oldMarker.DataRoot, proof.OldRoot) ||
		oldMarker.AccountID != proof.Connection.AccountID || !identity.ValidPathID(oldMarker.NativeServerID) ||
		oldServer != "" && oldMarker.NativeServerID != oldServer {
		return LegacyWechatReleaseResult{}, errors.New("旧微信桌面的正常停机记录已变化")
	}
	if hasIntent {
		out.State = "resume_required"
	}
	if req.ApplyDigest == "" {
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return LegacyWechatReleaseResult{}, err
	}
	if !hasIntent {
		stage := ".station-native-release-" + rand.Text() + ".tmp"
		defer stationRoot.Remove(stage)
		if err := writeLegacyStagedFile(stationRoot, stage, intent); err != nil {
			return LegacyWechatReleaseResult{}, errors.New("无法保存成长站微信原生启动禁用记录")
		}
		if err := stationRoot.Rename(stage, stationNativeReleaseFile); err != nil {
			return LegacyWechatReleaseResult{}, errors.New("无法启用成长站微信原生启动禁用记录")
		}
	}
	if err := oldRoot.Rename(legacyCutoverClaimFile, archiveName); err != nil {
		return LegacyWechatReleaseResult{}, errors.New("成长站微信原生启动已禁用；旧交接凭证尚未归档，请用同一令牌续办")
	}
	out.State, out.NativeFenced = "released", true
	return out, nil
}
