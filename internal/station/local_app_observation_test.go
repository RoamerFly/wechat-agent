package station

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

// syntheticObservationApp is deliberately an application registration instead
// of a test-only shortcut. It proves the station uses the same registration,
// grant and continuation boundaries that a future app adapter will use.
type syntheticObservationApp struct {
	mu            sync.Mutex
	allowed       bool
	grantVersion  string
	pages         map[apphost.ObservationField][]apphost.ObservationPage
	readCursors   map[apphost.ObservationField][]string
	interruptOnce map[apphost.ObservationField]bool
}

func newSyntheticObservationApp() *syntheticObservationApp {
	return &syntheticObservationApp{
		allowed: true, grantVersion: "grant-one", readCursors: map[apphost.ObservationField][]string{}, interruptOnce: map[apphost.ObservationField]bool{},
		pages: map[apphost.ObservationField][]apphost.ObservationPage{
			apphost.ObservationTitle: {{Chunks: []apphost.ObservationChunk{{Position: 0, Text: "蓝色夜海"}}, Coverage: "标题", Complete: true}},
			apphost.ObservationBody: {
				{Chunks: []apphost.ObservationChunk{{Position: 0, Text: "第一段正文"}}, Coverage: "正文第 1 页", NextCursor: "body-two"},
				{Chunks: []apphost.ObservationChunk{{Position: 1, Text: "第二段正文"}}, Coverage: "正文全文", Complete: true},
			},
			apphost.ObservationComments: {
				{Chunks: []apphost.ObservationChunk{{Position: 0, Text: "第一条评论"}}, Coverage: "评论第 1 页", NextCursor: "comments-two"},
				{Chunks: []apphost.ObservationChunk{{Position: 1, Text: "第二条评论"}}, Coverage: "评论全文", Complete: true},
			},
			apphost.ObservationAudio: {{Chunks: []apphost.ObservationChunk{{Position: 0, Text: "海风穿过礁石"}}, Coverage: "音轨 0-12 秒", Complete: true}},
			// The record has no later playback page, but the verified interval is
			// still only 0-12 seconds. Completion here means the adapter has no
			// additional result, never that the content was watched in full.
			apphost.ObservationPlayback: {{Playback: []apphost.PlaybackInterval{{StartSeconds: 0, EndSeconds: 12}}, Coverage: "实际播放 0-12 秒", Complete: true}},
		},
	}
}

func (*syntheticObservationApp) Connections(context.Context) ([]apphost.Connection, error) {
	return []apphost.Connection{{ID: "offline-connection", AccountID: "offline-owner", Configuration: "ready"}}, nil
}

func (p *syntheticObservationApp) Inspect(context.Context, string) (apphost.ConnectionStatus, error) {
	return apphost.ConnectionStatus{}, apphost.ErrNotFound
}

func (*syntheticObservationApp) ObservationTargets(context.Context, string) ([]apphost.ObservationTarget, error) {
	return []apphost.ObservationTarget{{ContentID: "night-sea", Label: "离线夜海资料"}}, nil
}

func (p *syntheticObservationApp) ObservationGrant(_ context.Context, _ apphost.ObservationAccess) (apphost.ObservationGrant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return apphost.ObservationGrant{Allowed: p.allowed, Version: p.grantVersion}, nil
}

func (p *syntheticObservationApp) ReadObservation(_ context.Context, request apphost.ObservationRequest) (apphost.ObservationPage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if request.ContentID != "night-sea" || request.Limit < 0 {
		return apphost.ObservationPage{}, errors.New("unexpected synthetic request")
	}
	p.readCursors[request.Field] = append(p.readCursors[request.Field], request.Cursor)
	pages, ok := p.pages[request.Field]
	if !ok {
		return apphost.ObservationPage{}, errors.New("missing synthetic field")
	}
	index := 0
	if request.Cursor != "" {
		if (request.Cursor == "body-two" && request.Field == apphost.ObservationBody) || (request.Cursor == "comments-two" && request.Field == apphost.ObservationComments) {
			index = 1
		} else {
			return apphost.ObservationPage{}, errors.New("unexpected continuation")
		}
	}
	if p.interruptOnce[request.Field] {
		delete(p.interruptOnce, request.Field)
		return apphost.ObservationPage{}, context.Canceled
	}
	return pages[index], nil
}

func syntheticObservationRegistry(t *testing.T, provider *syntheticObservationApp) apphost.Registry {
	t.Helper()
	registry, err := apphost.New(apphost.Registration{Application: apphost.Application{
		ID: "offline-library", Name: "离线资料库", InstallationID: "offline-library-test", Registration: "synthetic",
	}, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func syntheticObservationFixture(t *testing.T, provider *syntheticObservationApp) *localHomeFixture {
	t.Helper()
	base := t.TempDir()
	handler, err := NewLocalHomeHandlerWithApps(filepath.Join(base, "host"), filepath.Join(base, "workspace"), "synthetic-local-password", "", syntheticObservationRegistry(t, provider))
	if err != nil {
		t.Fatal(err)
	}
	f := newLocalAppObservationFixtureWithHandler(t, handler, filepath.Join(base, "host"), filepath.Join(base, "workspace"))
	f.login()
	if code, out := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != 200 {
		t.Fatalf("initialize: %d %#v", code, out)
	}
	return f
}

func newLocalAppObservationFixtureWithHandler(t *testing.T, handler *LocalHomeHandler, dir, root string) *localHomeFixture {
	t.Helper()
	// Keep construction alongside the normal fixture so this replay uses the
	// authenticated HTTP boundary rather than reaching into private fields.
	server := httptest.NewServer(handler)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}, dir: dir, root: root, handler: handler}
	t.Cleanup(func() { _ = handler.Close() })
	t.Cleanup(server.Close)
	return f
}

// TestLocalAppObservationReplay covers the 5.F.1 story through the local
// station's HTTP boundary. It has no network, model, or real application.
func TestLocalAppObservationReplay(t *testing.T) {
	provider := newSyntheticObservationApp()
	f := syntheticObservationFixture(t, provider)

	code, targets := f.request("GET", "/api/local/apps/observation", nil, true)
	if code != 200 || len(targets["items"].([]any)) != 1 {
		t.Fatalf("discover: %d %#v", code, targets)
	}
	read := func(field apphost.ObservationField) map[string]any {
		t.Helper()
		code, out := f.request("POST", "/api/local/apps/observation", map[string]any{
			"app_id": "offline-library", "connection_id": "offline-connection", "content_id": "night-sea", "field": field, "limit": 3,
		}, true)
		if code != 200 {
			t.Fatalf("read %s: %d %#v", field, code, out)
		}
		return out
	}
	for _, field := range []apphost.ObservationField{apphost.ObservationTitle, apphost.ObservationAudio} {
		result := read(field)
		if result["field"] != string(field) || result["coverage"] == "" {
			t.Fatalf("field evidence missing: %#v", result)
		}
	}
	playback := read(apphost.ObservationPlayback)
	if playback["complete"] != true || !strings.Contains(playback["coverage"].(string), "0-12") || strings.Contains(localAppObservationJSONString(playback), "watched") {
		t.Fatalf("partial playback became a full watch: %#v", playback)
	}

	firstBody := read(apphost.ObservationBody)
	if firstBody["complete"] != false || firstBody["resumed"] != false {
		t.Fatalf("first body page: %#v", firstBody)
	}
	firstComments := read(apphost.ObservationComments)
	if firstComments["complete"] != false {
		t.Fatalf("first comments page: %#v", firstComments)
	}
	provider.mu.Lock()
	provider.interruptOnce[apphost.ObservationComments] = true
	provider.mu.Unlock()
	if code, out := f.request("POST", "/api/local/apps/observation", map[string]any{
		"app_id": "offline-library", "connection_id": "offline-connection", "content_id": "night-sea", "field": apphost.ObservationComments, "limit": 3,
	}, true); code != 503 || !strings.Contains(localAppObservationJSONString(out), "读取") {
		t.Fatalf("interrupted comment page: %d %#v", code, out)
	}
	// Restart the whole local station with the same private host directory.
	f.server.Close()
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	restartedHandler, err := NewLocalHomeHandlerWithApps(f.dir, f.root, "synthetic-local-password", "", syntheticObservationRegistry(t, provider))
	if err != nil {
		t.Fatal(err)
	}
	restarted := newLocalAppObservationFixtureWithHandler(t, restartedHandler, f.dir, f.root)
	restarted.login()
	secondCode, secondBody := restarted.request("POST", "/api/local/apps/observation", map[string]any{
		"app_id": "offline-library", "connection_id": "offline-connection", "content_id": "night-sea", "field": apphost.ObservationBody, "limit": 3,
	}, true)
	if secondCode != 200 || secondBody["complete"] != true || secondBody["resumed"] != true {
		t.Fatalf("restart continuation: %d %#v", secondCode, secondBody)
	}
	provider.mu.Lock()
	bodyCursors := append([]string(nil), provider.readCursors[apphost.ObservationBody]...)
	provider.mu.Unlock()
	if len(bodyCursors) != 2 || bodyCursors[0] != "" || bodyCursors[1] != "body-two" {
		t.Fatalf("body did not resume opaque cursor: %#v", bodyCursors)
	}
	commentCode, comments := restarted.request("POST", "/api/local/apps/observation", map[string]any{
		"app_id": "offline-library", "connection_id": "offline-connection", "content_id": "night-sea", "field": apphost.ObservationComments, "limit": 3,
	}, true)
	if commentCode != 200 || comments["complete"] != true || comments["resumed"] != true {
		t.Fatalf("interrupted comments did not resume: %d %#v", commentCode, comments)
	}
	provider.mu.Lock()
	commentCursors := append([]string(nil), provider.readCursors[apphost.ObservationComments]...)
	provider.mu.Unlock()
	if len(commentCursors) != 3 || commentCursors[0] != "" || commentCursors[1] != "comments-two" || commentCursors[2] != "comments-two" {
		t.Fatalf("interrupted comment lost durable cursor: %#v", commentCursors)
	}

	provider.mu.Lock()
	provider.allowed = false
	provider.mu.Unlock()
	if code, hidden := restarted.request("GET", "/api/local/apps/observation", nil, true); code != 200 || len(localAppObservationItems(hidden)) != 0 {
		t.Fatalf("revoked target still discoverable: %d %#v", code, hidden)
	}
	if code, out := restarted.request("POST", "/api/local/apps/observation", map[string]any{
		"app_id": "offline-library", "connection_id": "offline-connection", "content_id": "night-sea", "field": apphost.ObservationTitle,
	}, true); code != 403 || strings.Contains(strings.ToLower(""+localAppObservationJSONString(out)), "grant-one") {
		t.Fatalf("revoked read: %d %#v", code, out)
	}
}

func TestLocalAppObservationWithoutConnectedApplicationIsEmpty(t *testing.T) {
	f := localChatFixture(t)
	if code, out := f.request("GET", "/api/local/apps/observation", nil, true); code != 200 || len(localAppObservationItems(out)) != 0 {
		t.Fatalf("zero-app station exposed a synthetic target: %d %#v", code, out)
	}
}

// TestLocalAppObservationChecksRevocationBeforePublishingReply verifies the
// normal local-chat path does not keep a reply that relied on a now-revoked
// source. The scripted model is solely a local function-call substitute.
func TestLocalAppObservationChecksRevocationBeforePublishingReply(t *testing.T) {
	provider := newSyntheticObservationApp()
	f := syntheticObservationFixture(t, provider)
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return &syntheticObservationModel{call: func(_ context.Context, _ string, turns []map[string]string, tools []mcptools.Tool) (string, error) {
			calls++
			if calls == 1 {
				if !hasTool(tools, "discover_tools") || hasTool(tools, localAppObservationToolName) {
					t.Fatal("local observation escaped lazy discovery", tools)
				}
				return `{"action":"tool","tool":"discover_tools","arguments":{"query":"app_observe"}}`, nil
			}
			if calls == 2 {
				if !hasTool(tools, localAppObservationToolName) {
					t.Fatal("observation tool was not discovered")
				}
				return `{"action":"tool","tool":"app_observe","arguments":{"action":"read","app_id":"offline-library","connection_id":"offline-connection","content_id":"night-sea","field":"title"}}`, nil
			}
			if !strings.Contains(localModelHistory(turns), "蓝色夜海") {
				t.Fatal("reply did not receive actual observed title", turns)
			}
			provider.mu.Lock()
			provider.allowed = false
			provider.mu.Unlock()
			return `{"action":"reply","messages":["不应写入的观察结果"],"reason":"synthetic"}`, nil
		}}, nil
	}
	if code, out := localChatSend(f, "observation-revoked", "看看离线资料库的标题"); code != 409 || !strings.Contains(localAppObservationJSONString(out), "授权") {
		t.Fatalf("late revocation published: %d %#v", code, out)
	}
	if _, page := f.request("GET", "/api/local/chat", nil, true); len(page["items"].([]any)) != 1 {
		t.Fatalf("revoked result wrote assistant history: %#v", page)
	}
}

type syntheticObservationModel struct {
	call func(context.Context, string, []map[string]string, []mcptools.Tool) (string, error)
}

func (m *syntheticObservationModel) Chat(ctx context.Context, system string, turns []map[string]string) (string, error) {
	return m.call(ctx, system, turns, nil)
}
func (m *syntheticObservationModel) ChatFunctions(ctx context.Context, system string, turns []map[string]string, tools []mcptools.Tool) (string, error) {
	return m.call(ctx, system, turns, tools)
}

func hasTool(tools []mcptools.Tool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}
func localAppObservationJSONString(value any) string {
	body, _ := json.Marshal(value)
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(string(body), "\n", " "), "\r", " "))
}

func localAppObservationItems(value map[string]any) []any {
	items, _ := value["items"].([]any)
	return items
}
