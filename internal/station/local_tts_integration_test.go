package station

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
)

type syntheticLocalTTS struct {
	voice   localTTSVoice
	wav     []byte
	started chan struct{}
	wrong   bool
	calls   atomic.Int32
}

func (s *syntheticLocalTTS) Info(context.Context, string) (localTTSDeployment, error) {
	return localTTSDeployment{Binding: strings.Repeat("b", 64), Voices: []localTTSVoice{s.voice}}, nil
}

func (s *syntheticLocalTTS) Synthesize(ctx context.Context, _ string, binding string, request localTTSRequest) (localTTSResult, error) {
	s.calls.Add(1)
	if binding != strings.Repeat("b", 64) || request.VoiceID != s.voice.ID || request.Speed == nil || *request.Speed != 0.8 || request.Language != "en" {
		return localTTSResult{}, errors.New("host failed to apply the selected voice and parameters")
	}
	if s.started != nil {
		close(s.started)
		<-ctx.Done()
	}
	result := localTTSResult{VoiceID: s.voice.ID, Version: s.voice.Version, Speed: *request.Speed, WAV: s.wav}
	if s.wrong {
		result.VoiceID = "unrequested-voice"
	}
	return result, nil
}

func newLocalTTSFixture(t *testing.T) (*localHomeFixture, *syntheticLocalTTS) {
	t.Helper()
	f := localChatFixture(t)
	backend := &syntheticLocalTTS{voice: localTTSVoice{ID: "fixture-voice", Label: "合成夹具声线", Version: strings.Repeat("a", 64), SpeedMin: 0.5, SpeedMax: 1.5},
		wav: syntheticMonoWAV(t, 16000, 8000)}
	catalog, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	f.handler.chat.projectTools.options.CatalogDirectory = catalog
	f.handler.chat.projectTools.tts = backend
	const route = "/api/local/project-tools/station-speech"
	if code, _ := f.request("PUT", route, map[string]any{"action": "install"}, true); code != http.StatusBadRequest {
		t.Fatalf("speech tool installed without owner approval: %d", code)
	}
	if code, out := f.request("PUT", route, map[string]any{"action": "install", "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatalf("install speech: %d %#v", code, out)
	}
	installed := localProjectToolByID(t, f, localTTSPackage)
	if code, out := f.request("PUT", route, map[string]any{"action": "enable", "revision": localProjectToolRevision(t, installed)}, true); code != http.StatusOK {
		t.Fatalf("enable speech: %d %#v", code, out)
	}
	if backend.calls.Load() != 0 {
		t.Fatal("enabling must not start speech synthesis")
	}
	return f, backend
}

func installLocalTTSModel(f *localHomeFixture, name string, arguments map[string]any, observed *string) {
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		called := false
		return localModelFunc(func(_ context.Context, system string, turns []map[string]string) (string, error) {
			if !called {
				called = true
				if !strings.Contains(system, name) {
					return "", errors.New("authorized speech tool is missing")
				}
				return localToolCall(name, arguments), nil
			}
			*observed = localModelHistory(turns)
			return `{"action":"reply","messages":["已核对语音合成结果。"]}`, nil
		}), nil
	}
}

func TestLocalSpeechChatSelectsVoiceAndDownloadsVerifiedWAV(t *testing.T) {
	f, backend := newLocalTTSFixture(t)
	const text = "本地慢读一段合成文本"
	var observed string
	installLocalTTSModel(f, localTTSVoicesTool, map[string]any{}, &observed)
	if code, _ := localChatSend(f, "speech-voices", "查看有哪些本地声线"); code != 200 || !strings.Contains(observed, backend.voice.ID) {
		t.Fatalf("speech voice list: %d %s", code, observed)
	}
	args := map[string]any{"voice_id": backend.voice.ID, "text": "A synthetic test.", "speed": 0.8, "text_language": "en"}
	installLocalTTSModel(f, localTTSStartTool, args, &observed)
	if code, out := localChatSend(f, "speech-generate", text); code != 200 || out["status"] != "completed" {
		t.Fatalf("synthesize: %d %#v", code, out)
	}
	id := regexp.MustCompile(`work-[0-9a-f]{64}`).FindString(observed)
	if id == "" || !strings.Contains(observed, backend.voice.Version) || !bytes.Equal(localArtifactDownload(t, f, id), backend.wav) {
		t.Fatal("verified speech artifact or voice receipt is missing")
	}
	if code, out := localChatSend(f, "speech-generate", text); code != 200 || out["reused"] != true || backend.calls.Load() != 1 {
		t.Fatalf("duplicate speech invocation: %d %#v %d", code, out, backend.calls.Load())
	}
	backend.wrong = true
	installLocalTTSModel(f, localTTSStartTool, args, &observed)
	if code, _ := localChatSend(f, "speech-mismatch", text); code != 200 || !strings.Contains(observed, "local_speech_voice_receipt_mismatch") || strings.Contains(observed, "unrequested-voice") {
		t.Fatalf("wrong voice was not safely withheld: %d %s", code, observed)
	}
	before := backend.calls.Load()
	args["voice_id"] = "unregistered-voice"
	installLocalTTSModel(f, localTTSStartTool, args, &observed)
	if code, _ := localChatSend(f, "speech-unknown", text); code != 200 || !strings.Contains(observed, "指定声线未登记或已撤权") || backend.calls.Load() != before {
		t.Fatalf("unknown voice should never start a worker: %d %s", code, observed)
	}
	store, input, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	host, err := f.handler.chat.projectTools.forFish(input.Account)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := host.store.Jobs(context.Background(), localTTSPackage, 0, 5)
	if err != nil || len(jobs.Items) != 2 || jobs.Items[0].State != "completed" || jobs.Items[1].State != "failed" {
		t.Fatalf("unexpected speech job history: %d %v", len(jobs.Items), err)
	}
	if strings.Contains(string(jobs.Items[0].Arguments), "A synthetic test.") || !strings.Contains(string(jobs.Items[0].Arguments), mediaspec.SpeechSHA256([]byte("A synthetic test."))) {
		t.Fatal("speech job does not bind text by digest only")
	}
	if err := store.Clear(companion.ScopeID(input.Channel, input.Account, input.Thread)); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request("GET", "/api/local/artifacts/"+id+"/download", nil, false); code != http.StatusNotFound {
		t.Fatalf("speech remained downloadable after forgetting its source: %d", code)
	}
}

func TestLocalSpeechDisableCancelsAndWithholdsLateWAV(t *testing.T) {
	f, backend := newLocalTTSFixture(t)
	backend.started = make(chan struct{})
	var observed string
	installLocalTTSModel(f, localTTSStartTool, map[string]any{"voice_id": backend.voice.ID, "text": "A synthetic test.", "speed": 0.8, "text_language": "en"}, &observed)
	done := make(chan int, 1)
	go func() {
		code, _ := localChatSend(f, "speech-cancel", "开始合成然后取消")
		done <- code
	}()
	select {
	case <-backend.started:
	case <-time.After(10 * time.Second):
		t.Fatal("speech worker never started")
	}
	installed := localProjectToolByID(t, f, localTTSPackage)
	if code, out := f.request("PUT", "/api/local/project-tools/station-speech", map[string]any{"action": "disable", "revision": localProjectToolRevision(t, installed)}, true); code != http.StatusOK {
		t.Fatalf("disable while synthesizing: %d %#v", code, out)
	}
	select {
	case code := <-done:
		if code != 200 || !strings.Contains(observed, "cancelled") || strings.Contains(observed, "work-") {
			t.Fatalf("late synthesis leaked or remained unresolved: %d %s", code, observed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("disabling did not cancel the speech context")
	}
}

func TestLocalSpeechJobStatusAndCancelAfterRestartNeverReplays(t *testing.T) {
	f, backend := newLocalTTSFixture(t)
	var observed string
	args := map[string]any{"voice_id": backend.voice.ID, "text": "A synthetic test.", "speed": 0.8, "text_language": "en"}
	installLocalTTSModel(f, localTTSStartTool, args, &observed)
	if code, out := localChatSend(f, "speech-job-original", "先合成一段本机语音"); code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("speech setup: %d %#v", code, out)
	}
	_, input, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	host, err := f.handler.chat.projectTools.forFish(input.Account)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := host.store.Jobs(context.Background(), localTTSPackage, 0, 5)
	if err != nil || len(jobs.Items) != 1 {
		t.Fatalf("speech history: %v %#v", err, jobs)
	}
	original := jobs.Items[0]
	// Construct a claimed durable record as if the one-shot worker stopped with
	// the desktop.  Its arguments contain no text, and status must not start the
	// backend after restart.
	pendingContext := original.Context
	pendingContext.RequestKey = strings.Repeat("9", 64)
	pending, created, err := host.store.CreateJob(context.Background(), pluginhost.Job{Plugin: localTTSPackage, Version: original.Version,
		PackageDigest: original.PackageDigest, ConfigRevision: original.ConfigRevision, Context: pendingContext, Tool: localTTSCapability, Arguments: original.Arguments})
	if err != nil || !created {
		t.Fatalf("create synthetic pending speech job: %v created=%v", err, created)
	}
	pending, err = host.store.ClaimJob(context.Background(), pending.ID, pending.Revision)
	if err != nil {
		t.Fatal(err)
	}
	next := localRestartChat(t, f)
	before := backend.calls.Load()
	installLocalTTSModel(next, localTTSJobTool, map[string]any{"job_id": pending.ID, "action": "status"}, &observed)
	if code, out := localChatSend(next, "speech-job-status", "查一下刚才语音任务的状态"); code != http.StatusOK || out["status"] != "completed" || !strings.Contains(observed, "original_worker_not_queryable") || backend.calls.Load() != before {
		t.Fatalf("restart status replayed or hid durable state: %d %#v calls=%d observed=%s", code, out, backend.calls.Load(), observed)
	}
	installLocalTTSModel(next, localTTSJobTool, map[string]any{"job_id": pending.ID, "action": "cancel"}, &observed)
	if code, out := localChatSend(next, "speech-job-cancel", "取消刚才的语音任务"); code != http.StatusOK || out["status"] != "completed" || strings.Contains(observed, "work-") || backend.calls.Load() != before {
		t.Fatalf("restart cancellation leaked or replayed speech: %d %#v calls=%d observed=%s", code, out, backend.calls.Load(), observed)
	}
	nextHost, err := next.handler.chat.projectTools.forFish(input.Account)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := nextHost.store.Job(context.Background(), pending.ID)
	if err != nil || finished.State != "cancelled" || len(finished.Artifacts) != 0 {
		t.Fatalf("orphaned speech job did not become a safe terminal cancellation: %#v err=%v", finished, err)
	}
}
