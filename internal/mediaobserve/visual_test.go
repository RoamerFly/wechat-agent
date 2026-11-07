package mediaobserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"testing"
	"time"

	"wechat-bot/internal/billing"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

type visualTestExecutor struct {
	pluginhost.Session
}

func (e visualTestExecutor) Start(context.Context, pluginhost.Package, pluginhost.Grant) (pluginhost.Session, error) {
	return e.Session, nil
}

type visualTestSession struct {
	calls int
}

func (*visualTestSession) Health(context.Context) error { return nil }
func (*visualTestSession) Close() error                 { return nil }
func (s *visualTestSession) Call(ctx context.Context, _ pluginhost.Invocation, tool string, raw json.RawMessage) (json.RawMessage, error) {
	s.calls++
	var args struct {
		Prompt    string
		Images    []ImageReference
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || tool != "media.observe" || len(args.Images) != 1 {
		return nil, errors.New("unexpected visual call")
	}
	ref := args.Images[0]
	request, err := json.Marshal(map[string]any{
		"route": "vision", "max_bytes": 4096, "timeout_seconds": 10,
		"body": map[string]any{"model": "synthetic-vision", "stream": false, "max_tokens": args.MaxTokens, "messages": []any{
			map[string]any{"role": "system", "content": "Describe only visible content."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": args.Prompt},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": map[string]any{"$media_ref": ref}, "detail": "auto"}},
			}},
		}},
	})
	if err != nil {
		return nil, err
	}
	if _, err := pluginruntime.InvokeHostService(ctx, "wechat/model_request", request); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"ok": true, "observation": "synthetic pixels", "status": "observed", "sources": args.Images,
		"coverage": "provided_images_only", "untrusted_external_content": true, "audio_observed": false,
		"requested_model": "synthetic-vision", "requested_max_tokens": args.MaxTokens, "thinking": false,
	})
}

func TestVisualIndependentRuntimeRecoversOnceAndRechecksOrigin(t *testing.T) {
	ctx := t.Context()
	store, err := pluginhost.OpenStore(t.TempDir(), "synthetic-connection")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	pkg, err := store.Prepare(ctx, "../../plugins/media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(ctx, pkg.Token, pkg.Digest, pkg.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveConfiguration(ctx, pkg.Package.ID, pluginhost.ConfigUpdate{
		Values:  map[string]json.RawMessage{"vision_model": json.RawMessage(`"synthetic-vision"`), "vision_endpoint": json.RawMessage(`"https://provider.example/chat/completions"`)},
		Secrets: map[string]string{"vision_key": "synthetic-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetEnabled(ctx, pkg.Package.ID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	ledger, err := billing.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var pngBytes bytes.Buffer
	if err = png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	session := &visualTestSession{}
	start := func() *pluginruntime.Controller {
		c, err := pluginruntime.NewController(store, visualTestExecutor{Session: session})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	controller := start()
	authorized := true
	paidCalls := 0
	newPolicy := func() *Visual {
		p, err := NewVisual(ctx, store, 4, "Observe a synthetic image", []deepseek.Image{{Data: pngBytes.Bytes()}}, 120)
		if err != nil {
			t.Fatal(err)
		}
		p.Bind(VisualBinding{
			Invocation: func() pluginhost.Invocation {
				return pluginhost.Invocation{Account: store.Account(), Thread: "synthetic-source", Requester: "owner", Purpose: "observe image", RequestKey: "synthetic-visual-once", Authorization: "source-proof", SourceVersions: map[string]int{"image": 4}}
			},
			CheckOrigin: func(context.Context, pluginhost.Job, pluginhost.Capability) error {
				if !authorized {
					return errors.New("source_revoked")
				}
				return nil
			},
			Controller: func() (*pluginruntime.Controller, error) { return controller, nil },
			Billing:    ledger,
			Transport: func(_ context.Context, _ string, _ map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
				paidCalls++
				if !bytes.Contains(body, []byte("data:image/png;base64,")) || bytes.Contains(body, []byte("$media_ref")) {
					t.Fatal("host did not resolve the verified image")
				}
				return json.RawMessage(`{"choices":[{"message":{"content":"synthetic pixels"},"finish_reason":"stop"}]}`), nil
			},
		})
		return p
	}
	if got, err := newPolicy().Observe(ctx); err != nil || got != "synthetic pixels" {
		t.Fatalf("first call failed: %q %v", got, err)
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	controller = start()
	recovered := newPolicy()
	if got, err := recovered.Observe(ctx); err != nil || got != "synthetic pixels" || session.calls != 1 || paidCalls != 1 || ledger.Totals().Requests != 1 {
		t.Fatalf("recovery replayed a paid call or lost the result: %q %v %d %d %d", got, err, session.calls, paidCalls, ledger.Totals().Requests)
	}
	authorized = false
	if err := recovered.CheckCompleted(ctx); err == nil || err.Error() != "source_revoked" {
		t.Fatalf("revoked source remained readable: %v", err)
	}
}
