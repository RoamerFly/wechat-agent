package localasr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func TestLocalASRRuntimeManifestRelativePathsStayCanonical(t *testing.T) {
	if _, err := localASRRuntimeRelative(`C:\sdk\payload.py`); err == nil {
		t.Fatal("Windows absolute path accepted")
	}
	for _, raw := range []string{"../payload.py", "nested/../payload.py", `nested\..\payload.py`, "/payload.py", "nested//payload.py"} {
		if _, err := localASRRuntimeRelative(raw); err == nil {
			t.Fatalf("non-canonical runtime path accepted: %q", raw)
		}
	}
	if runtime.GOOS == "windows" {
		got, err := localASRRuntimeRelative(`nested\safe/file.py`)
		if err != nil || got != filepath.Join("nested", "safe", "file.py") {
			t.Fatalf("Windows manifest separators were not canonically accepted: %q %v", got, err)
		}
	} else if _, err := localASRRuntimeRelative(`nested\safe\file.py`); err == nil {
		t.Fatal("non-Windows host accepted Windows separator form")
	}
}

func TestLocalASRRuntimeManifestRejectsNormalizedDuplicatePaths(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the alternate Windows spelling is intentionally rejected on non-Windows hosts")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("synthetic-sdk")
	if err := os.WriteFile(filepath.Join(root, "nested", "payload.py"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	raw, err := json.Marshal(map[string]any{"files": map[string]string{`nested\payload.py`: hex.EncodeToString(sum[:]), "nested/payload.py": hex.EncodeToString(sum[:])}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, localASRRuntimeManifest), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifyLocalASRRuntimeManifest(root); err == nil {
		t.Fatal("same SDK file under mixed separators was accepted twice")
	}
}

func TestLocalASRRunRejectsProfileFromAnotherOwner(t *testing.T) {
	store, err := pluginhost.OpenStore(t.TempDir(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	pkg := pluginhost.Package{ID: PluginID, Version: "0.1.0"}
	grant := pluginhost.Grant{Account: store.Account(), Plugin: pkg.ID, Version: pkg.Version}
	profile := Profile{
		resources: []pluginruntime.ReadOnlyResource{{Account: "other-owner", Plugin: pkg.ID, PackageVersion: pkg.Version}},
		runner:    localASRRunner{TimeoutSeconds: 30},
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	_, _, err = Run(t.Context(), store, scratch, nil, pkg, grant, profile, []byte("RIFF0000WAVE"), 10)
	if err == nil || err.Error() != "local_asr_authorization_invalid" {
		t.Fatalf("wrong-owner resource was not rejected at the authorization boundary: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("rejected execution touched the scratch directory: %v", err)
	}
}
