package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/websession"
)

type completedSpeechFixture struct {
	f              *webFixture
	policy         *speechPluginPolicy
	ctx            context.Context
	taskID         string
	scope          string
	sessionID      string
	audioURL       string
	sessionExpired atomic.Bool
}

// The provider callback is synthetic, but the source/session authority,
// Service/Validate checks and durable completion/private-result store are real.
func completedSpeechRevocationFixture(t *testing.T, admin bool) *completedSpeechFixture {
	t.Helper()
	f, access, _ := speechPluginFixture(t)
	fixture := &completedSpeechFixture{f: f, ctx: t.Context(), audioURL: "https://audio.example/synthetic-completed.wav"}
	if admin {
		f.login()
		request, err := http.NewRequestWithContext(t.Context(), "POST", f.server.URL+"/api/speech/test", nil)
		if err != nil {
			t.Fatal(err)
		}
		baseURL, err := url.Parse(f.server.URL)
		if err != nil {
			t.Fatal(err)
		}
		for _, cookie := range f.client.Jar.Cookies(baseURL) {
			request.AddCookie(cookie)
		}
		server := f.manager.accounts.root
		fixture.sessionID, _, _ = server.session(request)
		if fixture.sessionID == "" {
			t.Fatal("authenticated fixture session missing")
		}
		fixture.ctx = server.speechPluginTestContext(request)
		fixture.policy, err = f.manager.newAdminSpeechPluginPolicy(fixture.ctx, "synthetic completed administrator speech")
		if err != nil {
			t.Fatal(err)
		}
		// Simulate an expiry reported by the session authority. The real cookie
		// lifetime is exercised separately in the websession package.
		checkSession := fixture.policy.admin.Check
		fixture.policy.admin.Check = func(ctx context.Context) error {
			if fixture.sessionExpired.Load() {
				return websession.ErrSession
			}
			return checkSession(ctx)
		}
	} else {
		input := access.trace
		input.Text = "synthetic retained source for completed speech"
		input.Source = &companion.SourceRef{InstanceID: f.manager.memory.SourceInstanceID(), MessageID: input.TraceID, Revision: 1}
		memoryConfig := f.manager.Config().Companion
		memoryConfig.Enabled, memoryConfig.Permissions.Remember = true, true
		if err := f.manager.memory.Observe(input, memoryConfig, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		source, ok := f.manager.memory.SourceForInput(input)
		if !ok || source.EventID == "" {
			t.Fatal("real source fixture was not observed")
		}
		fixture.taskID = agentID("synthetic-speech-revocation-task")
		if err := f.manager.agent.task(agentTask{ID: fixture.taskID, Account: input.Account, Thread: input.Thread, Sender: input.Sender, State: "running", Title: "synthetic speech", Checkpoint: "awaiting audio"}); err != nil {
			t.Fatal(err)
		}
		access.trace.TaskID, access.trace.Source = fixture.taskID, source
		fixture.scope = companion.ScopeID(input.Channel, input.Account, input.Thread)
		var err error
		fixture.policy, err = access.newSpeechPluginPolicy(fixture.ctx, json.RawMessage(`{"text":"synthetic completed conversation speech","timeout_seconds":10}`))
		if err != nil {
			t.Fatal(err)
		}
	}
	policy := fixture.policy
	job, cap := speechPluginJob(policy)
	job.Arguments = policy.arguments
	job, created, err := policy.store.CreateJob(fixture.ctx, job)
	if err != nil || !created {
		t.Fatal("completed speech intent missing", err)
	}
	job, err = policy.store.ClaimJob(fixture.ctx, job.ID, job.Revision)
	if err != nil {
		t.Fatal(err)
	}
	policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		return json.Marshal(map[string]any{"model": policy.model, "output": map[string]any{"audio": map[string]string{"url": fixture.audioURL, "voice": policy.voice}}})
	}
	if _, err := policy.Service(fixture.ctx, job, cap, "wechat/model_request", speechPluginRequest(policy)); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"ok": true, "status": "provider_accepted", "provider_audio_url": fixture.audioURL, "provider_model": policy.model, "voice": policy.voice, "coverage": "provider_tts_response", "untrusted_external_content": true})
	verified, err := policy.Validate(fixture.ctx, job, cap, raw)
	if err != nil {
		t.Fatal(err)
	}
	job, err = policy.store.CompleteJobWithPrivate(fixture.ctx, job.ID, job.Revision, nil, verified.Result, verified.Private)
	if err != nil {
		t.Fatal(err)
	}
	policy.completedJob = job
	if err := policy.checkCompleted(fixture.ctx); err != nil {
		t.Fatal("fresh completion authority invalid", err)
	}
	private, err := policy.store.PrivateJobResult(fixture.ctx, job, func(ctx context.Context, current pluginhost.Job) error {
		return policy.Check(ctx, current, pluginhost.Capability{})
	})
	if err != nil || !bytes.Contains(private, []byte(fixture.audioURL)) {
		t.Fatal("verified completion private checkpoint unavailable", err)
	}
	return fixture
}

func revokeCompletedSpeech(t *testing.T, fixture *completedSpeechFixture, action string) {
	t.Helper()
	m, policy := fixture.f.manager, fixture.policy
	switch action {
	case "package_disabled", "configuration_changed":
		installed, err := policy.store.Installed(t.Context(), "speech")
		if err != nil {
			t.Fatal(err)
		}
		installed, err = policy.store.SetEnabled(t.Context(), "speech", installed.Revision, false)
		if err != nil {
			t.Fatal(err)
		}
		if action == "configuration_changed" {
			config, err := policy.store.Configuration(t.Context(), "speech")
			if err != nil {
				t.Fatal(err)
			}
			config.Values["default_voice"] = json.RawMessage(`"synthetic-reconfigured-voice"`)
			if _, err = policy.store.SaveConfiguration(t.Context(), "speech", pluginhost.ConfigUpdate{Revision: config.Revision, Values: config.Values}); err != nil {
				t.Fatal(err)
			}
			if _, err = policy.store.SetEnabled(t.Context(), "speech", installed.Revision, true); err != nil {
				t.Fatal(err)
			}
		}
	case "conversation_task_cancelled":
		task, ok := m.agent.lookupTask(fixture.taskID, "test", "filehelper")
		if !ok {
			t.Fatal("source task missing")
		}
		task.State = "cancelled"
		if err := m.agent.task(task); err != nil {
			t.Fatal(err)
		}
	case "source_forgotten":
		if err := m.memory.Clear(fixture.scope); err != nil {
			t.Fatal(err)
		}
	case "voice_permission_revoked":
		m.mu.Lock()
		m.config.Companion.Permissions.SendVoice = false
		m.mu.Unlock()
	case "session_revoked":
		if code, _, _ := fixture.f.request("POST", "/api/logout", map[string]any{}, true, true); code != 200 {
			t.Fatal("session logout failed", code)
		}
	case "session_expired":
		fixture.sessionExpired.Store(true)
	case "draining":
		m.mu.Lock()
		m.drainTicket = "synthetic-completed-speech-drain"
		m.mu.Unlock()
	default:
		t.Fatal("unknown fixture revocation", action)
	}
}

func TestSpeechPluginCompletedResultRechecksAuthority(t *testing.T) {
	for _, test := range []struct {
		action string
		admin  bool
	}{
		{"package_disabled", false}, {"configuration_changed", false}, {"conversation_task_cancelled", false}, {"source_forgotten", false}, {"voice_permission_revoked", false},
		{"package_disabled", true}, {"configuration_changed", true}, {"session_revoked", true}, {"draining", true},
	} {
		name := "conversation/" + test.action
		if test.admin {
			name = "administrator/" + test.action
		}
		t.Run(name, func(t *testing.T) {
			fixture := completedSpeechRevocationFixture(t, test.admin)
			revokeCompletedSpeech(t, fixture, test.action)
			if err := fixture.policy.checkCompleted(fixture.ctx); err == nil {
				t.Fatal("revoked completion still consumable", test.action)
			}
			calls := 0
			data, mime, err := fixture.policy.readAudio(fixture.ctx, fixture.audioURL, 8<<20, func(context.Context, string, int64) ([]byte, string, error) {
				calls++
				return []byte("synthetic private audio"), "audio/wav", nil
			})
			if err == nil || calls != 0 || len(data) != 0 || mime != "" {
				t.Fatal("revoked completion reached audio transport", test.action, calls, err)
			}
		})
	}
}

func TestSpeechPluginAudioDownloadRechecksAfterBlockedFetch(t *testing.T) {
	for _, test := range []struct {
		action string
		admin  bool
	}{
		{"package_disabled", false}, {"configuration_changed", false}, {"conversation_task_cancelled", false}, {"source_forgotten", false},
		{"session_revoked", true}, {"session_expired", true}, {"configuration_changed", true},
	} {
		name := "conversation/" + test.action
		if test.admin {
			name = "administrator/" + test.action
		}
		t.Run(name, func(t *testing.T) {
			fixture := completedSpeechRevocationFixture(t, test.admin)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseFetch := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseFetch)
			type downloadResult struct {
				data []byte
				mime string
				err  error
			}
			finished := make(chan downloadResult, 1)
			go func() {
				data, mime, err := fixture.policy.readAudio(fixture.ctx, fixture.audioURL, 12345, func(ctx context.Context, url string, limit int64) ([]byte, string, error) {
					if url != fixture.audioURL || limit != 12345 {
						return nil, "", errors.New("fetch binding changed")
					}
					close(entered)
					select {
					case <-release:
						return []byte("synthetic private audio that must be discarded"), "audio/wav", nil
					case <-ctx.Done():
						return nil, "", ctx.Err()
					}
				})
				finished <- downloadResult{data, mime, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("audio fetch did not start")
			}
			revokeCompletedSpeech(t, fixture, test.action)
			releaseFetch()
			select {
			case result := <-finished:
				if result.err == nil || len(result.data) != 0 || result.mime != "" {
					t.Fatal("revoked download returned consumable bytes", test.action, result.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("audio fetch did not finish")
			}
		})
	}
}

func TestSpeechPluginCompletedAudioControlsAndCancellation(t *testing.T) {
	for _, admin := range []bool{false, true} {
		name := "conversation"
		if admin {
			name = "administrator"
		}
		t.Run(name, func(t *testing.T) {
			fixture := completedSpeechRevocationFixture(t, admin)
			calls := 0
			data, mime, err := fixture.policy.readAudio(fixture.ctx, fixture.audioURL, 1024, func(_ context.Context, url string, limit int64) ([]byte, string, error) {
				calls++
				if url != fixture.audioURL || limit != 1024 {
					return nil, "", errors.New("fetch binding changed")
				}
				return []byte("synthetic audio"), "audio/wav", nil
			})
			if err != nil || calls != 1 || string(data) != "synthetic audio" || mime != "audio/wav" {
				t.Fatal("authorized download control failed", calls, err)
			}
			ctx, cancel := context.WithCancel(fixture.ctx)
			cancel()
			data, mime, err = fixture.policy.readAudio(ctx, fixture.audioURL, 1024, func(context.Context, string, int64) ([]byte, string, error) {
				t.Fatal("cancelled context reached fetch")
				return nil, "", nil
			})
			if !errors.Is(err, context.Canceled) || len(data) != 0 || mime != "" {
				t.Fatal("cancelled download remained usable", err)
			}
			fixture.policy.completedJob.Revision--
			if err := fixture.policy.checkCompleted(fixture.ctx); err == nil {
				t.Fatal("stale durable completion revision was accepted")
			}
			fixture.policy.completedJob = pluginhost.Job{}
			if err := fixture.policy.checkCompleted(fixture.ctx); err == nil {
				t.Fatal("missing durable completion was accepted")
			}
		})
	}
}
