package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaobserve"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

// TestStationWechatImageObservationUsesVerifiedIncomingImages is deliberately
// a host/contract test.  The visual runner and provider transport are both
// synthetic: it proves that ordinary image messages only reach a visual model
// after the native original, conversation binding, and source record have all
// been checked.  It must not require an LPAC installation or a real provider.
func TestStationWechatImageObservationUsesVerifiedIncomingImages(t *testing.T) {
	const account, thread = "one", "room@chatroom"
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Microsecond)
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL32QAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	payloads := map[string][]byte{"current": png, "quoted": png, "invalid": []byte("not-an-image")}
	messages := map[string]linuxnative.Message{}
	for id := range payloads {
		messages[id] = linuxnative.Message{ID: id, AccountID: account, ThreadID: thread, SenderID: "alice", ServerID: id + "-server", Type: 3, Direction: "incoming", Body: "synthetic " + id + " image", CreatedAt: now.Format(time.RFC3339Nano)}
	}
	foreign := messages["quoted"]
	foreign.ID, foreign.ThreadID, foreign.ServerID = "foreign", "other@chatroom", "foreign-server"
	messages[foreign.ID] = foreign
	notImage := messages["current"]
	notImage.ID, notImage.Type, notImage.ServerID, notImage.Body = "text", 1, "text-server", "not an image"
	messages[notImage.ID] = notImage

	var transfers, modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		if r.URL.Path == "/v1/status" {
			write(map[string]string{"serverId": "synthetic-image-dataset"})
			return
		}
		const messagePrefix = "/v1/accounts/one/messages/"
		if strings.HasPrefix(r.URL.Path, messagePrefix) {
			rest := strings.TrimPrefix(r.URL.Path, messagePrefix)
			id, suffix, _ := strings.Cut(rest, "/")
			message, ok := messages[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			switch r.Method + " " + suffix {
			case "GET ":
				write(message)
				return
			case "GET media":
				if message.Type != 3 {
					write(map[string]any{"items": []any{}})
					return
				}
				write(map[string]any{"items": []map[string]any{{"accountId": account, "messageId": id, "kind": "image", "variant": "original", "actualVariant": "original", "state": "available"}}})
				return
			case "POST media-transfers":
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["quality"] != "original" {
					t.Errorf("image transfer did not request original: %#v, %v", body, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				transfers.Add(1)
				write(map[string]any{"id": "download-" + id, "accountId": account, "kind": "download", "state": "available", "assetId": "native-" + id})
				return
			}
		}
		const mediaPrefix = "/v1/accounts/one/media/native-"
		if strings.HasPrefix(r.URL.Path, mediaPrefix) {
			id := strings.TrimPrefix(r.URL.Path, mediaPrefix)
			id = strings.TrimSuffix(id, "/content")
			data, ok := payloads[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/content") {
				_, _ = w.Write(data)
				return
			}
			sum := sha256.Sum256(data)
			write(map[string]any{"id": "native-" + id, "accountId": account, "messageId": id, "kind": "image", "state": "available", "variant": "original", "actualVariant": "original", "byteLength": len(data), "contentType": "image/png", "sha256": hex.EncodeToString(sum[:])})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	token := filepath.Join(root, "native-token")
	if err := os.WriteFile(token, []byte("synthetic-token"), 0600); err != nil {
		t.Fatal(err)
	}
	native, err := linuxnative.New(linuxnative.Config{URL: server.URL, Account: account, TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := companion.NewStore(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()

	refFor := func(id string) *companion.SourceRef {
		t.Helper()
		message := messages[id]
		sum := sha256.Sum256([]byte(message.Body))
		return &companion.SourceRef{Account: message.AccountID, Thread: message.ThreadID, Sender: message.SenderID, MessageID: message.ID, ServerID: message.ServerID,
			MessageType: message.Type, OccurredAt: message.CreatedAt, InstanceID: memory.SourceInstanceID(), BodyHash: hex.EncodeToString(sum[:]), Revision: 1}
	}
	observeSource := func(ref *companion.SourceRef) {
		t.Helper()
		message := messages[ref.MessageID]
		cfg := companion.Defaults()
		cfg.Permissions.Remember = true
		in := companion.Input{Channel: "wechat", Account: message.AccountID, Thread: message.ThreadID, Sender: message.SenderID, TraceID: message.ID, MessageTime: message.CreatedAt, Text: message.Body, Source: ref}
		if err := memory.Observe(in, cfg, now); err != nil {
			t.Fatal(err)
		}
	}
	currentRef, quotedRef := refFor("current"), refFor("quoted")
	observeSource(currentRef)
	observeSource(quotedRef)

	host := stationImageObservationHost(t, root, &modelCalls, 123)
	var blocked atomic.Bool
	tools := &stationAttachmentTools{native: native, store: memory, dir: root, blocked: blocked.Load, grants: map[string]time.Time{thread: now.Add(-time.Minute)}, shareHost: func() (*stationShareHost, error) { return host, nil }}
	tools.SetTrace(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: "owner", TraceID: "current", MessageTime: now.Format(time.RFC3339Nano), Source: currentRef, QuotedSource: quotedRef})
	discovered, err := tools.List(t.Context())
	if err != nil || !slices.ContainsFunc(discovered, func(tool mcptools.Tool) bool { return tool.Name == "wechat_image_observe" }) {
		t.Fatal("configured image observation is unavailable to the model", err)
	}

	call := func(message, question string, tokens int) (map[string]any, error) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"message_id": message, "question": question, "analysis_tokens": tokens})
		result, err := tools.Call(t.Context(), "wechat_image_observe", raw)
		if err != nil {
			return nil, err
		}
		if strings.Contains(result, "synthetic-vision-key") || strings.Contains(result, "provider.example") || strings.Contains(result, "native-") {
			t.Fatalf("private runtime detail leaked: %s", result)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(result), &out); err != nil {
			t.Fatal(err)
		}
		return out, nil
	}
	for _, test := range []struct{ message, question string }{{"current", "what is visible?"}, {"quoted", "describe the quoted image"}} {
		out, err := call(test.message, test.question, 123)
		if err != nil || !strings.Contains(fmt.Sprint(out), "synthetic visible image") || out["message_id"] != test.message {
			t.Fatalf("%s image observation: %#v, %v", test.message, out, err)
		}
	}
	storedCurrent, ok := memory.SourceForInput(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: "alice", TraceID: "current", MessageTime: now.Format(time.RFC3339Nano), Source: currentRef})
	if !ok {
		t.Fatal("current image source was not retained for automatic observation")
	}
	observer := stationMediaObserver{tools: tools, imageSince: now.Add(-time.Minute)}
	if !observer.SupportsMedia(t.Context(), messages["current"]) {
		t.Fatal("authorized current image was not available to the automatic observer")
	}
	text, evidence, err := observer.AnalyzeWithEvidence(companion.WithMediaSource(t.Context(), *storedCurrent), messages["current"])
	if err != nil || text != "synthetic visible image" || !companion.HasFirsthandObservation(evidence.Parts) || evidence.ObservedRange != "verified_image" {
		t.Fatalf("automatic image observation lacks verified evidence: text=%q evidence=%+v err=%v", text, evidence, err)
	}
	if (stationMediaObserver{tools: tools, imageSince: now.Add(time.Minute)}).SupportsMedia(t.Context(), messages["current"]) {
		t.Fatal("pre-cutover image unexpectedly became eligible for automatic observation")
	}
	if got := modelCalls.Load(); got != 3 {
		t.Fatalf("want current, quoted, and automatic visual calls, got %d", got)
	}
	beforeTransfers, beforeCalls := transfers.Load(), modelCalls.Load()
	if result, err := call("foreign", "must not read", 123); err == nil || result != nil {
		t.Fatalf("foreign image was readable: %#v, %v", result, err)
	}
	if result, err := call("text", "must not read", 123); err == nil || result != nil {
		t.Fatalf("non-image message was readable: %#v, %v", result, err)
	}
	if got := transfers.Load(); got != beforeTransfers {
		t.Fatalf("denied source downloaded media: before=%d after=%d", beforeTransfers, got)
	}
	if got := modelCalls.Load(); got != beforeCalls {
		t.Fatalf("denied source reached visual transport: before=%d after=%d", beforeCalls, got)
	}

	invalidRef := refFor("invalid")
	observeSource(invalidRef)
	tools.SetTrace(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: "owner", TraceID: "invalid", MessageTime: now.Format(time.RFC3339Nano), Source: invalidRef})
	beforeCalls = modelCalls.Load()
	if result, err := call("invalid", "must reject invalid image bytes", 123); err == nil || result != nil {
		t.Fatalf("invalid image reached a successful observation: %#v, %v", result, err)
	}
	if got := modelCalls.Load(); got != beforeCalls {
		t.Fatalf("invalid bytes reached visual transport: before=%d after=%d", beforeCalls, got)
	}
	tools.SetTrace(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: "owner", TraceID: "current", MessageTime: now.Format(time.RFC3339Nano), Source: currentRef})
	post := host.postModel
	host.postModel = func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
		result, err := post(ctx, endpoint, headers, body, limit, timeout)
		blocked.Store(true)
		return result, err
	}
	if result, err := call("current", "permission revoked during observation", 123); err == nil || result != nil || modelCalls.Load() != beforeCalls+1 {
		t.Fatalf("in-flight revocation leaked a visual result: %#v, %v, model calls=%d", result, err, modelCalls.Load())
	}
}

func stationImageObservationHost(t *testing.T, root string, calls *atomic.Int32, expectedTokens int) *stationShareHost {
	t.Helper()
	ctx := t.Context()
	store, err := pluginhost.OpenStore(filepath.Join(root, "image-tool"), "one")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	prepared, err := store.Prepare(ctx, filepath.Join("..", "..", "plugins", "media-understanding"))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(ctx, prepared.Token, prepared.Digest, []string{"artifact.read", "artifact.write", "model.invoke"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveConfiguration(ctx, "media-understanding", pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{
		"vision_endpoint": json.RawMessage(`"https://provider.example/chat/completions"`),
		"vision_model":    json.RawMessage(`"synthetic-vision"`),
		"max_tokens":      json.RawMessage(`123`),
	}, Secrets: map[string]string{"vision_key": "synthetic-vision-key"}}); err != nil {
		t.Fatal(err)
	}
	controller, err := pluginruntime.NewController(store, stationImageObservationExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	if err := controller.EnableOnDemand(ctx, "media-understanding", installed.Revision); err != nil {
		t.Fatal(err)
	}
	return &stationShareHost{store: store, controller: controller, postModel: func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
		calls.Add(1)
		if endpoint != "https://provider.example/chat/completions" || headers["Authorization"] != "Bearer synthetic-vision-key" || !bytes.Contains(body, []byte("data:image/png;base64,")) || !bytes.Contains(body, []byte(fmt.Sprintf(`"max_tokens":%d`, expectedTokens))) || bytes.Contains(body, []byte("native-")) {
			return nil, errors.New("synthetic_visual_transport_contract_failed")
		}
		return json.RawMessage(`{"choices":[{"finish_reason":"stop","message":{"content":"synthetic visible image"}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`), nil
	}}
}

type stationImageObservationExecutor struct{}

func (stationImageObservationExecutor) Start(context.Context, pluginhost.Package, pluginhost.Grant) (pluginhost.Session, error) {
	return stationImageObservationExecutor{}, nil
}
func (stationImageObservationExecutor) Health(ctx context.Context) error { return ctx.Err() }
func (stationImageObservationExecutor) Close() error                     { return nil }
func (stationImageObservationExecutor) Call(ctx context.Context, _ pluginhost.Invocation, tool string, raw json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Prompt    string                        `json:"prompt"`
		Images    []mediaobserve.ImageReference `json:"images"`
		MaxTokens int                           `json:"max_tokens"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || tool != "media.observe" || len(args.Images) != 1 || args.Prompt == "" || args.MaxTokens < 1 {
		return nil, errors.New("synthetic_visual_runner_contract_failed")
	}
	request, err := json.Marshal(map[string]any{
		"route":           "vision",
		"max_bytes":       4096,
		"timeout_seconds": 10,
		"body": map[string]any{
			"model":      "synthetic-vision",
			"stream":     false,
			"max_tokens": args.MaxTokens,
			"messages": []any{
				map[string]any{"role": "system", "content": "Describe only visible content."},
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": args.Prompt},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": map[string]any{"$media_ref": args.Images[0]}, "detail": "auto"}},
				}},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if _, err := pluginruntime.InvokeHostService(ctx, "wechat/model_request", request); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"ok": true, "observation": "synthetic visible image", "status": "observed", "sources": args.Images,
		"coverage": "provided_images_only", "untrusted_external_content": true, "audio_observed": false,
		"requested_model": "synthetic-vision", "requested_max_tokens": args.MaxTokens, "thinking": false})
}
