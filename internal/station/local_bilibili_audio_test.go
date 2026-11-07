package station

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func TestLocalBilibiliAudioUsesOwnerScopedLocalASRProfile(t *testing.T) {
	root := t.TempDir()
	data, homeRoot, resources := filepath.Join(root, "station"), filepath.Join(root, "fish-home"), filepath.Join(root, "asr-resources")
	for _, path := range []string{data, homeRoot, resources, filepath.Join(resources, "public_workspace")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	home := &localHomeService{identity: fishidentity.New(data), root: homeRoot, dataDir: data, windows: &agenthost.WindowsConfig{Pythonw: filepath.Join(root, "pythonw.exe"), Helper: filepath.Join(root, "agent-host.pyw"), Spool: filepath.Join(root, "spool")}}
	record, err := home.identity.InitializeLocal()
	if err != nil || record.LocalOwner == nil {
		t.Fatalf("initialize local owner: %#v, %v", record, err)
	}
	home.projectTools = &localProjectTools{home: home, options: pluginruntime.Options{CatalogDirectory: stationMediaCatalog(t)}}
	t.Cleanup(func() { _ = home.projectTools.close() })
	writeStationASRProfile(t, resources, "sensevoice-small-int8")
	registration, _ := json.Marshal(localStationASRRegistration{SchemaVersion: 1, Enabled: true, Workspace: resources, ProfileID: "sensevoice-small-int8"})
	if err := os.WriteFile(filepath.Join(data, localStationASRRegistrationFile), registration, 0600); err != nil {
		t.Fatal(err)
	}

	host, err := home.projectTools.forFish(record.FishID)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := host.store.Prepare(t.Context(), filepath.Join(stationMediaCatalog(t), localasr.PluginID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.store.Install(t.Context(), prepared.Token, prepared.Digest, []string{"artifact.read", "artifact.write", "model.invoke"}); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(resources, "public_workspace", "models", "sensevoice-small-int8", "model.int8")
	configurationBefore, err := host.store.Configuration(t.Context(), localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(modelPath); err != nil {
		t.Fatal(err)
	}
	if err = home.projectTools.configureStationASR(t.Context(), host); err == nil {
		t.Fatal("missing ASR model must prevent configuration and enablement")
	}
	configurationAfter, err := host.store.Configuration(t.Context(), localasr.PluginID)
	if err != nil || configurationAfter.Revision != configurationBefore.Revision {
		t.Fatalf("failed ASR admission changed configuration: %#v, %v", configurationAfter, err)
	}
	if err = os.WriteFile(modelPath, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = home.projectTools.configureStationASR(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	installed, err := host.store.Installed(t.Context(), localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.store.SetEnabled(t.Context(), localasr.PluginID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	installed, err = host.store.Installed(t.Context(), localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}

	first, err := home.localStationASRBinding(t.Context())
	if err != nil || first.version == "" || first.account != record.FishID || first.profile.Metadata().ProfileID != "sensevoice-small-int8" {
		t.Fatalf("owner local ASR binding = %#v, %v", first, err)
	}
	if err = os.Remove(modelPath); err != nil {
		t.Fatal(err)
	}
	view, err := home.projectTools.projectView(t.Context(), host, localasr.PluginID, "本地媒体识别")
	if err != nil || view.RuntimeReady || view.State != "runtime_missing" || view.Reason == "" {
		t.Fatalf("missing ASR resources reported ready: %#v, %v", view, err)
	}
	if err = os.WriteFile(modelPath, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	configuration, err := host.store.Configuration(t.Context(), localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]json.RawMessage{}
	for key, value := range configuration.Values {
		values[key] = value
	}
	values["asr_backend"] = json.RawMessage(`"remote"`)
	if _, err = host.store.SetEnabled(t.Context(), localasr.PluginID, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	if _, err = host.store.SaveConfiguration(t.Context(), localasr.PluginID, pluginhost.ConfigUpdate{Revision: configuration.Revision, Values: values}); err != nil {
		t.Fatal(err)
	}
	installed, err = host.store.Installed(t.Context(), localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = host.store.SetEnabled(t.Context(), localasr.PluginID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	if _, err = home.localStationASRBinding(context.Background()); err == nil {
		t.Fatal("remote or changed backend must not remain available to Bilibili audio")
	}
}

func stationMediaCatalog(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func writeStationASRProfile(t *testing.T, workspace, id string) {
	t.Helper()
	public := filepath.Join(workspace, "public_workspace")
	root := filepath.Join(public, "models", id)
	packages := filepath.Join(public, "sdk", id)
	for _, path := range []string{root, packages, filepath.Join(public, "manifests")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	python, entry, model, tokens := filepath.Join(root, "python.exe"), filepath.Join(root, "legacy-entry.py"), filepath.Join(root, "model.int8"), filepath.Join(root, "tokens.txt")
	for _, path := range []string{python, entry, model, tokens} {
		if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := json.Marshal(localasr.Manifest{ID: "asr-" + id, Root: root, Python: python, Entry: entry, Packages: packages, Model: model, Tokens: tokens, Threads: 1, Language: "auto", ChunkSeconds: 30, TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(public, "manifests", "asr-"+id+".json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
}
