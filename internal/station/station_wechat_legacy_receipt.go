package station

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

const legacyWorkerReceiptLookup = "legacy_snapshot"
const legacyManagementReceiptLookup = "legacy_management_snapshot"

type legacyReceiptPage struct {
	apphost.ReceiptPage
	ReceiptCount int `json:"receipt_count"`
	PendingCount int `json:"pending_count"`
}

// The management store has no per-record dataset binding. Its projected
// receipts can be displayed after snapshot validation, with no native lookup.
func readLegacyManagementReceipts(root *os.Root, config string, guard *legacySnapshotGuard, ref apphost.ConnectionRef) (map[string]apphost.Receipt, int, error) {
	if guard == nil {
		return nil, 0, nil
	}
	raw, err := guard.readFile(root, filepath.Join(filepath.Dir(config), "tool-state.json"), legacyImportArchiveLimit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, legacyImportReadError(err)
	}
	saved, err := decodeLegacyManagementSnapshot(raw, ref.AccountID)
	if err != nil {
		return nil, 0, errLegacySnapshotIntegrity
	}
	if len(saved.Sends) == 0 {
		return nil, 0, nil
	}
	receipts := make(map[string]apphost.Receipt, len(saved.Sends))
	pending := 0
	for key, record := range saved.Sends {
		status := linuxnative.ToolStatus{
			ID: record.Status.ID, State: record.Status.State, ServerID: record.Status.ServerID,
			CancelRequested: record.Status.CancelRequested, ErrorCode: record.Status.ErrorCode,
			VoicePartsIncomplete: record.Status.VoicePartsIncomplete,
		}
		for _, chunk := range record.Status.VoiceChunks {
			status.VoiceChunks = append(status.VoiceChunks, linuxnative.VoiceChunkStatus{
				Index: chunk.Index, State: chunk.State, ServerID: chunk.ServerID, HasReceipt: chunk.HasReceipt,
			})
		}
		receipt := apphost.Receipt{ConnectionRef: ref, ConversationID: record.Thread,
			DeliveryReceipt: apphost.DeliveryReceipt{IdempotencyKey: key, Kind: record.Kind, Lookup: legacyManagementReceiptLookup}}
		wechatapp.ProjectDeliveryReceipt(&receipt.DeliveryReceipt, status)
		if legacyReceiptPending(receipt) {
			pending++
		}
		receipts[key] = receipt
	}
	return receipts, pending, nil
}

func legacyReceiptOrigin(w http.ResponseWriter, r *http.Request) (management, ok bool) {
	switch r.URL.Query().Get("origin") {
	case "", "agent":
		return false, true
	case "management":
		return true, true
	default:
		websession.Fail(w, http.StatusBadRequest, "旧投递回执来源无效")
		return false, false
	}
}

func (s *stationWechat) legacyReceiptRoutes(server *websession.Store, mux *http.ServeMux, path string) {
	mux.HandleFunc("GET "+path, server.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		management, ok := legacyReceiptOrigin(w, r)
		if !ok {
			return
		}
		query := r.URL.Query()
		limit := 30
		if raw := query.Get("limit"); raw != "" {
			var err error
			if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > 200 {
				websession.Fail(w, http.StatusBadRequest, "每页请输入 1 到 200 条")
				return
			}
		}
		pending := query.Get("pending")
		if pending != "" && pending != "1" {
			websession.Fail(w, http.StatusBadRequest, "待核对筛选参数无效")
			return
		}
		cursor := query.Get("after")
		revision, key, _ := strings.Cut(cursor, ":")
		if cursor != "" && (!legacyMemoryDigestValid(revision) || !identity.ValidPathID(key)) {
			websession.Fail(w, http.StatusBadRequest, "旧回执游标无效，请重新查看")
			return
		}
		page, err := s.listLegacyReceipts(r.PathValue("connection"), revision, key, limit, pending == "1", management)
		if err != nil {
			legacyImportFailure(w, err)
			return
		}
		websession.Respond(w, http.StatusOK, page)
	}))
	mux.HandleFunc("GET "+path+"/{key}", server.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		management, ok := legacyReceiptOrigin(w, r)
		if !ok {
			return
		}
		id, key := r.PathValue("connection"), r.PathValue("key")
		archive, receipt, err := s.legacyReceiptAt(id, key, management)
		if errors.Is(err, apphost.ErrNotFound) {
			websession.Fail(w, http.StatusNotFound, "此连接没有这条旧投递回执")
			return
		}
		if err != nil {
			legacyImportFailure(w, err)
			return
		}
		if !management && legacyReceiptPending(receipt) {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			// The saved record stays available during a native outage. The
			// check itself uses a freshly roster-bound native transport.
			if receipt.OperationID == "" {
				receipt.Lookup = "legacy_operation_unknown"
			} else if archive.LegacyServer == "" {
				receipt.Lookup = "legacy_dataset_mismatch"
			} else if native, err := s.WorkerSender(ctx, id); err != nil {
				receipt.Lookup = "legacy_native_unavailable"
			} else {
				receipt = checkLegacyReceipt(ctx, native, archive, receipt)
			}
		}
		websession.Respond(w, http.StatusOK, receipt)
	}))
}

func (s *stationWechat) legacyReceiptArchive(id string) (legacyImportArchive, error) {
	if !artifactID(id, "wechat-") {
		return legacyImportArchive{}, apphost.ErrNotFound
	}
	root, err := workspacefs.OpenPath(filepath.Join(s.root, id))
	if errors.Is(err, os.ErrNotExist) {
		return legacyImportArchive{}, apphost.ErrNotFound
	}
	if err != nil {
		return legacyImportArchive{}, apphost.ErrUnavailable
	}
	defer root.Close()
	archive, err := readLegacyImportArchive(root)
	if errors.Is(err, os.ErrNotExist) {
		return legacyImportArchive{}, apphost.ErrNotFound
	}
	if err != nil || wechatapp.ConnectionID(archive.Plan.Account) != id || !legacyMemoryDigestValid(archive.Plan.Digest) {
		return legacyImportArchive{}, errLegacyImportInvalid
	}
	return archive, nil
}

func validLegacyReceipt(receipt apphost.Receipt, ref apphost.ConnectionRef, key, lookup string) bool {
	return receipt.ConnectionRef == ref && receipt.IdempotencyKey == key && identity.ValidPathID(key) &&
		identity.ValidPathID(receipt.ConversationID) && (receipt.OperationID == "" || identity.ValidPathID(receipt.OperationID)) &&
		receipt.Lookup == lookup
}

func legacyReceiptPending(receipt apphost.Receipt) bool {
	return receipt.State == "submission_unknown" || receipt.State == "submitted_pending" || receipt.PartsIncomplete
}

func (archive legacyImportArchive) receiptSet(management bool) (map[string]apphost.Receipt, string) {
	if management {
		return archive.ManagementReceipts, legacyManagementReceiptLookup
	}
	return archive.Receipts, legacyWorkerReceiptLookup
}

func (s *stationWechat) listLegacyReceipts(id, revision, after string, limit int, pendingOnly, management bool) (legacyReceiptPage, error) {
	archive, err := s.legacyReceiptArchive(id)
	if err != nil {
		return legacyReceiptPage{}, err
	}
	receipts, lookup := archive.receiptSet(management)
	page := legacyReceiptPage{ReceiptPage: apphost.ReceiptPage{Items: []apphost.Receipt{}, Coverage: lookup}, ReceiptCount: len(receipts)}
	if management {
		page.Snapshot = localCheckpointKey("legacy-management-receipts-v1", id, archive.Plan.Digest, receipts)
	} else {
		page.Snapshot = localCheckpointKey("legacy-receipts-v1", id, archive.Plan.Digest, archive.LegacyServer, receipts, archive.ReceiptSources)
	}
	marker := localCheckpointKey(page.Snapshot, pendingOnly)
	if revision != "" {
		item, exists := receipts[after]
		if revision != marker || !exists || pendingOnly && !legacyReceiptPending(item) {
			return legacyReceiptPage{}, errLegacyImportChanged
		}
	}
	ref := s.connectionRef(id, archive.Plan.Account)
	keys := make([]string, 0, len(receipts))
	for key, receipt := range receipts {
		if !validLegacyReceipt(receipt, ref, key, lookup) {
			return legacyReceiptPage{}, errLegacyImportInvalid
		}
		pending := legacyReceiptPending(receipt)
		if pending {
			page.PendingCount++
		}
		if key > after && (!pendingOnly || pending) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	if len(keys) > limit {
		keys = keys[:limit]
		page.NextAfter = marker + ":" + keys[len(keys)-1]
	}
	for _, key := range keys {
		page.Items = append(page.Items, receipts[key])
	}
	return page, nil
}

func (s *stationWechat) legacyReceiptAt(id, key string, management bool) (legacyImportArchive, apphost.Receipt, error) {
	if !identity.ValidPathID(key) {
		return legacyImportArchive{}, apphost.Receipt{}, apphost.ErrNotFound
	}
	archive, err := s.legacyReceiptArchive(id)
	if err != nil {
		return legacyImportArchive{}, apphost.Receipt{}, err
	}
	receipts, lookup := archive.receiptSet(management)
	receipt, exists := receipts[key]
	if !exists {
		return legacyImportArchive{}, apphost.Receipt{}, apphost.ErrNotFound
	}
	if !validLegacyReceipt(receipt, s.connectionRef(id, archive.Plan.Account), key, lookup) {
		return legacyImportArchive{}, apphost.Receipt{}, errLegacyImportInvalid
	}
	return archive, receipt, nil
}

func (s *stationWechat) readLegacyReceipt(ctx context.Context, id, key string, native *linuxnative.Sender) (apphost.Receipt, error) {
	archive, receipt, err := s.legacyReceiptAt(id, key, false)
	if errors.Is(err, errLegacyImportInvalid) {
		return apphost.Receipt{}, apphost.ErrUnavailable
	}
	if err != nil || !legacyReceiptPending(receipt) || receipt.OperationID == "" || archive.LegacyServer == "" {
		return receipt, err
	}
	receipt = checkLegacyReceipt(ctx, native, archive, receipt)
	// Keep the original fallback API's local snapshot result when the
	// native instance or dataset cannot be verified.
	if receipt.Lookup == "legacy_native_unavailable" || receipt.Lookup == "legacy_dataset_mismatch" {
		receipt.Lookup = "legacy_snapshot"
	}
	return receipt, nil
}

func checkLegacyReceipt(ctx context.Context, native *linuxnative.Sender, archive legacyImportArchive, receipt apphost.Receipt) apphost.Receipt {
	if native.Account() != receipt.AccountID {
		receipt.Lookup = "identity_mismatch"
		return receipt
	}
	dataset, err := native.DaemonStatus(ctx)
	if err != nil {
		receipt.Lookup = "legacy_native_unavailable"
		return receipt
	}
	if dataset.ServerID != archive.LegacyServer {
		receipt.Lookup = "legacy_dataset_mismatch"
		return receipt
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := native.ToolSendStatus(ctx, receipt.OperationID)
	source := archive.ReceiptSources[receipt.IdempotencyKey]
	switch {
	case errors.Is(err, linuxnative.ErrToolNotFound):
		receipt.Lookup = "not_found"
	case err != nil:
		receipt.Lookup = "unavailable"
	case status.ID != receipt.OperationID || status.ThreadID != receipt.ConversationID ||
		status.Kind != "" && status.Kind != "send" && status.Kind != receipt.Kind || source != "" && status.SourceID != source:
		receipt.Lookup = "identity_mismatch"
	default:
		latest, err := native.DaemonStatus(ctx)
		if err != nil || latest != dataset {
			receipt.Lookup = "legacy_native_unavailable"
		} else if projectLegacyReceipt(&receipt.DeliveryReceipt, status) {
			receipt.Lookup = "platform"
		} else {
			receipt.Lookup = "identity_mismatch"
		}
	}
	return receipt
}

// A later observation cannot erase confirmed parts or report a conflicting
// part ID as success. Preserve old unconfirmed parts absent from the reply.
func projectLegacyReceipt(receipt *apphost.DeliveryReceipt, status linuxnative.ToolStatus) bool {
	chunks := make(map[int]linuxnative.VoiceChunkStatus, len(status.VoiceChunks))
	for _, chunk := range status.VoiceChunks {
		if _, duplicate := chunks[chunk.Index]; duplicate {
			return false
		}
		chunks[chunk.Index] = chunk
	}
	for _, part := range receipt.AcceptedParts {
		chunk, found := chunks[part.Index]
		if found && chunk.HasReceipt && chunk.ServerID != part.ServerID {
			return false
		}
		if !found || !chunk.HasReceipt {
			status.VoicePartsIncomplete = true
		}
	}
	var omitted []apphost.ReceiptPart
	for _, part := range receipt.UnconfirmedParts {
		if _, found := chunks[part.Index]; !found {
			status.VoicePartsIncomplete = true
			omitted = append(omitted, part)
		}
	}
	if receipt.PartsIncomplete && len(chunks) == 0 {
		status.VoicePartsIncomplete = true
	}
	wechatapp.ProjectDeliveryReceipt(receipt, status)
	receipt.UnconfirmedParts = append(receipt.UnconfirmedParts, omitted...)
	return true
}
