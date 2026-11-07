package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediadecode"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/stationruntime"
)

func TestStationVideoOriginalFromNativeChannels(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	root := t.TempDir()
	sample := filepath.Join(root, "synthetic.mp4")
	if output, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=64x48:r=5:d=1", "-f", "lavfi", "-i", "sine=f=800:r=16000:d=1", "-c:v", "mpeg4", "-c:a", "aac", "-shortest", "-threads", "1", sample).CombinedOutput(); err != nil {
		t.Fatalf("synthetic video: %v %s", err, output)
	}
	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	const key, locator, thread = "18446744073709551612", "https://private.example/video?token=synthetic-secret", "room@chatroom"
	encrypted := bytes.Clone(data)
	if err := wechatapp.DecodeChannelsPayload(encrypted, key); err != nil || bytes.Equal(data, encrypted) {
		t.Fatal("fixture did not exercise decryption", err)
	}
	now := time.Now().UTC()
	subtype := 51
	messages := map[string]linuxnative.Message{}
	for _, id := range []string{"card", "rotates", "missing", "expired", "resolve-revoked", "other", "forgotten", "revoked", "corrupt", "direct-no-object"} {
		messages[id] = linuxnative.Message{ID: id, AccountID: "one", ThreadID: thread, SenderID: "alice", ServerID: id + "-srv", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), Type: 49, AppMessageType: &subtype, Direction: "incoming", Body: "synthetic card", Link: &linuxnative.LinkCard{Title: "Synthetic title"}}
	}
	other := messages["other"]
	other.ThreadID = "elsewhere@chatroom"
	messages["other"] = other
	normal := messages["card"]
	normal.ID, normal.Type, normal.AppMessageType, normal.Link = "normal-video", 43, nil, nil
	messages[normal.ID] = normal
	var objectChanged, blocked atomic.Bool
	var fetches, mutations, transfers atomic.Int32
	nativeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v1/accounts/one/"
		out := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.Method + " " + r.URL.Path {
		case "GET " + prefix + "messages/normal-video/media":
			out(map[string]any{"items": []map[string]any{{"accountId": "one", "messageId": "normal-video", "kind": "video", "variant": "original", "actualVariant": "original", "state": "available"}}})
			return
		case "POST " + prefix + "messages/normal-video/media-transfers":
			transfers.Add(1)
			out(map[string]any{"id": "normal-job", "accountId": "one", "kind": "download", "state": "available", "assetId": "normal-asset"})
			return
		case "GET " + prefix + "media/normal-asset":
			out(map[string]any{"id": "normal-asset", "accountId": "one", "messageId": "normal-video", "kind": "video", "state": "available", "variant": "original", "actualVariant": "original", "byteLength": len(data), "contentType": "video/mp4", "sha256": hash})
			return
		case "GET " + prefix + "media/normal-asset/content":
			_, _ = w.Write(data)
			return
		}
		if r.Method != "GET" {
			mutations.Add(1)
			w.WriteHeader(405)
			return
		}
		if r.URL.Path == "/v1/status" {
			out(map[string]string{"serverId": "station-synthetic-dataset"})
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v1/accounts/one/messages/")
		id, suffix, _ := strings.Cut(rest, "/")
		message, ok := messages[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if suffix == "" {
			out(message)
			return
		}
		if suffix != "channels-media" {
			w.WriteHeader(404)
			return
		}
		source := linuxnative.ChannelsSource{AccountID: "one", MessageID: id, ThreadID: message.ThreadID, ObjectID: "object-" + id, ObjectNonceID: "nonce", State: "available", DecodeKey: key, MediaURL: locator + "&case=" + id}
		switch id {
		case "missing", "resolve-revoked":
			source.State, source.MediaURL = "unavailable", ""
			source.ObjectID = "export/synthetic-" + id
		case "expired":
			source.ObjectID = "export/synthetic-expired"
		case "rotates":
			if objectChanged.Load() {
				source.ObjectNonceID = "different"
			}
		case "direct-no-object":
			source.ObjectID = ""
		}
		out(source)
	}))
	defer nativeServer.Close()
	token := filepath.Join(root, "synthetic-token")
	if err = os.WriteFile(token, []byte("synthetic-token"), 0600); err != nil {
		t.Fatal(err)
	}
	native, err := linuxnative.New(linuxnative.Config{URL: nativeServer.URL, Account: "one", TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := companion.NewStore(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	fetch := func(_ context.Context, url string, limit int64) ([]byte, string, error) {
		fetches.Add(1)
		if !strings.HasPrefix(url, locator+"&case=") || limit != 10<<20 && (url != locator+"&case=card" || limit != 50<<20) {
			t.Errorf("unexpected locator or budget: %d", limit)
		}
		switch strings.TrimPrefix(url, locator+"&case=") {
		case "rotates":
			objectChanged.Store(true)
		case "revoked":
			blocked.Store(true)
		case "corrupt":
			return []byte("not a video"), "video/mp4", nil
		case "expired":
			return nil, "", &mcptools.WebError{Code: "http_status", Status: 403}
		}
		return bytes.Clone(encrypted), "video/mp4", nil
	}
	tools := &stationAttachmentTools{native: native, store: memory, dir: root, blocked: blocked.Load, fetchVideo: fetch,
		grants: map[string]time.Time{thread: now.Add(-time.Minute)}}
	tools.SetTrace(companion.Input{Channel: "wechat", Account: "one", Thread: thread, Sender: "bob", TraceID: "question", MessageTime: now.Format(time.RFC3339Nano)})
	call := func(id string) (string, error) {
		return tools.Call(t.Context(), "wechat_video_read", json.RawMessage(fmt.Sprintf(`{"message_id":%q,"max_bytes":10485760}`, id)))
	}
	deny := func(id, code string) {
		t.Helper()
		if result, err := call(id); result != "" || err == nil || code != "" && err.Error() != code {
			t.Fatal(id, result, err)
		}
	}
	deny("card", "") // No history authorization for an old card.
	if fetches.Load() != 0 {
		t.Fatal("unauthorized download")
	}
	tools.reads = map[string]time.Time{thread: now.Add(-time.Minute)}
	before := fetches.Load()
	deny("other", "")
	deny("missing", "channels_media_locator_missing")
	if fetches.Load() != before {
		t.Fatal("wrong or absent locator was downloaded")
	}
	assets := make(map[string]string)
	for _, id := range []string{"card", "direct-no-object", "card", "normal-video", "normal-video"} {
		result, err := call(id)
		if err != nil {
			t.Fatal(id, err)
		}
		if strings.Contains(result, "synthetic-secret") || strings.Contains(result, key) || strings.Contains(result, "private.example") || strings.Contains(result, "Synthetic title") {
			t.Fatal("private locator or card copy entered video evidence")
		}
		var value struct {
			Asset       string             `json:"asset_id"`
			Hash        string             `json:"sha256"`
			Observed    bool               `json:"content_observed"`
			Transcribed bool               `json:"audio_transcribed"`
			Metadata    map[string]float64 `json:"metadata"`
		}
		if err = json.Unmarshal([]byte(result), &value); err != nil || !stationAttachmentID(value.Asset) || value.Hash != hash || value.Observed || value.Transcribed || value.Metadata["total_frames"] != 5 || value.Metadata["width"] != 64 {
			t.Fatal(value, err)
		}
		stored, err := os.ReadFile(filepath.Join(root, "attachments", value.Asset, "original"))
		if err != nil || !bytes.Equal(stored, data) {
			t.Fatal("original cache mismatch", err)
		}
		assets[id] = value.Asset
	}
	if fetches.Load() != 2 || transfers.Load() != 1 {
		t.Fatal("wrong transfer method or repeated download", fetches.Load(), transfers.Load())
	}
	t.Run("local_ASR_for_ordinary_video_and_Channels", func(t *testing.T) {
		testStationLocalVideoASR(t, tools, assets, &blocked)
	})
	deny("rotates", "")
	deny("corrupt", "wechat_video_probe_failed")
	deny("revoked", "")
	blocked.Store(false)
	m := messages["forgotten"]
	in := companion.Input{Channel: "wechat", Account: m.AccountID, Thread: m.ThreadID, Sender: m.SenderID, TraceID: m.ID, MessageTime: m.CreatedAt, Text: "synthetic observed source"}
	cfg := companion.Defaults()
	cfg.Permissions.Remember = true
	if err = memory.Observe(in, cfg, now); err != nil {
		t.Fatal(err)
	}
	if err = memory.Clear(companion.ScopeID("wechat", "one", thread)); err != nil {
		t.Fatal(err)
	}
	before = fetches.Load()
	deny("forgotten", "")
	if fetches.Load() != before || mutations.Load() != 0 {
		t.Fatal("forgetting or no-send contract failed", fetches.Load(), mutations.Load())
	}
	entries, err := os.ReadDir(filepath.Join(root, "attachments"))
	// The automatic ordinary-video contract deliberately uses the current
	// opt-in grant. It therefore owns one distinct, verified cache record in
	// addition to the three records exercised by the manual read cases.
	if err != nil || len(entries) != 4 {
		t.Fatal("failed/changed source was published or staged data was left", entries, err)
	}
	var routes int
	var export string
	share := stationShareTestHost(t, root, func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
		routes++
		switch {
		case strings.Contains(endpoint, "get_object_short_link"):
			if headers["Cookie"] != "synthetic-assistant-cookie" {
				t.Fatal("assistant credential missing at proxy")
			}
			var request map[string]any
			_ = json.Unmarshal(body, &request)
			export, _ = request["exportId"].(string)
			if export == "export/synthetic-resolve-revoked" {
				blocked.Store(true)
			}
			return json.RawMessage(`{"errCode":0,"data":{"shortUrl":"https://weixin.qq.com/sph/synthetic"}}`), nil
		case strings.Contains(endpoint, "get_parse_result"):
			if headers["Cookie"] != "synthetic-yuanbao-cookie" {
				t.Fatal("Yuanbao credential missing at proxy")
			}
			return json.RawMessage(fmt.Sprintf(`{"code":0,"data":{"playable_url":%q}}`, "https://channels.weixin.qq.com/finder-preview/detail?token=synthetic-preview&eid="+export)), nil
		case strings.Contains(endpoint, "get_feed_info"):
			if headers["Cookie"] != "" {
				t.Fatal("credentials escaped their declared route")
			}
			return json.RawMessage(fmt.Sprintf(`{"errCode":0,"data":{"sceneInfo":{"dynamicExportId":%q},"feedInfo":{"objectId":"12345","videoUrl":%q,"decodeKey":%q}}}`, export, locator+"&case=resolved", key)), nil
		default:
			return nil, errors.New("unexpected outbound route")
		}
	})
	tools.shareHost = func() (*stationShareHost, error) { return share, nil }
	for _, id := range []string{"missing", "expired", "missing"} {
		result, err := call(id)
		if err != nil || !strings.Contains(result, hash) || strings.Contains(result, "private.example") || strings.Contains(result, key) {
			t.Fatal(id, result, err)
		}
	}
	if routes != 6 {
		t.Fatal("wrong route count or cache not reused", routes)
	}
	before = fetches.Load()
	deny("resolve-revoked", "")
	if routes != 7 || fetches.Load() != before {
		t.Fatal("revocation did not block later parsing and media requests", routes, fetches.Load(), before)
	}
	blocked.Store(false)
	expectedAutomaticFetches := int32(0)
	t.Run("LPAC_media_observation", func(t *testing.T) {
		if os.Getenv("AHS_SHARE_TEST_RUNTIME_ROOT") == "" {
			t.Skip("set AHS_SHARE_TEST_RUNTIME_ROOT to exercise the Windows LPAC media process")
		}
		value, err := call("card")
		if err != nil {
			t.Fatal(err)
		}
		var acquired struct {
			Asset string `json:"asset_id"`
		}
		if err := json.Unmarshal([]byte(value), &acquired); err != nil || acquired.Asset == "" {
			t.Fatal(value, err)
		}
		// The automatic turn uses an earlier attachment grant for this
		// fixture. It gets one separately scoped cached original.
		expectedAutomaticFetches = 1
		testStationVideoObservation(t, tools, share, acquired.Asset, hash, &blocked)
	})
	// One verified transfer belongs to the initial manual ordinary-video read;
	// the automatic observer must rebind it to its current opt-in grant.
	if mutations.Load() != 0 || fetches.Load() != before+expectedAutomaticFetches || transfers.Load() != 2 {
		t.Fatal("unexpected original fetch or send during observation", mutations.Load(), fetches.Load(), transfers.Load())
	}
}

func testStationLocalVideoASR(t *testing.T, tools *stationAttachmentTools, assets map[string]string, blocked *atomic.Bool) {
	t.Helper()
	const plugin = "media-understanding"
	ctx := t.Context()
	store, err := pluginhost.OpenStore(filepath.Join(t.TempDir(), "weChat-media"), "one")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prepared, err := store.Prepare(ctx, filepath.Join("..", "..", "plugins", plugin))
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Install(ctx, prepared.Token, prepared.Digest, []string{"artifact.read", "artifact.write", "model.invoke"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveConfiguration(ctx, plugin, pluginhost.ConfigUpdate{
		Values:  map[string]json.RawMessage{"asr_backend": json.RawMessage(`"local"`), "asr_model": json.RawMessage(`"synthetic-must-not-call"`)},
		Secrets: map[string]string{"asr_key": "synthetic-must-not-call"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetEnabled(ctx, plugin, item.Revision, true); err != nil {
		t.Fatal(err)
	}
	localCalls := 0
	host := &stationShareHost{store: store,
		postModel: func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
			t.Fatal("local backend tried the remote ASR route")
			return nil, nil
		},
		localASR: stationLocalASR{transcribe: func(_ context.Context, wav []byte, verify func() error) (string, error) {
			if len(wav) < 1000 || !bytes.HasPrefix(wav, []byte("RIFF")) || string(wav[8:12]) != "WAVE" {
				t.Fatal("runner did not receive actual PCM WAV")
			}
			if err := verify(); err != nil {
				return "", err
			}
			localCalls++
			switch localCalls {
			case 2:
				return "", &mediaspec.Failure{Code: "ASR_RESPONSE_HAVE_NO_WORDS"}
			case 3:
				blocked.Store(true)
				return "synthetic revoked transcript", verify()
			default:
				return "Synthetic local transcript.", nil
			}
		}},
	}
	copy := *tools
	copy.shareHost = func() (*stationShareHost, error) { return host, nil }
	defer blocked.Store(false)
	t.Run("automatic_ordinary_video_uses_verified_local_audio", func(t *testing.T) {
		message, err := copy.native.Message(ctx, "normal-video")
		if err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339Nano, message.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		bodyHash := sha256.Sum256([]byte(message.Body))
		source := &companion.SourceRef{
			Account: message.AccountID, Thread: message.ThreadID, Sender: message.SenderID,
			MessageID: message.ID, ServerID: message.ServerID, MessageType: message.Type,
			OccurredAt: message.CreatedAt, InstanceID: copy.store.SourceInstanceID(),
			BodyHash: fmt.Sprintf("%x", bodyHash), Revision: 1,
		}
		empty := ""
		input := companion.Input{
			Channel: "wechat", Account: message.AccountID, Thread: message.ThreadID, Sender: message.SenderID,
			TraceID: message.ID, MessageTime: message.CreatedAt, Source: source, OriginalText: &empty,
			Text: "synthetic ordinary video", Mode: "direct", IsGroup: true,
			IncomingMedia: []companion.IncomingMediaRef{{Source: *source, Kind: "video", UploaderID: message.SenderID}},
		}
		cfg := companion.Defaults()
		cfg.Permissions.Remember = true
		if err := copy.store.Observe(input, cfg, at); err != nil {
			t.Fatal(err)
		}
		stored, ok := copy.store.SourceForInput(input)
		if !ok || stored.EventID == "" {
			t.Fatal("ordinary video source was not stored", stored)
		}

		remoteCalls, localCalls := 0, 0
		autoHost := &stationShareHost{
			store: store,
			postModel: func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
				remoteCalls++
				return nil, errors.New("automatic ordinary video must not call a remote model")
			},
			localASR: stationLocalASR{transcribe: func(_ context.Context, wav []byte, verify func() error) (string, error) {
				if len(wav) < 1000 || !bytes.HasPrefix(wav, []byte("RIFF")) || string(wav[8:12]) != "WAVE" {
					t.Fatal("automatic observer did not decode the synthetic video audio to WAV")
				}
				if err := verify(); err != nil {
					return "", err
				}
				localCalls++
				return "Automatic ordinary video transcript.", nil
			}},
		}
		autoTools := copy
		autoTools.grants = map[string]time.Time{message.ThreadID: at.Add(-time.Minute)}
		autoTools.shareHost = func() (*stationShareHost, error) { return autoHost, nil }
		beforeOptIn := stationMediaObserver{tools: &autoTools, videoSince: at.Add(time.Second)}
		if beforeOptIn.SupportsMedia(ctx, message) {
			t.Fatal("ordinary video before the opt-in timestamp became eligible")
		}
		observer := stationMediaObserver{tools: &autoTools, videoSince: at.Add(-time.Second)}
		if !observer.SupportsMedia(ctx, message) {
			t.Fatal("opted-in ordinary video with local ASR was not eligible")
		}
		text, evidence, err := observer.AnalyzeWithEvidence(companion.WithMediaSource(ctx, *stored), message)
		if err != nil {
			t.Fatal(err)
		}
		if remoteCalls != 0 || localCalls != 1 || !strings.Contains(text, "[普通微信视频实际内容观察]") || !strings.Contains(text, "Automatic ordinary video transcript.") {
			t.Fatalf("text=%q local=%d remote=%d", text, localCalls, remoteCalls)
		}
		if len(evidence.Parts) != 1 || evidence.Parts[0].Kind != companion.ObservationAudioTranscript ||
			!strings.Contains(evidence.Parts[0].Excerpt, "Automatic ordinary video transcript.") || !strings.Contains(evidence.ObservedRange, "视频音轨") {
			t.Fatalf("unexpected typed evidence: %#v", evidence)
		}
	})
	observe := func(message, asset string) (string, error) {
		t.Helper()
		return copy.Call(ctx, "wechat_video_observe", json.RawMessage(fmt.Sprintf(`{"message_id":%q,"asset_id":%q,"mode":"audio","duration_seconds":0.5}`, message, asset)))
	}
	if out, err := observe("normal-video", assets["card"]); out != "" || err == nil || localCalls != 0 {
		t.Fatal("the local runner accepted a source mismatch", out, err)
	}
	if out, err := observe("normal-video", assets["normal-video"]); err != nil || !strings.Contains(out, `"asr_backend":"local"`) || !strings.Contains(out, "Synthetic local transcript.") {
		t.Fatal("ordinary video did not use the local runner", out, err)
	}
	if out, err := observe("card", assets["card"]); err != nil || !strings.Contains(out, `"transcription_state":"no_recognizable_speech"`) {
		t.Fatal("video card did not preserve the local no-speech result", out, err)
	}
	if out, err := observe("card", assets["card"]); out != "" || err == nil || localCalls != 3 || host.usage != nil {
		t.Fatal("revocation disclosed content or local ASR opened paid accounting", out, err, localCalls)
	}
	blocked.Store(false)
	host.localASR = stationLocalASR{}
	if out, err := observe("card", assets["card"]); out != "" || err == nil || err.Error() != "station_asr_host_unavailable" {
		t.Fatal("missing local ASR did not fail without fallback", out, err)
	}
}

func testStationVideoObservation(t *testing.T, tools *stationAttachmentTools, host *stationShareHost, asset, hash string, blocked *atomic.Bool) {
	const plugin = "media-understanding"
	ctx := t.Context()
	prepared, err := host.store.Prepare(ctx, filepath.Join("..", "..", "plugins", plugin))
	if err != nil {
		t.Fatal(err)
	}
	item, err := host.store.Install(ctx, prepared.Token, prepared.Digest, []string{"artifact.read", "artifact.write", "model.invoke"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = host.store.SaveConfiguration(ctx, plugin, pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{
		"vision_endpoint": json.RawMessage(`"https://provider.example/chat/completions"`),
		"vision_model":    json.RawMessage(`"synthetic-vision"`),
		"asr_endpoint":    json.RawMessage(`"https://provider.example/api/v1/services/aigc/multimodal-generation/generation"`),
		"asr_model":       json.RawMessage(`"synthetic-asr"`),
	}, Secrets: map[string]string{"vision_key": "synthetic-vision-key", "asr_key": "synthetic-asr-key"}})
	if err != nil {
		t.Fatal(err)
	}
	var vision, audio int
	host.postModel = func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
		switch {
		case strings.HasSuffix(endpoint, "/chat/completions"):
			vision++
			if headers["Authorization"] != "Bearer synthetic-vision-key" || !strings.Contains(string(body), "data:image/jpeg;base64,") || !strings.Contains(string(body), "synthetic-vision") || strings.Contains(string(body), asset) {
				t.Error("vision route lost binding or disclosed the asset identifier")
			}
			if strings.Contains(string(body), "synthetic-revoke") {
				blocked.Store(true)
			}
			if strings.Contains(string(body), "synthetic-length") {
				return json.RawMessage(`{"choices":[{"finish_reason":"length","message":{"content":"Partial blue frame."}}]}`), nil
			}
			return json.RawMessage(`{"choices":[{"finish_reason":"stop","message":{"content":"The frames are blue."}}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`), nil
		case strings.HasSuffix(endpoint, "/generation"):
			audio++
			var req struct {
				Input struct {
					Messages []struct {
						Content []struct {
							Audio struct {
								Data string `json:"data"`
							} `json:"input_audio"`
						}
					}
				}
			}
			if json.Unmarshal(body, &req) != nil || len(req.Input.Messages) != 1 || len(req.Input.Messages[0].Content) != 1 || headers["Authorization"] != "Bearer synthetic-asr-key" {
				t.Error("ASR route lost the expected audio or credential")
				return nil, errors.New("bad synthetic input")
			}
			encoded := strings.TrimPrefix(req.Input.Messages[0].Content[0].Audio.Data, "data:audio/wav;base64,")
			wav, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil || !bytes.HasPrefix(wav, []byte("RIFF")) || len(wav) < 1000 {
				t.Error("ASR payload was not actual PCM WAV")
			}
			if audio == 2 {
				return json.RawMessage(`{"code":"ASR_RESPONSE_HAVE_NO_WORDS"}`), nil
			}
			return json.RawMessage(`{"output":{"text":"Synthetic line."}}`), nil
		default:
			return nil, errors.New("unexpected media route")
		}
	}
	observe := func(message, id, args string) (map[string]any, error) {
		raw, err := tools.Call(ctx, "wechat_video_observe", json.RawMessage(fmt.Sprintf(`{"message_id":%q,"asset_id":%q,%s}`, message, id, args)))
		if err != nil {
			if raw != "" {
				t.Fatal("failed operation disclosed content")
			}
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(raw), &out); err != nil || out["sha256"] != hash || out["asset_id"] != asset || strings.Contains(raw, "provider.example") || strings.Contains(raw, "synthetic-vision-key") {
			t.Fatal(raw, err)
		}
		return out, nil
	}
	const first = `"mode":"frames","page_size":2`
	if _, err := observe("card", asset, first); err == nil || err.Error() != "media_plugin_not_enabled" {
		t.Fatal(err)
	}
	if err := host.controller.EnableOnDemand(ctx, plugin, item.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := observe("normal-video", asset, first); err == nil || vision != 0 {
		t.Fatal("asset reuse across sources", err)
	}
	for range 2 {
		out, err := observe("card", asset, first)
		if err != nil || out["next_start_frame"] != float64(2) || out["coverage"] != "consecutive_frames" || vision != 1 {
			t.Fatal(out, vision, err)
		}
		row := out["batches"].([]any)[0].(map[string]any)
		times := row["timestamps_seconds"].([]any)
		if len(times) != 2 || times[0] != float64(0) || times[1] != .2 || row["analysis"] != "The frames are blue." {
			t.Fatal(row)
		}
	}
	out, err := observe("card", asset, `"mode":"frames","start_frame":2,"step":2,"question":"synthetic-length"`)
	if err != nil || out["coverage"] != "sampled_frames" || out["end_of_frame_selection"] != true || !strings.Contains(fmt.Sprint(out["batches"]), "不完整") {
		t.Fatal(out, err)
	}
	out, err = observe("card", asset, `"mode":"audio","duration_seconds":0.5`)
	if err != nil || out["transcription_state"] != "transcribed" || out["text"] != "Synthetic line." || out["next_start_seconds"] != .5 || out["audio_complete"] != false {
		t.Fatal(out, err)
	}
	out, err = observe("card", asset, `"mode":"audio","start_seconds":0.5`)
	if err != nil || out["transcription_state"] != "no_recognizable_speech" || out["text"] != nil || out["audio_complete"] != true || audio != 2 {
		t.Fatal(out, audio, err)
	}

	t.Run("automatic_group_share_uses_original_and_verified_transcript", func(t *testing.T) {
		card, err := tools.native.Message(t.Context(), "card")
		if err != nil {
			t.Fatal(err)
		}
		stamp, err := time.Parse(time.RFC3339Nano, card.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		copy := *tools
		copy.grants = map[string]time.Time{card.ThreadID: stamp.Add(-time.Minute)}
		source := &companion.SourceRef{Account: card.AccountID, Thread: card.ThreadID, Sender: card.SenderID,
			MessageID: card.ID, ServerID: card.ServerID, MessageType: card.Type, OccurredAt: card.CreatedAt,
			InstanceID: copy.store.SourceInstanceID(), BodyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(card.Body))), Revision: 1}
		original := ""
		in := companion.Input{Channel: "wechat", Account: card.AccountID, Thread: card.ThreadID, Sender: card.SenderID, TraceID: card.ID,
			Source: source, OriginalText: &original, Text: "synthetic media placeholder", MessageTime: card.CreatedAt, Mode: "direct", IsGroup: true, GroupShareSummary: true,
			IncomingMedia: []companion.IncomingMediaRef{{Source: *source, Kind: "share", UploaderID: card.SenderID}}}
		config := companion.Defaults()
		config.Permissions.Remember = true
		if err := copy.store.Observe(in, config, time.Now()); err != nil {
			t.Fatal(err)
		}
		beforeAudio, beforeVision := audio, vision
		summary, evidence, err := copy.readGroupShareEvidence(t.Context(), in, card)
		if err != nil || !strings.Contains(summary, "Synthetic line.") || strings.Contains(summary, "Synthetic title") ||
			len(evidence.Parts) != 1 || evidence.Parts[0].Kind != companion.ObservationAudioTranscript ||
			audio != beforeAudio+1 || vision != beforeVision {
			t.Fatalf("summary %q, evidence %#v, audio %d, vision %d: %v", summary, evidence, audio-beforeAudio, vision-beforeVision, err)
		}
		changed := card
		changed.Body += " altered"
		if result, _, err := copy.readGroupShareEvidence(t.Context(), in, changed); result != "" || err == nil || audio != beforeAudio+1 {
			t.Fatalf("changed source reached the observer: %q, %v", result, err)
		}
	})
	if _, err = observe("card", asset, `"mode":"frames","question":"synthetic-revoke"`); err == nil || vision != 3 {
		t.Fatal("in-flight revocation returned observation", vision, err)
	}
}

func stationShareTestHost(t *testing.T, root string, send pluginruntime.RouteTransport) *stationShareHost {
	t.Helper()
	ctx := t.Context()
	store, err := pluginhost.OpenStore(filepath.Join(root, "share-fixture"), "one")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	prepared, err := store.Prepare(ctx, filepath.Join("..", "..", "plugins", wechatapp.ShareReaderToolID))
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.Install(ctx, prepared.Token, prepared.Digest, []string{"network"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SaveConfiguration(ctx, wechatapp.ShareReaderToolID, pluginhost.ConfigUpdate{Secrets: map[string]string{"yuanbao_cookie": "synthetic-yuanbao-cookie", "assistant_cookie": "synthetic-assistant-cookie"}}); err != nil {
		t.Fatal(err)
	}
	var executor pluginhost.Executor = stationShareTestExecutor{}
	if runtimeRoot := os.Getenv("AHS_SHARE_TEST_RUNTIME_ROOT"); runtimeRoot != "" {
		python := filepath.Join(runtimeRoot, "windows-python-cli")
		options := pluginruntime.Options{Python: filepath.Join(python, "python.exe"), Pythonw: filepath.Join(python, "pythonw.exe"), WindowsHelper: filepath.Join(runtimeRoot, "windows-agent-host", "windows_plugin_host.pyw"), RuntimeRoots: []string{python}}
		if !stationruntime.ToolRuntimeReady(options, "") {
			t.Fatal("test LPAC runtime unavailable")
		}
		executor = pluginruntime.InstalledExecutor(store, options)
	}
	controller, err := pluginruntime.NewController(store, executor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close() })
	if err = controller.EnableOnDemand(ctx, wechatapp.ShareReaderToolID, item.Revision); err != nil {
		t.Fatal(err)
	}
	return &stationShareHost{store: store, controller: controller, send: send}
}

type stationShareTestExecutor struct{}

func (e stationShareTestExecutor) Start(context.Context, pluginhost.Package, pluginhost.Grant) (pluginhost.Session, error) {
	return e, nil
}
func (stationShareTestExecutor) Health(ctx context.Context) error { return ctx.Err() }
func (stationShareTestExecutor) Close() error                     { return nil }
func (stationShareTestExecutor) Call(ctx context.Context, _ pluginhost.Invocation, _ string, raw json.RawMessage) (json.RawMessage, error) {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	export, _ := args["export_id"].(string)
	const link = "https://weixin.qq.com/sph/synthetic"
	call := func(route string, body map[string]any, referer string) (json.RawMessage, error) {
		raw, _ := json.Marshal(map[string]any{"route": route, "body": body, "referer": referer, "timeout_seconds": 25, "max_bytes": 2 << 20})
		return pluginruntime.InvokeHostService(ctx, "wechat/http_route", raw)
	}
	if _, err := call("channels-shortlink", map[string]any{"exportId": export, "nonceId": args["nonce_id"], "scene": 40}, ""); err != nil {
		return nil, err
	}
	if _, err := call("channels-parse", map[string]any{"type": "video_channel_url", "url": link, "scene": 1}, ""); err != nil {
		return nil, err
	}
	feed, err := call("channels-feed", map[string]any{"baseReq": map[string]string{"generalToken": "synthetic-preview"}, "exportId": export}, "https://channels.weixin.qq.com/finder-preview/detail?token=synthetic-preview&eid="+export)
	if err != nil {
		return nil, err
	}
	var packet struct {
		Data struct {
			Feed map[string]string `json:"feedInfo"`
		} `json:"data"`
	}
	if err = json.Unmarshal(feed, &packet); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"ok": true, "platform": "channels", "source_url": link, "object_id": "12345", "title": "", "author": "", "private_media_locators": []string{packet.Data.Feed["videoUrl"]}, "private_decode_key": packet.Data.Feed["decodeKey"], "coverage": "media_locator_only", "video_observed": false, "media_downloaded": false, "untrusted_external_content": true})
}

func TestStationVideoDecoderUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := mediadecode.Probe(t.Context(), "irrelevant.mp4")
	if err == nil || stationProbeError(t.Context(), err).Error() != "wechat_video_decoder_unavailable" {
		t.Fatal(err)
	}
}
