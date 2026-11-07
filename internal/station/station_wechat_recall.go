package station

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/stationservices"

	"wechat-bot/internal/websession"
)

type stationRecallSource struct {
	worker                                 *stationWechatWorker
	account, thread, deliveryKey, serverID string
}

type stationRecallView struct {
	MessageID  string                 `json:"message_id,omitempty"`
	NextCursor string                 `json:"next_cursor,omitempty"`
	Operation  *apphost.ActionReceipt `json:"operation,omitempty"`
}

var errStationRecallCursor = errors.New("invalid recall cursor")

func (s *stationWechat) recallRoutes(owner *websession.Store, mux *http.ServeMux, identity stationservices.OwnerReader) {
	const route = "/api/apps/wechat/connections/{connection}/receipts/{receipt}/recall"
	mux.HandleFunc("GET "+route, owner.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
		id, key, server := r.PathValue("connection"), r.PathValue("receipt"), r.URL.Query().Get("server_id")
		source, err := s.stationRecallEvidence(ctx, id, key, server)
		if err == nil {
			var view stationRecallView
			view, err = s.readStationRecall(ctx, id, source, r.URL.Query().Get("cursor"))
			if err == nil {
				websession.Respond(w, http.StatusOK, view)
				return
			}
		}
		stationRecallFailure(w, err)
	}))
	mux.HandleFunc("POST "+route, owner.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var in struct {
			ServerID  string `json:"server_id"`
			MessageID string `json:"message_id"`
		}
		if !websession.Decode(w, r, &in) {
			return
		}
		if !wechatapp.ValidServerReceiptID(in.ServerID) || !validStationRecallMessageID(in.MessageID) {
			websession.Fail(w, http.StatusBadRequest, "请从投递记录中选择已核对的原消息")
			return
		}
		record, err := identity()
		if err != nil {
			websession.Fail(w, http.StatusConflict, "请先准备本地主人身份")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
		id, key := r.PathValue("connection"), r.PathValue("receipt")
		source, err := s.stationRecallEvidence(ctx, id, key, in.ServerID)
		if err == nil {
			var result apphost.ActionReceipt
			result, err = s.submitStationRecall(ctx, id, source, in.MessageID, record.ID, func() bool {
				_, _, ok := owner.Lookup(r)
				current, err := identity()
				return ok && err == nil && current == record
			})
			if err == nil {
				websession.Respond(w, http.StatusAccepted, result)
				return
			}
		}
		stationRecallFailure(w, err)
	}))
}

func stationRecallFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, apphost.ErrNotFound):
		websession.Fail(w, http.StatusNotFound, "未找到该连接中匹配的已接受投递")
	case errors.Is(err, errStationRecallCursor):
		websession.Fail(w, http.StatusBadRequest, "此查询位置已失效，请从首批重新查找")
	case errors.Is(err, apphost.ErrLifecycleCutover):
		websession.Fail(w, http.StatusConflict, "旧微信接续未获启用或凭证已失效，撤回未提交")
	case errors.Is(err, apphost.ErrLifecycleConflict), errors.Is(err, errContactLogin):
		websession.Fail(w, http.StatusConflict, "账号未登录、连接已暂停或原消息身份无法核对")
	default:
		websession.Fail(w, http.StatusServiceUnavailable, "暂时无法核对撤回，请查询原操作")
	}
}

func (s *stationWechat) stationRecallEvidence(ctx context.Context, id, key, server string) (stationRecallSource, error) {
	var out stationRecallSource
	if !identity.ValidPathID(key) || !wechatapp.ValidServerReceiptID(server) {
		return out, apphost.ErrNotFound
	}
	w, account, err := s.deliveryCheckpoint(id)
	if errors.Is(err, os.ErrNotExist) {
		return out, apphost.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	rows, err := bot.ReadDeliveryEvidence(filepath.Join(w.dir, "queue"), account, []string{key})
	if err != nil {
		return out, apphost.ErrUnavailable
	}
	row, ok := rows[key]
	if !ok || !identity.ValidPathID(row.Status.ThreadID) {
		return out, apphost.ErrNotFound
	}
	ref := apphost.DeliveryReceipt{IdempotencyKey: key, Kind: row.Status.Kind}
	wechatapp.ProjectDeliveryReceipt(&ref, row.Status)
	if !stationRecallAccepted(ref, server) {
		receipt, err := s.ReadReceipt(ctx, id, key)
		if err != nil {
			return out, err
		}
		if receipt.ConnectionRef != s.connectionRef(id, account) || receipt.ConversationID != row.Status.ThreadID ||
			!stationRecallAccepted(receipt.DeliveryReceipt, server) {
			return out, apphost.ErrNotFound
		}
	}
	return stationRecallSource{w, account, row.Status.ThreadID, key, server}, nil
}

func stationRecallAccepted(receipt apphost.DeliveryReceipt, server string) bool {
	if receipt.State == "accepted" && receipt.ServerID == server {
		return true
	}
	for _, part := range receipt.AcceptedParts {
		if part.ServerID == server {
			return true
		}
	}
	return false
}

func validStationRecallMessageID(id string) bool {
	raw, ok := strings.CutPrefix(id, "msg_")
	if !ok || raw == "" || len(id) > 8192 {
		return false
	}
	for _, c := range raw {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (source stationRecallSource) operationID(id string) string {
	return "station-recall-" + localCheckpointKey(id, source.thread, source.serverID)
}

func (s *stationWechat) stationRecallNative(ctx context.Context, id, account string) (*linuxnative.Sender, error) {
	current, err := s.signedInAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	if current != account {
		return nil, apphost.ErrLifecycleConflict
	}
	native, err := s.WorkerSender(ctx, id)
	if err != nil {
		return nil, err
	}
	if native.Account() != account {
		return nil, apphost.ErrUnavailable
	}
	return native, nil
}

func (s *stationWechat) readStationRecall(ctx context.Context, id string, source stationRecallSource, cursor string) (stationRecallView, error) {
	var view stationRecallView
	ledger, paused, err := s.stationActionLedger(id)
	if err != nil {
		return view, err
	}
	if p, ok := ledger.FindConnected(s.installation, id, source.operationID(id), "", "recall"); ok {
		result := s.lookupStationAction(ctx, ledger, p)
		view.Operation = &result
		return view, nil
	}
	if paused {
		return view, apphost.ErrLifecycleConflict
	}
	scope := []byte(localCheckpointKey("recall", id, source.deliveryKey, source.thread, source.serverID))
	platformCursor := ""
	if cursor != "" {
		if len(cursor) > stationContactCursorLimit {
			return view, errStationRecallCursor
		}
		packet, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return view, errStationRecallCursor
		}
		plain, err := s.contactCursor.Open(nil, nil, packet, scope)
		if err != nil || len(plain) == 0 || len(plain) > 8192 {
			return view, errStationRecallCursor
		}
		platformCursor = string(plain)
	}
	native, err := s.stationRecallNative(ctx, id, source.account)
	if err != nil {
		return view, err
	}
	page, err := native.CatalogWithLimit(ctx, "messages", source.thread, "", platformCursor, 50)
	if err != nil {
		var catalog *linuxnative.CatalogError
		if errors.As(err, &catalog) && catalog.Code == "catalog_cursor_invalid" {
			return view, errStationRecallCursor
		}
		return view, err
	}
	for _, item := range page.Items {
		message, _ := item["id"].(string)
		if item["serverId"] != source.serverID || item["threadId"] != source.thread ||
			item["direction"] != "outgoing" || !validStationRecallMessageID(message) {
			continue
		}
		if view.MessageID != "" && view.MessageID != message {
			return stationRecallView{}, apphost.ErrLifecycleConflict
		}
		view.MessageID = message
	}
	if view.MessageID == "" && page.NextCursor != nil && *page.NextCursor != "" {
		if len(*page.NextCursor) > 8192 || *page.NextCursor == platformCursor {
			return view, errStationRecallCursor
		}
		view.NextCursor = base64.RawURLEncoding.EncodeToString(s.contactCursor.Seal(nil, nil, []byte(*page.NextCursor), scope))
	}
	return view, nil
}

func (s *stationWechat) submitStationRecall(ctx context.Context, id string, source stationRecallSource, messageID, owner string, active func() bool) (apphost.ActionReceipt, error) {
	var out apphost.ActionReceipt
	operationID := source.operationID(id)
	w := source.worker
	w.mu.Lock()
	ledger, err := w.actionLedgerLocked()
	w.mu.Unlock()
	if err != nil {
		return out, err
	}
	prior := func() (apphost.ActionReceipt, bool, error) {
		if p, ok := ledger.FindConnected(s.installation, id, operationID, "", "recall"); ok {
			if p.Request.MessageID != messageID || p.Request.ServerID != source.serverID || p.Request.ThreadID != source.thread {
				return out, true, apphost.ErrLifecycleConflict
			}
			return p.Receipt(), true, nil
		}
		return out, false, nil
	}
	if !active() {
		return out, apphost.ErrLifecycleConflict
	}
	if result, found, err := prior(); found {
		return result, err
	}
	native, err := s.stationRecallNative(ctx, id, source.account)
	if err != nil {
		return out, err
	}
	message, err := native.Message(ctx, messageID)
	if err != nil || !wechatapp.OwnMessage(message, source.account, source.thread, messageID) || message.ServerID != source.serverID {
		return out, apphost.ErrLifecycleConflict
	}
	current, err := s.signedInAccount(ctx, id)
	if err != nil || current != source.account {
		return out, apphost.ErrLifecycleConflict
	}
	request := linuxnative.AdminAction{Kind: "recall", ThreadID: source.thread, MessageID: messageID, ServerID: source.serverID}
	intent := apphost.ActionIntent{ConnectionRef: s.connectionRef(id, source.account), Action: request.Kind,
		RequesterID: owner, Source: "local_admin", SourceThread: source.thread, SourceRequestID: source.deliveryKey,
		Target: source.thread, Purpose: "owner_approved_outgoing_recall", GrantVersion: "owner_approval:" + operationID, IdempotencyKey: operationID}
	p := wechatapp.ActionProposal{ID: operationID, Account: source.account, Request: request, State: "unknown", Created: time.Now().UTC(), AppAction: &intent}
	w.mu.Lock()
	if ctx.Err() != nil || !active() {
		w.mu.Unlock()
		return out, apphost.ErrLifecycleConflict
	}
	if result, found, err := prior(); found {
		w.mu.Unlock()
		return result, err
	}
	if w.policy.Paused {
		w.mu.Unlock()
		return out, apphost.ErrLifecycleConflict
	}
	if err := s.admitPlatformWrite(ctx, id, native); err != nil {
		w.mu.Unlock()
		return out, err
	}
	err = ledger.Save(p)
	w.mu.Unlock()
	if err != nil {
		return out, err
	}
	return submitReservedStationAction(ctx, native, ledger, p)
}
