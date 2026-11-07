package pluginruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/pluginhost"
)

// This child tests the wire state machine only; platform isolation has its own
// real LPAC/bubblewrap test. It must not be used as an Executor implementation.
func TestProtocolChild(t *testing.T) {
	if os.Getenv("WECHAT_PLUGIN_PROTOCOL_CHILD") != "1" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		exitAfterReply := false
		var q struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(scan.Bytes(), &q)
		if q.ID == 0 {
			continue
		}
		var result any = map[string]any{}
		if q.Method == "initialize" {
			result = map[string]string{"protocolVersion": "2025-06-18"}
		}
		if q.Method == "tools/call" {
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
				Meta      map[string]any `json:"_meta"`
			}
			_ = json.Unmarshal(q.Params, &p)
			switch p.Name {
			case "final":
				result = strings.Repeat("z", 700)
				exitAfterReply = true
			case "block":
				time.Sleep(time.Minute)
			case "wrong":
				q.ID++
			case "overflow":
				fmt.Println(strings.Repeat("x", 2048))
				continue
			case "crash":
				os.Exit(3)
			default:
				result = p.Meta
			}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": result})
		fmt.Println(string(b))
		if exitAfterReply {
			os.Exit(0)
		}
	}
	os.Exit(0)
}
func TestSessionRetainsFinalResponseOnImmediateExit(t *testing.T) {
	for i := 0; i < 12; i++ {
		s := testSession(t)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		result, err := s.Call(ctx, invocation(), "final", json.RawMessage(`{}`))
		cancel()
		if err != nil || string(result) != `"`+strings.Repeat("z", 700)+`"` {
			t.Fatalf("last result lost: %v", err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
func testSession(t *testing.T) *session {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProtocolChild$")
	cmd.Env = append(os.Environ(), "WECHAT_PLUGIN_PROTOCOL_CHILD=1")
	p := pluginhost.Package{ID: "sample", Version: "1", Status: "ready"}
	g := pluginhost.Grant{Account: "a", Plugin: "sample", Version: "1"}
	s, err := startSession(cmd, func() {}, p, g, 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = s.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}
func invocation() pluginhost.Invocation {
	return pluginhost.Invocation{Account: "a", Thread: "room", Requester: "person", Purpose: "synthetic", RequestKey: "key", SourceVersions: map[string]int{"source": 2}}
}
func TestSessionContextAndAuthorization(t *testing.T) {
	s := testSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Health(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := s.Call(ctx, invocation(), "echo", json.RawMessage(`{"account":"forged"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]pluginhost.Invocation
	if json.Unmarshal(r, &result) != nil || result["wechat/context"].Account != "a" || result["wechat/context"].SourceVersions["source"] != 2 {
		t.Fatalf("context lost: %s", r)
	}
	c := invocation()
	c.Account = "other"
	if _, err = s.Call(ctx, c, "echo", json.RawMessage(`{}`)); err == nil {
		t.Fatal("cross account accepted")
	}
}

func TestSessionCarriesHostBoundCallMetadata(t *testing.T) {
	s := testSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, callMetadataKey{}, callMetadataResult{metadata: map[string]json.RawMessage{
		"wechat/document": json.RawMessage(`{"inputs":[{"artifact_id":"asset-synthetic","path":"C:/private/input.png"}],"output_dir":"C:/private/out"}`),
		"station/audio":   json.RawMessage(`{"input":{"artifact_id":"work-synthetic","path":"C:/private/input.wav"}}`),
	}})
	raw, err := s.Call(ctx, invocation(), "echo", json.RawMessage(`{"artifact_id":"asset-synthetic"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || !strings.Contains(string(result["wechat/document"]), `asset-synthetic`) ||
		!strings.Contains(string(result["station/audio"]), `work-synthetic`) || strings.Contains(string(result["wechat/context"]), `private/input`) {
		t.Fatalf("host metadata not bound separately: %s", raw)
	}
}
func TestSessionRejectsBrokenFramesAndNoReplay(t *testing.T) {
	for _, tool := range []string{"wrong", "overflow", "crash", "block"} {
		t.Run(tool, func(t *testing.T) {
			s := testSession(t)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if _, err := s.Call(ctx, invocation(), tool, json.RawMessage(`{}`)); err == nil {
				t.Fatal("invalid response accepted")
			}
			if _, err := s.Call(context.Background(), invocation(), "echo", json.RawMessage(`{}`)); err == nil {
				t.Fatal("broken session reused")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if tool == "block" && s.cmd.ProcessState == nil {
				t.Fatal("blocking direct protocol child was not confirmed terminated")
			}
		})
	}
}
func TestSessionQueuedCancellationDoesNotKillActiveCall(t *testing.T) {
	s := testSession(t)
	s.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Call(ctx, invocation(), "echo", json.RawMessage(`{}`)); err == nil {
		t.Fatal("cancel ignored")
	}
	<-s.gate
	if s.dead.Load() {
		t.Fatal("queued cancellation killed session")
	}
}
