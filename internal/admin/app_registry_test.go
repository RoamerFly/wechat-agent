package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"wechat-bot/internal/apphost"
)

func TestApplicationRegistryReadsLegacyConnectionsAndCurrentScopedPolicy(t *testing.T) {
	f, registry := setupAccounts(t)
	native := f.manager.native.(*multiNative)
	reads := native.directoryReads.Load()
	second := registry.lookup("second").manager
	cfg := second.Config().Config
	cfg.APIKey = "synthetic-private-credential"
	cfg.Companion.Permissions.SendVoice = true
	if _, err := second.Save(ConfigInput{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	primaryCfg := f.manager.Config().Config
	primaryCfg.Companion.Permissions.SendVoice = false
	if _, err := f.manager.Save(ConfigInput{Config: primaryCfg}); err != nil {
		t.Fatal(err)
	}
	primaryReads, secondaryReads := native.primaryStatusReads.Load(), native.secondaryReads.Load()
	privateFiles := []string{filepath.Join(f.manager.dir, "accounts.json"), filepath.Join(f.manager.dir, "config.json"), filepath.Join(second.dir, "config.json")}
	before := make([][]byte, len(privateFiles))
	for i, path := range privateFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[i] = data
	}
	code, body, response := f.request("GET", "/api/apps", nil, true, false)
	var catalog struct{ Items []apphost.Application }
	if code != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" ||
		json.Unmarshal(body, &catalog) != nil || len(catalog.Items) != 1 {
		t.Fatalf("invalid host catalog: %d %s", code, body)
	}
	app := catalog.Items[0]
	if app.ID != "wechat" || app.InstallationID != "wechat-legacy" || app.Registration != "legacy" {
		t.Fatalf("invalid compatibility registration: %+v", app)
	}
	const path = "/api/apps/wechat/connections"
	code, body, _ = f.request("GET", path, nil, true, false)
	var connections struct{ Items []apphost.Connection }
	if code != http.StatusOK || json.Unmarshal(body, &connections) != nil || len(connections.Items) != 2 {
		t.Fatalf("invalid WeChat connections: %d %s", code, body)
	}
	if native.primaryStatusReads.Load() != primaryReads || native.secondaryReads.Load() != secondaryReads {
		t.Fatal("listing registered connections contacted a native account")
	}
	primary, secondary := connections.Items[0], connections.Items[1]
	if primary.AccountID != "test" || !primary.Primary || secondary.AccountID != "second" || primary.ID == secondary.ID {
		t.Fatalf("legacy account identities changed: %+v", connections.Items)
	}
	inspect := func(url string) apphost.ConnectionStatus {
		t.Helper()
		code, body, _ := f.request("GET", url, nil, true, false)
		var detail struct{ Connection apphost.ConnectionStatus }
		if code != http.StatusOK || json.Unmarshal(body, &detail) != nil ||
			strings.Contains(string(body), "synthetic-private-credential") || strings.Contains(string(body), "synthetic-account-test-token") {
			t.Fatalf("invalid or private connection response: status=%d", code)
		}
		return detail.Connection
	}
	// A legacy query selector cannot redirect an explicit app connection.
	detail := inspect(path + "/" + secondary.ID + "?account=test")
	if detail.AccountID != secondary.AccountID || detail.Runtime != "online" || detail.Login != "authenticated" || detail.CapabilityCheck != "checked" || detail.Policy == nil ||
		!slices.Contains(detail.Policy.Grants, "send.voice") || !slices.Contains(detail.SupportedActions, "send.voice") ||
		!slices.Contains(detail.SupportedActions, "send.quoted_text") || !slices.Contains(detail.SupportedActions, "account.recall") ||
		slices.Contains(detail.SupportedActions, "send.video") || slices.Contains(detail.SupportedActions, "send.unrecognized") ||
		slices.Contains(detail.SupportedActions, "account.unknown") || slices.Contains(detail.Policy.Grants, "account.recall") {
		t.Fatalf("runtime support and account permission were conflated: %+v", detail)
	}
	if native.primaryStatusReads.Load() != primaryReads || native.secondaryReads.Load() != secondaryReads+2 {
		t.Fatal("connection detail did not scope status and capability checks to the selected account")
	}
	offline := inspect(path + "/" + primary.ID)
	if offline.Runtime != "offline" || offline.Login != "signed_out" || len(offline.SupportedActions) != 0 ||
		offline.Policy == nil || slices.Contains(offline.Policy.Grants, "send.voice") {
		t.Fatalf("account state crossed connection scope: %+v", offline)
	}
	if code, _, _ := f.request("GET", path+"/second", nil, true, false); code != http.StatusNotFound {
		t.Fatal("unrecognized connection fell back to an account", code)
	}
	if native.directoryReads.Load() != reads {
		t.Fatal("read-only application registry invoked legacy account discovery")
	}
	for i, path := range privateFiles {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, before[i]) {
			t.Fatalf("application query changed authoritative account files: %v", err)
		}
	}
	cfg.Companion.Permissions.SendVoice = false
	if _, err := second.Save(ConfigInput{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	detail = inspect(path + "/" + secondary.ID)
	if slices.Contains(detail.Policy.Grants, "send.voice") || !slices.Contains(detail.SupportedActions, "send.voice") {
		t.Fatal("permission revocation left a stale projection")
	}
	if code, _, _ := f.request("POST", "/api/accounts/second/manage", map[string]string{"action": "rename", "label": "合成新备注"}, true, true); code != http.StatusOK {
		t.Fatal("legacy rename failed", code)
	}
	if renamed := inspect(path + "/" + secondary.ID); renamed.ID != secondary.ID || renamed.Label != "合成新备注" {
		t.Fatal("display name modified the connection identity", renamed)
	}
	if code, _, _ := f.request("POST", "/api/accounts/second/manage", map[string]string{"action": "remove"}, true, true); code != http.StatusOK {
		t.Fatal("legacy removal failed", code)
	}
	if code, _, _ := f.request("GET", path+"/"+secondary.ID, nil, true, false); code != http.StatusNotFound {
		t.Fatal("removed account is still exposed as a connection", code)
	}
	if code, _, _ := f.request("POST", "/api/accounts/second/manage", map[string]string{"action": "restore"}, true, true); code != http.StatusOK {
		t.Fatal("legacy restoration failed", code)
	}
	if restored := inspect(path + "/" + secondary.ID); restored.ID != secondary.ID || restored.Policy == nil || slices.Contains(restored.Policy.Grants, "send.voice") {
		t.Fatal("restoration changed the connection ID or reintroduced a revoked permission", restored)
	}
}
