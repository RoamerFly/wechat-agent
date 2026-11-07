package station

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
)

type localModelFunc func(context.Context, string, []map[string]string) (string, error)

func (f localModelFunc) Chat(ctx context.Context, s string, m []map[string]string) (string, error) {
	return f(ctx, s, m)
}

func localChatFixture(t *testing.T) *localHomeFixture {
	t.Helper()
	base := t.TempDir()
	f := newLocalHomeFixture(t, base+"/host", base+"/workspace")
	f.login()
	if code, _ := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != 200 {
		t.Fatal(code)
	}
	return f
}

func localChatSend(f *localHomeFixture, id, text string) (int, map[string]any) {
	return f.request("POST", "/api/local/chat", map[string]string{"request_id": id, "text": text}, true)
}

func TestLocalChatNormalEngineSourceHistoryAndIdempotency(t *testing.T) {
	f := localChatFixture(t)
	var calls atomic.Int32
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(ctx context.Context, system string, turns []map[string]string) (string, error) {
			call := calls.Add(1)
			if !strings.Contains(system, "本机主人") {
				t.Error("local channel policy absent")
			}
			var history string
			for _, turn := range turns {
				history += turn["content"]
			}
			if !strings.Contains(history, `"channel":"local"`) {
				t.Error("source channel missing")
			}
			if call == 2 && (!strings.Contains(history, "明天一起画海") || !strings.Contains(history, "记住了，明天画海")) {
				t.Error("prior real conversation was not assembled", history)
			}
			return `{"action":"reply","messages":["记住了，明天画海。"],"reason":"reply"}`, nil
		}), nil
	}
	if code, _ := f.request("POST", "/api/local/chat", map[string]string{"request_id": "first", "text": "明天一起画海"}, false); code != http.StatusForbidden {
		t.Fatal("chat bypassed CSRF", code)
	}
	for range 2 {
		if code, out := localChatSend(f, "first", "明天一起画海"); code != 200 || out["status"] != "completed" {
			t.Fatalf("send/replay: %d %#v", code, out)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate request billed another generation", calls.Load())
	}
	if code, _ := localChatSend(f, "first", "改成其他内容"); code != 409 {
		t.Fatal("changed id body accepted", code)
	}
	// Pausing execution of the fish home does not revoke text conversation.
	_, home := f.request("GET", "/api/local/home", nil, true)
	f.request("POST", "/api/local/home", map[string]any{"action": "pause", "revision": home["revision"]}, true)
	if code, out := localChatSend(f, "second", "明天的约定是什么？"); code != 200 {
		t.Fatalf("home pause blocked conversation: %d %#v", code, out)
	}
	code, page := f.request("GET", "/api/local/chat?limit=2", nil, true)
	if code != 200 || len(page["items"].([]any)) != 2 || page["has_more"] != true {
		t.Fatalf("history page: %d %#v", code, page)
	}
	items := page["items"].([]any)
	if items[0].(map[string]any)["role"] != "user" || items[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("history not chronological: %#v", items)
	}
	if code, out := f.request("GET", "/api/local/chat?limit=101", nil, true); code != 400 {
		t.Fatalf("explicit page size silently clamped: %d %#v", code, out)
	}
	f.server.Close()
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	restart := newLocalHomeFixture(t, f.dir, f.root)
	restart.login()
	restart.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		t.Error("restart replay called model")
		return nil, errors.New("unexpected")
	}
	if code, out := localChatSend(restart, "first", "明天一起画海"); code != 200 || out["reused"] != true {
		t.Fatalf("restart replay: %d %#v", code, out)
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected additional model calls", calls.Load())
	}
}

func TestLocalChatFailureRetentionAndExplicitRetry(t *testing.T) {
	f := localChatFixture(t)
	var calls atomic.Int32
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(context.Context, string, []map[string]string) (string, error) {
			if calls.Add(1) == 1 {
				return "", errors.New("synthetic transport failure secret must not be returned")
			}
			return `{"action":"reply","messages":["第二次完成。"],"reason":"reply"}`, nil
		}), nil
	}
	if code, out := localChatSend(f, "retry-one", "这个请求第一次失败"); code != 502 || strings.Contains(fmt.Sprint(out), "secret") {
		t.Fatalf("unsafe or wrong failure: %d %#v", code, out)
	}
	_, page := f.request("GET", "/api/local/chat", nil, true)
	if len(page["items"].([]any)) != 1 {
		t.Fatal("failed generation lost original or invented assistant", page)
	}
	if code, out := localChatSend(f, "retry-one", "这个请求第一次失败"); code != 200 {
		t.Fatalf("retry: %d %#v", code, out)
	}
	_, page = f.request("GET", "/api/local/chat", nil, true)
	if len(page["items"].([]any)) != 2 || calls.Load() != 2 {
		t.Fatal("retry duplicated original", page)
	}
}

func TestLocalChatForgetDuringGenerationCannotPublishOrResurrect(t *testing.T) {
	f := localChatFixture(t)
	store, input, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	scope := companion.ScopeID(input.Channel, input.Account, input.Thread)
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(context.Context, string, []map[string]string) (string, error) {
			if err := store.Clear(scope); err != nil {
				t.Fatal(err)
			}
			return `{"action":"reply","messages":["不应被保存"],"reason":"reply"}`, nil
		}), nil
	}
	code, _ := localChatSend(f, "forgotten", "请忘记前面的内容")
	if code == 200 {
		t.Fatal("late result bypassed forgotten source")
	}
	if code, _ := localChatSend(f, "forgotten", "请忘记前面的内容"); code != 409 {
		t.Fatal("old id resurrected forgotten source", code)
	}
}

func TestLocalChatBusyAndShutdownDoNotReplay(t *testing.T) {
	f := localChatFixture(t)
	started := make(chan struct{})
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(ctx context.Context, _ string, _ []map[string]string) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}), nil
	}
	finished := make(chan struct{})
	go func() { defer close(finished); localChatSend(f, "running", "等待取消的请求") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	if code, _ := localChatSend(f, "another", "不能并发改变上下文"); code != 409 {
		t.Fatal("parallel turn accepted", code)
	}
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel model")
	}
}

func TestLocalChatNoConfigurationDoesNotCallModelOrLoseDraft(t *testing.T) {
	f := localChatFixture(t)
	if code, out := localChatSend(f, "unconfigured", "模型不可用时仍须保存的原文"); code != 503 {
		t.Fatalf("missing configuration: %d %#v", code, out)
	}
	_, page := f.request("GET", "/api/local/chat", nil, true)
	items, _ := page["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["role"] != "user" || items[0].(map[string]any)["text"] != "模型不可用时仍须保存的原文" {
		t.Fatalf("unconfigured request was not retained: %#v", page)
	}
	code, out := f.request("GET", "/api/local/model", nil, true)
	if code != 200 || out["key_configured"] != false || out["thinking_enabled"] != true || out["reasoning_effort"] != "low" {
		t.Fatalf("model defaults: %d %#v", code, out)
	}
	// The same durable source event remains readable after a station restart;
	// it is not an in-memory draft or a request that will be auto-replayed.
	f.server.Close()
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	restart := newLocalHomeFixture(t, f.dir, f.root)
	restart.login()
	_, page = restart.request("GET", "/api/local/chat", nil, true)
	items, _ = page["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["text"] != "模型不可用时仍须保存的原文" {
		t.Fatalf("restart lost unconfigured source: %#v", page)
	}
}

func TestLocalChatCloseContextBoundsUncooperativeModel(t *testing.T) {
	f := localChatFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(context.Context, string, []map[string]string) (string, error) {
			close(started)
			<-release // Deliberately ignores its context until the test releases it.
			return "", context.Canceled
		}), nil
	}
	finished := make(chan struct{})
	go func() { defer close(finished); localChatSend(f, "stuck", "等待不可合作模型") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	deadline, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := f.handler.CloseContext(deadline)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "超过等待时限") {
		t.Fatalf("unbounded close or wrong error: %v", err)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("released model did not finish")
	}
	if err := f.handler.CloseContext(context.Background()); err != nil {
		t.Fatalf("close could not finish after release: %v", err)
	}
}
