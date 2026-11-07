package station

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/companion"
)

func localMemoryFixture(t *testing.T) (*companion.Store, companion.Input, *localMemoryTools) {
	t.Helper()
	store, err := companion.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	in := companion.Input{Channel: "local", Account: "fish-test", Thread: "owner-test", Sender: "owner-test", TraceID: "local-original", Text: strings.Repeat("本地原文。", 180), Mode: "direct"}
	config := companion.Defaults()
	config.Permissions.Remember = true
	if err = store.Observe(in, config, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	access := newLocalMemoryTools(store, in).(*localMemoryTools)
	return store, in, access
}

func localOriginalID(t *testing.T, store *companion.Store, in companion.Input) string {
	t.Helper()
	page, err := store.SearchMemory(context.Background(), companion.MemoryQuery{Scopes: []string{companion.ScopeID(in.Channel, in.Account, in.Thread)}, Originals: true, Query: "本地原文", Limit: 2})
	if err != nil || len(page.Items) != 1 || page.Items[0].Event == nil {
		t.Fatalf("original search = %#v, %v", page, err)
	}
	return page.Items[0].Event.ID
}

func TestLocalMemoryToolsPinnedScopeAndOriginalPaging(t *testing.T) {
	store, in, access := localMemoryFixture(t)
	if tools, err := access.List(t.Context()); err != nil || len(tools) != 2 || tools[0].Name != "memory_search" || tools[1].Name != "memory_read" {
		t.Fatalf("tools=%#v err=%v", tools, err)
	}
	if _, err := access.Call(t.Context(), "memory_search", json.RawMessage(`{"thread_id":"other","originals":true}`)); err == nil {
		t.Fatal("tool JSON changed fixed local scope")
	}
	if _, err := access.Call(t.Context(), "memory_search", json.RawMessage(`{"originals":true,"limit":0}`)); err == nil {
		t.Fatal("explicit zero search limit was silently defaulted")
	}
	result, err := access.Call(t.Context(), "memory_search", json.RawMessage(`{"originals":true,"query":"本地原文","limit":1}`))
	if err != nil || !strings.Contains(result, `"effective_limit":1`) {
		t.Fatalf("search=%s err=%v", result, err)
	}
	id := localOriginalID(t, store, in)
	result, err = access.Call(t.Context(), "memory_read", json.RawMessage(`{"id":"`+id+`","event":true,"limit":17}`))
	if err != nil || !strings.Contains(result, `"has_more":true`) || !strings.Contains(result, `"revision":`) {
		t.Fatalf("page=%s err=%v", result, err)
	}
	var page struct {
		Next  int `json:"next_offset"`
		Event struct {
			Text string `json:"text"`
		} `json:"event"`
	}
	if err = json.Unmarshal([]byte(result), &page); err != nil || page.Next <= 0 || page.Event.Text == "" {
		t.Fatalf("invalid continuation: %#v %v", page, err)
	}
	if _, err = access.Call(t.Context(), "memory_read", json.RawMessage(`{"id":"`+id+`","event":true,"length":32769}`)); err == nil {
		t.Fatal("oversized explicit page was silently clamped")
	}
}

func TestLocalMemoryToolsRejectOtherScopeAndForgottenEntries(t *testing.T) {
	store, in, access := localMemoryFixture(t)
	scope := companion.ScopeID(in.Channel, in.Account, in.Thread)
	other := companion.Input{Channel: "local", Account: "fish-test", Thread: "other-owner", Sender: "other-owner", TraceID: "other-original", Text: "其他范围的原文", Mode: "direct"}
	config := companion.Defaults()
	config.Permissions.Remember = true
	if err := store.Observe(other, config, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	otherID := localOriginalID(t, store, other)
	if _, err := access.Call(t.Context(), "memory_read", json.RawMessage(`{"id":"`+otherID+`","event":true}`)); err == nil {
		t.Fatal("other local conversation event was readable")
	}
	entry, err := store.SaveMemory(t.Context(), companion.MemoryEntry{Scope: scope, Subject: in.Sender, Kind: "fact", Detail: "应被遗忘的本地记忆", Manual: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	entry.Status = "deleted"
	if _, err = store.SaveMemory(t.Context(), entry, entry.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = access.Call(t.Context(), "memory_read", json.RawMessage(`{"id":"`+entry.ID+`"}`)); err == nil {
		t.Fatal("forgotten memory was readable")
	}
}
