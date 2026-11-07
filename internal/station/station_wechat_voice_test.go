package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
)

// TestStationWechatVoiceTranscriptionUsesVerifiedPlayableVoice is deliberately
// an entirely local contract test.  A normal native rendition must be decoded
// before local ASR sees it; an encoded SILK-like rendition is never treated as
// speech just because the attachment itself was downloadable.
func TestStationWechatVoiceTranscriptionUsesVerifiedPlayableVoice(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	wavPath := filepath.Join(root, "synthetic-voice.wav")
	if output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=f=660:r=16000:d=1", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-threads", "1", wavPath).CombinedOutput(); err != nil {
		t.Fatalf("generate synthetic WAV: %v %s", err, output)
	}
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(wav, []byte("RIFF")) || len(wav) < 1000 {
		t.Fatal("ffmpeg did not create a usable WAV fixture")
	}
	rawSILK := []byte("#!SILK_V3\nsynthetic encoded voice must not reach ASR")

	const account, thread = "one", "room@chatroom"
	now := time.Now().UTC().Add(-time.Minute)
	messages := map[string]linuxnative.Message{}
	for _, id := range []string{"voice-normal", "voice-silk", "voice-foreign"} {
		messages[id] = linuxnative.Message{
			ID: id, AccountID: account, ThreadID: thread, SenderID: "alice", ServerID: id + "-server",
			CreatedAt: now.Format(time.RFC3339Nano), Type: 34, Direction: "incoming", Body: "synthetic voice " + id,
		}
	}
	foreign := messages["voice-foreign"]
	foreign.ThreadID = "elsewhere@chatroom"
	messages[foreign.ID] = foreign

	var normalTransfers atomic.Int32
	nativeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v1/accounts/one/"
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		if r.URL.Path == "/v1/status" {
			write(map[string]string{"serverId": "station-voice-synthetic-dataset"})
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if strings.HasPrefix(rest, "messages/") {
			rest = strings.TrimPrefix(rest, "messages/")
			id, suffix, _ := strings.Cut(rest, "/")
			message, ok := messages[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if suffix == "" && r.Method == http.MethodGet {
				write(message)
				return
			}
			if suffix == "media-transfers" && r.Method == http.MethodPost {
				var request struct {
					Quality string `json:"quality"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.Quality != "normal" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				normalTransfers.Add(1)
				write(map[string]any{"id": "voice-normal-job-" + id, "accountId": account, "kind": "download", "state": "available", "assetId": "voice-normal-asset-" + id})
				return
			}
		}
		if strings.HasPrefix(rest, "media/voice-normal-asset-") {
			rest = strings.TrimPrefix(rest, "media/")
			asset, suffix, _ := strings.Cut(rest, "/")
			id := strings.TrimPrefix(asset, "voice-normal-asset-")
			_, ok := messages[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			payload := wav
			if id == "voice-silk" {
				payload = rawSILK
			}
			sum := sha256.Sum256(payload)
			if suffix == "" && r.Method == http.MethodGet {
				write(map[string]any{"id": asset, "accountId": account, "messageId": id, "kind": "voice", "state": "available", "variant": "normal", "actualVariant": "normal", "byteLength": len(payload), "contentType": "audio/wav", "sha256": hex.EncodeToString(sum[:])})
				return
			}
			if suffix == "content" && r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "audio/wav")
				_, _ = w.Write(payload)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer nativeServer.Close()
	token := filepath.Join(root, "native-token")
	if err := os.WriteFile(token, []byte("synthetic-token"), 0600); err != nil {
		t.Fatal(err)
	}
	native, err := linuxnative.New(linuxnative.Config{URL: nativeServer.URL, Account: account, TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := companion.NewStore(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()

	refFor := func(message linuxnative.Message) *companion.SourceRef {
		sum := sha256.Sum256([]byte(message.Body))
		return &companion.SourceRef{Account: message.AccountID, Thread: message.ThreadID, MessageID: message.ID, ServerID: message.ServerID, Sender: message.SenderID, MessageType: message.Type, OccurredAt: message.CreatedAt, BodyHash: hex.EncodeToString(sum[:])}
	}
	remember := companion.Defaults()
	remember.Permissions.Remember = true
	voiceRef := refFor(messages["voice-normal"])
	for _, id := range []string{"voice-normal", "voice-silk"} {
		message := messages[id]
		if err := memory.Observe(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: message.SenderID, TraceID: message.ID, MessageTime: message.CreatedAt, Source: refFor(message), Text: "[微信语音]"}, remember, now); err != nil {
			t.Fatal(err)
		}
	}

	pluginStore, err := pluginhost.OpenStore(filepath.Join(root, "project-tools"), account)
	if err != nil {
		t.Fatal(err)
	}
	defer pluginStore.Close()
	prepared, err := pluginStore.Prepare(t.Context(), filepath.Join("..", "..", "plugins", "media-understanding"))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := pluginStore.Install(t.Context(), prepared.Token, prepared.Digest, []string{"artifact.read", "artifact.write", "model.invoke"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pluginStore.SaveConfiguration(t.Context(), "media-understanding", pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{"asr_backend": json.RawMessage(`"local"`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pluginStore.SetEnabled(t.Context(), "media-understanding", installed.Revision, true); err != nil {
		t.Fatal(err)
	}

	var blocked atomic.Bool
	var revokeDuringASR, noSpeech bool
	var localCalls atomic.Int32
	host := &stationShareHost{store: pluginStore, localASR: stationLocalASR{
		version: func(context.Context) (string, error) { return "synthetic-local-asr-v1", nil },
		transcribe: func(_ context.Context, decoded []byte, verify func() error) (string, error) {
			if len(decoded) < 1000 || !bytes.HasPrefix(decoded, []byte("RIFF")) || string(decoded[8:12]) != "WAVE" {
				t.Fatal("local ASR was not given decoded WAV")
			}
			if err := verify(); err != nil {
				return "", err
			}
			localCalls.Add(1)
			if revokeDuringASR {
				blocked.Store(true)
				return "must not leak after grant revocation", verify()
			}
			if noSpeech {
				return "", &mediaspec.Failure{Code: "ASR_RESPONSE_HAVE_NO_WORDS"}
			}
			return "本地语音识别结果。", nil
		},
	}}
	tools := &stationAttachmentTools{native: native, store: memory, dir: root, blocked: blocked.Load,
		grants: map[string]time.Time{thread: now.Add(-time.Minute)}, shareHost: func() (*stationShareHost, error) { return host, nil }}
	call := func(trace companion.Input, raw string) (string, error) {
		tools.SetTrace(trace)
		return tools.Call(t.Context(), "wechat_voice_transcribe", json.RawMessage(raw))
	}
	direct := companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: "alice", TraceID: "voice-normal", MessageTime: now.Format(time.RFC3339Nano), Source: voiceRef}
	out, err := call(direct, `{"message_id":"voice-normal","start_seconds":0.1,"duration_seconds":0.5}`)
	if err != nil || !strings.Contains(out, `"asr_backend":"local"`) || !strings.Contains(out, `"transcription_state":"transcribed"`) || !strings.Contains(out, "本地语音识别结果") || !strings.Contains(out, `"source_variant":"normal"`) || !strings.Contains(out, `"next_start_seconds":0.6`) || !strings.Contains(out, `"audio_complete":false`) {
		t.Fatalf("normal voice was not transcribed locally: out=%s err=%v", out, err)
	}
	if normalTransfers.Load() != 1 {
		t.Fatal("normal playable voice was not requested exactly once", normalTransfers.Load())
	}

	requestRef := &companion.SourceRef{Account: account, Thread: thread, MessageID: "request", ServerID: "request-server", Sender: "owner", MessageType: 1, OccurredAt: now.Add(time.Second).Format(time.RFC3339Nano), BodyHash: "request-body"}
	quoted := companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: "owner", TraceID: "request", MessageTime: requestRef.OccurredAt, Source: requestRef, QuotedSource: voiceRef}
	out, err = call(quoted, `{"message_id":"voice-normal"}`)
	if err != nil || !strings.Contains(out, "本地语音识别结果") || localCalls.Load() != 2 {
		t.Fatalf("quoted voice did not resolve its original source: out=%s err=%v calls=%d", out, err, localCalls.Load())
	}

	foreignRef := refFor(messages["voice-foreign"])
	foreignTrace := direct
	foreignTrace.Source, foreignTrace.TraceID = foreignRef, foreignRef.MessageID
	if out, err = call(foreignTrace, fmt.Sprintf(`{"message_id":%q}`, foreignRef.MessageID)); out != "" || err == nil || localCalls.Load() != 2 {
		t.Fatalf("foreign voice escaped its conversation grant: out=%s err=%v calls=%d", out, err, localCalls.Load())
	}
	if out, err = call(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: "alice", TraceID: "voice-silk", MessageTime: now.Format(time.RFC3339Nano), Source: refFor(messages["voice-silk"])}, `{"message_id":"voice-silk"}`); out != "" || err == nil || localCalls.Load() != 2 {
		t.Fatalf("raw SILK-like payload reached local ASR or produced a transcript: out=%s err=%v calls=%d", out, err, localCalls.Load())
	}
	resolved, ok := memory.SourceForInput(direct)
	if !ok {
		t.Fatal("stored voice source unavailable")
	}
	observer := stationMediaObserver{tools: tools}
	message := messages["voice-normal"]
	if !observer.SupportsMedia(t.Context(), message) {
		t.Fatal("authorized voice observer not available")
	}
	ctx := companion.WithMediaSource(t.Context(), *resolved)
	if text, evidence, err := observer.AnalyzeWithEvidence(ctx, message); err != nil || text == "" || !companion.HasFirsthandObservation(evidence.Parts) {
		t.Fatalf("automatic transcription lacks audio evidence: %v %+v", err, evidence)
	}
	noSpeech = true
	if out, err = call(direct, `{"message_id":"voice-normal","start_seconds":0.6}`); err != nil || !strings.Contains(out, `"transcription_state":"no_recognizable_speech"`) || !strings.Contains(out, `"audio_complete":true`) || strings.Contains(out, `"text":`) {
		t.Fatalf("no-speech result claimed text or lost coverage: out=%s err=%v", out, err)
	}
	revokeDuringASR = true
	if out, err = call(direct, `{"message_id":"voice-normal"}`); out != "" || err == nil || localCalls.Load() != 5 {
		t.Fatalf("grant revocation leaked a transcript: out=%s err=%v calls=%d", out, err, localCalls.Load())
	}
	if host.usage != nil {
		t.Fatal("local speech unexpectedly opened remote model billing")
	}
}
