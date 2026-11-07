package companion

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"wechat-bot/internal/mcptools"
)

type localToolPolicyModel struct {
	calls   int
	systems []string
	toolRun bool
}

func (m *localToolPolicyModel) Chat(_ context.Context, system string, _ []map[string]string) (string, error) {
	m.calls++
	m.systems = append(m.systems, system)
	if m.toolRun && m.calls == 1 {
		return `{"action":"tool","tool":"workspace_list","arguments":{"path":"."}}`, nil
	}
	return `{"action":"reply","messages":["已经根据实际目录继续处理。"]}`, nil
}

type localToolPolicyAccess struct{ calls int }

func (*localToolPolicyAccess) List(context.Context) ([]mcptools.Tool, error) {
	return []mcptools.Tool{
		{Name: "workspace_list", Description: "List files inside the granted workspace.", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "memory_search", Description: "Search sourced memory.", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}, nil
}

func (a *localToolPolicyAccess) Call(_ context.Context, name string, _ json.RawMessage) (string, error) {
	if name != "workspace_list" {
		return "", nil
	}
	a.calls++
	return `{"entries":["notes.md"],"complete":true}`, nil
}

func localPolicyInput(channel string) Input {
	return Input{Channel: channel, Account: "fish", Thread: "home", Sender: "owner", TraceID: "local-policy", Mode: "direct", Text: "看看工作区，再继续。", Preview: true, Directed: true}
}

func TestLocalRespondUsesShortToolPolicyAndCanContinueAfterFileTool(t *testing.T) {
	model := &localToolPolicyModel{toolRun: true}
	e, err := NewPreview(enabledConfig(), model, "base persona")
	if err != nil {
		t.Fatal(err)
	}
	tools := &localToolPolicyAccess{}
	e.Tools = tools
	e.MaxToolCalls = 2
	d, err := e.Respond(context.Background(), localPolicyInput(LocalChannel))
	if err != nil || d.Action != "reply" || tools.calls != 1 || model.calls != 2 {
		t.Fatalf("decision=%+v err=%v tool_calls=%d model_calls=%d", d, err, tools.calls, model.calls)
	}
	if len(model.systems) == 0 {
		t.Fatal("model did not receive a system prompt")
	}
	system := model.systems[0]
	for _, forbidden := range []string{"wechat_read", "web_audio_search", "music_local_start", "RVC", "微信已接受"} {
		if strings.Contains(system, forbidden) {
			t.Fatalf("local system retained application protocol %q", forbidden)
		}
	}
	for _, required := range []string{"workspace_list", "memory_search", "默认值不是能力上限", "本机结果也不等于任何已接入应用已投递"} {
		if !strings.Contains(system, required) {
			t.Fatalf("local system omitted %q", required)
		}
	}
}

func TestLocalOrdinaryTurnHasOneModelCallAndSmallerWholeSystem(t *testing.T) {
	localModel := &localToolPolicyModel{}
	local, err := NewPreview(enabledConfig(), localModel, "base persona")
	if err != nil {
		t.Fatal(err)
	}
	local.Tools = &localToolPolicyAccess{}
	if _, err := local.Respond(context.Background(), localPolicyInput(LocalChannel)); err != nil {
		t.Fatal(err)
	}
	if localModel.calls != 1 {
		t.Fatalf("ordinary local turn used %d model calls", localModel.calls)
	}

	connectedModel := &localToolPolicyModel{}
	connected, err := NewPreview(enabledConfig(), connectedModel, "base persona")
	if err != nil {
		t.Fatal(err)
	}
	connected.Tools = &localToolPolicyAccess{}
	if _, err := connected.Respond(context.Background(), localPolicyInput("wechat")); err != nil {
		t.Fatal(err)
	}
	if len(localModel.systems) != 1 || len(connectedModel.systems) != 1 {
		t.Fatal("expected one captured system for each direct response")
	}
	localTokens := EstimateTokens(localModel.systems[0])
	connectedTokens := EstimateTokens(connectedModel.systems[0])
	if localTokens*100 >= connectedTokens*60 {
		t.Fatalf("whole local runner system was not reduced enough: local=%d connected=%d", localTokens, connectedTokens)
	}
	t.Logf("whole runner system conservative estimate: local=%d connected=%d", localTokens, connectedTokens)
}

type nativeLocalToolPolicyModel struct {
	localToolPolicyModel
	tools []mcptools.Tool
}

func (m *nativeLocalToolPolicyModel) ChatFunctions(ctx context.Context, system string, turns []map[string]string, tools []mcptools.Tool) (string, error) {
	m.tools = append([]mcptools.Tool(nil), tools...)
	return m.Chat(ctx, system, turns)
}

func TestLocalNativeRunnerRetainsDiscoveryWithoutApplicationProtocols(t *testing.T) {
	model := &nativeLocalToolPolicyModel{}
	e, err := NewPreview(enabledConfig(), model, "base persona")
	if err != nil {
		t.Fatal(err)
	}
	e.Tools = &localToolPolicyAccess{}
	if _, err := e.Respond(context.Background(), localPolicyInput(LocalChannel)); err != nil {
		t.Fatal(err)
	}
	if len(model.systems) != 1 || strings.Contains(model.systems[0], "web_audio_search") || strings.Contains(model.systems[0], "微信已接受") {
		t.Fatalf("unexpected native local system: %q", model.systems)
	}
	for _, tool := range model.tools {
		if tool.Name == "discover_tools" {
			return
		}
	}
	t.Fatalf("native local runner lost discover_tools: %#v", model.tools)
}
