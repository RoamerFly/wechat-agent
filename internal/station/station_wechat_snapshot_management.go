package station

// Read-only projection of the old web-management send ledger.  This deliberately
// does not depend on admin.toolStore: importing it as a live idempotency store
// could make historical rows executable again.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
)

const legacyManagementLedgerLimit = 2 << 20
const legacyManagementLedgerFiles = 4096

// The published form purposefully omits toolRecord.Hash, arbitrary reasons,
// source IDs, upload records and all request payloads/paths.
type legacyManagementSnapshot struct {
	Version int                               `json:"version"`
	Sends   map[string]legacyManagementRecord `json:"sends"`
}

type legacyManagementRecord struct {
	Key     string                 `json:"key"`
	Account string                 `json:"account"`
	Thread  string                 `json:"thread"`
	Kind    string                 `json:"kind"`
	Status  legacyManagementStatus `json:"status"`
	Created time.Time              `json:"created"`
}

// Keep the native status field names so station consumers can decode the
// established receipt shape, while excluding unsafe/free-form fields.
type legacyManagementStatus struct {
	Attempts             int                `json:"attempts,omitempty"`
	Retryable            bool               `json:"retryable,omitempty"`
	CancelRequested      bool               `json:"cancel_requested,omitempty"`
	ID                   string             `json:"id"`
	State                string             `json:"state"`
	Phase                string             `json:"phase"`
	Kind                 string             `json:"kind"`
	ThreadID             string             `json:"thread_id"`
	ServerID             string             `json:"server_id"`
	ErrorCode            string             `json:"error_code,omitempty"`
	VoicePartsIncomplete bool               `json:"voice_parts_incomplete,omitempty"`
	VoiceChunks          []legacyVoiceChunk `json:"voice_chunks,omitempty"`
	VoiceFailureStage    string             `json:"voice_failure_stage,omitempty"`
	VoiceTransportKind   string             `json:"voice_transport_kind,omitempty"`
	VoiceFailedOffset    *int64             `json:"voice_failed_offset,omitempty"`
}

type legacyVoiceChunk struct {
	ServerID      string `json:"server_id,omitempty"`
	Index         int    `json:"index"`
	State         string `json:"state"`
	DurationMS    int64  `json:"duration_ms"`
	PacingMS      int64  `json:"pacing_ms"`
	SubmittedAtMS int64  `json:"submitted_at_ms,omitempty"`
	AcceptedAtMS  int64  `json:"accepted_at_ms,omitempty"`
	HasReceipt    bool   `json:"has_receipt"`
	PrefixRetries int    `json:"prefix_retries,omitempty"`
}

type legacyManagementRawRecord struct {
	Key     string                 `json:"key"`
	Hash    string                 `json:"hash"`
	Account string                 `json:"account"`
	Thread  string                 `json:"thread"`
	Kind    string                 `json:"kind"`
	Status  linuxnative.ToolStatus `json:"status"`
	Created time.Time              `json:"created"`
	digest  string
}
type legacyManagementRawState struct {
	Version int                                  `json:"version"`
	Sends   map[string]legacyManagementRawRecord `json:"sends"`
}
type legacyManagementArchived struct {
	Version int             `json:"version"`
	Digest  string          `json:"digest"`
	Record  json.RawMessage `json:"record"`
}

// raw is the active legacy tool-state source (or nil); projected is a stable
// safe representation. coldHashes are source-only fingerprints for a caller's
// pre-publication check and must never be written into the projection.
func readLegacyManagementSnapshot(ctx context.Context, root *os.Root, adminDir, account string) (raw []byte, projected []byte, count int, coldHashes map[string]string, err error) {
	if root == nil || adminDir == "" || !identity.ValidPathID(account) {
		return nil, nil, 0, nil, errors.New("旧管理端发送记录参数无效")
	}
	statePath := filepath.Join(adminDir, "tool-state.json")
	state, readErr := readLegacyImportFile(root, statePath, legacyImportArchiveLimit)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, nil, 0, nil, errors.New("旧管理端发送记录不可安全读取")
	}

	// Version 2 is the snapshot projection. The live management store uses
	// version 1, including per-record request hashes and upload staging.
	if readErr == nil {
		var header struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(state, &header) != nil {
			return nil, nil, 0, nil, errors.New("旧管理端发送记录格式无效")
		}
		if header.Version == 2 {
			saved, err := decodeLegacyManagementSnapshot(state, account)
			if err != nil {
				return nil, nil, 0, nil, err
			}
			if present, presentErr := legacyManagementLedgerPresent(root, adminDir); presentErr != nil || present {
				return nil, nil, 0, nil, errors.New("旧管理端回执快照与冷发送索引冲突")
			}
			return state, state, len(saved.Sends), nil, nil
		}
	}

	active := map[string]legacyManagementRecord{}
	activeHashes := map[string]string{}
	if readErr == nil {
		var decoded legacyManagementRawState
		if len(state) > legacyManagementLedgerLimit || json.Unmarshal(state, &decoded) != nil || decoded.Version != 1 || decoded.Sends == nil || len(decoded.Sends) > 1000 {
			return nil, nil, 0, nil, errors.New("旧管理端发送记录格式无效")
		}
		for key, source := range decoded.Sends {
			row, rowErr := projectLegacyManagementRecord(source, account, false)
			if rowErr != nil || key != source.Key {
				return nil, nil, 0, nil, errors.New("旧管理端发送记录归属无效")
			}
			active[key], activeHashes[key] = row, source.Hash
		}
		raw = state
	}
	cold, hashes, coldErr := readLegacyManagementCold(ctx, root, adminDir, account)
	if coldErr != nil {
		return nil, nil, 0, nil, coldErr
	}
	for key, source := range cold {
		row, rowErr := projectLegacyManagementRecord(source, account, true)
		if rowErr != nil {
			return nil, nil, 0, nil, errors.New("旧管理端冷发送回执无效")
		}
		if old, exists := active[key]; exists && (activeHashes[key] != source.Hash || !reflect.DeepEqual(old, row)) {
			return nil, nil, 0, nil, errors.New("旧管理端活跃与归档发送记录冲突")
		}
		active[key] = row
	}
	if readErr != nil && len(active) == 0 {
		return nil, nil, 0, nil, nil
	}
	view := legacyManagementSnapshot{Version: 2, Sends: active}
	if !legacyManagementSnapshotValid(view, account) {
		return nil, nil, 0, nil, errors.New("旧管理端发送回执投影无效")
	}
	projected, err = json.Marshal(view)
	if err != nil || len(projected) > legacyImportArchiveLimit {
		return nil, nil, 0, nil, errors.New("旧管理端发送回执投影无法编码")
	}
	return raw, projected, len(active), hashes, nil
}

func decodeLegacyManagementSnapshot(raw []byte, account string) (legacyManagementSnapshot, error) {
	var saved legacyManagementSnapshot
	if json.Unmarshal(raw, &saved) == nil && legacyManagementSnapshotValid(saved, account) {
		canonical, err := json.Marshal(saved)
		if err == nil && bytes.Equal(raw, canonical) {
			return saved, nil
		}
	}
	return legacyManagementSnapshot{}, errors.New("旧管理端发送回执快照不是规范投影")
}

func readLegacyManagementCold(ctx context.Context, root *os.Root, adminDir, account string) (map[string]legacyManagementRawRecord, map[string]string, error) {
	requests, requestHashes, err := readLegacyManagementColdIndex(ctx, root, adminDir, account, "requests")
	if err != nil {
		return nil, nil, err
	}
	ids, idHashes, err := readLegacyManagementColdIndex(ctx, root, adminDir, account, "ids")
	if err != nil {
		return nil, nil, err
	}
	if len(requests) != len(ids) {
		return nil, nil, errors.New("旧管理端冷发送索引不完整")
	}
	for key, source := range requests {
		identity := source.Account + "\x00" + source.Status.ID
		paired, ok := ids[identity]
		if !ok || source.digest != paired.digest || key != source.Key {
			return nil, nil, errors.New("旧管理端冷发送索引记录不一致")
		}
	}
	hashes := make(map[string]string, len(requestHashes)+len(idHashes))
	for name, digest := range requestHashes {
		hashes[name] = digest
	}
	for name, digest := range idHashes {
		hashes[name] = digest
	}
	return requests, hashes, nil
}

func readLegacyManagementColdIndex(ctx context.Context, root *os.Root, adminDir, account, kind string) (map[string]legacyManagementRawRecord, map[string]string, error) {
	dir := path.Join(filepath.ToSlash(adminDir), "send-ledger", kind)
	items, hashes, files := map[string]legacyManagementRawRecord{}, map[string]string{}, 0
	err := fs.WalkDir(root.FS(), dir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) && name == dir {
				return fs.SkipDir
			}
			return errors.New("旧管理端冷发送索引无法读取")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return errors.New("旧管理端冷发送索引包含不安全链接")
		}
		if entry.IsDir() {
			if name == dir || (path.Dir(name) == dir && len(path.Base(name)) == 2 && isLowerHex(path.Base(name))) {
				return nil
			}
			return errors.New("旧管理端冷发送索引子目录无效")
		}
		if !entry.Type().IsRegular() || !legacyManagementColdLeaf(name, dir) {
			return errors.New("旧管理端冷发送索引路径无效")
		}
		files++
		if files > legacyManagementLedgerFiles {
			return errors.New("旧管理端冷发送索引超过每类 4096 条的单次快照上限")
		}
		blob, err := readLegacyImportFile(root, filepath.FromSlash(name), legacyManagementLedgerLimit+65536)
		if err != nil {
			return errors.New("旧管理端冷发送索引不可安全读取")
		}
		record, err := decodeLegacyManagementArchived(blob, kind, account, path.Base(name))
		if err != nil {
			return err
		}
		identity := record.Key
		if kind == "ids" {
			identity = record.Account + "\x00" + record.Status.ID
		}
		if _, duplicate := items[identity]; duplicate {
			return errors.New("旧管理端冷发送索引身份重复")
		}
		items[identity], hashes[name] = record, hexDigest(blob)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return items, hashes, nil
}

func decodeLegacyManagementArchived(blob []byte, kind, account, leaf string) (legacyManagementRawRecord, error) {
	var zero legacyManagementRawRecord
	z, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return zero, errors.New("旧管理端冷发送归档损坏")
	}
	body, readErr := io.ReadAll(io.LimitReader(z, legacyManagementLedgerLimit+1))
	closeErr := z.Close()
	var archived legacyManagementArchived
	if readErr != nil || closeErr != nil || len(body) > legacyManagementLedgerLimit || json.Unmarshal(body, &archived) != nil || archived.Version != 1 {
		return zero, errors.New("旧管理端冷发送归档无效")
	}
	var record legacyManagementRawRecord
	if archived.Digest != hexDigest(archived.Record) || json.Unmarshal(archived.Record, &record) != nil || !legacyManagementArchivedValid(record, account) {
		return zero, errors.New("旧管理端冷发送归档校验失败")
	}
	identity := record.Key
	if kind == "ids" {
		identity = record.Account + "\x00" + record.Status.ID
	}
	if leaf != legacyManagementLedgerName(kind, identity) {
		return zero, errors.New("旧管理端冷发送归档索引不匹配")
	}
	record.digest = archived.Digest
	return record, nil
}

func projectLegacyManagementRecord(source legacyManagementRawRecord, account string, archived bool) (legacyManagementRecord, error) {
	if !legacyManagementRawValid(source, account) || archived && !legacyManagementArchivedValid(source, account) {
		return legacyManagementRecord{}, errors.New("旧管理端发送记录无效")
	}
	chunks := make([]legacyVoiceChunk, 0, len(source.Status.VoiceChunks))
	seen := map[int]bool{}
	for _, chunk := range source.Status.VoiceChunks {
		if chunk.Index < 0 || seen[chunk.Index] || !legacyManagementCode(chunk.State) || chunk.DurationMS < 0 || chunk.PacingMS < 0 || chunk.ServerID != "" && !wechatapp.ValidServerReceiptID(chunk.ServerID) {
			return legacyManagementRecord{}, errors.New("旧管理端语音分段记录无效")
		}
		seen[chunk.Index] = true
		chunks = append(chunks, legacyVoiceChunk{
			ServerID: chunk.ServerID, Index: chunk.Index, State: chunk.State, HasReceipt: chunk.HasReceipt,
			DurationMS: chunk.DurationMS, PacingMS: chunk.PacingMS, SubmittedAtMS: chunk.SubmittedAtMS,
			AcceptedAtMS: chunk.AcceptedAtMS, PrefixRetries: chunk.PrefixRetries,
		})
	}
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Index < chunks[j].Index })
	status := source.Status
	return legacyManagementRecord{
		Key: source.Key, Account: source.Account, Thread: source.Thread, Kind: source.Kind, Created: source.Created.UTC(),
		Status: legacyManagementStatus{
			ID: status.ID, State: status.State, Phase: status.Phase, Kind: status.Kind, ThreadID: status.ThreadID,
			ServerID: status.ServerID, ErrorCode: status.ErrorCode, Attempts: status.Attempts,
			Retryable: status.Retryable, CancelRequested: status.CancelRequested, VoicePartsIncomplete: status.VoicePartsIncomplete,
			VoiceChunks: chunks, VoiceFailureStage: status.VoiceFailureStage, VoiceTransportKind: status.VoiceTransportKind,
			VoiceFailedOffset: status.VoiceFailedOffset,
		},
	}, nil
}

func legacyManagementRawValid(r legacyManagementRawRecord, account string) bool {
	status := r.Status
	return identity.ValidPathID(r.Key) && r.Hash != "" && len(r.Hash) <= 1024 && r.Account == account &&
		identity.ValidPathID(r.Account) && identity.ValidPathID(r.Thread) && legacyManagementCode(r.Kind) &&
		legacyManagementCode(status.State) && (status.Phase == "" || legacyManagementCode(status.Phase)) &&
		(status.Kind == "" || legacyManagementCode(status.Kind)) && (status.ErrorCode == "" || legacyManagementCode(status.ErrorCode)) &&
		(status.ID == "" || identity.ValidPathID(status.ID)) && (status.ThreadID == "" || status.ThreadID == r.Thread) &&
		(status.ServerID == "" || wechatapp.ValidServerReceiptID(status.ServerID)) &&
		(status.VoiceFailureStage == "" || legacyManagementCode(status.VoiceFailureStage)) &&
		(status.VoiceTransportKind == "" || legacyManagementCode(status.VoiceTransportKind))
}

func legacyManagementArchivedValid(r legacyManagementRawRecord, account string) bool {
	if !legacyManagementRawValid(r, account) || r.Status.State != "accepted" || r.Status.Retryable || r.Status.CancelRequested || r.Status.ID == "" || r.Status.ServerID == "" || r.Status.ThreadID != r.Thread {
		return false
	}
	for _, c := range r.Status.VoiceChunks {
		if c.State != "accepted" || !c.HasReceipt || !wechatapp.ValidServerReceiptID(c.ServerID) {
			return false
		}
	}
	return true
}

func legacyManagementSnapshotValid(s legacyManagementSnapshot, account string) bool {
	if s.Version != 2 || s.Sends == nil || len(s.Sends) > legacyManagementLedgerFiles+1000 {
		return false
	}
	for key, r := range s.Sends {
		status := r.Status
		if key != r.Key || r.Account != account || !identity.ValidPathID(r.Key) || !identity.ValidPathID(r.Account) ||
			!identity.ValidPathID(r.Thread) || !legacyManagementCode(r.Kind) || !legacyManagementCode(status.State) ||
			(status.Phase != "" && !legacyManagementCode(status.Phase)) || (status.Kind != "" && !legacyManagementCode(status.Kind)) ||
			(status.ErrorCode != "" && !legacyManagementCode(status.ErrorCode)) || (status.ID != "" && !identity.ValidPathID(status.ID)) ||
			(status.ThreadID != "" && status.ThreadID != r.Thread) || (status.ServerID != "" && !wechatapp.ValidServerReceiptID(status.ServerID)) {
			return false
		}
		if (r.Status.VoiceFailureStage != "" && !legacyManagementCode(r.Status.VoiceFailureStage)) || (r.Status.VoiceTransportKind != "" && !legacyManagementCode(r.Status.VoiceTransportKind)) {
			return false
		}
		previous := -1
		for _, c := range r.Status.VoiceChunks {
			if c.Index <= previous || !legacyManagementCode(c.State) || c.DurationMS < 0 || c.PacingMS < 0 || (c.ServerID != "" && !wechatapp.ValidServerReceiptID(c.ServerID)) {
				return false
			}
			previous = c.Index
		}
	}
	return true
}

func legacyManagementCode(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func legacyManagementLedgerName(kind, identity string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + identity))
	return hex.EncodeToString(h[:]) + ".json.gz"
}

func legacyManagementColdLeaf(name, root string) bool {
	prefix := strings.TrimSuffix(root, "/") + "/"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rel := strings.TrimPrefix(name, prefix)
	if strings.Count(rel, "/") != 1 {
		return false
	}
	bits := strings.Split(rel, "/")
	if len(bits) != 2 || len(bits[0]) != 2 || len(bits[1]) != 72 || bits[0] != bits[1][:2] || !strings.HasSuffix(bits[1], ".json.gz") {
		return false
	}
	return isLowerHex(strings.TrimSuffix(bits[1], ".json.gz"))
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func legacyManagementLedgerPresent(root *os.Root, adminDir string) (bool, error) {
	_, err := root.Lstat(filepath.Join(adminDir, "send-ledger"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
