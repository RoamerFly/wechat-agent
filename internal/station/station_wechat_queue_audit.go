package station

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/workspacefs"
)

type LegacyWechatQueueAudit struct {
	Accounts            int                       `json:"account_count"`
	WorkerLedgers       int                       `json:"worker_ledger_count"`
	WorkerDelivery      map[string]map[string]int `json:"worker_delivery"`
	WorkerDeliveryOpen  int                       `json:"worker_delivery_open"`
	WaitingWakes        int                       `json:"waiting_wakes"`
	InboxPending        int                       `json:"inbox_pending"`
	PlanPending         int                       `json:"plan_pending"`
	MediaReplyPending   int                       `json:"media_reply_pending"`
	AdminTasksPending   int                       `json:"admin_tasks_pending"`
	AdminOperationsOpen int                       `json:"admin_operations_open"`
	AdminDelivery       map[string]int            `json:"admin_delivery"`
	StopMarkerPresent   bool                      `json:"stop_marker_present"`
	ClaimPresent        bool                      `json:"claim_present"`
}

type legacyAuditRead struct {
	limit   int64
	present bool
	digest  [32]byte
}

// AuditLegacyWechatQueue inspects the old host's on-disk ledgers without a
// running daemon or stop proof. A stable read is advisory, never a stop proof.
func AuditLegacyWechatQueue(ctx context.Context, legacyDataDir, primary string) (LegacyWechatQueueAudit, error) {
	var out LegacyWechatQueueAudit
	legacyDataDir = cutoverPath(legacyDataDir)
	if runtime.GOOS != "windows" || !filepath.IsAbs(legacyDataDir) || len(filepath.VolumeName(legacyDataDir)) != 2 || !identity.ValidPathID(primary) {
		return out, errors.New("旧微信队列核对需要 Windows 本机绝对数据目录和主账号")
	}
	root, err := workspacefs.OpenPath(legacyDataDir)
	if err != nil {
		return out, errors.New("无法安全打开旧微信数据目录")
	}
	defer root.Close()
	if _, err := legacySnapshotLocalPath(root); err != nil {
		return out, errors.New("旧微信数据必须位于可核对的本机磁盘")
	}
	readFile := func(name string, limit int64) ([]byte, legacyAuditRead, error) {
		if err := ctx.Err(); err != nil {
			return nil, legacyAuditRead{}, err
		}
		raw, err := readLegacyImportFile(root, name, limit)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, legacyAuditRead{}, errors.New("旧微信队列核对文件不可读或超出支持范围")
		}
		return raw, legacyAuditRead{limit: limit, present: err == nil, digest: sha256.Sum256(raw)}, nil
	}
	reads := map[string]legacyAuditRead{}
	read := func(name string, limit int64) ([]byte, error) {
		raw, stamp, err := readFile(name, limit)
		reads[name] = stamp
		return raw, err
	}
	registry, err := read("admin/accounts.json", 1<<20)
	if err != nil {
		return out, err
	}
	accounts := []string{primary}
	if registry != nil {
		accounts, _, err = legacySnapshotAccounts(registry, primary)
		if err != nil {
			return out, err
		}
	}
	out = LegacyWechatQueueAudit{Accounts: len(accounts), WorkerDelivery: map[string]map[string]int{"text": {}, "media": {}}, AdminDelivery: map[string]int{}}
	for _, account := range accounts {
		worker, admin := "bot-state", "admin"
		if account != primary {
			id := wechatapp.AccountDirectory(account)
			worker = filepath.Join(worker, "accounts", id)
			admin = filepath.Join(admin, "accounts", id)
		}
		queue, err := read(filepath.Join(worker, "messages-v2.json"), bot.MaxDeliveryCheckpointBytes)
		if err != nil {
			return LegacyWechatQueueAudit{}, err
		}
		cursor, err := read(filepath.Join(worker, "checkpoint.json"), 128<<10)
		if err != nil {
			return LegacyWechatQueueAudit{}, err
		}
		tasks, err := read(filepath.Join(admin, "agent-state.json"), 16<<20)
		if err != nil {
			return LegacyWechatQueueAudit{}, err
		}
		sends, err := read(filepath.Join(admin, "tool-state.json"), 2<<20)
		if err != nil {
			return LegacyWechatQueueAudit{}, err
		}
		if err := out.addAccount(account, queue, cursor, tasks, sends); err != nil {
			return LegacyWechatQueueAudit{}, err
		}
	}
	for _, marker := range []struct {
		name string
		dest *bool
	}{{legacyStopMarkerFile, &out.StopMarkerPresent}, {legacyCutoverClaimFile, &out.ClaimPresent}} {
		if _, err := read(marker.name, 64<<10); err != nil {
			return LegacyWechatQueueAudit{}, err
		}
		*marker.dest = reads[marker.name].present
	}
	for name, previous := range reads {
		_, current, err := readFile(name, previous.limit)
		if err != nil || current != previous {
			return LegacyWechatQueueAudit{}, errors.New("旧微信队列核对期间内容有变化或无法复查，请在服务停止后重试")
		}
	}
	return out, nil
}

func (out *LegacyWechatQueueAudit) addAccount(account string, queue, cursor, tasks, sends []byte) error {
	server, evidence, err := bot.LegacyDeliveryEvidence(queue, cursor, account)
	if err != nil || (server != "" && !identity.ValidPathID(server)) {
		return errors.New("旧投递账本或游标的归属无法核对")
	}
	for key, row := range evidence {
		if !identity.ValidPathID(key) || !identity.ValidPathID(row.Status.ThreadID) || (row.Status.ID != "" && !identity.ValidPathID(row.Status.ID)) {
			return errors.New("旧投递记录的归属无法核对")
		}
		kind := "media"
		if row.Status.Kind == "text" {
			kind = "text"
		}
		addLegacyAuditReceipt(out.WorkerDelivery[kind], row.Status)
	}
	type row struct{ State string }
	var worker struct {
		Inbox      []struct{ Phase string }
		Deliveries []*row
		Media      []*row
		Wakes      []row `json:"conversation_wakes"`
		Plans      []row `json:"utterance_plans"`
		Replies    []row `json:"media_replies"`
	}
	var admin struct{ Tasks, Operations map[string]row }
	var tools struct {
		Version int
		Sends   map[string]struct{ Status linuxnative.ToolStatus }
	}
	if (queue != nil && json.Unmarshal(queue, &worker) != nil) ||
		(tasks != nil && json.Unmarshal(tasks, &admin) != nil) ||
		(sends != nil && (json.Unmarshal(sends, &tools) != nil || tools.Version != 1 || tools.Sends == nil)) {
		return errors.New("旧微信待办或管理端投递记录格式无效")
	}
	if queue != nil {
		out.WorkerLedgers++
	}
	for _, item := range worker.Deliveries {
		if item != nil && item.State != "accepted" && item.State != "stopped" {
			out.WorkerDeliveryOpen++
		}
	}
	for _, item := range worker.Media {
		if item != nil && item.State != "accepted" && item.State != "stopped" && item.State != "needs_review" {
			out.WorkerDeliveryOpen++
		}
	}
	for _, item := range worker.Inbox {
		switch item.Phase {
		case "completed", "failed", "interrupted", "needs_review", "replied", "silent", "skipped", "stopped":
		default:
			out.InboxPending++
		}
	}
	for _, item := range worker.Wakes {
		if item.State == "waiting" || item.State == "claimed" {
			out.WaitingWakes++
		}
	}
	for _, item := range worker.Plans {
		if item.State != "accepted" && item.State != "stopped" {
			out.PlanPending++
		}
	}
	for _, item := range worker.Replies {
		if item.State != "accepted" && item.State != "stopped" && item.State != "queued_plan" {
			out.MediaReplyPending++
		}
	}
	for _, item := range admin.Tasks {
		switch item.State {
		case "completed", "cancelled", "needs_review", "failed", "stopped":
		default:
			out.AdminTasksPending++
		}
	}
	for _, item := range admin.Operations {
		switch item.State {
		case "running", "queued", "pending", "submitting", "unknown":
			out.AdminOperationsOpen++
		}
	}
	for _, item := range tools.Sends {
		addLegacyAuditReceipt(out.AdminDelivery, item.Status)
	}
	return nil
}

func addLegacyAuditReceipt(counts map[string]int, status linuxnative.ToolStatus) {
	var receipt apphost.DeliveryReceipt
	wechatapp.ProjectDeliveryReceipt(&receipt, status)
	if receipt.PartsIncomplete && receipt.State != "submitted_pending" {
		receipt.State = "submission_unknown"
	}
	counts[receipt.State]++
}
