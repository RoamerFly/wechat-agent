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
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

type stationLegacyMemory struct {
	Version   int                             `json:"version"`
	Account   string                          `json:"account"`
	Thread    string                          `json:"thread"`
	Grant     time.Time                       `json:"grant"`
	Digest    string                          `json:"digest"`
	Selection companion.LegacyMemorySelection `json:"selection"`
}

type legacyMemoryImportView struct {
	Status      string `json:"status"`
	Digest      string `json:"digest,omitempty"`
	EventCount  int    `json:"event_count"`
	MemoryCount int    `json:"memory_count"`
	Reason      string `json:"reason,omitempty"`
}

func (m stationLegacyMemory) view(status string) legacyMemoryImportView {
	return legacyMemoryImportView{Status: status, Digest: m.Digest, EventCount: len(m.Selection.Events), MemoryCount: len(m.Selection.Memories)}
}

func stationLegacyMemoryFile(account, thread string) string {
	return "legacy-memory-" + companion.ScopeID("wechat", account, thread) + ".json"
}

// These files are host-private, bound to a connection's exact grant epoch, and
// never used as the worker's live memory database.
func readStationLegacyMemory(workerDir, account, thread string, grant time.Time) (stationLegacyMemory, error) {
	root, err := workspacefs.OpenPath(workerDir)
	if err != nil {
		return stationLegacyMemory{}, err
	}
	defer root.Close()
	return readStationLegacyMemoryAt(root, account, thread, grant)
}

func readStationLegacyMemoryAt(root *os.Root, account, thread string, grant time.Time) (stationLegacyMemory, error) {
	var item stationLegacyMemory
	raw, err := readLegacyImportFile(root, stationLegacyMemoryFile(account, thread), legacyImportArchiveLimit)
	if err != nil {
		return item, err
	}
	if json.Unmarshal(raw, &item) != nil || item.Version != 1 || item.Account != account || item.Thread != thread ||
		grant.IsZero() || !item.Grant.Equal(grant) || !legacyMemoryDigestValid(item.Digest) || len(item.Selection.Events) == 0 {
		return stationLegacyMemory{}, errLegacyImportConflict
	}
	return item, nil
}

func legacyMemoryDigestValid(digest string) bool {
	_, err := hex.DecodeString(digest)
	return len(digest) == sha256.Size*2 && err == nil
}

func (s *stationWechat) legacyMemoryImportRoutes(server *websession.Store, mux *http.ServeMux) {
	const path = "/api/apps/wechat/connections/{connection}/legacy-import/memory/{thread}"
	handle := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id, thread := r.PathValue("connection"), r.PathValue("thread")
		if !identity.ValidPathID(thread) {
			websession.Fail(w, http.StatusBadRequest, "会话编号无效")
			return
		}
		var digest string
		if r.Method == http.MethodPost {
			var input struct {
				Digest       string `json:"digest"`
				SameIdentity bool   `json:"same_identity"`
			}
			if !websession.Decode(w, r, &input) {
				return
			}
			if !legacyMemoryDigestValid(input.Digest) || !input.SameIdentity {
				websession.Fail(w, http.StatusBadRequest, "请先预演并确认新旧连接使用同一个微信身份")
				return
			}
			digest = input.Digest
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		view, err := s.legacyMemoryImport(ctx, id, thread, digest)
		if err != nil {
			legacyImportFailure(w, err)
			return
		}
		if digest != "" && view.Status != "applied" {
			websession.Fail(w, http.StatusConflict, view.Reason)
			return
		}
		websession.Respond(w, http.StatusOK, view)
	}
	mux.HandleFunc("GET "+path, server.Protect(handle))
	mux.HandleFunc("POST "+path, server.Protect(handle))
}

func (s *stationWechat) legacyMemoryImport(ctx context.Context, id, thread, digest string) (legacyMemoryImportView, error) {
	notReady := func(reason string) (legacyMemoryImportView, error) {
		return legacyMemoryImportView{Status: "not_ready", Reason: reason}, nil
	}
	conflict := func(reason string) (legacyMemoryImportView, error) {
		return legacyMemoryImportView{Status: "conflict", Reason: reason}, nil
	}
	native, err := s.WorkerSender(ctx, id)
	if err != nil {
		return legacyMemoryImportView{}, err
	}
	account := native.Account()
	w, err := s.worker(id, false)
	if err != nil {
		return legacyMemoryImportView{}, err
	}
	if w == nil {
		return notReady("请先保存旧版接入摘要，再授权此会话的历史读取")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	grant := w.policy.HistoryGrants[thread]
	if grant.IsZero() {
		return notReady("请先授权此会话的历史读取")
	}
	target, err := workspacefs.OpenPath(w.dir)
	if err != nil {
		return legacyMemoryImportView{}, apphost.ErrUnavailable
	}
	defer target.Close()
	prior, err := readStationLegacyMemoryAt(target, account, thread, grant)
	if err == nil {
		if digest != "" && prior.Digest != digest {
			return conflict("此会话已经接续旧记忆，请刷新查看")
		}
		return prior.view("applied"), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return conflict("此会话已有旧接续记录或原授权已变化，不能覆盖")
	}
	if !w.policy.Paused || w.run != nil {
		return notReady("请先暂停此连接并等候工作器退出")
	}
	archive, err := readLegacyImportArchive(target)
	if err != nil || archive.Plan.Account != account {
		return notReady("请先保存此连接的旧版接入摘要")
	}
	if view, ok := checkLegacyMemoryDestination(ctx, target, account, thread); !ok {
		return view, nil
	}
	snapshot, source, err := s.openLegacyImport()
	if err != nil {
		return legacyMemoryImportView{}, err
	}
	defer snapshot.Close()
	if !source.matchesArchive(archive) {
		return legacyMemoryImportView{}, errLegacySnapshotIntegrity
	}
	if source.guard != nil {
		if err := source.guard.verify(ctx, snapshot); err != nil {
			return legacyMemoryImportView{}, err
		}
	}
	config, _, err := legacyImportAccountPaths(snapshot, source, account)
	if err != nil {
		return legacyMemoryImportView{}, err
	}
	cutover, evidence := s.checkLegacyCutover(ctx, id, native, snapshot, source, archive.LegacyServer)
	if !legacyMemoryCutoverReady(cutover) {
		return notReady("请先核对旧宿主停机记录、原生档案导入回执与目标微信登录")
	}
	daemon, err := native.DaemonStatus(ctx)
	if err != nil {
		return notReady("目标微信原生服务暂时不可核对")
	}
	dir, err := workspacefs.OpenExistingDirectory(snapshot, filepath.Join(filepath.Dir(config), "companion"))
	if err != nil {
		return notReady("未找到此账号的旧 SQLite 记忆副本")
	}
	defer dir.Close()
	item := stationLegacyMemory{Version: 1, Account: account, Thread: thread, Grant: grant}
	selection, sourceFile, err := readStationLegacyScope(ctx, dir, account, thread, companion.ReadLegacyMemorySelection, legacyMemorySnapshotCheck{
		guard: source.guard, relative: filepath.ToSlash(filepath.Join(filepath.Dir(config), "companion", legacyMemoryDB)),
	})
	item.Selection = selection
	if err != nil {
		return notReady("旧记忆副本不完整、不可读或会话身份无法核对")
	}
	if len(item.Selection.Events) == 0 {
		return legacyMemoryImportView{Status: "empty", Reason: "没有来源身份和版本均可核对的原始记录"}, nil
	}
	binding := struct {
		Installation, Connection, Source, Server, Generation string
		Evidence                                             [2][32]byte
		SourceVersion                                        legacyMemoryVersion
		Item                                                 stationLegacyMemory
	}{s.installation, id, source.Root, daemon.ServerID, daemon.OwnershipGeneration, evidence, legacyMemoryVersion{Size: sourceFile.Size(), Modified: sourceFile.ModTime().UTC(), SnapshotDigest: source.guard.digest()}, item}
	fingerprint, err := json.Marshal(binding)
	if err != nil || len(fingerprint) > legacyImportArchiveLimit {
		return notReady("该会话的接续资料超过单次保存上限")
	}
	sum := sha256.Sum256(fingerprint)
	item.Digest = hex.EncodeToString(sum[:])
	if digest == "" {
		return item.view("ready"), nil
	}
	if item.Digest != digest {
		return conflict("旧记忆来源或目标授权已变化，请重新预演")
	}
	latest, latestEvidence := s.checkLegacyCutover(ctx, id, native, snapshot, source, archive.LegacyServer)
	status, err := native.DaemonStatus(ctx)
	if err != nil || status != daemon || !legacyMemoryCutoverReady(latest) || latestEvidence != evidence {
		return conflict("切换前置条件已变化，请重新预演")
	}
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return legacyMemoryImportView{}, apphost.ErrLifecycleConflict
	}
	temp := ".legacy-memory-" + identity.RandomToken()
	defer target.Remove(temp)
	if err := writeLegacyStagedFile(target, temp, item); err != nil {
		return legacyMemoryImportView{}, err
	}
	// Publication cannot replace an earlier record, including one from a
	// revoked grant. Policy changes are serialized by the same worker lock.
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return legacyMemoryImportView{}, apphost.ErrLifecycleConflict
	}
	if current, err := dir.Lstat(legacyMemoryDB); err != nil || !sameLegacyMemoryFile(sourceFile, current) || !standaloneLegacyMemory(dir) {
		return conflict("旧记忆副本在确认期间变化，请重新预演")
	}
	if source.guard != nil {
		if err := checkLegacyMemorySnapshotFile(ctx, legacyMemorySnapshotCheck{
			guard: source.guard, relative: filepath.ToSlash(filepath.Join(filepath.Dir(config), "companion", legacyMemoryDB)),
		}, dir); err != nil {
			return legacyMemoryImportView{}, err
		}
	}
	if err := target.Link(temp, stationLegacyMemoryFile(account, thread)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return conflict("此会话已存在接续记录，请刷新查看")
		}
		return legacyMemoryImportView{}, err
	}
	return item.view("applied"), nil
}

func legacyMemoryCutoverReady(view *legacyCutover) bool {
	return view.LegacyHost == "stop_record_matched" && view.NativeProfile == "import_receipt_matched" && view.TargetLogin == "online"
}

func checkLegacyMemoryDestination(ctx context.Context, worker *os.Root, account, thread string) (legacyMemoryImportView, bool) {
	dir, err := workspacefs.OpenExistingDirectory(worker, "memory")
	if errors.Is(err, os.ErrNotExist) {
		return legacyMemoryImportView{}, true
	}
	if err != nil {
		return legacyMemoryImportView{Status: "not_ready", Reason: "目标记忆暂时不可核对"}, false
	}
	defer dir.Close()
	report, _ := readLegacyMemoryInventory(ctx, dir, account, legacyMemorySnapshotCheck{})
	if report.Status == "missing" {
		return legacyMemoryImportView{}, true
	}
	if report.Status != "ready" || report.UnmappedScopes != 0 {
		return legacyMemoryImportView{Status: "not_ready", Reason: "目标记忆快照或会话身份暂时不可核对"}, false
	}
	for _, row := range report.Conversations {
		if row.ThreadID == thread {
			return legacyMemoryImportView{Status: "conflict", Reason: "目标会话已有记忆，不能覆盖接续"}, false
		}
	}
	return legacyMemoryImportView{}, true
}

func readStationLegacyScope[T any](ctx context.Context, dir *os.Root, account, thread string, read func(context.Context, *os.File, string, string) (T, error), check legacyMemorySnapshotCheck) (T, os.FileInfo, error) {
	var empty T
	if !standaloneLegacyMemory(dir) {
		return empty, nil, errLegacyImportInvalid
	}
	before, err := dir.Lstat(legacyMemoryDB)
	if err != nil || !before.Mode().IsRegular() {
		return empty, nil, errLegacyImportInvalid
	}
	f, err := workspacefs.OpenRegular(dir, legacyMemoryDB)
	if err != nil {
		return empty, nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !sameLegacyMemoryFile(before, actual) {
		return empty, nil, errLegacyImportChanged
	}
	if check.guard != nil {
		if err := check.guard.checkOpened(ctx, dir, check.relative, f); err != nil {
			return empty, nil, err
		}
		if _, err := f.Seek(0, 0); err != nil {
			return empty, nil, err
		}
	}
	selection, err := read(ctx, f, account, thread)
	after, statErr := dir.Lstat(legacyMemoryDB)
	if statErr != nil || !sameLegacyMemoryFile(before, after) || !standaloneLegacyMemory(dir) {
		return empty, nil, errLegacyImportChanged
	}
	return selection, before, err
}

func checkLegacyMemorySnapshotFile(ctx context.Context, check legacyMemorySnapshotCheck, dir *os.Root) error {
	if check.guard == nil {
		return nil
	}
	file, err := workspacefs.OpenRegular(dir, legacyMemoryDB)
	if err != nil {
		return err
	}
	defer file.Close()
	return check.guard.checkOpened(ctx, dir, check.relative, file)
}
