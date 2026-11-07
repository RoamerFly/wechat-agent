package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func songAudioFixture(t *testing.T, configured, remember bool) (*webFixture, *permittedTools, []byte, *songSpec, *musicAudioOrigin) {
	t.Helper()
	var f *webFixture
	if configured {
		f, _, _ = audioObservationFixture(t)
	} else {
		f = setup(t)
	}
	f.manager.config.Enabled = true
	f.manager.config.Companion.Enabled = true
	f.manager.config.Companion.Permissions.SendFile = true
	f.manager.config.Companion.Permissions.SendVoice = true
	f.manager.config.Companion.Permissions.Remember = remember
	p, _, err := f.manager.toolAccess()
	if err != nil {
		t.Fatal(err)
	}
	in := companion.Input{Channel: "wechat", Account: "test", Thread: "filehelper", Sender: "synthetic-song-requester", TraceID: "synthetic-song-result", Text: "请交付原任务的成品"}
	if remember {
		in.Source = &companion.SourceRef{MessageID: in.TraceID}
		if err = f.manager.memory.Observe(in, f.manager.Config().Companion, time.Now()); err != nil {
			t.Fatal(err)
		}
		var ok bool
		in.Source, ok = f.manager.memory.SourceForInput(in)
		if !ok {
			t.Fatal("missing canonical music source")
		}
	}
	p.SetTrace(in)
	spec := &songSpec{Mode: "original", Engine: "yue", VocalGender: "female", SingingVoice: "character", Duration: 4, Lyrics: "synthetic lyrics"}
	task := agentTask{ID: "synthetic-song-task", Account: in.Account, Thread: in.Thread, Sender: in.Sender, State: "running", MusicJob: "synthetic-song-job", MusicTitle: "晚风轻唱", MusicKind: "file", MusicSpec: spec, Constraints: p.taskConstraints("music", "", "", "")}
	if err = f.manager.agent.task(task); err != nil {
		t.Fatal(err)
	}
	reopened, err := newAgentControl(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.agent = reopened
	p.trace.TaskID = task.ID
	audio := syntheticReferenceWAV(t, 4)
	origin, err := p.newMusicAudioOrigin(t.Context(), audio, "female", task.MusicJob, spec, "file")
	if err != nil {
		t.Fatal(err)
	}
	return f, p, audio, spec, origin
}

func completeSongAudioObservation(t *testing.T, origin *musicAudioOrigin, wav []byte, windows []musicAudioWindow, status string) (*mediaAudioObservationPolicy, int) {
	t.Helper()
	policy, err := origin.observationPolicy(t.Context(), wav, windows)
	if err != nil {
		t.Fatal(err)
	}
	job, cap := audioObservationJob(policy)
	job.Arguments = policy.arguments
	job, _, err = policy.store.CreateJob(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	job, err = policy.store.ClaimJob(t.Context(), job.ID, job.Revision)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		finish := "stop"
		if status == "partial" {
			finish = "length"
		}
		return json.Marshal(map[string]any{"model": "synthetic-song-provider-model", "choices": []any{map[string]any{"message": map[string]any{"content": "synthetic sampled song opinion"}, "finish_reason": finish}}})
	}
	if _, err = policy.Service(t.Context(), job, cap, "wechat/model_request", audioObservationRequest(policy)); err != nil {
		t.Fatal(err)
	}
	verified, err := policy.Validate(t.Context(), job, cap, audioObservationResult(policy, status, "synthetic sampled song opinion"))
	if err != nil {
		t.Fatal(err)
	}
	policy.completedJob, err = policy.store.CompleteJobWithPrivate(t.Context(), job.ID, job.Revision, nil, verified.Result, verified.Private)
	if err != nil {
		t.Fatal(err)
	}
	if err = policy.checkCompleted(t.Context()); err != nil {
		t.Fatal(err)
	}
	return policy, calls
}

type songAudioNoReplayExecutor struct {
	calls   int
	failure bool
}

func (e *songAudioNoReplayExecutor) Start(context.Context, pluginhost.Package, pluginhost.Grant) (pluginhost.Session, error) {
	return e, nil
}
func (*songAudioNoReplayExecutor) Health(context.Context) error { return nil }
func (*songAudioNoReplayExecutor) Close() error                 { return nil }
func (e *songAudioNoReplayExecutor) Call(context.Context, pluginhost.Invocation, string, json.RawMessage) (json.RawMessage, error) {
	e.calls++
	if e.failure {
		return json.RawMessage(`{"ok":false,"error":"synthetic_provider_failed"}`), nil
	}
	return nil, errors.New("completed song observation must not replay")
}

func TestMusicAudioOriginBindsTaskAudioSamplesAndOriginalVoice(t *testing.T) {
	_, p, audio, spec, origin := songAudioFixture(t, true, false)
	if p.privileged() {
		t.Fatal("test must cover original non-owner requester")
	}
	wav, windows, err := prepareSongAudioSamples(t.Context(), p.manager.dir, audio)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := origin.observationPolicy(t.Context(), wav, windows)
	if err != nil {
		t.Fatal(err)
	}
	if policy.invocation().Requester != p.trace.Sender || policy.invocation().Thread != p.trace.Thread || policy.invocation().Purpose != "observe generated song samples" || policy.timeoutSeconds != 90 || policy.ref.SHA == origin.audioHash {
		t.Fatal("generated song was mistaken for an incoming/asset origin", policy.invocation())
	}
	modified := append([]byte(nil), audio...)
	modified[len(modified)-1] ^= 1
	other, err := p.newMusicAudioOrigin(t.Context(), modified, "female", origin.task.MusicJob, spec, "file")
	if err != nil {
		t.Fatal(err)
	}
	otherPolicy, err := other.observationPolicy(t.Context(), wav, windows)
	if err != nil || otherPolicy.invocation().RequestKey == policy.invocation().RequestKey {
		t.Fatal("different generated recording reused listening job", err)
	}
	changedWindows := append([]musicAudioWindow(nil), windows...)
	changedWindows[0].Offset += 0.01
	otherPolicy, err = origin.observationPolicy(t.Context(), wav, changedWindows)
	if err != nil || otherPolicy.invocation().RequestKey == policy.invocation().RequestKey {
		t.Fatal("different sample window reused listening job", err)
	}
	changedWAV := append([]byte(nil), wav...)
	changedWAV[len(changedWAV)-1] ^= 1
	otherPolicy, err = origin.observationPolicy(t.Context(), changedWAV, windows)
	if err != nil || otherPolicy.invocation().RequestKey == policy.invocation().RequestKey {
		t.Fatal("different sample WAV reused listening job", err)
	}
	if err = p.bindSelectedVoice("other-voice", "other-profile"); err != nil {
		t.Fatal(err)
	}
	if err = origin.Check(t.Context()); err == nil {
		t.Fatal("original task voice replaced during listening")
	}
}

func TestMusicAudioOriginRechecksRequesterScopeTaskSourceAndPermission(t *testing.T) {
	for _, change := range []string{"account", "thread", "requester", "task_id", "cancel", "tasks_disabled", "companion_disabled", "voice_permission", "file_permission", "source_forgotten", "voice_selection", "recipe"} {
		t.Run(change, func(t *testing.T) {
			f, p, _, _, origin := songAudioFixture(t, false, true)
			switch change {
			case "account":
				p.trace.Account = "other"
			case "thread":
				p.trace.Thread = "other"
			case "requester":
				p.trace.Sender = "other"
			case "task_id":
				p.trace.TaskID = "other-task"
			case "cancel":
				task, _ := f.manager.agent.lookupTask(origin.task.ID, origin.task.Account, origin.task.Thread)
				task.State = "cancelled"
				if err := f.manager.agent.task(task); err != nil {
					t.Fatal(err)
				}
			case "tasks_disabled":
				settings := f.manager.agent.config()
				settings.TasksEnabled = false
				if err := f.manager.agent.save(settings); err != nil {
					t.Fatal(err)
				}
			case "companion_disabled":
				f.manager.config.Companion.Enabled = false
			case "voice_permission":
				f.manager.config.Companion.Permissions.SendVoice = false
			case "file_permission":
				f.manager.config.Companion.Permissions.SendFile = false
			case "source_forgotten":
				if err := f.manager.memory.Clear(companion.ScopeID("wechat", "test", "filehelper")); err != nil {
					t.Fatal(err)
				}
			case "voice_selection":
				if err := p.bindSelectedVoice("other", ""); err != nil {
					t.Fatal(err)
				}
			case "recipe":
				task, _ := f.manager.agent.lookupTask(origin.task.ID, origin.task.Account, origin.task.Thread)
				task.MusicSpec.VocalGender = "male"
				if err := f.manager.agent.task(task); err != nil {
					t.Fatal(err)
				}
			}
			if err := origin.Check(t.Context()); err == nil {
				t.Fatal("revoked generated song origin accepted", change)
			}
		})
	}
}

func TestMusicAudioOriginPersistentTaskUsesExistingSchedulingRules(t *testing.T) {
	f, p, audio, spec, origin := songAudioFixture(t, false, false)
	f.manager.config.Enabled = false
	if err := origin.Check(t.Context()); err != nil {
		t.Fatal("reactive bot switch overruled an already-authorized persistent task", err)
	}
	settings := f.manager.agent.config()
	settings.TasksEnabled = false
	if err := f.manager.agent.save(settings); err != nil {
		t.Fatal(err)
	}
	f.manager.config.Companion.Enabled = false
	if err := origin.Check(t.Context()); err == nil {
		t.Fatal("persistent continuation ignored scheduler shutdown")
	}
	p.trace.TaskID = ""
	p.trace.TraceID = "synthetic-explicit-scoped-read"
	manual, err := p.newMusicAudioOrigin(t.Context(), audio, "female", origin.task.MusicJob, spec, "file")
	if err != nil || manual.Check(t.Context()) != nil {
		t.Fatal("explicit authorized task read acquired an unrelated scheduler prerequisite", err)
	}
}

func TestMusicAudioOriginSchedulerRevocationDuringProviderAttempt(t *testing.T) {
	for _, revoke := range []string{"tasks_disabled", "companion_disabled"} {
		t.Run(revoke, func(t *testing.T) {
			f, _, audio, _, origin := songAudioFixture(t, true, false)
			wav, windows, err := prepareSongAudioSamples(t.Context(), f.dir, audio)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := origin.observationPolicy(t.Context(), wav, windows)
			if err != nil {
				t.Fatal(err)
			}
			job, cap := audioObservationJob(policy)
			calls := 0
			policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
				calls++
				if revoke == "tasks_disabled" {
					settings := f.manager.agent.config()
					settings.TasksEnabled = false
					if err := f.manager.agent.save(settings); err != nil {
						t.Fatal(err)
					}
				} else {
					f.manager.config.Companion.Enabled = false
				}
				return json.RawMessage(`{"choices":[{"message":{"content":"synthetic in-flight opinion"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`), nil
			}
			if _, err = policy.Service(t.Context(), job, cap, "wechat/model_request", audioObservationRequest(policy)); err == nil {
				t.Fatal("scheduler revocation retained in-flight observation", revoke)
			}
			if _, err = policy.Validate(t.Context(), job, cap, audioObservationResult(policy, "observed", "synthetic in-flight opinion")); err == nil {
				t.Fatal("scheduler-revoked candidate was verified", revoke)
			}
			if _, err = policy.Service(t.Context(), job, cap, "wechat/model_request", audioObservationRequest(policy)); err == nil || calls != 1 {
				t.Fatal("revoked observation replayed provider", err, calls)
			}
			usage := f.manager.billing.PurposeTotals("plugin_media_audio_observation")
			if usage.Requests != 1 || usage.Reported != 1 || usage.Input != 3 || usage.Output != 2 {
				t.Fatal("revocation lost actual completed HTTP attempt usage", usage)
			}
		})
	}
}

func TestMusicAudioOriginRequiresMatchingPersistedTaskButNotRemember(t *testing.T) {
	f, p, audio, spec, origin := songAudioFixture(t, false, false)
	if err := origin.Check(t.Context()); err != nil {
		t.Fatal("remember-off task lost existing authorization", err)
	}
	if _, err := p.newMusicAudioOrigin(t.Context(), audio, "female", "unknown-song-job", spec, "file"); err == nil {
		t.Fatal("arbitrary job ID acquired audio-review authority")
	}
	changed := *spec
	changed.VocalGender = "male"
	if _, err := p.newMusicAudioOrigin(t.Context(), audio, "male", origin.task.MusicJob, &changed, "file"); err == nil {
		t.Fatal("recipe/gender substitution accepted")
	}
	p.trace.TaskID = ""
	p.trace.Sender = "other-requester"
	if _, err := p.newMusicAudioOrigin(t.Context(), audio, "female", origin.task.MusicJob, spec, "file"); err == nil {
		t.Fatal("another requester read a private generated song")
	}
	settings := f.manager.agent.config()
	settings.OwnerIDs = []string{p.trace.Sender}
	settings.PrivilegedThreads = []string{p.trace.Thread}
	if err := f.manager.agent.save(settings); err != nil {
		t.Fatal(err)
	}
	owner, err := p.newMusicAudioOrigin(t.Context(), audio, "female", origin.task.MusicJob, spec, "file")
	if err != nil {
		t.Fatal("current trusted owner could not inspect scoped task", err)
	}
	settings = f.manager.agent.config()
	settings.OwnerIDs = nil
	if err = f.manager.agent.save(settings); err != nil {
		t.Fatal(err)
	}
	if err = owner.Check(t.Context()); err == nil {
		t.Fatal("revoked owner kept another requester's song authority")
	}
}

func TestMusicAudioOriginWithoutGenderStillRequiresTaskAndOriginalVoiceProof(t *testing.T) {
	f, p, audio, _, origin := songAudioFixture(t, false, false)
	task, _ := f.manager.agent.lookupTask(origin.task.ID, origin.task.Account, origin.task.Thread)
	task.MusicSpec = nil
	if err := f.manager.agent.task(task); err != nil {
		t.Fatal(err)
	}
	guard, err := p.newMusicAudioOrigin(t.Context(), audio, "", task.MusicJob, nil, "file")
	if err != nil || guard.Check(t.Context()) != nil {
		t.Fatal("legacy task without gender lost scoped delivery", err)
	}
	f.manager.config.Companion.Permissions.Remember = true
	if err = guard.Check(t.Context()); err != nil {
		t.Fatal("memory switch incorrectly governed an existing music task", err)
	}
	other := agentTask{ID: "named-incomplete-task", Account: task.Account, Thread: task.Thread, Sender: task.Sender, State: "running", MusicJob: "named-incomplete-job", MusicKind: "file", Constraints: p.taskConstraints("music", "named-singer", "", "")}
	if err = f.manager.agent.task(other); err != nil {
		t.Fatal(err)
	}
	p.trace.TaskID = other.ID
	if _, err = p.newMusicAudioOrigin(t.Context(), audio, "", other.MusicJob, nil, "file"); err == nil {
		t.Fatal("missing recipe discarded a persisted named singing voice")
	}
}

func TestMusicAudioOriginRetainedSourceRemainsRequiredWithRememberOff(t *testing.T) {
	f, _, _, _, origin := songAudioFixture(t, false, true)
	f.manager.config.Companion.Permissions.Remember = false
	if err := origin.Check(t.Context()); err != nil {
		t.Fatal("turning off new memory retention revoked a still-authorized task", err)
	}
	if err := f.manager.memory.Clear(companion.ScopeID("wechat", origin.task.Account, origin.task.Thread)); err != nil {
		t.Fatal(err)
	}
	if err := origin.Check(t.Context()); err == nil {
		t.Fatal("remember-off bypassed forgetting the task's original canonical source")
	}
}

func TestSongAudioReviewReusesOnlyPrivateVerifiedCheckpointAndPreservesPartial(t *testing.T) {
	for _, status := range []string{"observed", "partial"} {
		t.Run(status, func(t *testing.T) {
			f, p, audio, spec, origin := songAudioFixture(t, true, false)
			wav, windows, err := prepareSongAudioSamples(t.Context(), f.dir, audio)
			if err != nil {
				t.Fatal(err)
			}
			policy, calls := completeSongAudioObservation(t, origin, wav, windows, status)
			executor := &songAudioNoReplayExecutor{}
			controller, err := pluginruntime.NewController(policy.store, executor)
			if err != nil {
				t.Fatal(err)
			}
			f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
			t.Cleanup(func() { _ = controller.Close() })
			for i := 0; i < 2; i++ {
				review, check := p.reviewSongAudio(t.Context(), audio, "female", origin.task.MusicJob, spec, "file")
				if review["state"] != status || review["observation"] != "synthetic sampled song opinion" || review["model"] != "synthetic-audio-model" || review["provider_model"] != "synthetic-song-provider-model" || review["voice_identity_evidence"] != "model_opinion_only" || check == nil || check(t.Context()) != nil {
					t.Fatal("private checkpoint was not safely reused", review)
				}
				if status == "partial" && !strings.Contains(review["instruction"].(string), "截断") {
					t.Fatal("partial listening result presented as complete")
				}
			}
			if executor.calls != 0 || calls != 1 {
				t.Fatal("completed listening checkpoint replayed model", executor.calls, calls)
			}
			f.manager.config.Companion.Permissions.SendFile = false
			if err = policy.checkCompleted(t.Context()); err == nil {
				t.Fatal("verified checkpoint outlived delivery permission")
			}
		})
	}
}

func TestSongAudioReviewAuthorityGuardsCoreUpload(t *testing.T) {
	f, p, audio, spec, origin := songAudioFixture(t, false, false)
	native := &outputAuthorityNative{fakeTools: &fakeTools{fakeNative: f.native}}
	f.manager.native = native
	_, check := p.reviewSongAudio(t.Context(), audio, "female", origin.task.MusicJob, spec, "file")
	native.afterUpload = func(int) {
		task, _ := f.manager.agent.lookupTask(origin.task.ID, origin.task.Account, origin.task.Thread)
		task.State = "cancelled"
		if err := f.manager.agent.task(task); err != nil {
			t.Fatal(err)
		}
	}
	result, err := p.stageSongOutputSpec(withOutputAuthority(t.Context(), check), audio, "audio/wav", "晚风轻唱", "file", spec)
	if err == nil || result != "" || native.uploads != 1 {
		t.Fatal("cancelled song became prepared media", result, err, native.uploads)
	}
	assertOutputAuthorityDidNotPublish(t, f, p, native)
}

func TestSongAudioReviewCannotReusePrivateObservationAfterConfigurationChange(t *testing.T) {
	f, p, audio, spec, origin := songAudioFixture(t, true, false)
	wav, windows, err := prepareSongAudioSamples(t.Context(), f.dir, audio)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := completeSongAudioObservation(t, origin, wav, windows, "observed")
	installed, _ := old.store.Installed(t.Context(), "media-understanding")
	if _, err = old.store.SetEnabled(t.Context(), installed.Package.ID, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	if _, err = old.store.SaveConfiguration(t.Context(), installed.Package.ID, pluginhost.ConfigUpdate{Revision: old.configRevision, Values: map[string]json.RawMessage{"audio_review_endpoint": json.RawMessage(`"https://audio-provider.example/compatible-mode/v1/chat/completions"`), "audio_review_model": json.RawMessage(`"synthetic-new-model"`), "max_tokens": json.RawMessage(`1120`)}}); err != nil {
		t.Fatal(err)
	}
	installed, _ = old.store.Installed(t.Context(), installed.Package.ID)
	if _, err = old.store.SetEnabled(t.Context(), installed.Package.ID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	executor := &songAudioNoReplayExecutor{}
	controller, err := pluginruntime.NewController(old.store, executor)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	t.Cleanup(func() { _ = controller.Close() })
	review, check := p.reviewSongAudio(t.Context(), audio, "female", origin.task.MusicJob, spec, "file")
	if review["state"] != "failed" || review["observation"] != nil || review["model"] != "synthetic-new-model" || executor.calls != 1 || check == nil || check(t.Context()) != nil {
		t.Fatal("changed configuration reused old listening evidence", review, executor.calls)
	}
	if err = old.checkCompleted(t.Context()); err == nil {
		t.Fatal("old configuration checkpoint stayed actionable")
	}
}

func TestSongAudioReviewOrdinaryProviderFailureKeepsOriginalTaskProof(t *testing.T) {
	f, p, audio, spec, origin := songAudioFixture(t, true, false)
	store, _ := f.manager.packageStore()
	executor := &songAudioNoReplayExecutor{failure: true}
	controller, err := pluginruntime.NewController(store, executor)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	t.Cleanup(func() { _ = controller.Close() })
	review, check := p.reviewSongAudio(t.Context(), audio, "female", origin.task.MusicJob, spec, "file")
	if review["state"] != "failed" || check == nil || check(t.Context()) != nil || executor.calls != 1 {
		t.Fatal("provider failure changed original task authorization", review, executor.calls)
	}
	if spec.VocalGender != "female" || spec.SingingVoice != "character" {
		t.Fatal("listening failure substituted another singing voice")
	}
}

func TestSongAudioReviewYuePreservesUpstreamOutputAuthority(t *testing.T) {
	f, p, audio, _, origin := songAudioFixture(t, false, false)
	task, _ := f.manager.agent.lookupTask(origin.task.ID, origin.task.Account, origin.task.Thread)
	task.RecoveryMessageID = "synthetic-adopted-request"
	if err := f.manager.agent.task(task); err != nil {
		t.Fatal(err)
	}
	native := &outputAuthorityNative{fakeTools: &fakeTools{fakeNative: f.native}}
	f.manager.native = native
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("song observation resubmitted generation")
			w.WriteHeader(500)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/audio") {
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(audio)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": origin.task.MusicJob, "state": "completed"})
	}))
	defer server.Close()
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.YuEEnabled, legacySpeechFixture.YuEEndpoint, legacySpeechFixture.YuEToken = true, server.URL, "synthetic-local-only"
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	installLegacyLocalMusicPlugin(t, f.manager)
	blocked := errors.New("synthetic upstream output authority revoked")
	ctx := withOutputAuthority(t.Context(), func(context.Context) error { return blocked })
	raw, _ := json.Marshal(map[string]any{"job_id": origin.task.MusicJob, "kind": "file"})
	result, err := p.callYuE(ctx, "music_local_status", raw)
	if !errors.Is(err, blocked) || result != "" || native.uploads != 0 {
		t.Fatal("song listening guard replaced upstream authority", result, err, native.uploads)
	}
	assertOutputAuthorityDidNotPublish(t, f, p, native)
}

func TestLiveGeneratedSongAudioOriginPrivateCheckpointAndCancellation(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in isolated generated-song observation with fake provider")
	}
	python, _ = filepath.Abs(python)
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	f, p, audio, _, origin := songAudioFixture(t, true, true)
	wav, windows, err := prepareSongAudioSamples(t.Context(), f.dir, audio)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := origin.observationPolicy(t.Context(), wav, windows)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := pluginruntime.NewController(policy.store, pluginruntime.InstalledExecutor(policy.store, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
	if err != nil {
		t.Fatal(err)
	}
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	t.Cleanup(func() { _ = controller.Close() })
	calls := 0
	policy.post = func(_ context.Context, _ string, _ map[string]string, body json.RawMessage, _ int64, timeout time.Duration) (json.RawMessage, error) {
		calls++
		if !sameShareBody(body, policy.requestBody(true)) || timeout != 90*time.Second {
			t.Fatal("generated song samples changed before provider")
		}
		return json.RawMessage(`{"choices":[{"message":{"content":"synthetic generated song observation"},"finish_reason":"stop"}]}`), nil
	}
	result, err := policy.observe(t.Context())
	if err != nil || calls != 1 || result.VoiceEvidence != "model_opinion_only" {
		t.Fatal("isolated generated song observation failed", result, err, calls)
	}
	task, _ := f.manager.agent.lookupTask(origin.task.ID, p.trace.Account, p.trace.Thread)
	task.State = "cancelled"
	if err = f.manager.agent.task(task); err != nil {
		t.Fatal(err)
	}
	if err = policy.checkCompleted(t.Context()); err == nil {
		t.Fatal("completed generated-song observation bypassed cancellation")
	}
}
