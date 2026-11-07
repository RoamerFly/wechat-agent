package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/pluginhost"
)

const syntheticBridgeFeed = `{"code":0,"data":{"object":{"id":"12345","contact":{"nickname":"synthetic author"},"objectDesc":{"description":"synthetic bridge video","media":[{"url":"https://video.example.test/object.mp4","urlToken":"","decodeKey":"18446744073709551615"}]}}}}`

func configureReceivedBridge(t *testing.T, p *receivedSharePolicy, ctx context.Context, address string) pluginhost.Job {
	t.Helper()
	i, err := p.store.Installed(ctx, "share-reader")
	if err != nil {
		t.Fatal(err)
	}
	if i, err = p.store.SetEnabled(ctx, "share-reader", i.Revision, false); err != nil {
		t.Fatal(err)
	}
	previous, err := p.store.Configuration(ctx, "share-reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.store.SaveConfiguration(ctx, "share-reader", pluginhost.ConfigUpdate{
		Revision: previous.Revision,
		Values:   map[string]json.RawMessage{"object_bridge_url": json.RawMessage(strconv.Quote(address))},
		Secrets:  map[string]string{"yuanbao_cookie": "synthetic=secret"},
	}); err != nil {
		t.Fatal(err)
	}
	i, err = p.store.Installed(ctx, "share-reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.store.SetEnabled(ctx, "share-reader", i.Revision, true); err != nil {
		t.Fatal(err)
	}
	configuration, err := p.store.Configuration(ctx, "share-reader")
	if err != nil {
		t.Fatal(err)
	}
	i, err = p.store.Installed(ctx, "share-reader")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := newShareRouteBinding(p.message, p.manager.native.(*receivedPluginNative).source, p.source, "bridge")
	if err != nil {
		t.Fatal(err)
	}
	p.binding = binding
	return pluginhost.Job{Plugin: "share-reader", Version: i.Package.Version, PackageDigest: i.Digest, ConfigRevision: strconv.Itoa(configuration.Revision), Tool: "share.resolve_channels", Context: p.invocation()}
}

func TestChannelsBridgeUsesInstalledPluginConfigAndLoopbackOnly(t *testing.T) {
	_, _, p, ctx := receivedPolicyFixture(t, false)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/channels/feed/profile" || r.URL.Query().Get("object_id") != "12345" || r.URL.Query().Get("nonce_id") != "" {
			t.Errorf("unexpected bounded bridge request: %s %s", r.Method, r.URL.String())
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(syntheticBridgeFeed))
	}))
	defer server.Close()
	job := configureReceivedBridge(t, p, ctx, server.URL)
	job.Arguments = json.RawMessage(`{"object_id":"12345","timeout_seconds":90}`)

	result, err := p.Service(ctx, job, pluginhost.Capability{ID: job.Tool}, "wechat/channels_bridge", json.RawMessage(`{"object_id":"12345","timeout_seconds":90}`))
	if err != nil || requests.Load() != 1 {
		t.Fatalf("installed loopback bridge failed: calls=%d err=%v", requests.Load(), err)
	}
	var received struct {
		SourceURL string          `json:"source_url"`
		Feed      json.RawMessage `json:"feed"`
	}
	if json.Unmarshal(result, &received) != nil || received.SourceURL != server.URL || string(received.Feed) != syntheticBridgeFeed {
		t.Fatalf("unexpected bridge result: %s", result)
	}
	if _, err = p.Service(ctx, job, pluginhost.Capability{ID: job.Tool}, "wechat/channels_bridge", json.RawMessage(`{"object_id":"12345","timeout_seconds":90}`)); err == nil || requests.Load() != 1 {
		t.Fatalf("duplicate bridge callback reached transport: calls=%d err=%v", requests.Load(), err)
	}
}

func TestChannelsBridgeRejectsForeignArgumentsBeforeTransport(t *testing.T) {
	_, _, p, ctx := receivedPolicyFixture(t, false)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(syntheticBridgeFeed))
	}))
	defer server.Close()
	job := configureReceivedBridge(t, p, ctx, server.URL)
	if _, err := p.Service(ctx, job, pluginhost.Capability{ID: job.Tool}, "wechat/channels_bridge", json.RawMessage(`{"object_id":"99999"}`)); err == nil || requests.Load() != 0 {
		t.Fatalf("foreign bridge arguments reached transport: calls=%d err=%v", requests.Load(), err)
	}
}

func TestChannelsBridgeCannotExpandJobTimeoutBeforeTransport(t *testing.T) {
	_, _, p, ctx := receivedPolicyFixture(t, false)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(syntheticBridgeFeed))
	}))
	defer server.Close()
	job := configureReceivedBridge(t, p, ctx, server.URL) // no explicit timeout: 25 seconds
	if _, err := p.Service(ctx, job, pluginhost.Capability{ID: job.Tool}, "wechat/channels_bridge", json.RawMessage(`{"object_id":"12345","timeout_seconds":90}`)); err == nil || requests.Load() != 0 {
		t.Fatalf("callback expanded the host deadline: calls=%d err=%v", requests.Load(), err)
	}
}

func TestChannelsBridgeRejectsChangedConfigurationSnapshot(t *testing.T) {
	_, _, p, ctx := receivedPolicyFixture(t, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(syntheticBridgeFeed)) }))
	defer server.Close()
	job := configureReceivedBridge(t, p, ctx, server.URL)

	// A new config revision is created while the plugin is stopped.  The old
	// callback snapshot must not be accepted after it is enabled again.
	i, err := p.store.Installed(ctx, "share-reader")
	if err != nil {
		t.Fatal(err)
	}
	if i, err = p.store.SetEnabled(ctx, "share-reader", i.Revision, false); err != nil {
		t.Fatal(err)
	}
	configuration, err := p.store.Configuration(ctx, "share-reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.store.SaveConfiguration(ctx, "share-reader", pluginhost.ConfigUpdate{Revision: configuration.Revision, Values: map[string]json.RawMessage{"object_bridge_url": json.RawMessage(strconv.Quote(server.URL))}, Secrets: map[string]string{"yuanbao_cookie": "changed=secret"}}); err != nil {
		t.Fatal(err)
	}
	i, _ = p.store.Installed(ctx, "share-reader")
	if _, err = p.store.SetEnabled(ctx, "share-reader", i.Revision, true); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Service(ctx, job, pluginhost.Capability{ID: job.Tool}, "wechat/channels_bridge", json.RawMessage(`{"object_id":"12345"}`)); err == nil {
		t.Fatal("stale configuration snapshot accepted")
	}
}

func TestConfiguredChannelsBridgeRejectsNonLoopback(t *testing.T) {
	_, _, p, ctx := receivedPolicyFixture(t, false)
	job := configureReceivedBridge(t, p, ctx, "http://example.test:8080")
	if _, err := p.Service(context.Background(), job, pluginhost.Capability{ID: job.Tool}, "wechat/channels_bridge", json.RawMessage(`{"object_id":"12345"}`)); err == nil {
		t.Fatal("non-loopback bridge accepted")
	}
}

func TestConfiguredChannelsBridgeNeverReadsLegacyBridgeSetting(t *testing.T) {
	f, _, p, ctx := receivedPolicyFixture(t, false)
	if err := os.WriteFile(filepath.Join(f.dir, "channels-config.json"), []byte(`{"enabled":true,"object_bridge_url":"http://127.0.0.1:8080"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if bridge, found, err := wechatapp.ConfiguredChannelsBridge(ctx, p.store); err != nil || found || bridge != "" {
		t.Fatalf("legacy bridge remained authoritative: bridge=%q found=%t err=%v", bridge, found, err)
	}
}

func TestChannelsBridgeRechecksNativeSourceAfterTransport(t *testing.T) {
	_, native, p, ctx := receivedPolicyFixture(t, false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		native.msg.Body = "changed while bridge request was in flight"
		_, _ = w.Write([]byte(syntheticBridgeFeed))
	}))
	defer server.Close()
	job := configureReceivedBridge(t, p, ctx, server.URL)
	if _, err := p.Service(ctx, job, pluginhost.Capability{ID: job.Tool}, "wechat/channels_bridge", json.RawMessage(`{"object_id":"12345"}`)); err == nil {
		t.Fatal("changed native source accepted after bridge response")
	}
}
