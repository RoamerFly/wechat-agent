package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
)

func TestLocalASRPluginRegistrationUsesBoundResourceRootsAndMetadata(t *testing.T) {
	f, _, policy, ctx := mediaPolicyFixture(t, false)
	p, g, expected := writeRegisteredLocalASRProfile(t, f.manager)
	p, g = configureLocalASRProfileForTest(t, policy.store, ctx, expected.Initializer.ProfileID)
	resources, err := f.manager.localASRPluginResources(context.Background(), p, g)
	if err != nil || len(resources) != 4 {
		t.Fatal("registered local ASR resources unavailable", err, resources)
	}
	for _, resource := range resources {
		relative, relErr := filepath.Rel(expected.Public, resource.Root)
		if resource.Account != "test" || resource.Plugin != p.ID || resource.PackageVersion != p.Version || resource.ManifestSHA256 != expected.ResourceVersion || relErr != nil || !filepath.IsLocal(relative) {
			t.Fatal("resource binding widened or changed", resource)
		}
		if resource.Root == expected.Public {
			t.Fatal("entire public workspace was mounted")
		}
	}
	metadata, err := f.manager.localASRPluginMetadata(context.Background(), p, g)
	if err != nil || len(metadata) != 1 || metadata["local_asr"] == nil {
		t.Fatal("trusted initializer metadata unavailable", err, metadata)
	}
	var got localasr.Initializer
	if json.Unmarshal(metadata["local_asr"], &got) != nil || got != expected.Initializer {
		t.Fatal("initializer metadata changed", got, expected.Initializer)
	}
	if got.ProfileID == "" || got.ResourceVersion == "" {
		t.Fatal("incomplete local runner contract", got)
	}
	foreign := g
	foreign.Account = "other-account"
	if resources, err := f.manager.localASRPluginResources(context.Background(), p, foreign); err != nil || len(resources) != 0 {
		t.Fatal("foreign account received local ASR mount", err, resources)
	}
	wrongPlugin := p
	wrongPlugin.ID = "music"
	if resources, err := f.manager.localASRPluginResources(context.Background(), wrongPlugin, g); err != nil || len(resources) != 0 {
		t.Fatal("other plugin received local ASR mount", err, resources)
	}
}

func TestLocalASRPluginRegistrationFailsClosedOnTamperedContracts(t *testing.T) {
	f, _, policy, ctx := mediaPolicyFixture(t, false)
	p, g, expected := writeRegisteredLocalASRProfile(t, f.manager)
	p, g = configureLocalASRProfileForTest(t, policy.store, ctx, expected.Initializer.ProfileID)
	if err := os.WriteFile(expected.VAD, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if resources, err := f.manager.localASRPluginResources(context.Background(), p, g); err != nil || len(resources) != 0 {
		t.Fatal("tampered VAD was registered", err, resources)
	}
	if metadata, err := f.manager.localASRPluginMetadata(context.Background(), p, g); err != nil || len(metadata) != 0 {
		t.Fatal("tampered registration reached initializer", err, metadata)
	}
	if err := os.WriteFile(expected.VAD, []byte("synthetic-vad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(expected.SDKFile, []byte("tampered-sdk"), 0600); err != nil {
		t.Fatal(err)
	}
	if resources, err := f.manager.localASRPluginResources(context.Background(), p, g); err != nil || len(resources) != 0 {
		t.Fatal("tampered SDK was registered", err, resources)
	}
	manifest, err := os.ReadFile(expected.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if json.Unmarshal(manifest, &parsed) != nil {
		t.Fatal("invalid synthetic manifest")
	}
	parsed["model"] = filepath.Join(t.TempDir(), "foreign.onnx")
	updated, _ := json.Marshal(parsed)
	if err := os.WriteFile(expected.Manifest, updated, 0600); err != nil {
		t.Fatal(err)
	}
	if resources, err := f.manager.localASRPluginResources(context.Background(), p, g); err != nil || len(resources) != 0 {
		t.Fatal("external model was registered", err, resources)
	}
}

func TestLocalASRProfileSelectionComesOnlyFromMediaConfiguration(t *testing.T) {
	f, _, policy, ctx := mediaPolicyFixture(t, false)
	p, g, expected := writeRegisteredLocalASRProfile(t, f.manager)
	p, g = configureLocalASRProfileForTest(t, policy.store, ctx, expected.Initializer.ProfileID)

	// A stale legacy speech setting cannot select, replace, or disable the
	// media plugin's explicitly selected profile.
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.ASRLocalModel = "not-a-media-profile"
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	metadata, err := f.manager.localASRPluginMetadata(ctx, p, g)
	if err != nil || metadata["local_asr"] == nil {
		t.Fatal("legacy speech setting changed the selected media profile", err, metadata)
	}

	installed, err := policy.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	installed, err = policy.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := policy.store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]json.RawMessage, len(configuration.Values))
	for key, value := range configuration.Values {
		values[key] = append(json.RawMessage(nil), value...)
	}
	delete(values, "local_asr_profile_id")
	if _, err = policy.store.SaveConfiguration(ctx, localasr.PluginID, pluginhost.ConfigUpdate{Revision: configuration.Revision, Values: values}); err != nil {
		t.Fatal(err)
	}
	installed, err = policy.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	if resources, err := f.manager.localASRPluginResources(ctx, p, g); err != nil || len(resources) != 0 {
		t.Fatal("missing media profile selection fell back to a legacy/default profile", err, resources)
	}
}

// configureLocalASRProfileForTest follows the same enabled/configured path as
// production. A legacy speech setting never makes a local profile visible.
func configureLocalASRProfileForTest(t *testing.T, store *pluginhost.Store, ctx context.Context, id string) (pluginhost.Package, pluginhost.Grant) {
	t.Helper()
	installed, err := store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Enabled {
		installed, err = store.SetEnabled(ctx, localasr.PluginID, installed.Revision, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	configuration, err := store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]json.RawMessage, len(configuration.Values)+2)
	for key, value := range configuration.Values {
		values[key] = append(json.RawMessage(nil), value...)
	}
	values["asr_backend"] = json.RawMessage(`"local"`)
	values["local_asr_profile_id"] = mustJSONRaw(t, id)
	if _, err = store.SaveConfiguration(ctx, localasr.PluginID, pluginhost.ConfigUpdate{Revision: configuration.Revision, Values: values, ClearSecrets: []string{"asr_key"}}); err != nil {
		t.Fatal(err)
	}
	installed, err = store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	installed, err = store.SetEnabled(ctx, localasr.PluginID, installed.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ConfigurationForExecution(ctx, localasr.PluginID); err != nil {
		t.Fatalf("configured local ASR profile was not executable: %v", err)
	}
	return installed.Package, pluginhost.Grant{Account: store.Account(), Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}
}

func mustJSONRaw(t *testing.T, value string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type localASRTestProfile struct {
	Public, Manifest, VAD, SDKFile string
	ResourceVersion                string
	Initializer                    localasr.Initializer
}

func writeRegisteredLocalASRProfile(t *testing.T, manager *Manager) (pluginhost.Package, pluginhost.Grant, localASRTestProfile) {
	t.Helper()
	public := filepath.Join(manager.managedRoot(), "public_workspace")
	modelRoot := filepath.Join(public, "models", "asr", "sensevoice-small-int8")
	interpreterRoot := filepath.Join(public, "runtimes", "python")
	sdkRoot := filepath.Join(public, "runtimes", "sherpa")
	entryRoot := filepath.Join(public, "projects", "agent-tools")
	for _, dir := range []string{modelRoot, interpreterRoot, sdkRoot, entryRoot, filepath.Join(public, "manifests")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	python := filepath.Join(interpreterRoot, "python.exe")
	entry := filepath.Join(entryRoot, "local_asr.py")
	model, tokens := filepath.Join(modelRoot, "model.int8.onnx"), filepath.Join(modelRoot, "tokens.txt")
	vad := filepath.Join(public, "models", "silero_vad.onnx")
	sdkFile := filepath.Join(sdkRoot, "sherpa_onnx.pyd")
	for path, body := range map[string]string{python: "synthetic-python", entry: "synthetic-entry", model: "synthetic-model", tokens: "synthetic-tokens", vad: "synthetic-vad", sdkFile: "synthetic-sdk"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sdkSum := sha256.Sum256([]byte("synthetic-sdk"))
	runtimeManifest, _ := json.Marshal(map[string]any{"files": map[string]string{"sherpa_onnx.pyd": hex.EncodeToString(sdkSum[:])}})
	if err := os.WriteFile(filepath.Join(sdkRoot, "runtime-manifest.json"), runtimeManifest, 0600); err != nil {
		t.Fatal(err)
	}
	vadSum := sha256.Sum256([]byte("synthetic-vad"))
	manifest := localasr.Manifest{ID: "asr-sensevoice-small-int8", Root: modelRoot, Python: python, Entry: entry, Packages: sdkRoot, Model: model, Tokens: tokens, VADModel: vad, VADSHA256: hex.EncodeToString(vadSum[:]), VADThreshold: .5, Threads: 2, Language: "auto", ChunkSeconds: 30, TimeoutSeconds: 120}
	manifestRaw, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(public, "manifests", "asr-sensevoice-small-int8.json")
	if err := os.WriteFile(manifestPath, manifestRaw, 0600); err != nil {
		t.Fatal(err)
	}
	manifestSum := sha256.Sum256(manifestRaw)
	p := pluginhost.Package{ID: localasr.PluginID, Version: "0.1.1"}
	g := pluginhost.Grant{Account: "test", Plugin: p.ID, Version: p.Version}
	profile := localASRTestProfile{Public: public, Manifest: manifestPath, VAD: vad, SDKFile: sdkFile, ResourceVersion: hex.EncodeToString(manifestSum[:]), Initializer: localasr.Initializer{ProfileID: "sensevoice-small-int8", ResourceVersion: hex.EncodeToString(manifestSum[:])}}
	return p, g, profile
}
