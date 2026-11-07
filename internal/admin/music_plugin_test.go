package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

const musicPluginArguments = `{"title":"晚风轻唱","prompt":"a synthetic song","lyrics":"synthetic lyrics","kind":"voice","timeout_seconds":7200}`

func installMusicPluginFixture(t *testing.T, f *webFixture, permissions []string, enable bool) *pluginhost.Store {
	t.Helper()
	s, err := f.manager.packageStore()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Prepare(t.Context(), "../../plugins/music")
	if err != nil {
		t.Fatal(err)
	}
	// Enabling the combined official package requires approval for its local
	// bridge as well as its cloud capability. The disabled negative fixture
	// deliberately retains its missing grant.
	if enable {
		permissions = append(append([]string(nil), permissions...), "network")
	}
	i, err := s.Install(t.Context(), prepared.Token, prepared.Digest, permissions)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveConfiguration(t.Context(), "music", pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{"music_endpoint": json.RawMessage(`"https://provider.example/api/v1/services/audio/music/generation"`), "music_model": json.RawMessage(`"synthetic-music"`)}, Secrets: map[string]string{"music_key": "synthetic-private-key"}})
	if err != nil {
		t.Fatal(err)
	}
	if enable {
		if _, err = s.SetEnabled(t.Context(), "music", i.Revision, true); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func musicPluginFixture(t *testing.T) (*webFixture, *permittedTools, *musicPluginPolicy) {
	t.Helper()
	f := setup(t)
	installMusicPluginFixture(t, f, []string{"model.invoke"}, true)
	f.manager.config.Enabled = true
	f.manager.config.Companion.Permissions.SendVoice = true
	f.manager.config.Companion.Permissions.SendFile = true
	f.manager.config.Companion.Permissions.Remember = true
	p := &permittedTools{manager: f.manager, trace: companion.Input{Channel: "wechat", Account: "test", Thread: "filehelper", Sender: "synthetic-user", TraceID: "synthetic-music", Text: "创作一首原创歌", Source: &companion.SourceRef{MessageID: "synthetic-music"}}}
	if err := f.manager.memory.Observe(p.trace, f.manager.config.Companion, time.Now()); err != nil {
		t.Fatal(err)
	}
	source, ok := f.manager.memory.SourceForInput(p.trace)
	if !ok || source.EventID == "" {
		t.Fatal("synthetic music source was not retained")
	}
	p.trace.Source = source
	policy, err := p.newMusicPluginPolicy(t.Context(), json.RawMessage(musicPluginArguments))
	if err != nil {
		t.Fatal(err)
	}
	return f, p, policy
}

func musicPluginJob(p *musicPluginPolicy) (pluginhost.Job, pluginhost.Capability) {
	return pluginhost.Job{Plugin: "music", Version: p.version, PackageDigest: p.digest, ConfigRevision: strconv.Itoa(p.configRevision), Tool: "music.generate", Context: p.invocation}, pluginhost.Capability{ID: "music.generate", Implementation: "ready", Permissions: []string{"model.invoke"}}
}

func musicPluginRequest(p *musicPluginPolicy) json.RawMessage {
	input := map[string]any{"prompt": p.prompt, "gender": "female", "format": "mp3"}
	if p.lyrics != "" {
		input["lyrics"] = p.lyrics
	}
	b, _ := json.Marshal(map[string]any{"route": "generate", "body": map[string]any{"model": p.model, "input": input}, "max_bytes": 1 << 20, "timeout_seconds": p.timeout})
	return b
}

func musicPluginOutput(url, model string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"ok": true, "status": "provider_accepted", "provider_audio_url": url, "provider_model": model, "coverage": "provider_music_response", "untrusted_external_content": true})
	return b
}

func TestMusicPluginBindsSingleProviderAttemptAndPrivateProvenance(t *testing.T) {
	f, _, p := musicPluginFixture(t)
	j, cap := musicPluginJob(p)
	calls := 0
	p.post = func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
		calls++
		expected := json.RawMessage(`{"model":"synthetic-music","input":{"prompt":"a synthetic song","lyrics":"synthetic lyrics","gender":"female","format":"mp3"}}`)
		if endpoint != "https://provider.example/api/v1/services/audio/music/generation" || headers["Authorization"] != "Bearer synthetic-private-key" || !sameJSON(body, expected) || limit != 1<<20 || timeout != 7200*time.Second {
			t.Fatal("unbound music transport", endpoint, string(body), limit, timeout)
		}
		return json.RawMessage(`{"model":"provider-music-revision","output":{"audio":{"url":"https://audio.example/private.mp3?token=synthetic"}},"usage":{"prompt_tokens":7,"completion_tokens":3}}`), nil
	}
	request := musicPluginRequest(p)
	for _, change := range [][2]string{{"synthetic-music", "wrong-model"}, {"a synthetic song", "wrong prompt"}, {"synthetic lyrics", "wrong lyrics"}, {"female", "male"}, {"mp3", "wav"}, {"7200", "180"}, {"1048576", "4096"}, {"generate", "synthesize"}} {
		bad := bytes.Replace(request, []byte(change[0]), []byte(change[1]), 1)
		if _, err := p.Service(t.Context(), j, cap, "wechat/model_request", bad); err == nil || calls != 0 {
			t.Fatal("changed provider request reached transport", change, err, calls)
		}
	}
	if _, err := p.Service(t.Context(), j, cap, "wechat/model_request", request); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Service(t.Context(), j, cap, "wechat/model_request", request); err == nil || calls != 1 {
		t.Fatal("music provider attempt repeated", err, calls)
	}
	verified, err := p.Validate(t.Context(), j, cap, musicPluginOutput("https://audio.example/private.mp3?token=synthetic", "provider-music-revision"))
	if err != nil || bytes.Contains(verified.Result, []byte("token=synthetic")) || !bytes.Contains(verified.Private, []byte("token=synthetic")) || !bytes.Contains(verified.Result, []byte(`"requested_model":"synthetic-music"`)) || !bytes.Contains(verified.Result, []byte(`"provider_model":"provider-music-revision"`)) || !bytes.Contains(verified.Result, []byte(`"artifact_verified":false`)) || !bytes.Contains(verified.Result, []byte(`"wechat_sends":0`)) {
		t.Fatal("provider acceptance/provenance is not private and bounded", err, string(verified.Result), string(verified.Private))
	}
	u := f.manager.billing.PurposeTotals("plugin_music_generation")
	if u.Requests != 1 || u.Reported != 1 || u.Input != 7 || u.Output != 3 {
		t.Fatal("actual transport usage lost", u)
	}
}

func TestMusicPluginRejectsFabricatedProviderResult(t *testing.T) {
	for _, test := range []struct{ name, provider, output string }{
		{"no_request", "", string(musicPluginOutput("https://audio.example/a.mp3", "synthetic-music"))},
		{"wrong_url", `{"model":"synthetic-music","output":{"audio":{"url":"https://audio.example/a.mp3"}}}`, string(musicPluginOutput("https://audio.example/forged.mp3", "synthetic-music"))},
		{"wrong_model", `{"model":"synthetic-music","output":{"audio":{"url":"https://audio.example/a.mp3"}}}`, string(musicPluginOutput("https://audio.example/a.mp3", "forged-model"))},
		{"missing_provider_model", `{"output":{"audio":{"url":"https://audio.example/a.mp3"}}}`, string(musicPluginOutput("https://audio.example/a.mp3", "synthetic-music"))},
		{"unsafe_url", `{"model":"synthetic-music","output":{"audio":{"url":"file:///private.mp3"}}}`, string(musicPluginOutput("file:///private.mp3", "synthetic-music"))},
		{"duplicate_provider", `{"model":"synthetic-music","model":"other","output":{"audio":{"url":"https://audio.example/a.mp3"}}}`, string(musicPluginOutput("https://audio.example/a.mp3", "other"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, p := musicPluginFixture(t)
			j, cap := musicPluginJob(p)
			if test.provider != "" {
				p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
					return json.RawMessage(test.provider), nil
				}
				if _, err := p.Service(t.Context(), j, cap, "wechat/model_request", musicPluginRequest(p)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.Validate(t.Context(), j, cap, json.RawMessage(test.output)); err == nil {
				t.Fatal("fabricated music result accepted")
			}
		})
	}
}

func TestMusicPluginUnknownAttemptUsageAndNoRetry(t *testing.T) {
	f, _, p := musicPluginFixture(t)
	j, cap := musicPluginJob(p)
	calls := 0
	p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		return nil, errors.New("synthetic uncertain transport")
	}
	for n := 0; n < 2; n++ {
		if _, err := p.Service(t.Context(), j, cap, "wechat/model_request", musicPluginRequest(p)); err == nil {
			t.Fatal("uncertain provider request succeeded")
		}
	}
	if calls != 1 || f.manager.billing.PurposeTotals("plugin_music_generation").Requests != 1 || f.manager.billing.PurposeTotals("plugin_music_generation").Reported != 0 {
		t.Fatal("unknown usage became zero/replayed", calls, f.manager.billing.Totals())
	}
	if _, err := p.Validate(t.Context(), j, cap, musicPluginOutput("https://audio.example/a.mp3", "synthetic-music")); err == nil {
		t.Fatal("transport failure created verified result")
	}
}

func TestMusicPluginRechecksCurrentAuthorization(t *testing.T) {
	for _, mutation := range []string{"account", "thread", "requester", "source", "voice_permission", "config", "disabled", "cancelled_context", "epoch", "selected_voice", "wrong_package"} {
		t.Run(mutation, func(t *testing.T) {
			f, access, p := musicPluginFixture(t)
			j, cap := musicPluginJob(p)
			ctx := t.Context()
			switch mutation {
			case "account":
				access.trace.Account = "other-account"
			case "thread":
				access.trace.Thread = "other-thread"
			case "requester":
				access.trace.Sender = "other-user"
			case "source":
				access.trace.Source = &companion.SourceRef{EventID: "invented", Revision: 1}
			case "voice_permission":
				f.manager.config.Companion.Permissions.SendVoice = false
			case "config", "disabled":
				i, _ := p.store.Installed(ctx, "music")
				if _, err := p.store.SetEnabled(ctx, "music", i.Revision, false); err != nil {
					t.Fatal(err)
				}
				if mutation == "config" {
					if _, err := p.store.SaveConfiguration(ctx, "music", pluginhost.ConfigUpdate{Revision: p.configRevision, Values: map[string]json.RawMessage{"music_endpoint": json.RawMessage(`"https://provider.example/api/v1/services/audio/music/generation"`), "music_model": json.RawMessage(`"changed-model"`)}}); err != nil {
						t.Fatal(err)
					}
				}
			case "cancelled_context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "epoch":
				if err := f.manager.memory.Clear(companion.ScopeID("wechat", "test", "filehelper")); err != nil {
					t.Fatal(err)
				}
			case "selected_voice":
				if err := access.bindSelectedVoice("named-singer", "named-profile"); err != nil {
					t.Fatal(err)
				}
			case "wrong_package":
				j.PackageDigest = strings.Repeat("f", 64)
			}
			calls := 0
			p.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
				calls++
				return nil, nil
			}
			if _, err := p.Service(ctx, j, cap, "wechat/model_request", musicPluginRequest(p)); err == nil || calls != 0 {
				t.Fatal("revoked context reached provider", mutation, err, calls)
			}
			if _, err := p.Validate(ctx, j, cap, musicPluginOutput("https://audio.example/a.mp3", "synthetic-music")); err == nil {
				t.Fatal("revoked context validated output")
			}
		})
	}
}

func TestMusicPluginDownloadRequiresActualAudio(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	for _, invalid := range [][]byte{nil, []byte("<html>provider failed</html>"), []byte("synthetic invalid audio")} {
		if _, err := verifyMusicPluginAudio(t.Context(), dir, invalid); err == nil {
			t.Fatal("non-audio became a song artifact")
		}
	}
	if mime, err := verifyMusicPluginAudio(t.Context(), dir, syntheticReferenceWAV(t, 1)); err != nil || mime != "audio/wav" {
		t.Fatal("valid downloaded audio rejected", mime, err)
	}
}

func TestMusicPluginJobCancellationAndTaskVoiceConstraints(t *testing.T) {
	f, access, p := musicPluginFixture(t)
	j, cap := musicPluginJob(p)
	j.Arguments = p.arguments
	j, _, err := p.store.CreateJob(t.Context(), j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.store.RequestJobCancel(t.Context(), j.ID, j.Revision); err != nil {
		t.Fatal(err)
	}
	if err = p.Check(t.Context(), j, cap); err == nil {
		t.Fatal("cancelled stored job accepted")
	}
	task := agentTask{ID: "synthetic-music-task", Account: access.trace.Account, Thread: access.trace.Thread, Sender: access.trace.Sender, State: "running", Constraints: access.taskConstraints("music", "named-voice", "", "")}
	if err = f.manager.agent.task(task); err != nil {
		t.Fatal(err)
	}
	access.trace.TaskID = task.ID
	if _, err = access.newMusicPluginPolicy(t.Context(), json.RawMessage(musicPluginArguments)); err == nil {
		t.Fatal("task-bound singing voice silently replaced")
	}
	access.trace.TaskID = ""
	access.trace.ExpectedVoiceProfile = "bound-profile"
	if _, err = access.newMusicPluginPolicy(t.Context(), json.RawMessage(musicPluginArguments)); err == nil {
		t.Fatal("expected singing profile silently replaced")
	}
}

func TestMusicPluginBindsActualSourceRevisionAndActor(t *testing.T) {
	f, access, p := musicPluginFixture(t)
	j, cap := musicPluginJob(p)
	p.invocation.SourceVersions[access.trace.Source.EventID]++
	if err := p.Check(t.Context(), j, cap); err == nil {
		t.Fatal("stale source revision accepted")
	}
	other := access.trace
	other.TraceID, other.Sender, other.Text, other.Source = "other-source", "other-user", "synthetic other user request", &companion.SourceRef{MessageID: "other-source"}
	if err := f.manager.memory.Observe(other, f.manager.config.Companion, time.Now()); err != nil {
		t.Fatal(err)
	}
	source, ok := f.manager.memory.SourceForInput(other)
	if !ok || source.EventID == "" {
		t.Fatal("missing other synthetic source")
	}
	// A ref's actor metadata cannot overrule the actual retained event actor.
	source.Sender = access.trace.Sender
	access.trace.Source = source
	if _, err := access.newMusicPluginPolicy(t.Context(), json.RawMessage(musicPluginArguments)); err == nil {
		t.Fatal("another requester's event authorized a new model attempt")
	}
}

func TestMusicPluginPreservesTaskSongTitleDeliveryAndRequester(t *testing.T) {
	for _, change := range []string{"title", "kind", "requester", "cancelled", "profile"} {
		t.Run(change, func(t *testing.T) {
			f, access, _ := musicPluginFixture(t)
			task := agentTask{ID: "original-music-task", Account: access.trace.Account, Thread: access.trace.Thread, Sender: access.trace.Sender, State: "running", MusicTitle: "晚风轻唱", MusicKind: "voice", Constraints: access.taskConstraints("music", "", "", "")}
			switch change {
			case "title":
				task.MusicTitle = "另一首歌"
			case "kind":
				task.MusicKind = "both"
			case "requester":
				task.Sender = "other-user"
				task.Constraints.Requester = task.Sender
				task.Constraints.Source = nil
			case "cancelled":
				task.State = "cancelled"
			case "profile":
				task.Constraints.VoiceProfile = "bound-profile"
				task.MusicSpec = &songSpec{VoiceProfile: "bound-profile"}
			}
			if err := f.manager.agent.task(task); err != nil {
				t.Fatal(err)
			}
			access.trace.TaskID = task.ID
			if _, err := access.newMusicPluginPolicy(t.Context(), json.RawMessage(musicPluginArguments)); err == nil {
				t.Fatal("original song task constraint replaced", change)
			}
		})
	}
}

func TestMusicPluginUnicodeTimeoutAndDeliveryPermissions(t *testing.T) {
	f, access, _ := musicPluginFixture(t)
	args, _ := json.Marshal(map[string]any{"title": "晚风轻唱", "prompt": strings.Repeat("歌", 2000), "kind": "file", "timeout_seconds": 86400})
	f.manager.config.Companion.Permissions.SendVoice = false
	p, err := access.newMusicPluginPolicy(t.Context(), args)
	if err != nil || p.timeout != 86400 || p.kind != "file" || p.title != "晚风轻唱" {
		t.Fatal("valid explicit range/file-only rejected", p, err)
	}
	for _, invalid := range []string{
		`{"title":"晚风轻唱","prompt":"song","kind":"both"}`,
		`{"title":"晚风轻唱","prompt":"song","kind":"voice"}`,
		`{"title":"晚风轻唱","prompt":"song","kind":"file","timeout_seconds":0}`,
		`{"title":"晚风轻唱","prompt":"song","kind":"file","timeout_seconds":null}`,
		`{"title":"晚风轻唱","prompt":"song","kind":"file","timeout_seconds":1.5}`,
		`{"title":"晚风轻唱","prompt":"song","kind":"file","timeout_seconds":9223372036854775807}`,
		`{"title":"晚风轻唱","prompt":"song","kind":"file","voice_id":"named-voice"}`,
		`{"title":"晚风轻唱","prompt":"song","prompt":"changed","kind":"file"}`,
	} {
		if _, err := access.newMusicPluginPolicy(t.Context(), json.RawMessage(invalid)); err == nil {
			t.Fatal("invalid music request accepted", invalid)
		}
	}
	args, _ = json.Marshal(map[string]any{"title": "晚风轻唱", "prompt": strings.Repeat("歌", 2001), "kind": "file"})
	if _, err = access.newMusicPluginPolicy(t.Context(), args); err == nil {
		t.Fatal("over-limit prompt accepted")
	}
	if _, err = access.newMusicPluginPolicy(t.Context(), json.RawMessage(`{"title":"晚风轻唱","prompt":"song","kind":"file"}`)); err != nil {
		t.Fatal("default 180 timeout rejected", err)
	}
}

func TestMusicPluginDiscoveryIgnoresLegacyAndRequiresGrant(t *testing.T) {
	f := setup(t)
	f.manager.config.Enabled = true
	f.manager.config.Companion.Permissions.SendVoice = true
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.MusicEnabled, legacySpeechFixture.Key = true, "synthetic-legacy-key"
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	p := &permittedTools{manager: f.manager, trace: companion.Input{Channel: "wechat", Account: "test", Thread: "filehelper", Sender: "synthetic-user", TraceID: "migration", Text: "原创歌曲"}}
	for _, tool := range p.speechTools() {
		if tool.Name == "music_generate" {
			t.Fatal("legacy cloud configuration advertised music")
		}
	}
	if result, err := p.generateSpeech(t.Context(), "music_generate", json.RawMessage(musicPluginArguments)); err == nil || !strings.Contains(err.Error(), "music_plugin_migration_required") || result != "" {
		t.Fatal("legacy music fallback or media on failure", result, err)
	}
	s := installMusicPluginFixture(t, f, nil, false)
	i, _ := s.Installed(t.Context(), "music")
	if _, err := s.SetEnabled(t.Context(), "music", i.Revision, true); err == nil {
		t.Fatal("missing model grant enabled")
	}
	if f.manager.musicPluginConfigured(t.Context()) {
		t.Fatal("ungranted music advertised")
	}
	if result, err := p.generateSpeech(t.Context(), "music_generate", json.RawMessage(musicPluginArguments)); err == nil || result != "" {
		t.Fatal("disabled plugin fell back or staged output", result, err)
	}
}

func TestMusicPluginRegisteredSingingNameNeverDefaults(t *testing.T) {
	f, p, _, public := voiceAgentFixture(t)
	f.manager.config.Enabled = true
	installMusicPluginFixture(t, f, []string{"model.invoke"}, true)
	public.Name, public.Engine, public.ProfileID = "SyntheticSinger", "RVC", "synthetic-singing-profile"
	data, _ := json.Marshal(map[string]any{"items": []voiceEntry{public}})
	if err := os.WriteFile(filepath.Join(p.executionContext().Public, "models", "voice-library", "catalog.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	p.trace.RequestedText = "请用 SyntheticSinger 的声线唱一首原创歌"
	if result, err := p.generateSpeech(t.Context(), "music_generate", json.RawMessage(musicPluginArguments)); err == nil || result != "" {
		t.Fatal("named singing request substituted provider voice", result, err)
	}
}

// Opt-in isolated stdio/LPAC contract with an injected provider transport.
// No cloud model, audio download, native WeChat action or device is used.
func TestLiveMusicPluginGenerationContract(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in isolated music plugin contract")
	}
	python, _ = filepath.Abs(python)
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	f, _, policy := musicPluginFixture(t)
	calls := 0
	policy.post = func(_ context.Context, _ string, _ map[string]string, body json.RawMessage, _ int64, timeout time.Duration) (json.RawMessage, error) {
		calls++
		if _, err := policy.materialize(t.Context(), "generate", body); err != nil || timeout != 7200*time.Second {
			t.Fatal("runtime changed music body", string(body), timeout, err)
		}
		return json.RawMessage(`{"model":"synthetic-music","output":{"audio":{"url":"https://audio.example/private.mp3?token=synthetic"}}}`), nil
	}
	c, err := pluginruntime.NewController(policy.store, pluginruntime.InstalledExecutor(policy.store, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
	if err != nil {
		t.Fatal(err)
	}
	f.manager.packageRuntime, f.manager.packageRuntimeReady = c, true
	defer c.Close()
	result, err := policy.generate(t.Context())
	if err != nil || calls != 1 || result.URL != "https://audio.example/private.mp3?token=synthetic" || result.RequestedModel != "synthetic-music" {
		t.Fatal("isolated cloud music contract", result, err, calls)
	}
	page, err := policy.store.Jobs(t.Context(), "music", 0, 8)
	if err != nil || len(page.Items) != 1 || bytes.Contains(page.Items[0].Result, []byte("token=synthetic")) {
		t.Fatal("cloud checkpoint leaked private URL", page, err)
	}
	for _, pending := range []string{"music.cover", "music.train"} {
		if _, err := c.Invoke(t.Context(), pluginruntime.CallRequest{Plugin: "music", Capability: pending, Context: policy.invocation, Arguments: json.RawMessage(`{}`)}, policy); err == nil || calls != 1 {
			t.Fatal("pending capability reached provider", pending, err, calls)
		}
	}
}
