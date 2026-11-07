package station

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

func TestStationDeliveryStopIsPersistentScopedAndOrderedWithSubmit(t *testing.T) {
	w := &stationWechatWorker{dir: t.TempDir()}
	in := companion.Input{Account: "one", Thread: "room", TraceID: "original"}
	stop := stationDeliveryStop{Account: in.Account, Thread: in.Thread, Trace: in.TraceID, Key: "first-bubble", At: time.Now().UTC()}
	entered, release, sent := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		sent <- w.withDeliverySubmit(t.Context(), in, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	started, saved := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		w.deliveryMu.Lock()
		defer w.deliveryMu.Unlock()
		saved <- w.saveDeliveryStop(stop)
	}()
	<-started
	select {
	case err := <-saved:
		t.Fatal("stop passed an active submission", err)
	default:
	}
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	// A new host object reads the same fence; no in-memory cancellation is required.
	w = &stationWechatWorker{dir: w.dir}
	called := false
	if err := w.withDeliverySubmit(t.Context(), in, func() error { called = true; return nil }); !errors.Is(err, bot.ErrDeliveryCancelled) || called {
		t.Fatal("cancelled turn submitted", called, err)
	}
	for _, foreign := range []companion.Input{{Account: "other", Thread: in.Thread, TraceID: in.TraceID}, {Account: in.Account, Thread: "other", TraceID: in.TraceID}, {Account: in.Account, Thread: in.Thread, TraceID: "next"}} {
		cancelled, err := w.deliveryCancelled(foreign)
		if cancelled || err != nil {
			t.Fatal("cancel crossed request scope", foreign, cancelled, err)
		}
	}
	w.deliveryMu.Lock()
	if err := w.saveDeliveryStop(stop); err != nil {
		t.Fatal(err)
	}
	rows, err := w.readDeliveryStops()
	w.deliveryMu.Unlock()
	if err != nil || len(rows) != 1 {
		t.Fatal("duplicate cancellation was not idempotent", rows, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := w.withDeliverySubmit(ctx, companion.Input{}, func() error { t.Fatal("cancelled context entered native call"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "delivery-stops.json"), []byte(`{"broken":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.withDeliverySubmit(t.Context(), in, func() error { t.Fatal("corrupt journal bypassed"); return nil }); err == nil {
		t.Fatal("corrupt journal accepted")
	}
}

func TestStationStopsOnlyOriginalVerifiedNativeMedia(t *testing.T) {
	for _, scenario := range []string{"known-job", "lost-job-id", "wrong-source", "wrong-account", "wrong-thread", "accepted", "lookup-missing"} {
		t.Run(scenario, func(t *testing.T) {
			cancels, submissions := 0, 0
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if strings.HasSuffix(r.URL.Path, "/jobs/original-job/cancel") {
						cancels++
					} else {
						submissions++
						t.Error("unexpected mutation", r.URL.Path)
					}
				}
				if scenario == "lookup-missing" {
					w.WriteHeader(404)
					return
				}
				value := map[string]any{"id": "original-job", "kind": "send", "state": "running", "accountId": "one", "sourceId": "original-source", "threadId": "room"}
				switch scenario {
				case "wrong-source":
					value["sourceId"] = "foreign-source"
				case "wrong-account":
					value["accountId"] = "other"
				case "wrong-thread":
					value["threadId"] = "other-room"
				case "accepted":
					value["state"] = "accepted"
				}
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer daemon.Close()
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("synthetic-native-control-token"), 0600); err != nil {
				t.Fatal(err)
			}
			native, err := linuxnative.New(linuxnative.Config{URL: daemon.URL, TokenFile: token, Account: "one"})
			if err != nil {
				t.Fatal(err)
			}
			row := bot.DeliveryEvidence{Trace: "origin", SourceID: "original-source", StrictSource: true, Status: linuxnative.ToolStatus{ID: "original-job", ThreadID: "room", Kind: "voice", State: "unknown"}}
			if scenario == "lost-job-id" || scenario == "lookup-missing" {
				row.Status.ID = ""
			}
			(&stationWechat{}).cancelNativeDelivery(t.Context(), native, "original-key", row)
			want := 0
			if scenario == "known-job" || scenario == "lost-job-id" {
				want = 1
			}
			if cancels != want || submissions != 0 {
				t.Fatal("wrong native task mutation", cancels, want, submissions)
			}
		})
	}
}
