package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
)

// The existing registration fixture writes only public synthetic files. It
// does not install a package or open a Store; lock that property into this
// migration test so preview can never obtain resources by side effect.
func writeReadOnlyMigrationASRProfile(t *testing.T, manager *Manager, id string) localASRTestProfile {
	t.Helper()
	_, _, profile := writeRegisteredLocalASRProfile(t, manager)
	if id != profile.Initializer.ProfileID {
		raw, err := os.ReadFile(profile.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		var manifest localasr.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.ID = "asr-" + id
		raw, err = json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		profile.Manifest = filepath.Join(profile.Public, "manifests", manifest.ID+".json")
		if err := os.WriteFile(profile.Manifest, raw, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		profile.ResourceVersion = hex.EncodeToString(sum[:])
		profile.Initializer = localasr.Initializer{ProfileID: id, ResourceVersion: profile.ResourceVersion}
	}
	assertMigrationStoreAbsent(t, manager)
	return profile
}

func assertMigrationStoreAbsent(t *testing.T, manager *Manager) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(manager.dir, "plugin-host", "plugins.sqlite")); !os.IsNotExist(err) {
		t.Fatal("read-only resource verification opened a Store", err)
	}
}

func migrationRegistrationSnapshot(t *testing.T, public string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(public, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(public, path)
		if err != nil {
			return err
		}
		files[relative] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestPluginMigrationRegisteredLocalASRCanApplyDisabledThenEnable(t *testing.T) {
	useOfficialMigrationCatalog(t)
	for _, test := range []struct{ name, legacy, selected string }{
		{"default", "", "sensevoice-small-int8"}, {"custom", "sensevoice-custom", "sensevoice-custom"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := setup(t)
			f.login()
			legacy := writeLocalASRMigrationSource(t, f, test.legacy, "local", true)
			profile := writeReadOnlyMigrationASRProfile(t, f.manager, test.selected)
			resources := migrationRegistrationSnapshot(t, profile.Public)
			preview, err := f.manager.pluginMigrationPreview(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			step := previewStep(t, preview, localasr.PluginID)
			if step.Status != "ready_for_administrator_review" || len(step.RequiredMissing) != 0 || step.ProposedEnabled || !step.EligibleToEnable || step.ProfileID != test.selected || string(step.update.Values["local_asr_profile_id"]) != strconv.Quote(test.selected) || preview.Accounts[0].SourceHashes["local_asr_profile_resource_version"] != profile.ResourceVersion {
				t.Fatal("existing verified local registration did not unblock the reviewed plan", step, preview.Accounts[0].SourceHashes)
			}
			assertMigrationStoreAbsent(t, f.manager)
			if !reflect.DeepEqual(resources, migrationRegistrationSnapshot(t, profile.Public)) {
				t.Fatal("preview changed public registration")
			}
			body := map[string]any{"account": "test", "plugin": localasr.PluginID, "apply_source_hash": step.ApplySourceHash, "prepared_package_digest": step.Package.ExpectedDigest, "permissions": step.Permissions, "confirm": true}
			if code, raw, _ := f.request("POST", "/api/plugin-migrations/apply", body, true, true); code != 201 {
				t.Fatalf("actual registered local migration failed: %d %s", code, raw)
			}
			store, err := f.manager.packageStore()
			if err != nil {
				t.Fatal(err)
			}
			installed, err := store.Installed(t.Context(), localasr.PluginID)
			if err != nil || installed.Enabled {
				t.Fatal("migration enabled its package", installed, err)
			}
			config, err := store.Configuration(t.Context(), localasr.PluginID)
			if err != nil || string(config.Values["local_asr_profile_id"]) != strconv.Quote(test.selected) || string(config.Values["asr_backend"]) != `"local"` {
				t.Fatal("migration lost local selection", config, err)
			}
			if f.manager.mediaTranscriptionConfigured(t.Context()) {
				t.Fatal("disabled migration advertised active transcription")
			}
			installed, err = store.SetEnabled(t.Context(), localasr.PluginID, installed.Revision, true)
			if err != nil {
				t.Fatal(err)
			}
			if !f.manager.mediaTranscriptionConfigured(t.Context()) {
				t.Fatal("explicitly enabled registered local profile is unavailable")
			}
			grant := pluginhost.Grant{Account: "test", Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}
			metadata, err := f.manager.localASRPluginMetadata(t.Context(), installed.Package, grant)
			var initializer localasr.Initializer
			if err != nil || json.Unmarshal(metadata["local_asr"], &initializer) != nil || initializer != profile.Initializer {
				t.Fatal("migrated profile and runtime metadata differ", initializer, profile.Initializer, err)
			}
			after, err := os.ReadFile(filepath.Join(f.dir, "speech-config.json"))
			if err != nil || string(after) != string(legacy) || readLegacySpeechTestConfig(t, f.manager).ASRLocalModel != test.legacy || !reflect.DeepEqual(resources, migrationRegistrationSnapshot(t, profile.Public)) {
				t.Fatal("migration or enable modified legacy/public sources", err)
			}
		})
	}
}

func TestPluginMigrationRegisteredLocalASRResourceChangesInvalidateApproval(t *testing.T) {
	useOfficialMigrationCatalog(t)
	for _, action := range []string{"vad_tampered", "sdk_tampered", "valid_manifest_update"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t)
			legacy := writeLocalASRMigrationSource(t, f, "", "local", true)
			profile := writeReadOnlyMigrationASRProfile(t, f.manager, "sensevoice-small-int8")
			first, err := f.manager.pluginMigrationPreview(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			before := previewStep(t, first, localasr.PluginID)
			if before.Status != "ready_for_administrator_review" {
				t.Fatal("fixture was not initially verified", before)
			}
			switch action {
			case "vad_tampered":
				err = os.WriteFile(profile.VAD, []byte("synthetic tampered VAD"), 0600)
			case "sdk_tampered":
				err = os.WriteFile(profile.SDKFile, []byte("synthetic tampered SDK"), 0600)
			case "valid_manifest_update":
				var raw []byte
				raw, err = os.ReadFile(profile.Manifest)
				if err != nil {
					t.Fatal(err)
				}
				var manifest localasr.Manifest
				if err = json.Unmarshal(raw, &manifest); err != nil {
					t.Fatal(err)
				}
				manifest.Threads++
				raw, err = json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				err = os.WriteFile(profile.Manifest, raw, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			resources := migrationRegistrationSnapshot(t, profile.Public)
			second, err := f.manager.pluginMigrationPreview(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			after := previewStep(t, second, localasr.PluginID)
			if before.ApplySourceHash == after.ApplySourceHash || first.Accounts[0].SourceHashes["local_asr_profile_resource_version"] == second.Accounts[0].SourceHashes["local_asr_profile_resource_version"] {
				t.Fatal("resource proof did not invalidate old approval", before, after)
			}
			if first.Accounts[0].SourceHashes["speech_config_json"] != second.Accounts[0].SourceHashes["speech_config_json"] {
				t.Fatal("resource inspection changed legacy source")
			}
			if action == "valid_manifest_update" {
				if after.Status != "ready_for_administrator_review" || second.Accounts[0].SourceHashes["local_asr_profile_resource_version"] == "" {
					t.Fatal("valid updated manifest stayed permanently blocked", after)
				}
			} else if after.Status != "blocked" || !migrationStringContains(after.RequiredMissing, "registered_local_asr_profile:sensevoice-small-int8") {
				t.Fatal("tampered resource was accepted", after)
			}
			if _, _, _, err := f.manager.applyPluginMigration(t.Context(), "test", before.Plugin, before.ApplySourceHash, before.Package.ExpectedDigest, before.Permissions); err == nil {
				t.Fatal("stale resource approval was applied")
			}
			assertMigrationStoreAbsent(t, f.manager)
			source, err := os.ReadFile(filepath.Join(f.dir, "speech-config.json"))
			if err != nil || string(source) != string(legacy) || !reflect.DeepEqual(resources, migrationRegistrationSnapshot(t, profile.Public)) {
				t.Fatal("rejected apply modified source or resources", err)
			}
		})
	}
}
