package station

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/companion"
)

func TestLocalChatWritesPersistentToolThroughNormalEngine(t *testing.T) {
	f := localChatFixture(t)
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(ctx context.Context, system string, messages []map[string]string) (string, error) {
			calls++
			switch calls {
			case 1:
				return `{"action":"tool","tool":"server_write","arguments":{"path":"Project Tools/hello.py","content":"print('hello-home')\n"}}`, nil
			case 2:
				return `{"action":"tool","tool":"server_read","arguments":{"path":"Project Tools/hello.py"}}`, nil
			default:
				last := messages[len(messages)-1]["content"]
				if !strings.Contains(last, "hello-home") {
					t.Error("reply did not see real file result", last)
				}
				return `{"action":"reply","messages":["工具已保存，读回内容也一致。"],"reason":"reply"}`, nil
			}
		}), nil
	}
	if code, out := localChatSend(f, "write-tool", "写一个可保留的工具并读回来检查"); code != 200 || out["status"] != "completed" {
		t.Fatalf("normal entry failed: %d %#v", code, out)
	}
	if calls != 3 {
		t.Fatal("unexpected extra model calls", calls)
	}
	_, home := f.request("GET", "/api/local/home", nil, true)
	path := filepath.Join(home["directory"].(string), "Project Tools", "hello.py")
	if content, err := os.ReadFile(path); err != nil || string(content) != "print('hello-home')\n" {
		t.Fatal("tool was not persisted", err, string(content))
	}
	f.server.Close()
	f.handler.Close()
	restarted := newLocalHomeFixture(t, f.dir, f.root)
	restarted.login()
	store, in, err := restarted.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	tools := restarted.handler.chat.tools(store, in)
	result, err := tools.Call(context.Background(), "server_read", json.RawMessage(`{"path":"Project Tools/hello.py"}`))
	if err != nil || !strings.Contains(result, "hello-home") {
		t.Fatal("restart lost persistent tool", result, err)
	}
	_, current := restarted.request("GET", "/api/local/home", nil, true)
	restarted.request("POST", "/api/local/home", map[string]any{"action": "pause", "revision": current["revision"]}, true)
	if _, err = tools.Call(context.Background(), "server_read", json.RawMessage(`{"path":"Project Tools/hello.py"}`)); err == nil {
		t.Fatal("retained tool access bypassed pause")
	}
}

func TestLocalToolsDoNotExposeHostCredentialOrOtherDirectories(t *testing.T) {
	f := localChatFixture(t)
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	tools := f.handler.chat.tools(store, in)
	secret := filepath.Join(f.dir, "synthetic-host-secret.txt")
	if err = os.WriteFile(secret, []byte("synthetic-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{secret, "../../host/synthetic-host-secret.txt"} {
		raw, _ := json.Marshal(map[string]string{"path": path})
		if out, err := tools.Call(context.Background(), "server_read", raw); err == nil || strings.Contains(out, "synthetic-secret") {
			t.Fatal("private path escaped", out, err)
		}
	}
}
