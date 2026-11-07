package station

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/workspacefs"
)

const legacyWechatSnapshotManifestFile = "legacy-wechat-snapshot.json"

type LegacyWechatSnapshotRequest struct {
	LegacyDataDir, SnapshotDir, PrimaryAccount, ApplyDigest string
	Verify, CheckSource                                     bool
}

type LegacyWechatSnapshotResult struct {
	State                     string `json:"state"`
	ApplyDigest               string `json:"apply_digest,omitempty"`
	ManifestDigest            string `json:"manifest_digest,omitempty"`
	Files                     int    `json:"file_count"`
	Accounts                  int    `json:"account_count"`
	MemoryFiles               int    `json:"memory_file_count"`
	DeliveryRecords           int    `json:"delivery_record_count"`
	ManagementDeliveryRecords int    `json:"management_delivery_record_count,omitempty"`
	PayloadBytes              int64  `json:"payload_bytes"`
}

type legacySnapshotEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Digest string `json:"sha256"`
}

type legacySnapshotManifest struct {
	Version                   int                   `json:"version"`
	Primary                   string                `json:"primary_account"`
	Accounts                  int                   `json:"account_count"`
	MemoryFiles               int                   `json:"memory_file_count"`
	DeliveryRecords           int                   `json:"delivery_record_count"`
	ManagementDeliveryRecords int                   `json:"management_delivery_record_count,omitempty"`
	Files                     []legacySnapshotEntry `json:"files"`
}

type legacySnapshotPlan struct {
	manifest  legacySnapshotManifest
	projected map[string][]byte
	source    map[string]string
}

// CaptureLegacyWechatSnapshot never starts either host. Capture holds the old
// desktop's byte-range lease throughout validation and publication.
func CaptureLegacyWechatSnapshot(ctx context.Context, req LegacyWechatSnapshotRequest) (LegacyWechatSnapshotResult, error) {
	var out LegacyWechatSnapshotResult
	if runtime.GOOS != "windows" {
		return out, errors.New("旧微信停机快照仅支持 Windows 桌面")
	}
	req.SnapshotDir = cutoverPath(req.SnapshotDir)
	parentPath := filepath.Dir(req.SnapshotDir)
	if !filepath.IsAbs(req.SnapshotDir) || len(filepath.VolumeName(req.SnapshotDir)) != 2 || filepath.Clean(req.SnapshotDir) != req.SnapshotDir || parentPath == req.SnapshotDir {
		return out, errors.New("请指定快照的绝对路径及已存在的父目录")
	}
	parent, err := workspacefs.OpenPath(parentPath)
	if err != nil {
		return out, errors.New("快照父目录不存在或无法安全打开")
	}
	defer parent.Close()
	physicalParent, err := legacySnapshotLocalPath(parent)
	if err != nil {
		return out, errors.New("快照父目录必须位于可核对的本机磁盘")
	}
	dest := filepath.Base(req.SnapshotDir)
	if !filepath.IsLocal(dest) || strings.Contains(dest, ":") || strings.TrimRight(dest, ". ") != dest {
		return out, errors.New("快照目录名无效")
	}
	if req.Verify {
		if req.LegacyDataDir != "" || req.PrimaryAccount != "" || req.ApplyDigest != "" || req.CheckSource {
			return out, errors.New("校验快照仅接受快照路径")
		}
		return verifyLegacyWechatSnapshot(ctx, parent, dest, filepath.Join(physicalParent, dest))
	}
	if req.CheckSource && req.ApplyDigest != "" {
		return out, errors.New("只读来源核对不能执行快照写入")
	}
	req.LegacyDataDir = cutoverPath(req.LegacyDataDir)
	if !filepath.IsAbs(req.LegacyDataDir) || len(filepath.VolumeName(req.LegacyDataDir)) != 2 || filepath.Clean(req.LegacyDataDir) != req.LegacyDataDir || legacyImportOverlap(req.LegacyDataDir, req.SnapshotDir) ||
		!identity.ValidPathID(req.PrimaryAccount) || req.ApplyDigest != "" && !legacyMemoryDigestValid(req.ApplyDigest) {
		return out, errors.New("旧数据路径、快照路径、账号或执行令牌无效，目录必须分开")
	}
	if _, err := parent.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		return out, errors.New("快照目标已存在或无法核对，不能覆盖")
	}
	source, err := workspacefs.OpenPath(req.LegacyDataDir)
	if err != nil {
		return out, errors.New("无法安全打开旧微信数据目录")
	}
	defer source.Close()
	physicalSource, err := legacySnapshotLocalPath(source)
	if err != nil || legacyImportOverlap(physicalSource, filepath.Join(physicalParent, dest)) {
		return out, errors.New("旧微信数据必须位于本机独立目录")
	}
	if req.CheckSource {
		return checkLegacyWechatSnapshotSource(ctx, source, req.PrimaryAccount)
	}
	unlock, err := lockReleaseRoot(source, req.LegacyDataDir, legacyCutoverLockFile)
	if err != nil {
		return out, errors.New("旧桌面仍在使用或缺少兼容的停机锁")
	}
	defer unlock()
	if _, err := source.Lstat(legacyCutoverClaimFile); !errors.Is(err, os.ErrNotExist) {
		return out, errors.New("旧微信数据目录已交接或无法核对交接凭证")
	}
	plan, err := planLegacyWechatSnapshot(ctx, source, req.LegacyDataDir, req.PrimaryAccount, true)
	if err != nil {
		return out, err
	}
	manifest, _ := json.Marshal(plan.manifest)
	out = plan.manifest.result("ready", manifest)
	out.ApplyDigest = localCheckpointKey("legacy-wechat-snapshot-v1", req.LegacyDataDir, req.SnapshotDir, out.ManifestDigest, plan.source)
	if req.ApplyDigest == "" {
		return out, nil
	}
	if out.ApplyDigest != req.ApplyDigest {
		return LegacyWechatSnapshotResult{}, errors.New("停机证据或源文件已变化，请重新预演")
	}
	if err := writeLegacyWechatSnapshot(ctx, source, parent, dest, req, plan, manifest); err != nil {
		return LegacyWechatSnapshotResult{}, err
	}
	out.State = "created"
	return out, nil
}

func (m legacySnapshotManifest) result(state string, manifest []byte) LegacyWechatSnapshotResult {
	out := LegacyWechatSnapshotResult{State: state, Files: len(m.Files), Accounts: m.Accounts,
		MemoryFiles: m.MemoryFiles, DeliveryRecords: m.DeliveryRecords, ManagementDeliveryRecords: m.ManagementDeliveryRecords}
	if manifest != nil {
		out.ManifestDigest = hexDigest(manifest)
	}
	for _, file := range m.Files {
		out.PayloadBytes += file.Bytes
	}
	return out
}

// checkLegacyWechatSnapshotSource reads the capture inputs without creating a
// lock or claiming the old directory. Only capture can validate a stop proof.
func checkLegacyWechatSnapshotSource(ctx context.Context, source *os.Root, primary string) (LegacyWechatSnapshotResult, error) {
	if _, err := source.Lstat(legacyCutoverClaimFile); !errors.Is(err, os.ErrNotExist) {
		return LegacyWechatSnapshotResult{}, errors.New("旧微信数据目录已交接或无法核对交接凭证")
	}
	first := newLegacySnapshotPlan(primary)
	if err := first.collect(ctx, source, "", true); err != nil {
		return LegacyWechatSnapshotResult{}, err
	}
	second := newLegacySnapshotPlan(primary)
	if err := second.collect(ctx, source, "", false); err != nil {
		return LegacyWechatSnapshotResult{}, err
	}
	if _, err := source.Lstat(legacyCutoverClaimFile); !maps.Equal(first.source, second.source) || !errors.Is(err, os.ErrNotExist) {
		return LegacyWechatSnapshotResult{}, errors.New("来源核对期间内容或交接状态已变化，请停止旧服务后重试")
	}
	return first.manifest.result("source_checked", nil), nil
}

func newLegacySnapshotPlan(primary string) legacySnapshotPlan {
	return legacySnapshotPlan{manifest: legacySnapshotManifest{Version: 1, Primary: primary}, projected: map[string][]byte{}, source: map[string]string{}}
}

func planLegacyWechatSnapshot(ctx context.Context, source *os.Root, oldRoot, primary string, checkSQLite bool) (legacySnapshotPlan, error) {
	plan := newLegacySnapshotPlan(primary)
	stop, err := readLegacyImportFile(source, legacyStopMarkerFile, 16<<10)
	var marker legacyStopMarker
	if err != nil || json.Unmarshal(stop, &marker) != nil || marker.Version != 2 || marker.State != "stopped" || !marker.validMode() || marker.AccountID != primary ||
		!validRollbackEpoch(marker.Epoch) || !sameCutoverPath(oldRoot, marker.DataRoot) || !identity.ValidPathID(marker.NativeServerID) ||
		marker.At.IsZero() || marker.At.After(time.Now().Add(time.Minute)) {
		return plan, errors.New("缺少与主账号和原目录一致的第 2 版正常停机凭证")
	}
	plan.addSmall(legacyStopMarkerFile, stop, nil)
	err = plan.collect(ctx, source, marker.NativeServerID, checkSQLite)
	return plan, err
}

func (p *legacySnapshotPlan) collect(ctx context.Context, source *os.Root, dataset string, checkSQLite bool) error {
	primary := p.manifest.Primary
	accounts, err := p.snapshotAccounts(source, primary)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if err := ctx.Err(); err != nil {
			return err
		}
		cfg := filepath.Join("admin", "config.json")
		queue := "bot-state"
		if account != primary {
			id := wechatapp.AccountDirectory(account)
			cfg = filepath.Join("admin", "accounts", id, "config.json")
			queue = filepath.Join(queue, "accounts", id)
		}
		if err := p.snapshotAccount(ctx, source, cfg, queue, account, &dataset, checkSQLite); err != nil {
			return err
		}
	}
	p.manifest.Accounts = len(accounts)
	slices.SortFunc(p.manifest.Files, func(a, b legacySnapshotEntry) int { return strings.Compare(a.Path, b.Path) })
	return nil
}

func (p *legacySnapshotPlan) snapshotAccounts(root *os.Root, primary string) ([]string, error) {
	registryPath := filepath.Join("admin", "accounts.json")
	raw, err := readLegacyImportFile(root, registryPath, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return []string{primary}, nil
	}
	if err != nil {
		return nil, errors.New("旧微信账号登记无效或不可读")
	}
	accounts, selected, err := legacySnapshotAccounts(raw, primary)
	if err != nil {
		return nil, err
	}
	safe, _ := json.Marshal(selected)
	p.addSmall(registryPath, raw, safe)
	return accounts, nil
}

func legacySnapshotAccounts(raw []byte, primary string) ([]string, []wechatapp.LegacyAccountRecord, error) {
	accounts := []string{primary}
	var registered []wechatapp.LegacyAccountRecord
	if json.Unmarshal(raw, &registered) != nil {
		return nil, nil, errors.New("旧微信账号登记无效或不可读")
	}
	seen := map[string]bool{primary: true}
	selected := make([]wechatapp.LegacyAccountRecord, 0, len(registered))
	for _, row := range registered {
		if !identity.ValidPathID(row.ID) || seen[row.ID] {
			return nil, nil, errors.New("旧微信账号登记存在无效或重复身份")
		}
		seen[row.ID] = true
		if wechatapp.LegacyAccountRegistered(row) {
			accounts = append(accounts, row.ID)
			selected = append(selected, row)
		}
	}
	return accounts, selected, nil
}

func (p *legacySnapshotPlan) snapshotAccount(ctx context.Context, root *os.Root, config, queue, account string, dataset *string, checkSQLite bool) error {
	raw, err := readLegacyImportFile(root, config, 128<<10)
	policy, policyErr := decodeLegacyImportPolicy(raw)
	if err != nil || policyErr != nil {
		return errors.New("在册账号缺少可核对的微信规则配置")
	}
	safe, _ := json.Marshal(policy)
	p.addSmall(config, raw, safe)
	queuePath, cursorPath := filepath.Join(queue, "messages-v2.json"), filepath.Join(queue, "checkpoint.json")
	rows, err := readLegacyImportFile(root, queuePath, bot.MaxDeliveryCheckpointBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("旧投递账本不可读或超出支持范围")
	}
	cursor, err := readLegacyImportFile(root, cursorPath, 128<<10)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("旧投递检查点不可读或超出支持范围")
	}
	server, evidence, err := bot.LegacyDeliveryEvidence(rows, cursor, account)
	if err != nil || server != "" && (!identity.ValidPathID(server) || *dataset != "" && server != *dataset) {
		return errors.New("旧投递账本、检查点或停机凭证的账号或数据集不一致")
	}
	if server != "" {
		*dataset = server
	}
	p.manifest.DeliveryRecords += len(evidence)
	p.addSmall(queuePath, rows, nil)
	p.addSmall(cursorPath, cursor, nil)
	adminDir := filepath.Dir(config)
	rawManagement, projectedManagement, managementCount, coldHashes, err := readLegacyManagementSnapshot(ctx, root, adminDir, account)
	if err != nil {
		return err
	}
	maps.Copy(p.source, coldHashes)
	p.addSmall(filepath.Join(adminDir, "tool-state.json"), rawManagement, projectedManagement)
	p.manifest.ManagementDeliveryRecords += managementCount
	return p.snapshotMemory(ctx, root, filepath.Join(adminDir, "companion"), checkSQLite)
}

func (p *legacySnapshotPlan) addSmall(name string, raw, projected []byte) {
	if len(raw) == 0 && projected == nil {
		return
	}
	name = filepath.ToSlash(name)
	p.source[name] = hexDigest(raw)
	if projected != nil {
		p.projected[name] = projected
		raw = projected
	}
	p.manifest.Files = append(p.manifest.Files, legacySnapshotEntry{Path: name, Bytes: int64(len(raw)), Digest: hexDigest(raw)})
}

func (p *legacySnapshotPlan) snapshotMemory(ctx context.Context, root *os.Root, location string, checkSQLite bool) error {
	dir, err := workspacefs.OpenExistingDirectory(root, location)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("旧记忆目录不可安全读取")
	}
	defer dir.Close()
	if !standaloneLegacyMemory(dir) {
		return errors.New("旧记忆仍有 SQLite 伴随文件，请先正常关闭并核对一致性")
	}
	if _, err := dir.Lstat(legacyMemoryDB); errors.Is(err, os.ErrNotExist) {
		if _, err := dir.Lstat("memory.json"); !errors.Is(err, os.ErrNotExist) {
			return errors.New("发现仅有旧 JSON 记忆，需先通过已有迁移流程生成受支持的 SQLite 记忆")
		}
		return nil
	} else if err != nil {
		return errors.New("旧 SQLite 记忆不可读")
	}
	file, err := workspacefs.OpenRegular(dir, legacyMemoryDB)
	if err != nil {
		return errors.New("旧 SQLite 记忆不可安全读取")
	}
	defer file.Close()
	if checkSQLite && companion.ValidateLegacyMemorySnapshot(ctx, file) != nil {
		return errors.New("旧 SQLite 记忆格式不支持或一致性校验失败")
	}
	name := filepath.ToSlash(filepath.Join(location, legacyMemoryDB))
	entry, err := hashLegacySnapshotFile(ctx, dir, legacyMemoryDB, file, io.Discard)
	if err != nil || !standaloneLegacyMemory(dir) {
		return errors.New("读取期间旧 SQLite 记忆变化或不可用")
	}
	entry.Path = name
	p.source[name] = entry.Digest
	p.manifest.Files = append(p.manifest.Files, entry)
	p.manifest.MemoryFiles++
	return nil
}

func hashLegacySnapshotFile(ctx context.Context, dir *os.Root, name string, file *os.File, dst io.Writer) (legacySnapshotEntry, error) {
	var entry legacySnapshotEntry
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return entry, errLegacyImportInvalid
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return entry, err
	}
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(dst, hash), io.LimitReader(legacySnapshotReader{ctx, file}, info.Size()))
	current, statErr := dir.Lstat(name)
	if err != nil || statErr != nil || count != info.Size() || !sameLegacyMemoryFile(info, current) {
		return entry, errLegacyImportInvalid
	}
	return legacySnapshotEntry{Bytes: count, Digest: hex.EncodeToString(hash.Sum(nil))}, nil
}

type legacySnapshotReader struct {
	ctx context.Context
	io.Reader
}

func (r legacySnapshotReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(data)
}

func writeLegacyWechatSnapshot(ctx context.Context, source, parent *os.Root, dest string, req LegacyWechatSnapshotRequest, plan legacySnapshotPlan, manifest []byte) error {
	name := ".legacy-wechat-snapshot-" + rand.Text()
	if err := createLegacySnapshotStage(parent, name); err != nil {
		return errors.New("无法在目标父目录安全创建私有暂存目录")
	}
	initial, err := parent.Lstat(name)
	if err != nil {
		return errors.New("无法核对私有暂存目录")
	}
	defer func() {
		if current, err := parent.Lstat(name); err == nil && os.SameFile(initial, current) {
			_ = parent.RemoveAll(name)
		}
	}()
	stage, err := workspacefs.OpenExistingDirectory(parent, name)
	if err != nil {
		return errors.New("无法打开私有暂存目录")
	}
	defer stage.Close()
	if verifyLegacySnapshotPrivacy(stage) != nil {
		return errors.New("私有暂存目录权限不符合要求")
	}
	for _, entry := range plan.manifest.Files {
		if err := copyLegacySnapshotEntry(ctx, source, stage, entry, plan.projected[entry.Path]); err != nil {
			return errors.New("复制或校验快照文件失败，未发布快照")
		}
	}
	if err := writeLegacyStagedFile(stage, legacyWechatSnapshotManifestFile, json.RawMessage(manifest)); err != nil {
		return errors.New("无法写入快照完整性清单")
	}
	if err := stage.Close(); err != nil {
		return errors.New("无法关闭私有暂存目录")
	}
	checked, err := verifyLegacyWechatSnapshot(ctx, parent, name, req.SnapshotDir)
	if err != nil || checked.ManifestDigest != hexDigest(manifest) {
		return errors.New("写入后复验失败，未发布快照")
	}
	// Check source bytes again under the lease. This also catches a missing
	// sidecar or optional file changing during the copy.
	latest, err := planLegacyWechatSnapshot(ctx, source, req.LegacyDataDir, req.PrimaryAccount, false)
	if err != nil || localCheckpointKey(latest.source) != localCheckpointKey(plan.source) {
		return errors.New("制作过程中旧数据发生变化，未发布快照")
	}
	if _, err := source.Lstat(legacyCutoverClaimFile); !errors.Is(err, os.ErrNotExist) {
		return errors.New("制作过程中旧微信交接状态发生变化，未发布快照")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publishLegacySnapshot(parent, name, dest); err != nil {
		return errors.New("无法原子发布快照，请核对目标是否已存在")
	}
	return nil
}

func copyLegacySnapshotEntry(ctx context.Context, source, stage *os.Root, expected legacySnapshotEntry, projected []byte) error {
	name := filepath.FromSlash(expected.Path)
	dir, err := workspacefs.EnsureDirectory(stage, filepath.Dir(name))
	if err != nil {
		return err
	}
	defer dir.Close()
	leaf := filepath.Base(name)
	file, err := dir.OpenFile(leaf, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = func() error {
		if projected != nil {
			_, err := file.Write(projected)
			return err
		}
		origin, err := workspacefs.OpenExistingDirectory(source, filepath.Dir(name))
		if err != nil {
			return err
		}
		defer origin.Close()
		input, err := workspacefs.OpenRegular(origin, leaf)
		if err != nil {
			return err
		}
		defer input.Close()
		copied, err := hashLegacySnapshotFile(ctx, origin, leaf, input, file)
		if err != nil || copied.Bytes != expected.Bytes || copied.Digest != expected.Digest {
			return errLegacyImportChanged
		}
		return nil
	}()
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

func verifyLegacyWechatSnapshot(ctx context.Context, parent *os.Root, name, target string) (LegacyWechatSnapshotResult, error) {
	var out LegacyWechatSnapshotResult
	root, err := workspacefs.OpenExistingDirectory(parent, name)
	if err != nil {
		return out, errors.New("快照目录不存在或无法安全打开")
	}
	defer root.Close()
	if verifyLegacySnapshotPrivacy(root) != nil {
		return out, errors.New("快照目录权限不符合要求")
	}
	raw, err := readLegacyImportFile(root, legacyWechatSnapshotManifestFile, legacyImportArchiveLimit)
	stop, stopErr := readLegacyImportFile(root, legacyStopMarkerFile, 16<<10)
	var saved legacySnapshotManifest
	var marker legacyStopMarker
	if err != nil || stopErr != nil || json.Unmarshal(raw, &saved) != nil || json.Unmarshal(stop, &marker) != nil ||
		saved.Version != 1 || !identity.ValidPathID(saved.Primary) || legacyImportOverlap(marker.DataRoot, target) {
		return out, errors.New("快照清单或停机记录无效")
	}
	plan, err := planLegacyWechatSnapshot(ctx, root, marker.DataRoot, saved.Primary, true)
	if err != nil {
		return out, err
	}
	canonical, _ := json.Marshal(plan.manifest)
	if !bytes.Equal(raw, canonical) || !legacySnapshotTreeMatches(root, plan) {
		return out, errors.New("快照文件内容或文件集合与完整性清单不一致")
	}
	return plan.manifest.result("verified", canonical), nil
}

func legacySnapshotTreeMatches(root *os.Root, plan legacySnapshotPlan) bool {
	files := map[string]bool{legacyWechatSnapshotManifestFile: true}
	dirs := map[string]bool{".": true}
	for _, entry := range plan.manifest.Files {
		if plan.source[entry.Path] != entry.Digest {
			return false
		}
		files[entry.Path] = true
		for dir := path.Dir(entry.Path); !dirs[dir]; dir = path.Dir(dir) {
			dirs[dir] = true
		}
	}
	count := 0
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() && !dirs[name] || !entry.IsDir() && (!entry.Type().IsRegular() || !files[name]) {
			return errLegacyImportInvalid
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	return err == nil && count == len(files)
}
