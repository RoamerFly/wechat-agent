package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func referenceTranscriptionFixture(t *testing.T) (*webFixture, *permittedTools, mediaAsset, []byte, *mediaTranscriptionPolicy) {
	t.Helper()
	f, _, _, _ := transcriptionFixture(t, false)
	f.manager.config.Companion.Permissions.Remember = true
	in := companion.Input{Channel: "wechat", Account: "test", Thread: "filehelper", Sender: "synthetic-requester", TraceID: "reference-asr-request", Text: "请识别已选录音的一段歌词", Source: &companion.SourceRef{MessageID: "reference-asr-request"}}
	if err := f.manager.memory.Observe(in, f.manager.Config().Companion, time.Now()); err != nil {
		t.Fatal(err)
	}
	source, ok := f.manager.memory.SourceForInput(in)
	if !ok || source.EventID == "" {
		t.Fatal("missing canonical reference ASR requester source")
	}
	in.Source = source
	access := &permittedTools{manager: f.manager, trace: in}
	if access.privileged() {
		t.Fatal("reference ASR fixture must exercise an authorized non-owner")
	}
	upload := in
	upload.Sender, upload.TraceID, upload.Source = "synthetic-original-uploader", "reference-upload", nil
	asset, err := f.manager.rememberAssetFormats(upload, assetOriginal{Data: syntheticReferenceWAV(t, 4), MIME: "audio/wav", Filename: "synthetic-original.wav"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := agentTask{ID: "reference-asr-task", Account: in.Account, Thread: in.Thread, Sender: in.Sender, State: "running", RequestedText: in.Text, Constraints: access.taskConstraints("reference_transcription", "", "", asset.ID), MusicSpec: &songSpec{Mode: "cover", Reference: asset.ID, ReferenceHash: asset.Hash}}
	if err = f.manager.agent.task(task); err != nil {
		t.Fatal(err)
	}
	reopened, err := newAgentControl(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := reopened.lookupTask(task.ID, in.Account, in.Thread)
	if !ok || stored.Constraints == nil || stored.Constraints.Source == nil || stored.Constraints.Source.EventID != source.EventID || stored.Constraints.AssetID != asset.ID {
		t.Fatal("reference task/source binding did not persist")
	}
	f.manager.agent = reopened
	access.trace.TaskID = task.ID
	_, data, err := access.referenceAsset(asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	wav, err := musicTranscriptionWindow(t.Context(), f.dir, data, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := access.newReferenceTranscriptionPolicy(t.Context(), asset, wav, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	return f, access, asset, wav, policy
}

func referenceTranscriptResult(p *mediaTranscriptionPolicy) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"ok": true, "text": "synthetic candidate lyrics", "status": "transcribed", "sources": []mediaAudioReference{p.Reference()}, "coverage": "provided_audio_only", "untrusted_external_content": true})
	return raw
}

func TestReferenceTranscriptionBindsSelectedAssetWindowAndExactWAV(t *testing.T) {
	_, access, asset, wav, policy := referenceTranscriptionFixture(t)
	sum := sha256.Sum256(wav)
	if policy.receivedSharePolicy != nil || policy.Reference().SHA != hex.EncodeToString(sum[:]) || policy.invocation().Account != access.trace.Account || policy.invocation().Thread != access.trace.Thread || policy.invocation().Requester != access.trace.Sender || policy.invocation().SourceVersions[access.trace.Source.EventID] != access.trace.Source.Revision || policy.invocation().SourceVersions["asset:"+asset.ID+":"+referenceTranscriptionAssetVersion(asset)] != 1 {
		t.Fatal("reference origin lost its immutable source/audio binding", policy.invocation(), policy.Reference())
	}
	for _, window := range [][2]int{{0, 2}, {1, 1}} {
		other, err := access.newReferenceTranscriptionPolicy(t.Context(), asset, wav, window[0], window[1])
		if err != nil || other.invocation().RequestKey == policy.invocation().RequestKey {
			t.Fatal("different selected window reused a model job", window, err)
		}
	}
	changed := append([]byte(nil), wav...)
	changed[len(changed)-1] ^= 1
	other, err := access.newReferenceTranscriptionPolicy(t.Context(), asset, changed, 1, 2)
	if err != nil || other.Reference().SHA == policy.Reference().SHA || other.invocation().RequestKey == policy.invocation().RequestKey {
		t.Fatal("different WAV reused an audio reference or model job", err)
	}
	original := append([]byte(nil), wav...)
	wav[len(wav)-1] ^= 1 // Caller mutation must not change materialized input.
	job, cap := transcriptionJob(policy)
	calls := 0
	policy.post = func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
		calls++
		if endpoint != "https://provider.example/api/v1/services/aigc/multimodal-generation/generation" || headers["Authorization"] != "Bearer synthetic-asr-key" || bytes.Contains(body, []byte("$media_ref")) || !bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(original))) {
			t.Fatal("reference ASR used substituted bytes/configuration")
		}
		return json.RawMessage(`{"output":{"text":"synthetic candidate lyrics"}}`), nil
	}
	bad := bytes.Replace(transcriptionRequest(policy), []byte(policy.Reference().SHA), []byte(strings.Repeat("f", 64)), 1)
	if _, err = policy.Service(t.Context(), job, cap, "wechat/model_request", bad); err == nil || calls != 0 {
		t.Fatal("substituted WAV reference reached provider", err, calls)
	}
	if _, err = policy.Service(t.Context(), job, cap, "wechat/model_request", transcriptionRequest(policy)); err != nil {
		t.Fatal(err)
	}
	if _, err = policy.Service(t.Context(), job, cap, "wechat/model_request", transcriptionRequest(policy)); err == nil || calls != 1 {
		t.Fatal("reference ASR repeated a provider attempt", err, calls)
	}
	verified, err := policy.Validate(t.Context(), job, cap, referenceTranscriptResult(policy))
	if err != nil || bytes.Contains(verified.Result, []byte("synthetic candidate lyrics")) || !bytes.Contains(verified.Private, []byte("synthetic candidate lyrics")) {
		t.Fatal("candidate transcript lost private provider verification", err)
	}
}

func replaceReferenceAssetIndex(t *testing.T, manager *Manager, id string) {
	t.Helper()
	items, err := manager.loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	for i := range items {
		if items[i].ID == id {
			items[i].Hash = strings.Repeat("e", 64)
		}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if err = writePrivate(filepath.Join(manager.dir, "media-assets.json"), raw); err != nil {
		t.Fatal(err)
	}
}

func TestReferenceTranscriptionRechecksScopeRequesterAssetAndTask(t *testing.T) {
	for _, change := range []string{"account", "thread", "requester", "remember", "agent", "task_cancel", "source_forgotten", "asset_replaced", "plugin_disabled", "cancelled_context"} {
		t.Run(change, func(t *testing.T) {
			f, access, asset, _, policy := referenceTranscriptionFixture(t)
			job, cap := transcriptionJob(policy)
			ctx := t.Context()
			switch change {
			case "account":
				access.trace.Account = "other-account"
			case "thread":
				access.trace.Thread = "other-thread"
			case "requester":
				access.trace.Sender = "other-requester"
			case "remember":
				f.manager.config.Companion.Permissions.Remember = false
			case "agent":
				settings := f.manager.agent.config()
				settings.Enabled = false
				if err := f.manager.agent.save(settings); err != nil {
					t.Fatal(err)
				}
			case "task_cancel":
				task, _ := f.manager.agent.lookupTask(access.trace.TaskID, access.trace.Account, access.trace.Thread)
				task.State = "cancelled"
				if err := f.manager.agent.task(task); err != nil {
					t.Fatal(err)
				}
			case "source_forgotten":
				if err := f.manager.memory.Clear(asset.Scope); err != nil {
					t.Fatal(err)
				}
			case "asset_replaced":
				replaceReferenceAssetIndex(t, f.manager, asset.ID)
			case "plugin_disabled":
				installed, _ := policy.store.Installed(ctx, "media-understanding")
				if _, err := policy.store.SetEnabled(ctx, "media-understanding", installed.Revision, false); err != nil {
					t.Fatal(err)
				}
			case "cancelled_context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			calls := 0
			policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
				calls++
				return nil, nil
			}
			if _, err := policy.Service(ctx, job, cap, "wechat/model_request", transcriptionRequest(policy)); err == nil || calls != 0 {
				t.Fatal("revoked reference ASR reached provider", err, calls)
			}
			if _, err := policy.Validate(ctx, job, cap, referenceTranscriptResult(policy)); err == nil {
				t.Fatal("revoked reference ASR accepted candidate")
			}
		})
	}
}

func TestReferenceTranscriptionRejectsForeignAssetAndTaskAssetSelection(t *testing.T) {
	f, access, asset, wav, _ := referenceTranscriptionFixture(t)
	for _, change := range []string{"account", "scope", "hash", "id"} {
		changed := asset
		switch change {
		case "account":
			changed.Account = "other"
		case "scope":
			changed.Scope = companion.ScopeID("wechat", "test", "other")
		case "hash":
			changed.Hash = strings.Repeat("f", 64)
		case "id":
			changed.ID = "asset-other"
		}
		if _, err := access.newReferenceTranscriptionPolicy(t.Context(), changed, wav, 1, 2); err == nil {
			t.Fatal("substituted selected asset accepted", change)
		}
	}
	other, err := f.manager.rememberAssetFormats(access.trace, assetOriginal{Data: syntheticReferenceWAV(t, 5), MIME: "audio/wav", Filename: "other-reference.wav"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.newReferenceTranscriptionPolicy(t.Context(), other, wav, 1, 2); err == nil {
		t.Fatal("task's selected asset silently replaced")
	}
}

func TestReferenceTranscriptionUnknownProviderAttemptNeverRetries(t *testing.T) {
	f, _, _, _, policy := referenceTranscriptionFixture(t)
	job, cap := transcriptionJob(policy)
	calls := 0
	policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		return nil, errors.New("synthetic uncertain ASR attempt")
	}
	for i := 0; i < 2; i++ {
		if _, err := policy.Service(t.Context(), job, cap, "wechat/model_request", transcriptionRequest(policy)); err == nil {
			t.Fatal("uncertain reference ASR succeeded")
		}
	}
	if calls != 1 || f.manager.billing.PurposeTotals("plugin_media_transcription").Requests != 1 || f.manager.billing.PurposeTotals("plugin_media_transcription").Reported != 0 {
		t.Fatal("uncertain ASR was replayed or usage called zero", calls)
	}
	if _, err := policy.Validate(t.Context(), job, cap, referenceTranscriptResult(policy)); err == nil {
		t.Fatal("failed reference ASR fabricated a candidate")
	}
}

func TestReferenceTranscriptionCompletedCheckpointCannotOutliveAuthority(t *testing.T) {
	for _, revoke := range []string{"remember", "task_cancel", "source_forgotten", "asset_replaced"} {
		t.Run(revoke, func(t *testing.T) {
			f, access, asset, _, policy := referenceTranscriptionFixture(t)
			job, cap := transcriptionJob(policy)
			job.Arguments = policy.Arguments()
			job, _, err := policy.store.CreateJob(t.Context(), job)
			if err != nil {
				t.Fatal(err)
			}
			job, err = policy.store.ClaimJob(t.Context(), job.ID, job.Revision)
			if err != nil {
				t.Fatal(err)
			}
			policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
				return json.RawMessage(`{"output":{"text":"synthetic candidate lyrics"}}`), nil
			}
			if _, err = policy.Service(t.Context(), job, cap, "wechat/model_request", transcriptionRequest(policy)); err != nil {
				t.Fatal(err)
			}
			verified, err := policy.Validate(t.Context(), job, cap, referenceTranscriptResult(policy))
			if err != nil {
				t.Fatal(err)
			}
			completed, err := policy.store.CompleteJobWithPrivate(t.Context(), job.ID, job.Revision, nil, verified.Result, verified.Private)
			if err != nil {
				t.Fatal(err)
			}
			if err = policy.Check(t.Context(), completed, pluginhost.Capability{}); err != nil {
				t.Fatal("verified reference checkpoint was not initially usable", err)
			}
			switch revoke {
			case "remember":
				f.manager.config.Companion.Permissions.Remember = false
			case "task_cancel":
				task, _ := f.manager.agent.lookupTask(access.trace.TaskID, access.trace.Account, access.trace.Thread)
				task.State = "cancelled"
				if err = f.manager.agent.task(task); err != nil {
					t.Fatal(err)
				}
			case "source_forgotten":
				if err = f.manager.memory.Clear(asset.Scope); err != nil {
					t.Fatal(err)
				}
			case "asset_replaced":
				replaceReferenceAssetIndex(t, f.manager, asset.ID)
			}
			if err = policy.Check(t.Context(), completed, pluginhost.Capability{}); err == nil {
				t.Fatal("completed candidate remained actionable after authority changed", revoke)
			}
			if _, err = policy.store.PrivateJobResult(t.Context(), completed, func(ctx context.Context, current pluginhost.Job) error { return policy.Check(ctx, current, cap) }); err == nil {
				t.Fatal("revoked private candidate remained readable", revoke)
			}
			assertNoReferenceASRCandidates(t, f.dir)
		})
	}
}

type referenceTranscriptionFakeExecutor struct {
	calls  int
	revoke func()
}

func (e *referenceTranscriptionFakeExecutor) Start(context.Context, pluginhost.Package, pluginhost.Grant) (pluginhost.Session, error) {
	return e, nil
}
func (*referenceTranscriptionFakeExecutor) Health(context.Context) error { return nil }
func (*referenceTranscriptionFakeExecutor) Close() error                 { return nil }
func (e *referenceTranscriptionFakeExecutor) Call(_ context.Context, _ pluginhost.Invocation, tool string, raw json.RawMessage) (json.RawMessage, error) {
	e.calls++
	if tool != "media.transcribe" {
		return nil, errors.New("unexpected reference tool")
	}
	var args struct {
		Audio mediaAudioReference `json:"audio"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if e.revoke != nil {
		e.revoke()
	}
	return json.Marshal(map[string]any{"ok": true, "text": "fabricated candidate without provider", "status": "transcribed", "sources": []mediaAudioReference{args.Audio}, "coverage": "provided_audio_only", "untrusted_external_content": true})
}

func assertNoReferenceASRCandidates(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "music-lyrics"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("unverified/revoked transcription persisted lyric evidence")
	}
}

func TestReferenceInspectControllerNeverPersistsFabricatedOrRevokedCandidate(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "fabricated", true: "revoked"}[revoke], func(t *testing.T) {
			f, access, asset, _, policy := referenceTranscriptionFixture(t)
			executor := &referenceTranscriptionFakeExecutor{}
			if revoke {
				executor.revoke = func() { f.manager.config.Companion.Permissions.Remember = false }
			}
			controller, err := pluginruntime.NewController(policy.store, executor)
			if err != nil {
				t.Fatal(err)
			}
			f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
			t.Cleanup(func() { _ = controller.Close() })
			raw, _ := json.Marshal(map[string]any{"asset_id": asset.ID, "transcribe": true, "offset_seconds": 1, "seconds": 2})
			result, err := access.inspectMusicReference(t.Context(), raw)
			if err == nil || result != "" || executor.calls != 1 {
				t.Fatal("inspect accepted fabricated/revoked candidate", result, err, executor.calls)
			}
			if result, err = access.inspectMusicReference(t.Context(), raw); err == nil || result != "" || executor.calls != 1 {
				t.Fatal("uncertain reference inspection replayed a plugin or persisted a candidate", result, err, executor.calls)
			}
			assertNoReferenceASRCandidates(t, f.dir)
			page, err := policy.store.Jobs(t.Context(), "media-understanding", 0, 8)
			if err != nil || len(page.Items) != 1 || page.Items[0].State != "needs_review" {
				t.Fatal("unverified inspect result falsely completed", page, err)
			}
		})
	}
}

func TestReferenceInspectDisabledPluginNeverFallsBackToLegacyASR(t *testing.T) {
	f, access, asset, _, policy := referenceTranscriptionFixture(t)
	installed, _ := policy.store.Installed(t.Context(), "media-understanding")
	if _, err := policy.store.SetEnabled(t.Context(), "media-understanding", installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.ASREnabled, legacySpeechFixture.ASRBackend, legacySpeechFixture.Endpoint, legacySpeechFixture.Key = true, "bailian", "https://legacy.invalid", "synthetic-legacy-key"
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	raw, _ := json.Marshal(map[string]any{"asset_id": asset.ID, "transcribe": true, "seconds": 1})
	result, err := access.inspectMusicReference(t.Context(), raw)
	if err == nil || result != "" || !strings.Contains(err.Error(), "media_transcription_plugin_required_or_unconfigured") {
		t.Fatal("disabled installed media ASR fell back to legacy", result, err)
	}
	assertNoReferenceASRCandidates(t, f.dir)
}

func TestLiveReferenceTranscriptionCompletedCandidateRequiresCurrentAuthority(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in isolated reference ASR contract with synthetic provider only")
	}
	python, _ = filepath.Abs(python)
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	options := pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}
	f, access, asset, _, policy := referenceTranscriptionFixture(t)
	controller, err := pluginruntime.NewController(policy.store, pluginruntime.InstalledExecutor(policy.store, options))
	if err != nil {
		t.Fatal(err)
	}
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	t.Cleanup(func() { _ = controller.Close() })
	calls := 0
	policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"output":{"text":"synthetic candidate lyrics"}}`), nil
	}
	text, err := policy.transcribe(t.Context())
	if err != nil || text != "synthetic candidate lyrics" || calls != 1 || policy.CompletedJob().State != "completed" {
		t.Fatal("isolated selected-reference ASR failed", text, err, calls)
	}
	if err = policy.checkCompleted(t.Context()); err != nil {
		t.Fatal("completed reference ASR lost its valid source", err)
	}
	f.manager.config.Companion.Permissions.Remember = false
	if err = policy.checkCompleted(t.Context()); err == nil {
		t.Fatal("completed candidate remained actionable after permission revocation")
	}
	assertNoReferenceASRCandidates(t, f.dir)
	if len(access.availableLyricEvidence(asset)) != 0 {
		t.Fatal("revoked candidate appeared in available lyric evidence")
	}
}
