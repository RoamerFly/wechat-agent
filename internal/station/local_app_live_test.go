package station

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

// Only synthetic platform receipts and local mock output are used here.
type localLiveToolProvider struct {
	localLiveRouteProvider
	starts   atomic.Int32
	denied   atomic.Bool
	unknown  atomic.Bool
	startErr error
	ref      apphost.LiveReference
	onGrant  func()
}

func (p *localLiveToolProvider) LiveGrant(ctx context.Context, ref apphost.ConnectionRef) (apphost.LiveGrant, error) {
	g, err := p.localLiveRouteProvider.LiveGrant(ctx, ref)
	g.Allowed = !p.denied.Load()
	if p.onGrant != nil {
		p.onGrant()
	}
	return g, err
}
func (p *localLiveToolProvider) StartLive(ctx context.Context, ref apphost.LiveReference) (apphost.LiveStartResult, error) {
	p.starts.Add(1)
	p.ref = ref
	if p.startErr != nil {
		return apphost.LiveStartResult{}, p.startErr
	}
	return p.localLiveRouteProvider.StartLive(ctx, ref)
}
func (p *localLiveToolProvider) QueryLive(ctx context.Context, ref apphost.LiveReference) (apphost.LiveReceipt, error) {
	if p.unknown.Load() {
		return apphost.LiveReceipt{Reference: ref, State: "unknown"}, nil
	}
	return p.localLiveRouteProvider.QueryLive(ctx, ref)
}

func installLocalLiveToolFixture(t *testing.T, f *localHomeFixture) *localLiveToolProvider {
	t.Helper()
	p := &localLiveToolProvider{localLiveRouteProvider: localLiveRouteProvider{connection: apphost.Connection{ID: "broadcast", AccountID: "synthetic-owner", Label: "合成直播目标"}}}
	registry, err := apphost.New(apphost.Registration{Application: apphost.Application{ID: "synthetic-live", Name: "合成直播", InstallationID: "synthetic-install", Registration: "synthetic-registration"}, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := f.handler.home.identity.Read()
	if err != nil {
		t.Fatal(err)
	}
	host, err := f.handler.chat.projectTools.forFish(identity.FishID)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.chat.applications = registry
	f.handler.chat.live = apphost.NewLiveController(registry, host.store, localLiveRouteOutput{})
	f.handler.chat.liveFish = identity.FishID
	return p
}

func localLiveTestStart() map[string]any {
	return map[string]any{"action": "start", "app_id": "synthetic-live", "connection_id": "broadcast", "duration_seconds": 60}
}

func TestLocalAppLiveNormalChatDiscoverLifecycleAndReplay(t *testing.T) {
	f := localChatFixture(t)
	p := installLocalLiveToolFixture(t, f)
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return &syntheticObservationModel{call: func(_ context.Context, _ string, turns []map[string]string, tools []mcptools.Tool) (string, error) {
			calls++
			switch calls {
			case 1:
				for _, tool := range tools {
					if tool.Name == localLiveToolName {
						t.Fatal("live tool eagerly exposed")
					}
				}
				return localToolCall("discover_tools", map[string]any{"query": "app_live"}), nil
			case 2:
				return localToolCall(localLiveToolName, map[string]any{"action": "targets"}), nil
			case 3:
				if !strings.Contains(localModelHistory(turns), `\"available\":true`) {
					t.Fatal("missing authorized targets", localModelHistory(turns))
				}
				return localToolCall(localLiveToolName, localLiveTestStart()), nil
			case 4:
				return localToolCall(localLiveToolName, localLiveTestStart()), nil
			case 5:
				return localToolCall(localLiveToolName, map[string]any{"action": "status", "id": p.ref.ID}), nil
			case 6:
				return localToolCall(localLiveToolName, map[string]any{"action": "stop", "id": p.ref.ID}), nil
			default:
				if !strings.Contains(localModelHistory(turns), `\"remote_ended\":true`) {
					t.Fatal("missing settled receipt", localModelHistory(turns))
				}
				return `{"action":"reply","messages":["合成直播会话已停止并核对。"]}`, nil
			}
		}}, nil
	}
	if code, out := localChatSend(f, "live-normal", "按已授权目标直播60秒后停止"); code != 200 || out["status"] != "completed" || p.starts.Load() != 1 {
		t.Fatalf("normal chat %d %#v starts=%d", code, out, p.starts.Load())
	}
	if code, out := localChatSend(f, "live-normal", "按已授权目标直播60秒后停止"); code != 200 || calls != 7 || p.starts.Load() != 1 {
		t.Fatalf("replay %d %#v calls=%d starts=%d", code, out, calls, p.starts.Load())
	}
}

func localLiveToolRunFixture(t *testing.T, f *localHomeFixture) *localToolSet {
	t.Helper()
	const request, text = "live-boundary", "合成直播工具边界"
	if code, _ := localChatSend(f, request, text); code != 503 {
		t.Fatal("expected unconfigured model", code)
	}
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	in.TraceID, in.OriginalText = request, new(string)
	*in.OriginalText = text
	run, err := store.OpenLocalToolRun(t.Context(), in, false)
	if err != nil {
		t.Fatal(err)
	}
	tools := f.handler.chat.tools(store, in)
	tools.run = run
	return tools
}

func liveToolTestCall(t *testing.T, tools *localToolSet, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tools.Call(t.Context(), localLiveToolName, raw)
}

func requireLiveToolFailure(t *testing.T, err error, code string) {
	t.Helper()
	var safe interface{ SafeToolFailure() (string, string) }
	if !errors.As(err, &safe) {
		t.Fatalf("missing safe failure: %v", err)
	}
	got, message := safe.SafeToolFailure()
	if got != code || strings.Contains(message, "synthetic-secret") || strings.Contains(message, "rtmp://") {
		t.Fatalf("unsafe or wrong error %s %s", got, message)
	}
}

func TestLocalAppLivePermissionIdentityRecoveryAndUnknown(t *testing.T) {
	f := localChatFixture(t)
	p := installLocalLiveToolFixture(t, f)
	tools := localLiveToolRunFixture(t, f)
	for _, owner := range []string{"", "other-owner"} {
		original := tools.access.ownerID
		tools.access.ownerID = owner
		_, err := liveToolTestCall(t, tools, map[string]any{"action": "targets"})
		requireLiveToolFailure(t, err, "app_live_403")
		tools.access.ownerID = original
	}
	p.denied.Store(true)
	value, err := liveToolTestCall(t, tools, map[string]any{"action": "targets"})
	if err != nil || !strings.Contains(value, `"available":false`) {
		t.Fatal(value, err)
	}
	_, err = liveToolTestCall(t, tools, localLiveTestStart())
	requireLiveToolFailure(t, err, "app_live_403")
	if p.starts.Load() != 0 {
		t.Fatal("denied target started")
	}
	p.denied.Store(false)
	tools.inspectOnly = true
	_, err = liveToolTestCall(t, tools, localLiveTestStart())
	requireLiveToolFailure(t, err, "app_live_409")
	tools.inspectOnly = false
	value, err = liveToolTestCall(t, tools, localLiveTestStart())
	if err != nil || !strings.Contains(value, `"state":"live"`) {
		t.Fatal(value, err)
	}
	tools.inspectOnly = true
	value, err = liveToolTestCall(t, tools, map[string]any{"action": "targets"})
	if err != nil || !strings.Contains(value, `"active":{"id":"`+p.ref.ID+`"`) || p.starts.Load() != 1 {
		t.Fatal("active session recovery did not preserve original id", value, err)
	}
	p.unknown.Store(true)
	for _, action := range []string{"status", "reconcile", "status"} {
		if _, err := liveToolTestCall(t, tools, map[string]any{"action": action, "id": p.ref.ID}); err != nil {
			t.Fatal(action, err)
		}
	}
	if p.starts.Load() != 1 {
		t.Fatal("query started another broadcast")
	}
	if _, err := liveToolTestCall(t, tools, map[string]any{"action": "stop", "id": p.ref.ID}); err != nil {
		t.Fatal(err)
	}
	for _, in := range []map[string]any{
		{"action": "status", "id": strings.Repeat("a", 64)},
		{"action": "stop", "id": p.ref.ID, "app_id": "redirect"},
		{"action": "status", "id": p.ref.ID, "connection_id": ""},
		{"action": "synthetic-secret"},
		{"action": "start", "app_id": "synthetic-live", "connection_id": "broadcast", "duration_seconds": 60, "request_key": "model-chosen"},
	} {
		_, err := liveToolTestCall(t, tools, in)
		requireLiveToolFailure(t, err, "app_live_400")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = tools.Call(ctx, localLiveToolName, json.RawMessage(`{"action":"targets"}`))
	requireLiveToolFailure(t, err, "app_live_409")
}

func TestLocalAppLiveChecksSourceBeforeAndAfterQuery(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
			f := localChatFixture(t)
			p := installLocalLiveToolFixture(t, f)
			tools := localLiveToolRunFixture(t, f)
			clear := func() {
				if err := tools.store.Clear(companion.ScopeID("local", tools.access.fishID, tools.access.ownerID)); err != nil {
					t.Fatal(err)
				}
			}
			if after {
				p.onGrant = clear
			} else {
				clear()
			}
			_, err := liveToolTestCall(t, tools, map[string]any{"action": "targets"})
			requireLiveToolFailure(t, err, "app_live_403")
			if p.starts.Load() != 0 {
				t.Fatal("source query dispatched a start")
			}
		})
	}
}

func TestLocalAppLiveNoProductionTargetAndSafeProviderFailure(t *testing.T) {
	f := localChatFixture(t)
	tools := localLiveToolRunFixture(t, f)
	value, err := liveToolTestCall(t, tools, map[string]any{"action": "targets"})
	if err != nil || !strings.Contains(value, `"available":false`) || !strings.Contains(value, localLiveUnavailable) {
		t.Fatal(value, err)
	}
	p := installLocalLiveToolFixture(t, f)
	p.startErr = errors.New("synthetic-secret rtmp://credential/private/path")
	value, err = liveToolTestCall(t, tools, localLiveTestStart())
	if err != nil {
		requireLiveToolFailure(t, err, "app_live_409")
	}
	if strings.Contains(value, "synthetic-secret") || strings.Contains(value, "rtmp://") {
		t.Fatal("private provider error leaked", value)
	}
	if p.starts.Load() != 1 {
		t.Fatal("missing synthetic dispatch")
	}
}
