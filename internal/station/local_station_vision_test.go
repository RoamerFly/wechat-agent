package station

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func newStationVisionBridgeFixture(t *testing.T) (*localHomeService, []deepseek.Image, *int) {
	t.Helper()
	dir := t.TempDir()
	home := &localHomeService{dataDir: dir, root: filepath.Join(dir, "workspace"), identity: fishidentity.New(dir)}
	identity, err := home.identity.InitializeLocal()
	if err != nil {
		t.Fatal(err)
	}
	store, err := pluginhost.OpenStore(filepath.Join(dir, "project-tools", identity.FishID), identity.FishID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	prepared, err := store.Prepare(t.Context(), "../../plugins/media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(t.Context(), prepared.Token, prepared.Digest, []string{"artifact.read", "artifact.write", "model.invoke"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveConfiguration(t.Context(), installed.Package.ID, pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{"vision_endpoint": json.RawMessage(`"https://provider.example/chat/completions"`), "vision_model": json.RawMessage(`"synthetic-vision"`)}, Secrets: map[string]string{"vision_key": "synthetic-owner-visual-key"}})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := pluginruntime.NewController(store, stationImageObservationExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	if err := controller.EnableOnDemand(t.Context(), installed.Package.ID, installed.Revision); err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	home.projectTools = &localProjectTools{home: home, host: &localProjectToolHost{store: store, controller: controller}, visionPost: func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
		*calls++
		if endpoint != "https://provider.example/chat/completions" || headers["Authorization"] != "Bearer synthetic-owner-visual-key" || !bytes.Contains(body, []byte("data:image/png;base64,")) || bytes.Contains(body, []byte("$media_ref")) {
			return nil, errors.New("synthetic visual boundary mismatch")
		}
		return json.RawMessage(`{"choices":[{"finish_reason":"stop","message":{"content":"synthetic visible image"}}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`), nil
	}}
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 3))); err != nil {
		t.Fatal(err)
	}
	return home, []deepseek.Image{{Data: data.Bytes(), MIME: "image/png"}}, calls
}

func TestLocalStationVisionUsesFishToolAndReusesCompletedObservation(t *testing.T) {
	home, images, calls := newStationVisionBridgeFixture(t)
	version, err := home.localStationVisionVersion(t.Context())
	if err != nil || version == "" {
		t.Fatal(version, err)
	}
	allowed := true
	origin := func() error {
		if !allowed {
			return errors.New("synthetic_source_revoked")
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		text, err := home.observeLocalStationImages(t.Context(), "app:offline/video:1/frame:3", "Describe this exact synthetic frame", images, 120, origin)
		if err != nil || text != "synthetic visible image" {
			t.Fatalf("observation %d: %q %v", i, text, err)
		}
	}
	if *calls != 1 {
		t.Fatal("completed observation repeated provider work", *calls)
	}
	b, err := home.localStationVisionBinding(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if b.fish != b.host.store.Account() || b.fish == "one" {
		t.Fatal("borrowed connected account")
	}
	ledger, err := home.projectTools.localVisionUsage(b.fish)
	if err != nil || ledger.Totals().Requests != 1 {
		t.Fatal("actual attempts not counted once", err)
	}
	if _, err := home.projectTools.localVisionUsage("other-fish"); err == nil {
		t.Fatal("reused another fish billing scope")
	}
	allowed = false
	if text, err := home.observeLocalStationImages(t.Context(), "app:offline/video:1/frame:3", "Describe this exact synthetic frame", images, 120, origin); err == nil || text != "" || *calls != 1 {
		t.Fatal("revoked source returned cached observation")
	}
}

func TestLocalStationVisionLateProviderSuccessIsRejectedAfterSourceRevocation(t *testing.T) {
	home, images, calls := newStationVisionBridgeFixture(t)
	original := home.projectTools.visionPost
	allowed := true
	home.projectTools.visionPost = func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, limit int64, timeout time.Duration) (json.RawMessage, error) {
		reply, err := original(ctx, endpoint, headers, body, limit, timeout)
		allowed = false
		return reply, err
	}
	origin := func() error {
		if !allowed {
			return errors.New("synthetic_source_revoked")
		}
		return nil
	}
	text, err := home.observeLocalStationImages(t.Context(), "synthetic-source", "Observe actual pixels", images, 120, origin)
	if err == nil || text != "" || *calls != 1 {
		t.Fatal("late reply escaped source revocation", text, err)
	}
	if home.projectTools.isActive("media-understanding") {
		t.Fatal("visual lease was retained after failure")
	}
}

func TestLocalStationVisionDoesNotBorrowWeChatOrAcceptMissingSource(t *testing.T) {
	home, images, calls := newStationVisionBridgeFixture(t)
	if _, err := home.observeLocalStationImages(t.Context(), "", "Observe", images, 120, func() error { return nil }); err == nil {
		t.Fatal("missing source accepted")
	}
	b, err := home.localStationVisionBinding(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.host.store.Configuration(t.Context(), "media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.host.controller.ConfigureIdle(t.Context(), "media-understanding", pluginhost.ConfigUpdate{Revision: c.Revision, Values: c.Values, ClearSecrets: []string{"vision_key"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := home.localStationVisionVersion(t.Context()); err == nil || !strings.Contains(err.Error(), "not_configured") {
		t.Fatal("cleared local credential silently borrowed another service", err)
	}
	if *calls != 0 {
		t.Fatal("configuration performed model test")
	}
}
