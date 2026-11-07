package station

import (
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

type stationResumeControl struct {
	bot.MediaResumeRequest
	bot.MediaResumeResult
	RequestKey  string    `json:"request_key"`
	ParentTrace string    `json:"parent_trace"`
	Created     time.Time `json:"created"`
	Next        time.Time `json:"next,omitempty"`
}

// Called under deliveryMu after the original stop is durable. Stop every
// queued continuation descended from that turn before releasing the send gate.
func (w *stationWechatWorker) cancelResumeControls(stop stationDeliveryStop) (map[string]bool, error) {
	traces := map[string]bool{stop.Trace: true}
	rows, err := w.readResumeControls()
	if err != nil {
		return traces, err
	}
	changed := false
	for added := true; added; {
		added = false
		for key, r := range rows {
			if r.Account != stop.Account || r.Thread != stop.Thread || !traces[r.ParentTrace] && !traces[r.Trace] {
				continue
			}
			if !traces[r.Trace] {
				if err := w.saveDeliveryStop(stationDeliveryStop{Account: r.Account, Thread: r.Thread, Trace: r.Trace, Key: r.Key, At: stop.At}); err != nil {
					return traces, err
				}
				traces[r.Trace], added = true, true
			}
			if r.State != "cancelled" {
				r.State, r.Reason = "cancelled", "media_cancelled_before_native_submission"
				rows[key] = r
				changed = true
			}
		}
	}
	if changed {
		err = writeStationControlJournal(w.dir, "delivery-resumes.json", rows)
	}
	return traces, err
}

func (w *stationWechatWorker) readResumeControls() (map[string]stationResumeControl, error) {
	rows, err := readStationControlJournal[stationResumeControl](w.dir, "delivery-resumes.json")
	if err != nil {
		return nil, err
	}
	for key, r := range rows {
		if key != r.Key || r.ID != "resume-"+localCheckpointKey(r.Account, r.Key, r.RequestKey) || r.Trace != "owner-"+r.ID || r.Thread == "" || r.Account == "" || !identity.ValidPathID(r.RequestKey) || r.Created.IsZero() || r.ParentTrace == "" {
			return nil, errors.New("invalid resume control identity")
		}
		switch r.State {
		case "requested", "waiting_receipt", "tracking", "blocked", "cancelled":
		default:
			return nil, errors.New("invalid resume control state")
		}
	}
	return rows, nil
}

func (w *stationWechatWorker) nextMediaResume(now time.Time) (*bot.MediaResumeRequest, error) {
	w.deliveryMu.Lock()
	defer w.deliveryMu.Unlock()
	rows, err := w.readResumeControls()
	if err != nil {
		return nil, err
	}
	var next *stationResumeControl
	for _, r := range rows {
		if (r.State == "requested" || r.State == "waiting_receipt") && !now.Before(r.Next) && (next == nil || r.Created.Before(next.Created) || r.Created.Equal(next.Created) && r.ID < next.ID) {
			copy := r
			next = &copy
		}
	}
	if next == nil {
		return nil, nil
	}
	request := next.MediaResumeRequest
	return &request, nil
}

func (w *stationWechatWorker) mediaResumeResult(request bot.MediaResumeRequest, result bot.MediaResumeResult) error {
	w.deliveryMu.Lock()
	defer w.deliveryMu.Unlock()
	rows, err := w.readResumeControls()
	if err != nil {
		return err
	}
	r, exists := rows[request.Key]
	if !exists || r.MediaResumeRequest != request {
		return errors.New("resume control changed")
	}
	if r.State == "cancelled" {
		return nil
	} // A late actor result cannot undo stop.
	r.MediaResumeResult = result
	r.Next = time.Time{}
	if result.State == "waiting_receipt" {
		r.Next = time.Now().Add(bot.DeliveryRetryBackoff(1))
	}
	rows[request.Key] = r
	return writeStationControlJournal(w.dir, "delivery-resumes.json", rows)
}

func resumeControlView(r stationResumeControl) *apphost.ReceiptContinuation {
	message := map[string]string{"requested": "续办请求已保存", "waiting_receipt": "等待原任务分段回执", "tracking": "已建立续发任务", "blocked": "暂不能续办，请核对原任务", "cancelled": "续办已停止"}[r.State]
	if r.State == "blocked" {
		switch r.Reason {
		case "media_resume_missing_not_authorized":
			message = "续办权限已变化"
		case "media_resume_missing_source_invalid":
			message = "原素材或来源已变化"
		case "media_expected_voice_mismatch", "media_expected_voice_profile_mismatch":
			message = "指定声线与原成品不符"
		case "native_resume_missing_not_supported":
			message = "当前原生服务不支持分段续办"
		case "media_resume_original_not_submitted":
			message = "原语音未提交分段任务，可重新请求发送原成品"
		case "media_resume_already_accepted":
			message = "原语音已完整送达，无需续发"
		}
	}
	return &apphost.ReceiptContinuation{State: r.State, Message: message, Code: r.Reason, DeliveryKey: r.DeliveryKey}
}

func (s *stationWechat) requestDeliveryResume(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		RequestKey string `json:"request_key"`
	}
	if !websession.Decode(w, r, &input) {
		return
	}
	key := r.PathValue("key")
	if !identity.ValidPathID(input.RequestKey) || !identity.ValidPathID(key) {
		websession.Fail(w, 400, "请提供有效的原投递编号和请求编号")
		return
	}
	worker, account, err := s.deliveryCheckpoint(r.PathValue("connection"))
	if err != nil || worker == nil {
		websession.Fail(w, 404, "原投递记录不存在")
		return
	}
	worker.deliveryMu.Lock()
	defer worker.deliveryMu.Unlock()
	rows, err := worker.readResumeControls()
	if err != nil {
		websession.Fail(w, 503, "续办请求暂不可读")
		return
	}
	if saved, exists := rows[key]; exists {
		websession.Respond(w, 202, map[string]any{"idempotency_key": key, "continuation": resumeControlView(saved)})
		return
	}
	worker.platformMu.RLock()
	defer worker.platformMu.RUnlock()
	root, err := workspacefs.OpenPath(worker.dir)
	if err != nil {
		websession.Fail(w, 503, "连接状态暂不可读")
		return
	}
	policy, err := readStationWechatPolicy(root)
	root.Close()
	if err != nil {
		websession.Fail(w, 503, "连接状态暂不可读")
		return
	}
	if !policy.RollbackFrozenAt.IsZero() {
		websession.Fail(w, 409, "连接已为回退停发，未登记新续发")
		return
	}
	evidence, err := bot.ReadDeliveryEvidence(filepath.Join(worker.dir, "queue"), account, []string{key})
	old, exists := evidence[key]
	if err != nil || !exists || old.Trace == "" {
		websession.Fail(w, 404, "原投递记录不存在")
		return
	}
	stops, err := worker.readDeliveryStops()
	if err != nil {
		websession.Fail(w, 503, "停止记录暂不可读")
		return
	}
	_, stopped := stops[deliveryStopID(companion.Input{Account: account, Thread: old.Status.ThreadID, TraceID: old.Trace})]
	if old.Status.Kind != "voice" || old.Status.State == "accepted" || !stopped && !old.Status.CancelRequested && old.Status.State != "cancelled" {
		websession.Fail(w, 409, "请选择已停止的分段语音；已完整送达的内容无需续发")
		return
	}
	if old.Status.ID == "" && old.Status.State == "cancelled" {
		websession.Fail(w, 409, "原语音尚未提交分段任务，可重新请求发送原成品")
		return
	}
	// Keep the original blocked forever; only this new explicit operation may run.
	if err := worker.saveDeliveryStop(stationDeliveryStop{Account: account, Thread: old.Status.ThreadID, Trace: old.Trace, Key: key, At: time.Now().UTC()}); err != nil {
		websession.Fail(w, 503, "原任务停止状态尚未保存")
		return
	}
	id := "resume-" + localCheckpointKey(account, key, input.RequestKey)
	saved := stationResumeControl{MediaResumeRequest: bot.MediaResumeRequest{ID: id, Account: account, Thread: old.Status.ThreadID, Key: key, Trace: "owner-" + id}, MediaResumeResult: bot.MediaResumeResult{State: "requested"}, RequestKey: input.RequestKey, ParentTrace: old.Trace, Created: time.Now().UTC()}
	rows[key] = saved
	if err := writeStationControlJournal(worker.dir, "delivery-resumes.json", rows); err != nil {
		websession.Fail(w, 503, "续办请求尚未保存，请重试")
		return
	}
	websession.Respond(w, 202, map[string]any{"idempotency_key": key, "continuation": resumeControlView(saved)})
}
