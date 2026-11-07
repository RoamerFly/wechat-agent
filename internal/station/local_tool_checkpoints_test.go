package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/companion"
)

func localToolCall(name string, args any) string {
	data, _ := json.Marshal(map[string]any{"action": "tool", "tool": name, "arguments": args})
	return string(data)
}

func localModelHistory(turns []map[string]string) string {
	var all strings.Builder
	for _, turn := range turns {
		all.WriteString(turn["content"])
	}
	return all.String()
}

func localRestartChat(t *testing.T, f *localHomeFixture) *localHomeFixture {
	t.Helper()
	f.server.Close()
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	next := newLocalHomeFixture(t, f.dir, f.root)
	next.login()
	return next
}

func TestLocalToolCheckpointRecoversWrittenFileWithoutWritingAgain(t *testing.T) {
	f := localChatFixture(t)
	_, home := f.request("GET", "/api/local/home", nil, true)
	dir := home["directory"].(string)
	path := filepath.Join(dir, "works", "once.txt")
	write := localToolCall("server_write", map[string]string{"path": "works/once.txt", "content": "synthetic-once"})
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls := 0
		return localModelFunc(func(context.Context, string, []map[string]string) (string, error) {
			calls++
			if calls == 1 {
				return write, nil
			}
			return "", errors.New("synthetic model interruption")
		}), nil
	}
	if code, out := localChatSend(f, "write-once", "写一个文件"); code != 502 {
		t.Fatalf("expected failed reply after write: %d %#v", code, out)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "synthetic-once" {
		t.Fatal("first dispatch did not write the file", err)
	}
	// Removing the synthetic file makes a duplicate dispatch observable.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	next := localRestartChat(t, f)
	next.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls := 0
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			calls++
			history := localModelHistory(turns)
			switch calls {
			case 1:
				if !strings.Contains(history, "tool_recovery") || !strings.Contains(history, "recorded") || strings.Contains(history, "synthetic-once") {
					t.Error("missing metadata-only write checkpoint")
				}
				// Absolute and relative paths resolve to the same operation.
				return localToolCall("server_write", map[string]string{"content": "synthetic-once", "path": path}), nil
			case 2:
				if !strings.Contains(history, "previous_result") {
					t.Error("same call did not recover its prior result")
				}
				return localToolCall("server_write", map[string]string{"path": "works/extra.txt", "content": "must-not-write"}), nil
			default:
				if !strings.Contains(history, "新增写入和启动需要主人发起新请求") {
					t.Error("retry silently accepted a different mutation")
				}
				return `{"action":"reply","messages":["已核对前一次写入。"]}`, nil
			}
		}), nil
	}
	if code, out := localChatSend(next, "write-once", "写一个文件"); code != 200 || out["status"] != "completed" {
		t.Fatalf("retry failed: %d %#v", code, out)
	}
	for _, file := range []string{path, filepath.Join(dir, "works", "extra.txt")} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("retry touched %q: %v", file, err)
		}
	}
}

func TestLocalToolCheckpointFindsInterruptedProcessBeforeModelRetry(t *testing.T) {
	f := localChatFixture(t)
	const request, original = "process-once", "启动项目程序"
	if code, _ := localChatSend(f, request, original); code != 503 {
		t.Fatal("expected an unfinished request with no configured model", code)
	}
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	in.TraceID, in.OriginalText = request, new(string)
	*in.OriginalText = original
	run, err := store.OpenLocalToolRun(t.Context(), in, false)
	if err != nil {
		t.Fatal(err)
	}
	_, home := f.request("GET", "/api/local/home", nil, true)
	dir := home["directory"].(string)
	scope := localProcessScope(in.Account, in.Sender)
	argv := []string{filepath.Join(dir, "synthetic.exe"), "private-argument"}
	key := localCheckpointKey(localProcessTool, scope, request, dir, argv, 0)
	if err := run.Reserve(t.Context(), companion.LocalToolCheckpoint{Key: key, Tool: localProcessTool, Grant: scope}); err != nil {
		t.Fatal(err)
	}
	// Simulate a stop after Jobs committed the dispatch and before the chat
	// journal received its result. OpenJobs reconciles the running record.
	job := agenthost.Process{ID: "process-synthetic-checkpoint", Scope: scope, Key: "local-" + key, State: "running", Started: time.Now().UTC(), Updated: time.Now().UTC(), Output: "private-process-output"}
	root := filepath.Join(f.dir, "local-processes")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(root, job.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	next := localRestartChat(t, f)
	next.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls := 0
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			calls++
			history := localModelHistory(turns)
			if !strings.Contains(history, job.ID) || !strings.Contains(history, "interrupted") || strings.Contains(history, "private-process-output") || calls == 1 && strings.Contains(history, "private-argument") {
				t.Error("restart failed to recover scoped process metadata")
			}
			if calls == 1 {
				return localToolCall(localProcessTool, map[string]any{"action": "start", "path": dir, "argv": argv, "request_key": "different-model-key"}), nil
			}
			if !strings.Contains(history, "recovered") || strings.Contains(history, "windows_runtime_not_configured") {
				t.Error("replay attempted a new start instead of querying the old job")
			}
			return `{"action":"reply","messages":["原进程已中断。"]}`, nil
		}), nil
	}
	if code, out := localChatSend(next, request, original); code != 200 || out["status"] != "completed" {
		t.Fatalf("retry failed: %d %#v", code, out)
	}
	if jobs := next.handler.chat.processes.jobs.List(scope); len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].State != "interrupted" {
		t.Fatal("restart changed the authoritative job set", jobs)
	}
}

func TestLocalToolCheckpointRejectsInvalidProcessBeforeReserveAndContinuesTurn(t *testing.T) {
	f := localChatFixture(t)
	_, home := f.request("GET", "/api/local/home", nil, true)
	dir := home["directory"].(string)
	runtime := t.TempDir()
	// The files need not exist for this preflight-only test; the config is
	// syntactically valid so argv authorization is reached before any launch.
	f.handler.chat.windows = &agenthost.WindowsConfig{
		Pythonw: filepath.Join(runtime, "windows-python-cli", "pythonw.exe"),
		Helper:  filepath.Join(runtime, "windows_agent_host.pyw"),
		Spool:   filepath.Join(runtime, "spool"),
	}
	path := filepath.Join(dir, "works", "after-preflight.txt")
	invalid := localToolCall(localProcessTool, map[string]any{
		"action": "start", "path": "not-an-absolute-home-directory",
		"argv": []string{filepath.Join(dir, "tool.exe")},
	})
	invalidExe := localToolCall(localProcessTool, map[string]any{
		"action": "start", "path": dir,
		"argv": []string{"not-an-authorized-absolute-exe"},
	})
	write := localToolCall("server_write", map[string]string{"path": path, "content": "still-writable"})
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls := 0
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			calls++
			history := localModelHistory(turns)
			switch calls {
			case 1:
				return invalid, nil
			case 2:
				if !strings.Contains(history, "参数 $.path") || !strings.Contains(history, "校验阶段未执行工具") || strings.Contains(history, "新增写入和启动需要主人发起新请求") {
					t.Error("invalid start was not exposed as a repairable preflight failure", history)
				}
				return invalidExe, nil
			case 3:
				if !strings.Contains(history, "参数 $.argv[0]") || !strings.Contains(history, "校验阶段未执行工具") || strings.Contains(history, "新增写入和启动需要主人发起新请求") {
					t.Error("invalid executable was not exposed as a repairable preflight failure", history)
				}
				return write, nil
			default:
				if strings.Contains(history, "inspect-only") {
					t.Error("known invalid start incorrectly made the turn inspect-only", history)
				}
				return `{"action":"reply","messages":["已写入。"]}`, nil
			}
		}), nil
	}
	if code, out := localChatSend(f, "invalid-process-then-write", "先试运行，再写文件"); code != 200 || out["status"] != "completed" {
		t.Fatalf("same-turn repair failed: %d %#v", code, out)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "still-writable" {
		t.Fatalf("write after rejected process did not complete: %q, %v", data, err)
	}
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	original := "先试运行，再写文件"
	in.TraceID, in.Text, in.OriginalText = "invalid-process-then-write", original, &original
	run, err := store.OpenLocalToolRun(t.Context(), in, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Operations) != 1 || run.Operations[0].Tool != "server_write" {
		t.Fatalf("invalid process start was reserved: %#v", run.Operations)
	}
}

func TestLocalToolCheckpointUncertainWriteAndLegacyRequestStayReadOnly(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "reserved without result"
		if legacy {
			name = "pre-checkpoint request"
		}
		t.Run(name, func(t *testing.T) {
			f := localChatFixture(t)
			_, home := f.request("GET", "/api/local/home", nil, true)
			path := filepath.Join(home["directory"].(string), "works", "uncertain.txt")
			store, in, err := f.handler.chat.conversation()
			if err != nil {
				t.Fatal(err)
			}
			original := "写一次"
			in.TraceID, in.Text, in.OriginalText = "uncertain-write", original, &original
			in.Source = &companion.SourceRef{MessageID: in.TraceID}
			now := time.Now().UTC()
			in.MessageTime = now.Format(time.RFC3339Nano)
			if err := store.Observe(in, localConversationConfig(""), now); err != nil {
				t.Fatal(err)
			}
			if !legacy {
				run, err := store.OpenLocalToolRun(t.Context(), in, true)
				if err != nil {
					t.Fatal(err)
				}
				access := localWorkspaceAccess{home: f.handler.chat.home}
				grant, err := access.resolve()
				if err != nil {
					t.Fatal(err)
				}
				op := companion.LocalToolCheckpoint{
					Key: localCheckpointKey("server_write", path, "synthetic"), Tool: "server_write",
					Grant: localCheckpointKey(grant), Target: filepath.Join("works", "uncertain.txt"),
				}
				if err := run.Reserve(t.Context(), op); err != nil {
					t.Fatal(err)
				}
			}
			next := localRestartChat(t, f)
			next.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
				calls := 0
				return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
					calls++
					history := localModelHistory(turns)
					if calls == 1 {
						if !strings.Contains(history, "tool_recovery") || !legacy && !strings.Contains(history, "needs_review") {
							t.Error("retry did not identify its uncertain history")
						}
						return localToolCall("server_write", map[string]string{"path": path, "content": "synthetic"}), nil
					}
					return `{"action":"reply","messages":["请核对旧操作。"]}`, nil
				}), nil
			}
			if code, _ := localChatSend(next, in.TraceID, original); code != 200 {
				t.Fatal("read-only continuation failed", code)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("uncertain write was dispatched", err)
			}
		})
	}
}
