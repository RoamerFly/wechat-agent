package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

const legacyMemoryDB = "memory.sqlite"

var errLegacyMemoryPage = errors.New("旧记忆核对游标无效，请重新查看")

type legacyMemoryConversation struct {
	ThreadID       string `json:"thread_id"`
	EventCount     int    `json:"event_count"`
	WithheldEvents int    `json:"withheld_events"`
	MemoryCount    int    `json:"memory_count"`
	HasSummary     bool   `json:"has_summary"`
	HasPreference  bool   `json:"has_preference,omitempty"`
	HistoryGranted bool   `json:"history_granted"`
}

type legacyMemoryPreview struct {
	Status         string                     `json:"status"`
	Backend        string                     `json:"backend,omitempty"`
	MatchedScopes  int                        `json:"matched_scopes"`
	UnmappedScopes int                        `json:"unmapped_scopes"`
	ForeignScopes  int                        `json:"foreign_scopes"`
	Conversations  []legacyMemoryConversation `json:"conversations"`
	NextAfter      string                     `json:"next_after,omitempty"`
}

func (s *stationWechat) legacyMemoryRoute(server *websession.Store, mux *http.ServeMux) {
	s.legacyMemoryImportRoutes(server, mux)
	mux.HandleFunc("GET /api/apps/wechat/connections/{connection}/legacy-import/memory", server.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		limit := 30
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 200 {
				websession.Fail(w, http.StatusBadRequest, "每页请输入 1 到 200 条")
				return
			}
		}
		after := r.URL.Query().Get("after")
		if after != "" {
			revision, thread, ok := strings.Cut(after, ":")
			_, err := hex.DecodeString(revision)
			if !ok || len(revision) != 64 || err != nil || !identity.ValidPathID(thread) {
				websession.Fail(w, http.StatusBadRequest, errLegacyMemoryPage.Error())
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		report, err := s.previewLegacyMemory(ctx, r.PathValue("connection"), after, limit)
		switch {
		case errors.Is(err, errLegacyMemoryPage):
			websession.Fail(w, http.StatusConflict, err.Error())
		case err != nil:
			legacyImportFailure(w, err)
		default:
			websession.Respond(w, http.StatusOK, report)
		}
	}))
}

func (s *stationWechat) previewLegacyMemory(ctx context.Context, id, after string, limit int) (legacyMemoryPreview, error) {
	var report legacyMemoryPreview
	connection, err := s.stationWechatNative.Inspect(ctx, id)
	if err != nil {
		return report, err
	}
	if connection.ID != id || !identity.ValidPathID(connection.AccountID) {
		return report, apphost.ErrNotFound
	}
	snapshot, source, err := s.openLegacyImport()
	if err != nil {
		return report, err
	}
	defer snapshot.Close()
	if source.guard != nil {
		if err := source.guard.verify(ctx, snapshot); err != nil {
			return report, err
		}
	}
	config, _, err := legacyImportAccountPaths(snapshot, source, connection.AccountID)
	if err != nil {
		return report, err
	}
	dir, err := workspacefs.OpenExistingDirectory(snapshot, filepath.Join(filepath.Dir(config), "companion"))
	if errors.Is(err, os.ErrNotExist) {
		return legacyMemoryPreview{Status: "missing"}, nil
	}
	if err != nil {
		return legacyMemoryPreview{Status: "unavailable"}, nil
	}
	defer dir.Close()
	report, version := readLegacyMemoryInventory(ctx, dir, connection.AccountID, legacyMemorySnapshotCheck{
		guard: source.guard, relative: filepath.ToSlash(filepath.Join(filepath.Dir(config), "companion", legacyMemoryDB)),
	})
	if source.guard != nil {
		version.SnapshotDigest = source.guard.digest()
	}
	if report.Status != "ready" {
		if after != "" {
			return legacyMemoryPreview{}, errLegacyMemoryPage
		}
		return report, nil
	}
	w, err := s.worker(id, false)
	if err != nil {
		return report, err
	}
	if w != nil {
		w.mu.Lock()
		for i := range report.Conversations {
			row := &report.Conversations[i]
			row.HistoryGranted = !w.policy.HistoryGrants[row.ThreadID].IsZero()
		}
		w.mu.Unlock()
	}
	return pageLegacyMemory(report, id, version, after, limit)
}

// A sidecar can contain committed data absent from the main file. Refuse the
// snapshot without letting SQLite recover it or make files beside the source.
func standaloneLegacyMemory(dir *os.Root) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := dir.Lstat(legacyMemoryDB + suffix); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

type legacyMemoryVersion struct {
	Size           int64
	Modified       time.Time
	SnapshotDigest string `json:"snapshot_digest,omitempty"`
}

type legacyMemorySnapshotCheck struct {
	guard    *legacySnapshotGuard
	relative string
}

func readLegacyMemoryInventory(ctx context.Context, dir *os.Root, account string, check legacyMemorySnapshotCheck) (legacyMemoryPreview, legacyMemoryVersion) {
	var version legacyMemoryVersion
	if !standaloneLegacyMemory(dir) {
		return legacyMemoryPreview{Status: "snapshot_incomplete", Backend: "sqlite"}, version
	}
	before, err := dir.Lstat(legacyMemoryDB)
	if errors.Is(err, os.ErrNotExist) {
		if _, jsonErr := dir.Lstat("memory.json"); jsonErr == nil {
			return legacyMemoryPreview{Status: "unsupported", Backend: "legacy_json"}, version
		} else if !errors.Is(jsonErr, os.ErrNotExist) {
			return legacyMemoryPreview{Status: "unavailable"}, version
		}
		return legacyMemoryPreview{Status: "missing"}, version
	}
	if err != nil || !before.Mode().IsRegular() {
		return legacyMemoryPreview{Status: "unavailable", Backend: "sqlite"}, version
	}
	file, err := workspacefs.OpenRegular(dir, legacyMemoryDB)
	if err != nil {
		return legacyMemoryPreview{Status: "unavailable", Backend: "sqlite"}, version
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !sameLegacyMemoryFile(before, actual) {
		return legacyMemoryPreview{Status: "snapshot_incomplete", Backend: "sqlite"}, version
	}
	if check.guard != nil {
		if err := check.guard.checkOpened(ctx, dir, check.relative, file); err != nil {
			return legacyMemoryPreview{Status: "snapshot_incomplete", Backend: "sqlite"}, version
		}
		if _, err := file.Seek(0, 0); err != nil {
			return legacyMemoryPreview{Status: "snapshot_incomplete", Backend: "sqlite"}, version
		}
	}
	inventory, err := companion.LegacyMemoryInventory(ctx, file, account)
	current, statErr := dir.Lstat(legacyMemoryDB)
	if statErr != nil || !sameLegacyMemoryFile(before, current) || !standaloneLegacyMemory(dir) {
		return legacyMemoryPreview{Status: "snapshot_incomplete", Backend: "sqlite"}, version
	}
	if errors.Is(err, companion.ErrLegacyMemoryUnsupported) {
		return legacyMemoryPreview{Status: "unsupported", Backend: "sqlite"}, version
	}
	if err != nil {
		return legacyMemoryPreview{Status: "unavailable", Backend: "sqlite"}, version
	}
	report := legacyMemoryPreview{Status: "ready", Backend: "sqlite", UnmappedScopes: inventory.UnmappedScopes,
		ForeignScopes: inventory.ForeignScopes, Conversations: make([]legacyMemoryConversation, 0, len(inventory.Conversations))}
	for _, row := range inventory.Conversations {
		if !identity.ValidPathID(row.ThreadID) {
			report.UnmappedScopes++
			continue
		}
		report.Conversations = append(report.Conversations, legacyMemoryConversation{
			ThreadID: row.ThreadID, EventCount: row.EventCount, WithheldEvents: row.WithheldEvents,
			MemoryCount: row.MemoryCount, HasSummary: row.HasSummary, HasPreference: row.HasPreference,
		})
	}
	report.MatchedScopes = len(report.Conversations)
	return report, legacyMemoryVersion{Size: before.Size(), Modified: before.ModTime().UTC()}
}

func sameLegacyMemoryFile(first, second os.FileInfo) bool {
	return second.Mode().IsRegular() && os.SameFile(first, second) && first.Size() == second.Size() && first.ModTime().Equal(second.ModTime())
}

func pageLegacyMemory(report legacyMemoryPreview, id string, version legacyMemoryVersion, after string, limit int) (legacyMemoryPreview, error) {
	compare := func(row legacyMemoryConversation, thread string) int { return strings.Compare(row.ThreadID, thread) }
	slices.SortFunc(report.Conversations, func(a, b legacyMemoryConversation) int { return compare(a, b.ThreadID) })
	if after == "" && len(report.Conversations) <= limit {
		return report, nil
	}
	data, err := json.Marshal(struct {
		Connection string
		Version    legacyMemoryVersion
		Report     legacyMemoryPreview
	}{id, version, report})
	if err != nil {
		return legacyMemoryPreview{}, apphost.ErrUnavailable
	}
	digest := sha256.Sum256(data)
	revision := hex.EncodeToString(digest[:])
	start := 0
	if after != "" {
		prior, thread, _ := strings.Cut(after, ":")
		pos, found := slices.BinarySearchFunc(report.Conversations, thread, compare)
		if prior != revision || !found || pos == len(report.Conversations)-1 {
			return legacyMemoryPreview{}, errLegacyMemoryPage
		}
		start = pos + 1
	}
	end := min(start+limit, len(report.Conversations))
	if end < len(report.Conversations) {
		report.NextAfter = revision + ":" + report.Conversations[end-1].ThreadID
	}
	report.Conversations = report.Conversations[start:end]
	return report, nil
}
