package station

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
)

func newLocalVisionConfigurationFixture(t *testing.T, install bool) (*localHomeFixture, *localProjectToolHost) {
	t.Helper()
	f := newLocalHomeFixture(t, filepath.Join(t.TempDir(), "host"), filepath.Join(t.TempDir(), "home"))
	f.login()
	if code, out := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != 200 {
		t.Fatalf("initialize: %d %v", code, out)
	}
	identity, err := f.handler.home.identity.Read()
	if err != nil {
		t.Fatal(err)
	}
	host, err := f.handler.home.projectTools.forFish(identity.FishID)
	if err != nil {
		t.Fatal(err)
	}
	if install {
		prepared, err := host.store.Prepare(t.Context(), filepath.Join(stationMediaCatalog(t), localasr.PluginID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = host.store.Install(t.Context(), prepared.Token, prepared.Digest, []string{"artifact.read", "artifact.write", "model.invoke"}); err != nil {
			t.Fatal(err)
		}
	}
	return f, host
}

func syntheticLocalVisionInput(revision int) localVisionInput {
	return localVisionInput{Revision: revision, Configuration: &localVisionConfiguration{Endpoint: "https://vision.example/chat/completions", Model: "synthetic-vision", Backend: "deepseek", MaxTokens: 1024, Thinking: true, ReasoningEffort: "low"}, APIKey: "synthetic-private-vision-value", ApproveModel: true}
}

func localVisionAssertNoSecrets(t *testing.T, out map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(out)
	for _, secret := range []string{"synthetic-private-vision-value", "synthetic-private-asr-value", "synthetic-private-audio-value"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private secret returned", secret)
		}
	}
}

func TestLocalVisionConfigurationOwnerAuthAndUninstalledView(t *testing.T) {
	f := newLocalHomeFixture(t, filepath.Join(t.TempDir(), "host"), filepath.Join(t.TempDir(), "home"))
	if code, _ := f.request("GET", localVisionConfigurationRoute, nil, false); code != http.StatusUnauthorized {
		t.Fatal("unprotected visual configuration", code)
	}
	f.login()
	if code, _ := f.request("GET", localVisionConfigurationRoute, nil, false); code != 409 {
		t.Fatal("missing identity accepted", code)
	}
	if code, _ := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != 200 {
		t.Fatal(code)
	}
	code, view := f.request("GET", localVisionConfigurationRoute, nil, false)
	if code != 200 || view["available"] != false || view["ready"] != false || view["key_configured"] != false || view["reason"] == "" {
		t.Fatalf("uninstalled view: %d %v", code, view)
	}
	if code, _ := f.request("PUT", localVisionConfigurationRoute, syntheticLocalVisionInput(0), true); code != 409 {
		t.Fatal("configuration installed a missing tool", code)
	}
	f, _ = newLocalVisionConfigurationFixture(t, true)
	if code, _ := f.request("PUT", localVisionConfigurationRoute, syntheticLocalVisionInput(0), false); code != http.StatusForbidden {
		t.Fatal("missing CSRF accepted", code)
	}
}

func TestLocalVisionConfigurationPreservesASRAndEnabledState(t *testing.T) {
	f, host := newLocalVisionConfigurationFixture(t, true)
	asrValues := map[string]json.RawMessage{"asr_backend": json.RawMessage(`"local"`), "local_asr_profile_id": json.RawMessage(`"synthetic-profile"`), "audio_review_endpoint": json.RawMessage(`"https://audio.example/listen"`), "audio_review_model": json.RawMessage(`"synthetic-audio"`)}
	config, err := host.store.SaveConfiguration(t.Context(), localasr.PluginID, pluginhost.ConfigUpdate{Values: asrValues, Secrets: map[string]string{"asr_key": "synthetic-private-asr-value", "audio_review_key": "synthetic-private-audio-value"}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := host.store.Installed(t.Context(), localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	item, err = host.store.SetEnabled(t.Context(), localasr.PluginID, item.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	input := syntheticLocalVisionInput(config.Revision)
	code, view := f.request("PUT", localVisionConfigurationRoute, input, true)
	if code != 200 || view["key_configured"] != true || view["available"] != true || view["revision"] == float64(config.Revision) {
		t.Fatalf("save enabled visual config: %d %v", code, view)
	}
	localVisionAssertNoSecrets(t, view)
	after, err := host.store.Configuration(t.Context(), localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range asrValues {
		if !reflect.DeepEqual(after.Values[name], want) {
			t.Fatal("changed unrelated ASR/audio configuration", name)
		}
	}
	stillEnabled, err := host.store.Installed(t.Context(), localasr.PluginID)
	if err != nil || !stillEnabled.Enabled || stillEnabled.Revision != item.Revision || stillEnabled.Digest != item.Digest {
		t.Fatal("saving visual config changed tool activation or package", err)
	}
	if !reflect.DeepEqual(after.ConfiguredSecrets, []string{"asr_key", "audio_review_key", "vision_key"}) {
		t.Fatal("lost unrelated secrets", after.ConfiguredSecrets)
	}
	if code, _ := f.request("PUT", localVisionConfigurationRoute, input, true); code != 409 {
		t.Fatal("stale visual config overwrote newer version", code)
	}
	input.Revision, input.APIKey = after.Revision, ""
	input.Configuration.MaxTokens = 2048
	code, view = f.request("PUT", localVisionConfigurationRoute, input, true)
	if code != 200 || view["key_configured"] != true {
		t.Fatalf("blank key erased stored key: %d %v", code, view)
	}
	input.Revision, input.ClearKey = int(view["revision"].(float64)), true
	code, view = f.request("PUT", localVisionConfigurationRoute, input, true)
	if code != 200 || view["key_configured"] != false || view["ready"] != false {
		t.Fatalf("clear key failed: %d %v", code, view)
	}
	remaining, err := host.store.Configuration(t.Context(), localasr.PluginID)
	if err != nil || !reflect.DeepEqual(remaining.ConfiguredSecrets, []string{"asr_key", "audio_review_key"}) {
		t.Fatal("clear vision erased ASR key", remaining.ConfiguredSecrets, err)
	}
	code, get := f.request("GET", localVisionConfigurationRoute, nil, false)
	if code != 200 {
		t.Fatal(code)
	}
	localVisionAssertNoSecrets(t, get)
}

func TestLocalVisionConfigurationRejectsMalformedAndUndeclaredFields(t *testing.T) {
	f, host := newLocalVisionConfigurationFixture(t, true)
	for _, endpoint := range []string{"http://vision.example/chat/completions", "https://user:pass@vision.example/chat/completions", "https://vision.example/chat/completions?key=not-secret", "https://vision.example/chat/completions#fragment", "https://vision.example/images", "https://vision.example/chat%2fcompletions", "https://vision.example\\other/chat/completions"} {
		input := syntheticLocalVisionInput(0)
		input.Configuration.Endpoint = endpoint
		if code, _ := f.request("PUT", localVisionConfigurationRoute, input, true); code != 400 {
			t.Fatal("invalid endpoint accepted", endpoint, code)
		}
	}
	for _, mutate := range []func(*localVisionInput){
		func(i *localVisionInput) { i.ApproveModel = false }, func(i *localVisionInput) { i.Configuration = nil }, func(i *localVisionInput) { i.Revision = -1 },
		func(i *localVisionInput) { i.Configuration.Model = "unsafe\nmodel" }, func(i *localVisionInput) { i.Configuration.MaxTokens = 0 },
		func(i *localVisionInput) { i.Configuration.Backend = "unknown" }, func(i *localVisionInput) { i.Configuration.Backend = "openai-compatible" },
		func(i *localVisionInput) { i.Configuration.ReasoningEffort = "medium" }, func(i *localVisionInput) { i.APIKey = "unsafe\r\nvalue" }, func(i *localVisionInput) { i.ClearKey = true },
	} {
		input := syntheticLocalVisionInput(0)
		mutate(&input)
		if code, _ := f.request("PUT", localVisionConfigurationRoute, input, true); code != 400 {
			t.Fatal("invalid visual input accepted", code)
		}
	}
	for _, body := range []map[string]any{
		{"revision": 0, "configuration": map[string]any{"endpoint": "https://vision.example/chat/completions", "model": "synthetic", "backend": "deepseek", "max_tokens": 1, "reasoning_effort": "low", "asr_backend": "remote"}, "approve_model": true},
		{"revision": 0, "configuration": syntheticLocalVisionInput(0).Configuration, "approve_model": true, "secrets": map[string]string{"asr_key": "should-not-write"}},
	} {
		if code, _ := f.request("PUT", localVisionConfigurationRoute, body, true); code != 400 {
			t.Fatal("unknown fields accepted", code)
		}
	}
	config, err := host.store.Configuration(t.Context(), localasr.PluginID)
	if err != nil || config.Revision != 0 || len(config.ConfiguredSecrets) != 0 {
		t.Fatal("rejected input changed stored data", config, err)
	}
}

func TestLocalVisionConfigurationRefusesActiveAndPendingWork(t *testing.T) {
	f, host := newLocalVisionConfigurationFixture(t, true)
	s := f.handler.home.projectTools
	release := s.markActive(localasr.PluginID)
	if code, _ := f.request("PUT", localVisionConfigurationRoute, syntheticLocalVisionInput(0), true); code != 409 {
		t.Fatal("active media configuration changed", code)
	}
	release()
	item, err := host.store.Installed(t.Context(), localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.store.SetEnabled(t.Context(), localasr.PluginID, item.Revision, true); err != nil {
		t.Fatal(err)
	}
	_, _, err = host.store.CreateJob(t.Context(), pluginhost.Job{Plugin: localasr.PluginID, Version: item.Package.Version, PackageDigest: item.Digest, ConfigRevision: strconv.Itoa(0), Tool: "media.observe", Context: pluginhost.Invocation{Account: host.store.Account(), Thread: "synthetic-local-thread", Requester: "synthetic-owner", Purpose: "synthetic pending visual work", RequestKey: "pending"}, Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request("PUT", localVisionConfigurationRoute, syntheticLocalVisionInput(0), true); code != 409 {
		t.Fatal("pending media job ignored", code)
	}
	config, err := host.store.Configuration(context.Background(), localasr.PluginID)
	if err != nil || config.Revision != 0 {
		t.Fatal("pending work changed configuration", config, err)
	}
}
