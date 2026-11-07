package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/workspacefs"
)

const legacyImportSourceFile = "legacy-import-source.json"
const legacyImportArchiveFile = "legacy-import.json"
const legacyImportArchiveLimit = 24 << 20

var (
	errLegacyImportNotConfigured = errors.New("未配置已停机的旧版隔离快照")
	errLegacyImportInvalid       = errors.New("旧版快照或账号身份校验失败")
	errLegacyImportChanged       = errors.New("快照或目标已变化，请重新预演")
	errLegacyImportConflict      = errors.New("目标已有工作记录，无法覆盖")
)

type legacyImportSource struct {
	Root           string `json:"snapshot_root"`
	LegacyHostRoot string `json:"legacy_host_root,omitempty"`
	Primary        string `json:"primary_account"`
	Stopped        bool   `json:"snapshot_stopped"`
	ManifestDigest string `json:"snapshot_manifest_digest,omitempty"`
	guard          *legacySnapshotGuard
}

type legacyImportPlan struct {
	Digest               string         `json:"digest"`
	Account              string         `json:"account_id"`
	LegacyEnabled        bool           `json:"legacy_enabled"`
	LegacyPaused         bool           `json:"legacy_paused"`
	SameDataset          bool           `json:"same_native_dataset"`
	SuggestedThreads     []string       `json:"suggested_reply_threads"`
	Omitted              []string       `json:"requires_manual_review"`
	Receipts             int            `json:"receipt_records"`
	Unresolved           int            `json:"unresolved_receipts"`
	ManagementReceipts   int            `json:"management_receipt_records,omitempty"`
	ManagementUnresolved int            `json:"management_unresolved_receipts,omitempty"`
	Applied              bool           `json:"applied"`
	Cutover              *legacyCutover `json:"cutover,omitempty"`
}

type legacyImportArchive struct {
	Version      int                        `json:"version"`
	Plan         legacyImportPlan           `json:"plan"`
	LegacyServer string                     `json:"legacy_server"`
	Receipts     map[string]apphost.Receipt `json:"receipts"`
	// Management receipts are read-only archive rows. They never participate
	// in worker fallback, native lookups or send/retry idempotency checks.
	ManagementReceipts map[string]apphost.Receipt `json:"management_receipts,omitempty"`
	// Native source identifiers are private lookup evidence, never part of a receipt response.
	ReceiptSources         map[string]string `json:"receipt_sources,omitempty"`
	SourceRevision         string            `json:"source_revision,omitempty"`
	SnapshotManifestDigest string            `json:"snapshot_manifest_digest,omitempty"`
}

type legacyImportPolicy struct {
	Enabled          bool                        `json:"enabled"`
	ConnectionPaused bool                        `json:"connection_paused"`
	Filehelper       bool                        `json:"filehelper_enabled"`
	Private          bool                        `json:"private_enabled"`
	AllFriends       bool                        `json:"private_all_friends"`
	DirectIDs        []string                    `json:"direct_ids"`
	Prefix           string                      `json:"prefix"`
	Groups           []wechatapp.LegacyGroupRule `json:"group_rules"`
	Companion        struct {
		Enabled     bool `json:"enabled"`
		Permissions struct {
			ReplyText bool `json:"reply_text"`
		} `json:"permissions"`
	} `json:"companion"`
}

func legacySuggestedThreads(policy legacyImportPolicy) (threads, omitted []string) {
	threads, omitted = []string{}, []string{}
	if policy.Companion.Enabled && !policy.Companion.Permissions.ReplyText {
		return threads, []string{"legacy_text_reply_disabled"}
	}
	set := map[string]bool{}
	if policy.Filehelper {
		if policy.Prefix == "/ai " {
			set["filehelper"] = true
		} else {
			omitted = append(omitted, "custom_filehelper_prefix")
		}
	}
	if policy.Private {
		if policy.AllFriends {
			omitted = append(omitted, "all_friends")
		}
		for _, thread := range policy.DirectIDs {
			if identity.ValidPathID(thread) && thread != "filehelper" && !strings.HasSuffix(thread, "@chatroom") {
				set[thread] = true
			}
		}
	}
	for _, group := range policy.Groups {
		if identity.ValidPathID(group.ID) && strings.HasSuffix(group.ID, "@chatroom") && group.Mode == "mention" && group.Prefix == "" {
			set[group.ID] = true
			if group.Persona == "" && !group.ForceReplyWhenMentioned && policy.Prefix == "/ai " {
				continue
			}
		}
		if !slices.Contains(omitted, "group_rule") {
			omitted = append(omitted, "group_rule")
		}
	}
	for thread := range set {
		threads = append(threads, thread)
	}
	slices.Sort(threads)
	return threads, omitted
}

func decodeLegacyImportPolicy(raw []byte) (legacyImportPolicy, error) {
	policy := legacyImportPolicy{Filehelper: true, Prefix: "/ai "}
	policy.Companion.Permissions.ReplyText = true
	if json.Unmarshal(raw, &policy) != nil || len(policy.DirectIDs) > 100 || len(policy.Groups) > 100 {
		return legacyImportPolicy{}, errLegacyImportInvalid
	}
	return policy, nil
}

func readLegacyImportFile(root *os.Root, name string, max int64) ([]byte, error) {
	dir, err := workspacefs.OpenExistingDirectory(root, filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	leaf := filepath.Base(name)
	before, err := dir.Lstat(leaf)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > max {
		return nil, errLegacyImportInvalid
	}
	file, err := dir.Open(leaf)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(before, actual) {
		return nil, errLegacyImportInvalid
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || int64(len(data)) != before.Size() || !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() || len(data) == 0 {
		return nil, errLegacyImportInvalid
	}
	return data, nil
}

func legacyImportOverlap(first, second string) bool {
	first, second = cutoverPath(first), cutoverPath(second)
	within := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	return within(first, second) || within(second, first)
}

func (s *stationWechat) openLegacyImport() (*os.Root, legacyImportSource, error) {
	var source legacyImportSource
	application, err := workspacefs.OpenPath(filepath.Dir(s.root))
	if err != nil {
		return nil, source, errLegacyImportNotConfigured
	}
	defer application.Close()
	raw, err := readLegacyImportFile(application, legacyImportSourceFile, 16<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil, source, errLegacyImportNotConfigured
	}
	if err != nil || json.Unmarshal(raw, &source) != nil {
		return nil, source, errLegacyImportInvalid
	}
	source.Root = cutoverPath(source.Root)
	if !source.Stopped || !identity.ValidPathID(source.Primary) ||
		!filepath.IsAbs(source.Root) || filepath.Clean(source.Root) != source.Root ||
		legacyImportOverlap(source.Root, s.dataDir) || legacyImportOverlap(source.Root, s.workspaceRoot) {
		return nil, source, errLegacyImportInvalid
	}
	root, err := workspacefs.OpenPath(source.Root)
	if err != nil {
		return nil, source, errLegacyImportInvalid
	}
	source.guard, err = loadLegacySnapshotGuard(root, source)
	if err != nil {
		root.Close()
		return nil, source, err
	}
	return root, source, nil
}

func (source legacyImportSource) matchesArchive(archive legacyImportArchive) bool {
	return archive.SnapshotManifestDigest == "" || archive.SnapshotManifestDigest == source.guard.digest()
}

func legacyImportAccountPaths(root *os.Root, source legacyImportSource, account string) (config, queue string, err error) {
	primary := source.Primary
	if account == primary {
		return filepath.Join("admin", "config.json"), "bot-state", nil
	}
	raw, err := source.guard.readFile(root, filepath.Join("admin", "accounts.json"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", apphost.ErrNotFound
	}
	var records []wechatapp.LegacyAccountRecord
	if err != nil || json.Unmarshal(raw, &records) != nil {
		return "", "", legacyImportReadError(err)
	}
	seen := map[string]bool{}
	found := false
	for _, row := range records {
		if !identity.ValidPathID(row.ID) || row.ID == primary || seen[row.ID] {
			return "", "", errLegacyImportInvalid
		}
		seen[row.ID] = true
		if row.ID == account && wechatapp.LegacyAccountRegistered(row) {
			found = true
		}
	}
	if !found {
		return "", "", apphost.ErrNotFound
	}
	digest := wechatapp.AccountDirectory(account)
	return filepath.Join("admin", "accounts", digest, "config.json"), filepath.Join("bot-state", "accounts", digest), nil
}

func legacyImportReadError(err error) error {
	if errors.Is(err, errLegacySnapshotIntegrity) {
		return errLegacySnapshotIntegrity
	}
	return errLegacyImportInvalid
}

// legacyImportState is an entry-point hint. The full archive, account and
// digest checks run on the explicit preview or apply request.
func (s *stationWechat) legacyImportState(account string, worker *stationWechatWorker) string {
	if worker != nil {
		destination, err := workspacefs.OpenPath(worker.dir)
		if err != nil {
			return "invalid"
		}
		info, err := destination.Lstat(legacyImportArchiveFile)
		_ = destination.Close()
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() > legacyImportArchiveLimit {
				return "invalid"
			}
			return "applied"
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "invalid"
		}
	}
	root, source, err := s.openLegacyImport()
	if errors.Is(err, errLegacyImportNotConfigured) {
		return ""
	}
	if err != nil {
		return "invalid"
	}
	defer root.Close()
	if _, _, err := legacyImportAccountPaths(root, source, account); err != nil {
		if errors.Is(err, apphost.ErrNotFound) {
			return ""
		}
		return "invalid"
	}
	if worker != nil {
		return "conflict"
	}
	return "available"
}

func (s *stationWechat) previewLegacyImport(ctx context.Context, id string) (legacyImportArchive, error) {
	native, err := s.WorkerSender(ctx, id)
	if err != nil {
		return legacyImportArchive{}, err
	}
	status, err := native.DaemonStatus(ctx)
	if err != nil {
		return legacyImportArchive{}, apphost.ErrUnavailable
	}
	root, source, err := s.openLegacyImport()
	if err != nil {
		return legacyImportArchive{}, err
	}
	defer root.Close()
	return s.previewLegacyImportFrom(ctx, id, native, status, root, source)
}

func (s *stationWechat) previewLegacyImportFrom(ctx context.Context, id string, native *linuxnative.Sender, status linuxnative.DaemonStatus, root *os.Root, source legacyImportSource) (legacyImportArchive, error) {
	if err := source.guard.verify(ctx, root); err != nil {
		return legacyImportArchive{}, err
	}
	account := native.Account()
	cfgPath, queueDir, err := legacyImportAccountPaths(root, source, account)
	if err != nil {
		return legacyImportArchive{}, err
	}
	cfg, err := source.guard.readFile(root, cfgPath, 128<<10)
	if err != nil {
		return legacyImportArchive{}, legacyImportReadError(err)
	}
	policy, err := decodeLegacyImportPolicy(cfg)
	if err != nil {
		return legacyImportArchive{}, err
	}
	queue, err := source.guard.readFile(root, filepath.Join(queueDir, "messages-v2.json"), bot.MaxDeliveryCheckpointBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return legacyImportArchive{}, legacyImportReadError(err)
	}
	cursor, err := source.guard.readFile(root, filepath.Join(queueDir, "checkpoint.json"), 128<<10)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return legacyImportArchive{}, legacyImportReadError(err)
	}
	oldServer, evidence, err := bot.LegacyDeliveryEvidence(queue, cursor, account)
	if err != nil || oldServer != "" && !identity.ValidPathID(oldServer) {
		return legacyImportArchive{}, errLegacyImportInvalid
	}
	suggested, omitted := legacySuggestedThreads(policy)
	queueHash, cursorHash := sha256.Sum256(queue), sha256.Sum256(cursor)
	archive := legacyImportArchive{
		Version: 1, LegacyServer: oldServer, Receipts: map[string]apphost.Receipt{},
		Plan: legacyImportPlan{Account: account, LegacyEnabled: policy.Enabled, LegacyPaused: policy.ConnectionPaused,
			SameDataset: oldServer != "" && oldServer == status.ServerID, SuggestedThreads: suggested, Omitted: omitted},
		SourceRevision: localCheckpointKey(policy, queueHash, cursorHash), SnapshotManifestDigest: source.guard.digest(),
	}
	for key, row := range evidence {
		if !identity.ValidPathID(key) || !identity.ValidPathID(row.Status.ThreadID) || row.Status.ID != "" && !identity.ValidPathID(row.Status.ID) {
			return legacyImportArchive{}, errLegacyImportInvalid
		}
		receipt := apphost.Receipt{ConnectionRef: s.connectionRef(id, account), ConversationID: row.Status.ThreadID, ResumedFrom: row.ResumedFrom,
			DeliveryReceipt: apphost.DeliveryReceipt{IdempotencyKey: key, Kind: row.Status.Kind, Lookup: "legacy_snapshot"}}
		wechatapp.ProjectDeliveryReceipt(&receipt.DeliveryReceipt, row.Status)
		if legacyReceiptPending(receipt) {
			archive.Plan.Unresolved++
		}
		archive.Receipts[key] = receipt
		if identity.ValidPathID(row.SourceID) {
			if archive.ReceiptSources == nil {
				archive.ReceiptSources = make(map[string]string)
			}
			archive.ReceiptSources[key] = row.SourceID
		}
	}
	archive.Plan.Receipts = len(archive.Receipts)
	archive.ManagementReceipts, archive.Plan.ManagementUnresolved, err = readLegacyManagementReceipts(root, cfgPath, source.guard, s.connectionRef(id, account))
	if err != nil {
		return legacyImportArchive{}, err
	}
	archive.Plan.ManagementReceipts = len(archive.ManagementReceipts)
	cutover, cutoverDigests := s.checkLegacyCutover(ctx, id, native, root, source, oldServer)
	archive.Plan.Cutover = cutover
	// A changed queue or cursor invalidates the preview, even if the visible
	// receipt projection is unchanged. Credentials never enter this digest.
	fingerprint, err := json.Marshal(struct {
		Archive       legacyImportArchive
		Binding       []string
		Queue, Cursor [32]byte
		Cutover       [2][32]byte
	}{archive, []string{source.Root, source.Primary, id, status.ServerID, status.OwnershipGeneration, s.installation}, queueHash, cursorHash, cutoverDigests})
	if err != nil || len(fingerprint) > legacyImportArchiveLimit {
		return legacyImportArchive{}, errLegacyImportInvalid
	}
	sum := sha256.Sum256(fingerprint)
	archive.Plan.Digest = hex.EncodeToString(sum[:])
	return archive, nil
}

func (s *stationWechat) connectionRef(id, account string) apphost.ConnectionRef {
	return apphost.ConnectionRef{AppID: wechatapp.ID, InstallationID: s.installation, ConnectionID: id, AccountID: account}
}

func readLegacyImportArchive(root *os.Root) (legacyImportArchive, error) {
	var archive legacyImportArchive
	data, err := readLegacyImportFile(root, legacyImportArchiveFile, legacyImportArchiveLimit)
	if err != nil {
		return archive, err
	}
	if json.Unmarshal(data, &archive) != nil || archive.Version != 1 || !archive.Plan.Applied || len(archive.Plan.Digest) != 64 || !identity.ValidPathID(archive.Plan.Account) ||
		archive.SnapshotManifestDigest != "" && !legacyMemoryDigestValid(archive.SnapshotManifestDigest) {
		return legacyImportArchive{}, errLegacyImportInvalid
	}
	return archive, nil
}

func (s *stationWechat) savedLegacyImport(ctx context.Context, id string, refreshCutover bool) (legacyImportPlan, bool, error) {
	native, err := s.WorkerSender(ctx, id)
	if err != nil {
		return legacyImportPlan{}, false, err
	}
	root, err := workspacefs.OpenPath(filepath.Join(s.root, id))
	if errors.Is(err, os.ErrNotExist) {
		return legacyImportPlan{}, false, nil
	}
	if err != nil {
		return legacyImportPlan{}, false, err
	}
	defer root.Close()
	archive, err := readLegacyImportArchive(root)
	if errors.Is(err, os.ErrNotExist) {
		return legacyImportPlan{}, false, nil
	}
	if err != nil || archive.Plan.Account != native.Account() {
		return legacyImportPlan{}, false, errLegacyImportInvalid
	}
	if refreshCutover {
		snapshot, source, _ := s.openLegacyImport()
		if snapshot != nil {
			defer snapshot.Close()
		}
		if source.matchesArchive(archive) {
			archive.Plan.Cutover, _ = s.checkLegacyCutover(ctx, id, native, snapshot, source, archive.LegacyServer)
		} else {
			archive.Plan.Cutover, _ = s.checkLegacyCutover(ctx, id, native, nil, source, archive.LegacyServer)
		}
	}
	return archive.Plan, true, nil
}

func writeLegacyStagedFile(root *os.Root, path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > legacyImportArchiveLimit {
		return errLegacyImportInvalid
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

func (s *stationWechat) applyLegacyImport(ctx context.Context, id, digest string) (legacyImportPlan, error) {
	prior, found, err := s.savedLegacyImport(ctx, id, false)
	if err != nil {
		return legacyImportPlan{}, err
	}
	if found {
		if prior.Digest != digest {
			return legacyImportPlan{}, errLegacyImportConflict
		}
		return prior, nil
	}
	archive, err := s.previewLegacyImport(ctx, id)
	if err != nil {
		return legacyImportPlan{}, err
	}
	if digest != archive.Plan.Digest {
		return legacyImportPlan{}, errLegacyImportChanged
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || ctx.Err() != nil {
		return legacyImportPlan{}, apphost.ErrLifecycleConflict
	}
	root, err := workspacefs.EnsurePath(s.root)
	if err != nil {
		return legacyImportPlan{}, err
	}
	defer root.Close()
	if _, err := root.Lstat(id); err == nil {
		destination, err := workspacefs.OpenExistingDirectory(root, id)
		if err != nil {
			return legacyImportPlan{}, errLegacyImportConflict
		}
		defer destination.Close()
		prior, err := readLegacyImportArchive(destination)
		if err == nil && prior.Plan.Digest == digest && prior.Plan.Account == archive.Plan.Account {
			return prior.Plan, nil
		}
		return legacyImportPlan{}, errLegacyImportConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return legacyImportPlan{}, err
	}
	archive.Plan.Applied = true
	staging := ".legacy-import-" + identity.RandomToken()
	if err := root.Mkdir(staging, 0700); err != nil {
		return legacyImportPlan{}, err
	}
	policyPath, archivePath := filepath.Join(staging, "worker.json"), filepath.Join(staging, legacyImportArchiveFile)
	defer func() {
		_ = root.Remove(policyPath)
		_ = root.Remove(archivePath)
		_ = root.Remove(staging)
	}()
	// An imported policy is dormant. A later owner policy save explicitly
	// grants the confirmed account new permissions; old scopes are suggestions.
	policy := stationWechatPolicy{Enabled: archive.Plan.LegacyEnabled, Paused: true, Grants: map[string]time.Time{}, LegacyArchiveDigest: archive.Plan.Digest}
	if err := writeLegacyStagedFile(root, policyPath, policy); err != nil {
		return legacyImportPlan{}, err
	}
	if err := writeLegacyStagedFile(root, archivePath, archive); err != nil {
		return legacyImportPlan{}, err
	}
	if ctx.Err() != nil {
		return legacyImportPlan{}, ctx.Err()
	}
	if err := root.Rename(staging, id); err != nil {
		return legacyImportPlan{}, errLegacyImportConflict
	}
	return archive.Plan, nil
}
