package station

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/websession"
)

type offlineResumeNative struct{ stationWechatNative }

func (offlineResumeNative) WorkerSender(context.Context, string) (*linuxnative.Sender, error) {
	return nil, errors.New("synthetic native offline")
}

func TestStationOwnerResumeIsAuthenticatedPersistentAndCancelable(t *testing.T) {
	id := wechatapp.ConnectionID("fixture")
	base := t.TempDir()
	dir := filepath.Join(base, id)
	if err := os.MkdirAll(filepath.Join(dir, "queue"), 0700); err != nil {
		t.Fatal(err)
	}
	queue := map[string]any{"version": 2, "account": "fixture", "media": []any{map[string]any{
		"key": "original-key", "trace": "original-turn", "input": map[string]any{"account": "fixture", "thread": "room"}, "id": "original-job", "kind": "voice", "state": "stopped", "user_cancelled": true,
		"status": map[string]any{"id": "original-job", "thread_id": "room", "state": "cancelled"},
	}}}
	raw, _ := json.Marshal(queue)
	if err := os.WriteFile(filepath.Join(dir, "queue", "messages-v2.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	worker := &stationWechatWorker{dir: dir}
	station := &stationWechat{stationWechatNative: offlineResumeNative{}, ctx: context.Background(), root: base, workers: map[string]*stationWechatWorker{id: worker}}
	owner, err := websession.New("synthetic-local-password", "", localOwnerCookie)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", owner.Login)
	station.deliveryControlRoutes(owner, mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	f := &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}}
	path := "/api/apps/wechat/connections/" + id + "/receipts/original-key/resume"
	call := func(target string, body any, csrf bool, want int) map[string]any {
		t.Helper()
		code, value := f.request("POST", target, body, csrf)
		if code != want {
			t.Fatal(code, want, value)
		}
		return value
	}
	call(path, map[string]any{"request_key": "request-one"}, true, 401)
	f.login()
	call(path, map[string]any{"request_key": "request-one"}, false, 403)
	call(path, map[string]any{}, true, 400)
	originalRaw := append([]byte(nil), raw...)
	for _, scenario := range []string{"text", "accepted", "not-submitted"} {
		var variant map[string]any
		if json.Unmarshal(originalRaw, &variant) != nil {
			t.Fatal("bad synthetic queue")
		}
		row := variant["media"].([]any)[0].(map[string]any)
		status := row["status"].(map[string]any)
		switch scenario {
		case "text":
			row["kind"] = "file"
		case "accepted":
			row["state"], status["state"] = "accepted", "accepted"
		case "not-submitted":
			row["id"], status["id"] = "", ""
		}
		value, _ := json.Marshal(variant)
		if err := os.WriteFile(filepath.Join(dir, "queue", "messages-v2.json"), value, 0600); err != nil {
			t.Fatal(err)
		}
		call(path, map[string]any{"request_key": "request-one"}, true, 409)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue", "messages-v2.json"), originalRaw, 0600); err != nil {
		t.Fatal(err)
	}
	call("/api/apps/wechat/connections/"+wechatapp.ConnectionID("other")+"/receipts/original-key/resume", map[string]any{"request_key": "request-one"}, true, 404)
	first := call(path, map[string]any{"request_key": "request-one"}, true, 202)
	if first["continuation"].(map[string]any)["state"] != "requested" {
		t.Fatal(first)
	}
	call(path, map[string]any{"request_key": "different-click"}, true, 202)
	// Reconstruct the controller: neither refresh nor restart creates a new operation.
	worker = &stationWechatWorker{dir: dir}
	station.workers[id] = worker
	request, err := worker.nextMediaResume(time.Now())
	if err != nil || request == nil || request.Account != "fixture" || request.Thread != "room" {
		t.Fatal(request, err)
	}
	rows, err := worker.readResumeControls()
	if err != nil || len(rows) != 1 || rows["original-key"].RequestKey != "request-one" {
		t.Fatal(rows, err)
	}
	page, err := station.ListReceipts(t.Context(), id, "", 20)
	if err != nil || page.Items[0].Continuation == nil || page.Items[0].Continuation.State != "requested" {
		t.Fatal(page, err)
	}
	if err := worker.mediaResumeResult(*request, bot.MediaResumeResult{State: "waiting_receipt", Reason: "media_resume_original_receipt_unavailable"}); err != nil {
		t.Fatal(err)
	}
	if next, err := worker.nextMediaResume(time.Now()); err != nil || next != nil {
		t.Fatal("backoff ignored", next, err)
	}
	if next, err := worker.nextMediaResume(time.Now().Add(24 * time.Hour)); err != nil || next == nil || *next != *request {
		t.Fatal(next, err)
	}
	call("/api/apps/wechat/connections/"+id+"/receipts/original-key/cancel", map[string]any{}, true, 202)
	if err := worker.mediaResumeResult(*request, bot.MediaResumeResult{State: "tracking", DeliveryKey: "new-key"}); err != nil {
		t.Fatal(err)
	}
	rows, err = worker.readResumeControls()
	if err != nil || rows["original-key"].State != "cancelled" {
		t.Fatal("late result revived stopped resume", rows, err)
	}
	if next, err := worker.nextMediaResume(time.Now().Add(24 * time.Hour)); err != nil || next != nil {
		t.Fatal(next, err)
	}
	if err := worker.withDeliverySubmit(t.Context(), companion.Input{Account: request.Account, Thread: request.Thread, TraceID: request.Trace}, func() error { t.Fatal("stopped resume entered native service"); return nil }); !errors.Is(err, bot.ErrDeliveryCancelled) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "delivery-resumes.json"), []byte(`{"broken":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.nextMediaResume(time.Now()); err == nil {
		t.Fatal("corrupt host control accepted")
	}
}
