package station

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

// The authoritative process registry is synthetic here. The real admission,
// checkpoint, model loop, SQLite queue and background worker are exercised;
// no native executable or external model is launched.
func syntheticFollowupProcess(t *testing.T, f *localHomeFixture, request string) (string, []string, string) {
	t.Helper()
	_, home := f.request("GET", "/api/local/home", nil, false)
	directory := home["directory"].(string)
	argv := []string{filepath.Join(directory, "synthetic-followup.exe")}
	if err := os.WriteFile(argv[0], []byte("synthetic executable; never executed"), 0600); err != nil {
		t.Fatal(err)
	}
	runtime := t.TempDir()
	f.handler.chat.windows = &agenthost.WindowsConfig{Pythonw: filepath.Join(runtime, "pythonw.exe"), Helper: filepath.Join(runtime, "windows_agent_host.pyw"), Spool: filepath.Join(runtime, "spool")}
	identity, _ := f.handler.home.identity.Read()
	scope := localProcessScope(identity.FishID, identity.LocalOwner.ID)
	key := localCheckpointKey(localProcessTool, scope, request, directory, argv, 0)
	hash := localCheckpointKey("windows-direct-argv-v1", directory, argv, 0, "windows")
	id := "process-synthetic-followup"
	if err := os.MkdirAll(filepath.Join(f.dir, "local-processes"), 0700); err != nil {
		t.Fatal(err)
	}
	writeLocalProcessRecord(t, filepath.Join(f.dir, "local-processes"), map[string]any{"id": id, "scope": scope, "request_key": "local-" + key, "request_hash": hash, "state": "completed", "output": "SYNTHETIC VERIFIED RESULT", "exit_code": 0, "started": time.Now().UTC(), "updated": time.Now().UTC()})
	return directory, argv, id
}

func TestLocalTaskFollowupNormalChatCompletesWithoutNewUserEvent(t *testing.T) {
	f := localChatFixture(t)
	const request = "followup-normal"
	directory, argv, processID := syntheticFollowupProcess(t, f, request)
	var calls atomic.Int32
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return &syntheticObservationModel{call: func(_ context.Context, _ string, history []map[string]string, _ []mcptools.Tool) (string, error) {
			switch calls.Add(1) {
			case 1:
				return localToolCall("discover_tools", map[string]string{"query": localProcessTool}), nil
			case 2:
				return localToolCall(localProcessTool, map[string]any{"action": "start", "path": directory, "argv": argv, "timeout_seconds": 0}), nil
			case 3:
				return localToolCall("discover_tools", map[string]string{"query": localFollowupToolName}), nil
			case 4:
				return localToolCall(localFollowupToolName, map[string]string{"action": "schedule", "process_id": processID}), nil
			case 5:
				if !strings.Contains(localModelHistory(history), "local-task-") {
					t.Error("schedule has no durable id")
				}
				return `{"action":"reply","messages":["任务已登记，完成后继续。"]}`, nil
			case 6:
				text := localModelHistory(history)
				if !strings.Contains(text, "SYNTHETIC VERIFIED RESULT") || !strings.Contains(text, `"background_result":true`) {
					t.Error("missing actual background evidence", text)
				}
				return localToolCall("server_write", map[string]string{"path": "works/followup-result.txt", "content": "SYNTHETIC VERIFIED RESULT"}), nil
			case 7:
				return localToolCall(localArtifactPublishTool, map[string]string{"path": "works/followup-result.txt"}), nil
			default:
				if len(localPublicationWorkIDs(localModelHistory(history))) != 1 {
					t.Error("continuation did not register its finished artifact")
				}
				return `{"action":"reply","messages":["已核对程序结果并登记成品。"]}`, nil
			}
		}}, nil
	}
	if code, out := localChatSend(f, request, "运行这个程序，结束后整理为文件并告诉我结果"); code != 200 || out["status"] != "completed" {
		t.Fatal(code, out)
	}
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	var job companion.LocalTaskFollowup
	for deadline := time.Now().Add(6 * time.Second); ; {
		page, e := store.ListLocalTaskFollowups(t.Context(), in, "", 20)
		if e != nil {
			t.Fatal(e)
		}
		if len(page.Items) == 1 {
			job = page.Items[0]
			if job.State == "completed" {
				break
			}
			if job.State == "failed" || job.State == "cancelled" {
				t.Fatal("background continuation failed", job)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("followup did not finish", job, calls.Load())
		}
		time.Sleep(15 * time.Millisecond)
	}
	page, err := store.LocalConversationPage(t.Context(), in, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	users, assistants := 0, 0
	for _, item := range page.Items {
		if item.Role == "user" {
			users++
		} else if item.Role == "assistant" {
			assistants++
		}
	}
	if users != 1 || assistants != 2 || calls.Load() != 8 {
		t.Fatal("duplicated user or reply group", users, assistants, calls.Load())
	}
	if count := len(f.handler.chat.processes.jobs.List(localProcessScope(in.Account, in.Sender))); count != 1 {
		t.Fatal("program replayed", count)
	}
	if code, _ := localChatSend(f, request, "运行这个程序，结束后整理为文件并告诉我结果"); code != 200 || calls.Load() != 8 {
		t.Fatal("original request replay invoked model", code, calls.Load())
	}
	if code, view := f.request("GET", "/api/local/chat", nil, false); code != 200 || view["background_pending"] != false {
		t.Fatal(code, view)
	}
	if code, view := f.request("POST", "/api/local/task-followups", map[string]string{"action": "cancel", "id": job.ID}, true); code != 200 || view["state"] != "completed" {
		t.Fatal("late cancel hid accepted reply", code, view)
	}
}

func setupWaitingFollowupLocked(t *testing.T, f *localHomeFixture) (*companion.Store, companion.Input, companion.LocalTaskFollowup) {
	t.Helper()
	const request, text = "followup-seeded", "合成等待原程序"
	_, _, processID := syntheticFollowupProcess(t, f, request)
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	in.TraceID, in.Text, in.OriginalText = request, text, new(string)
	*in.OriginalText = text
	in.Source = &companion.SourceRef{MessageID: request}
	in.MessageTime = time.Now().UTC().Format(time.RFC3339Nano)
	if err := store.Observe(in, localConversationConfig(""), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	source, ok := store.SourceForInput(in)
	if !ok {
		t.Fatal("missing source")
	}
	in.Source = source
	run, err := store.OpenLocalToolRun(t.Context(), in, true)
	if err != nil {
		t.Fatal(err)
	}
	tools := f.handler.chat.tools(store, in)
	tools.run = run
	grant, err := tools.access.resolve()
	if err != nil {
		t.Fatal(err)
	}
	processes := tools.process.jobs.List(localProcessScope(grant.FishID, grant.OwnerID))
	if len(processes) != 1 {
		t.Fatal(processes)
	}
	key := strings.TrimPrefix(processes[0].Key, "local-")
	if err := run.Reserve(t.Context(), companion.LocalToolCheckpoint{Key: key, Tool: localProcessTool, Grant: localProcessScope(grant.FishID, grant.OwnerID)}); err != nil {
		t.Fatal(err)
	}
	job, err := store.ScheduleLocalTaskFollowup(t.Context(), in, *source, processID, grant.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return store, in, job
}

func TestLocalTaskFollowupCancelledForgottenAndRevokedDoNotGenerate(t *testing.T) {
	for _, mode := range []string{"cancelled", "forgotten", "revoked", "unknown_generation"} {
		t.Run(mode, func(t *testing.T) {
			f := localChatFixture(t)
			f.handler.chat.turn.Lock()
			store, in, job := setupWaitingFollowupLocked(t, f)
			var calls atomic.Int32
			f.handler.chat.modelFactory = func() (companion.LanguageModel, error) { calls.Add(1); return nil, errors.New("must not call model") }
			switch mode {
			case "cancelled":
				_, _ = store.UpdateLocalTaskFollowup(t.Context(), in, job.ID, job.Revision, "cancelled", time.Now().UTC(), "owner_cancelled")
			case "forgotten":
				if err := store.Clear(companion.ScopeID("local", in.Account, in.Thread)); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				identity, _ := f.handler.home.identity.Read()
				if _, err := f.handler.home.identity.SetHomeAccess(identity.LocalOwner.Revision, false); err != nil {
					t.Fatal(err)
				}
			case "unknown_generation":
				job, _ = store.UpdateLocalTaskFollowup(t.Context(), in, job.ID, job.Revision, "ready", time.Now().UTC(), "")
				job, _ = store.UpdateLocalTaskFollowup(t.Context(), in, job.ID, job.Revision, "generating", time.Now().UTC(), "")
			}
			f.handler.chat.turn.Unlock()
			f.handler.chat.followups.tick(t.Context())
			latest, err := store.GetLocalTaskFollowup(t.Context(), in, job.ID)
			if err != nil || calls.Load() != 0 || latest.State != "cancelled" && latest.State != "failed" {
				t.Fatal(mode, latest, err, calls.Load())
			}
		})
	}
}

func TestLocalTaskFollowupForegroundPriorityAndLateCancel(t *testing.T) {
	for _, mode := range []string{"foreground", "cancel", "revoke_during_model"} {
		t.Run(mode, func(t *testing.T) {
			f := localChatFixture(t)
			f.handler.chat.turn.Lock()
			store, in, job := setupWaitingFollowupLocked(t, f)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
				return localModelFunc(func(ctx context.Context, _ string, _ []map[string]string) (string, error) {
					if calls.Add(1) == 1 {
						close(entered)
						if mode == "foreground" {
							<-ctx.Done()
							return "", ctx.Err()
						}
						<-release // Intentionally ignore cancellation to test late results.
						return `{"action":"reply","messages":["迟到回复不应保存。"]}`, nil
					}
					return `{"action":"reply","messages":["先回答当前问题。"]}`, nil
				}), nil
			}
			f.handler.chat.turn.Unlock()
			finished := make(chan struct{})
			go func() { f.handler.chat.followups.tick(t.Context()); close(finished) }()
			select {
			case <-entered:
			case <-time.After(4 * time.Second):
				t.Fatal("background model did not start")
			}
			if mode == "foreground" {
				if code, out := localChatSend(f, "priority-new", "先回答这个问题"); code != 200 || out["status"] != "completed" {
					t.Fatal(code, out)
				}
			} else if mode == "cancel" {
				if code, view := f.request("POST", "/api/local/task-followups", map[string]string{"action": "cancel", "id": job.ID}, true); code != 200 || view["state"] != "cancelled" {
					t.Fatal(code, view)
				}
				close(release)
			} else {
				identity, _ := f.handler.home.identity.Read()
				if _, err := f.handler.home.identity.SetHomeAccess(identity.LocalOwner.Revision, false); err != nil {
					t.Fatal(err)
				}
				close(release)
			}
			select {
			case <-finished:
			case <-time.After(4 * time.Second):
				t.Fatal("background worker did not release turn")
			}
			if accepted, err := store.LocalContinuationAccepted(t.Context(), in, job.ID); err != nil || accepted {
				t.Fatal("late cancelled result was accepted", accepted, err)
			}
			latest, err := store.GetLocalTaskFollowup(t.Context(), in, job.ID)
			if err != nil || latest.State != "cancelled" && latest.State != "failed" {
				t.Fatal(latest, err)
			}
		})
	}
}

func TestLocalTaskFollowupRestartReconcilesCommittedGroup(t *testing.T) {
	f := localChatFixture(t)
	f.handler.chat.turn.Lock()
	store, in, job := setupWaitingFollowupLocked(t, f)
	job, err := store.UpdateLocalTaskFollowup(t.Context(), in, job.ID, job.Revision, "ready", time.Now().UTC(), "")
	if err != nil {
		t.Fatal(err)
	}
	job, err = store.UpdateLocalTaskFollowup(t.Context(), in, job.ID, job.Revision, "generating", time.Now().UTC().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptLocalContinuation(t.Context(), in, job.ID, []string{"已保存的续办回复。"}, localConversationConfig(""), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	f.handler.chat.turn.Unlock()
	next := localRestartChat(t, f)
	var calls atomic.Int32
	next.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls.Add(1)
		return nil, errors.New("must reconcile without another model call")
	}
	reopened, again, err := next.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	again.TraceID, again.Text, again.OriginalText, again.Source = in.TraceID, in.Text, in.OriginalText, in.Source
	job, err = reopened.UpdateLocalTaskFollowup(t.Context(), again, job.ID, job.Revision, "generating", time.Now().UTC(), "")
	if err != nil {
		t.Fatal(err)
	}
	next.handler.chat.followups.tick(t.Context())
	latest, err := reopened.GetLocalTaskFollowup(t.Context(), again, job.ID)
	if err != nil || latest.State != "completed" || calls.Load() != 0 {
		t.Fatal("restart repeated a settled generation", latest, err, calls.Load())
	}
}
