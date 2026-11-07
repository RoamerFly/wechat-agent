package station

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/mediaspec"
)

type legacySpeechFixture struct {
	data, workspace, executable, optionsFile, catalogFile string
	options                                               legacySpeechOptions
	entry                                                 mediaspec.VoiceEntry
	registration                                          localTTSRegistration
	registry                                              *fishidentity.Registry
}

func newLegacySpeechFixture(t *testing.T) legacySpeechFixture {
	t.Helper()
	data, workspace, executable, registration := localTTSRegistrationFixture(t)
	if err := os.Remove(filepath.Join(data, localTTSRegistrationFile)); err != nil {
		t.Fatal(err)
	}
	f := legacySpeechFixture{data: data, workspace: workspace, executable: executable, registration: registration}
	f.registry = fishidentity.New(data)
	if _, err := f.registry.InitializeLocal(); err != nil {
		t.Fatal(err)
	}
	voice := registration.Voices[0]
	f.entry = mediaspec.VoiceEntry{
		ID: voice.ID, Name: voice.Label, Owner: "synthetic-original-owner", Engine: "GPT-SoVITS", Status: "ready",
		Recommended: map[string]string{},
	}
	for _, resource := range []struct{ path, kind string }{{voice.GPT, "gpt"}, {voice.SoVITS, "sovits"}, {voice.Reference, "reference"}, {voice.Prompt, "prompt"}} {
		info, err := os.Stat(resource.path)
		if err != nil {
			t.Fatal(err)
		}
		item := mediaspec.VoiceFile{
			Relative: filepath.Base(resource.path), Filename: filepath.Base(resource.path),
			Kind: resource.kind, Bytes: info.Size(), Hash: localTTSFileSHA(resource.path),
		}
		if resource.kind == "gpt" || resource.kind == "sovits" {
			f.entry.Weights = append(f.entry.Weights, item)
			f.entry.Recommended[resource.kind] = item.Hash + filepath.Ext(item.Filename)
		} else {
			f.entry.References = append(f.entry.References, item)
		}
	}
	registration.Voices = nil
	f.options = legacySpeechOptions{
		Version: 1, Library: filepath.Dir(voice.GPT), SourceOwner: f.entry.Owner, VoiceIDs: []string{voice.ID},
		PromptLanguage: "zh", TextLanguage: "auto", Deployment: registration,
	}
	f.optionsFile, f.catalogFile = filepath.Join(t.TempDir(), "speech-plan.json"), filepath.Join(f.options.Library, "catalog.json")
	f.write(t)
	return f
}

func (f legacySpeechFixture) write(t *testing.T) {
	t.Helper()
	for name, value := range map[string]any{f.optionsFile: f.options, f.catalogFile: map[string]any{"items": []mediaspec.VoiceEntry{f.entry}}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacySpeechPreservesVoiceResourcesAndReadsRegisteredIdentityAfterRestart(t *testing.T) {
	f := newLegacySpeechFixture(t)
	before, _ := os.ReadFile(f.catalogFile)
	plan, err := PreviewLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile)
	if err != nil || len(plan.Voices) != 1 || plan.BytesVerified <= 0 || len(plan.Digest) != 64 {
		t.Fatal("legacy speech preview failed", plan, err)
	}
	if _, err := os.Stat(filepath.Join(f.data, localTTSRegistrationFile)); !os.IsNotExist(err) {
		t.Fatal("preview installed speech", err)
	}
	view, _ := json.Marshal(plan)
	if bytes.Contains(view, []byte(f.options.Library)) || bytes.Contains(view, []byte("已登记的提示词")) {
		t.Fatal("preview exposed private resources")
	}
	for range 2 {
		if _, err := ImportLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile, plan.Digest); err != nil {
			t.Fatal("speech import failed", err)
		}
	}
	var receipt legacySpeechReceipt
	rawReceipt, err := os.ReadFile(filepath.Join(f.data, legacySpeechReceiptFile))
	if err != nil || json.Unmarshal(rawReceipt, &receipt) != nil || receipt.Plan.Digest != plan.Digest ||
		receipt.Plan.OwnerID != plan.OwnerID || receipt.Plan.SourceOwner != f.entry.Owner ||
		receipt.Library != f.options.Library || receipt.CatalogSHA256 != legacySettingsHash(before) {
		t.Fatal("private import provenance was not retained", receipt, err)
	}
	after, _ := os.ReadFile(f.catalogFile)
	if !bytes.Equal(before, after) {
		t.Fatal("source catalog changed")
	}
	backend := newLocalTTSBackendAt(f.data, f.workspace, f.executable)
	info, err := backend.Info(t.Context(), plan.FishID)
	if err != nil || len(info.Voices) != 1 || info.Voices[0].ID != f.entry.ID || info.Voices[0].Version != plan.Voices[0].Version || info.Binding != plan.Binding {
		t.Fatal("registered voice changed after reopening", info, err)
	}
	registration, err := backend.loadRegistration(t.Context())
	if err != nil || registration.Voices[0].GPT != f.registration.Voices[0].GPT || registration.verifyResources(registration.Voices[0]) != nil {
		t.Fatal("original weight/reference mapping was not retained", err)
	}
}

func TestLegacySpeechResumesJournaledImportAndSerializesRetries(t *testing.T) {
	f := newLegacySpeechFixture(t)
	loaded, err := loadLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the private journal committed, before enabling
	// speech. A retry must finish the same registration without new resources.
	raw, _ := json.Marshal(loaded.Receipt)
	if err := os.WriteFile(filepath.Join(f.data, legacySpeechReceiptFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errors := make(chan error, 3)
	for range 3 {
		wait.Go(func() {
			_, err := ImportLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile, loaded.Plan.Digest)
			errors <- err
		})
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal("interrupted import was not resumable", err)
		}
	}
	registered, err := newLocalTTSBackendAt(f.data, f.workspace, f.executable).loadRegistration(t.Context())
	if err != nil || registered.Voices[0].ID != f.entry.ID {
		t.Fatal("journal resumed wrong registration", err)
	}
}

func TestLegacySpeechImportsExplicitUnownedArchiveWithDefaultDesktopLayout(t *testing.T) {
	f := newLegacySpeechFixture(t)
	f.workspace = filepath.Join(f.data, "managed-workspaces")
	f.entry.Owner, f.entry.Status = "", "已归档，待运行验证"
	f.options.SourceOwner = ""
	if err := os.Mkdir(filepath.Join(f.options.Library, "files"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, files := range [][]mediaspec.VoiceFile{f.entry.Weights, f.entry.References} {
		for i := range files {
			item := &files[i]
			item.File = item.Hash + filepath.Ext(item.Filename)
			if err := os.Rename(filepath.Join(f.options.Library, item.Relative), filepath.Join(f.options.Library, "files", item.File)); err != nil {
				t.Fatal(err)
			}
			item.Relative = ""
		}
	}
	f.write(t)
	if _, err := PreviewLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile); err == nil {
		t.Fatal("unowned archive was accepted without explicit selection")
	}
	f.options.AllowUnowned = true
	f.write(t)
	plan, err := PreviewLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile)
	if err != nil || !plan.UnownedSource || plan.SourceOwner != "" {
		t.Fatal("explicit unowned archive was not accepted", plan, err)
	}
	if _, err := ImportLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile, plan.Digest); err != nil {
		t.Fatal(err)
	}
	backend := newLocalTTSBackendAt(f.data, f.workspace, f.executable)
	registration, err := backend.loadRegistration(t.Context())
	if err != nil || registration.Voices[0].ID != f.entry.ID || registration.verifyResources(registration.Voices[0]) != nil {
		t.Fatal("default desktop layout could not load imported archive", err)
	}
}

func TestLegacySpeechRejectsConflictingOrUnwritableJournalBeforeEnabling(t *testing.T) {
	for _, kind := range []string{"conflict", "directory"} {
		t.Run(kind, func(t *testing.T) {
			f := newLegacySpeechFixture(t)
			plan, err := PreviewLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile)
			if err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(f.data, legacySpeechReceiptFile)
			if kind == "conflict" {
				err = os.WriteFile(journal, []byte(`{"schema_version":1}`), 0600)
			} else {
				err = os.Mkdir(journal, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ImportLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile, plan.Digest); err == nil {
				t.Fatal("unavailable import journal was accepted")
			}
			if _, err := os.Stat(filepath.Join(f.data, localTTSRegistrationFile)); !os.IsNotExist(err) {
				t.Fatal("speech enabled without matching journal", err)
			}
		})
	}
}

func TestLegacySpeechRejectsTamperingOwnershipAndExistingConfiguration(t *testing.T) {
	for _, change := range []string{"weights", "owner", "revision", "target", "ambiguous", "traversal", "cancel"} {
		t.Run(change, func(t *testing.T) {
			f := newLegacySpeechFixture(t)
			plan, err := PreviewLegacySpeech(t.Context(), f.data, f.workspace, f.optionsFile)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			switch change {
			case "weights":
				err = os.WriteFile(f.registration.Voices[0].GPT, []byte("new"), 0600)
			case "owner":
				f.entry.Owner = "another-owner"
				f.write(t)
			case "revision":
				_, err = f.registry.SetHomeAccess(plan.OwnerRevision, false)
			case "target":
				other := f.registration
				other.Voices[0].ID = "another-voice"
				localTTSWriteRegistration(t, f.data, other)
			case "ambiguous":
				delete(f.entry.Recommended, "gpt")
				f.entry.Weights = append(f.entry.Weights, f.entry.Weights[0])
				f.write(t)
			case "traversal":
				f.entry.Weights[0].Relative = "../outside.ckpt"
				f.write(t)
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ImportLegacySpeech(ctx, f.data, f.workspace, f.optionsFile, plan.Digest); err == nil {
				t.Fatal("unsafe voice migration was accepted", change)
			}
			if change != "target" {
				if _, err := os.Stat(filepath.Join(f.data, localTTSRegistrationFile)); !os.IsNotExist(err) {
					t.Fatal("rejected migration installed speech", err)
				}
			} else if raw, _ := os.ReadFile(filepath.Join(f.data, localTTSRegistrationFile)); !strings.Contains(string(raw), "another-voice") {
				t.Fatal("existing registration was overwritten")
			}
		})
	}
}
