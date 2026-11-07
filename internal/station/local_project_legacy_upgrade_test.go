package station

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/pluginruntime"
)

func TestLiveLocalProjectDocumentLegacyUpgrade(t *testing.T) {
	legacy := os.Getenv("AHS_LOCAL_PROJECT_DOCUMENT_LEGACY_CATALOG")
	if runtime.GOOS != "windows" || os.Getenv("AHS_LOCAL_PROJECT_DOCUMENT_LPAC_TEST") != "1" || legacy == "" {
		t.Skip("set the Windows LPAC fixture and an earlier official document catalog path")
	}
	options := localProjectToolWindowsOptions(t)
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "host"), filepath.Join(base, "workspace"))
	f.handler.chat.projectTools.options = options
	f.login()
	if code, out := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatalf("initialize: %d %#v", code, out)
	}
	_, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	host, err := f.handler.chat.projectTools.forFish(in.Account)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := host.store.Prepare(t.Context(), filepath.Join(legacy, pluginruntime.DocumentWorkshopID))
	previousMinor := compatibleDocumentDigest(projectOperation(localDocumentExportTool), prepared.Digest)
	if err != nil || !previousMinor && prepared.Digest != localDocumentLegacyDigest {
		t.Fatalf("legacy official catalog not verified: %v", err)
	}
	old, err := host.store.Install(t.Context(), prepared.Token, prepared.Digest, []string{"artifact.read", "artifact.write"})
	if err != nil {
		t.Fatal(err)
	}
	const route = "/api/local/project-tools/document-workshop"
	if code, out := f.request("PUT", route, map[string]any{"action": "enable", "revision": old.Revision}, true); code != http.StatusOK {
		t.Fatalf("legacy enable: %d %#v", code, out)
	}
	before := localProjectToolsView(t, f)
	if before["update_available"] != true || before["pdf_ready"] != previousMinor {
		t.Fatal("legacy package advertised unsupported PDF", before)
	}
	var artifact string
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		round := 0
		return localModelFunc(func(_ context.Context, system string, turns []map[string]string) (string, error) {
			round++
			if round == 1 {
				if !strings.Contains(system, localDocumentToolName) || strings.Contains(system, localDocumentInspectTool) != previousMinor ||
					strings.Contains(system, localDocumentExportTool) != previousMinor {
					t.Error("legacy discovery did not preserve the capabilities of the installed release")
				}
				return localToolCall(localDocumentToolName, map[string]any{
					"layout": map[string]any{"blocks": []any{map[string]string{"kind": "paragraph", "text": "保留的旧版成品"}}},
				}), nil
			}
			all := localModelHistory(turns)
			if index := strings.Index(all, "work-"); index >= 0 && len(all) >= index+69 {
				artifact = all[index : index+69]
			}
			return `{"action":"reply","messages":["文档已登记 ` + artifact + `"]}`, nil
		}), nil
	}
	if code, out := localChatSend(f, "legacy-doc-before-upgrade", "用旧版保存一份文档"); code != http.StatusOK || !artifactID(artifact, "work-") {
		t.Fatalf("legacy artifact creation: %d %#v", code, out)
	}
	original := localArtifactDownload(t, f, artifact)
	if code, _ := f.request("PUT", route, map[string]any{"action": "upgrade", "revision": localProjectToolRevision(t, before), "approve_artifacts": true}, true); code != http.StatusConflict {
		t.Fatalf("upgrade should wait for disable: %d", code)
	}
	if code, out := f.request("PUT", route, map[string]any{"action": "disable", "revision": localProjectToolRevision(t, before)}, true); code != http.StatusOK {
		t.Fatalf("disable legacy: %d %#v", code, out)
	}
	disabled := localProjectToolsView(t, f)
	if code, _ := f.request("PUT", route, map[string]any{"action": "upgrade", "revision": localProjectToolRevision(t, disabled)}, true); code != http.StatusBadRequest {
		t.Fatalf("upgrade should require permission approval: %d", code)
	}
	if code, out := f.request("PUT", route, map[string]any{"action": "upgrade", "revision": localProjectToolRevision(t, disabled), "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatalf("upgrade legacy: %d %#v", code, out)
	}
	upgraded := localProjectToolsView(t, f)
	if upgraded["enabled"] != false || upgraded["update_available"] != false || upgraded["pdf_ready"] != true {
		t.Fatal("upgrade should leave the current package disabled with PDF resources available", upgraded)
	}
	if !bytes.Equal(original, localArtifactDownload(t, f, artifact)) {
		t.Fatal("legacy artifact bytes changed after upgrade")
	}
}
