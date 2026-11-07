package admin

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// observationGrantFixture registers the same hash-verified mobile package and
// profile shape used by the normal device integration tests.  It deliberately
// stops before broker creation: application grants are host metadata and must
// not wake ADB, a plugin process, a model, or WeChat.
func observationGrantFixture(t *testing.T) (*webFixture, *mobileWorkflowService, string, string, string) {
	t.Helper()
	f := setup(t)
	service := f.manager.mobile.(*mobileWorkflowService)
	settings := service.Settings()
	settings.Enabled = true
	settings.Devices = []string{"synthetic"}
	settings.Actions = []string{"capture"}
	options, settings := mobileDeviceIOResourceFixture(t, f.manager, settings, "")
	rawOptions, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WECHAT_PLUGIN_RUNTIME_CONFIG", string(rawOptions))
	if err = service.Save(settings); err != nil {
		t.Fatal(err)
	}
	store, err := f.manager.packageStore()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.Prepare(t.Context(), "../../plugins/mobile-control")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(t.Context(), prepared.Token, prepared.Digest, prepared.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetEnabled(t.Context(), installed.Package.ID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.fish.Bind(f.native.Account()); err != nil {
		t.Fatal(err)
	}
	f.login()
	path := "/api/mobile/overview/synthetic/bind"
	if code, body, _ := f.request("POST", path, map[string]any{}, true, true); code != 200 {
		t.Fatalf("bind=%d body=%s", code, body)
	}
	code, body, _ := f.request("GET", "/api/mobile/overview", nil, false, false)
	if code != 200 {
		t.Fatalf("overview=%d body=%s", code, body)
	}
	device := mobileFishDevice(t, decodeMobileFishOverview(t, body), "synthetic")
	if device.BindingID == "" || device.FishID == "" || device.AuthorizationState != "ready" {
		t.Fatalf("fixture did not expose actual installed profile/binding: %#v", device)
	}
	return f, service, device.BindingID, "com.example.home", settings.Bridge.Args[5]
}

func observationGrantDevice(t *testing.T, raw []byte) mobileDeviceOverview {
	t.Helper()
	overview := decodeMobileFishOverview(t, raw)
	for _, device := range overview.Devices {
		if device.DeviceID == "synthetic" {
			return device
		}
	}
	t.Fatalf("device %q absent from overview %#v raw=%s", "synthetic", overview, raw)
	return mobileDeviceOverview{}
}

func observationGrantView(t *testing.T, device mobileDeviceOverview, packageName string) mobileObservationAppGrantView {
	t.Helper()
	for _, grant := range device.ObservationApps {
		if grant.Package == packageName {
			return grant
		}
	}
	t.Fatalf("package %q absent from observation grants %#v", packageName, device.ObservationApps)
	return mobileObservationAppGrantView{}
}

// TestMobileObservationAppGrantManagementLifecycle exercises the normal
// protected management route, including the deliberately separate host
// profile allowlist.  It proves the state change never resumes a device.
func TestMobileObservationAppGrantManagementLifecycle(t *testing.T) {
	f, _, binding, packageName, _ := observationGrantFixture(t)
	path := "/api/mobile/overview/synthetic/observation-apps"
	request := map[string]any{"binding_id": binding, "package": packageName, "enabled": true}

	if code, _, _ := f.request("POST", path, request, true, false); code != 403 {
		t.Fatalf("grant without csrf=%d, want 403", code)
	}
	if code, _, _ := f.request("POST", path, map[string]any{"binding_id": binding, "package": packageName}, true, true); code != 400 {
		t.Fatalf("grant with missing enabled=%d, want 400", code)
	}
	if code, body, _ := f.request("POST", path, request, true, true); code != 200 {
		t.Fatalf("grant=%d body=%s", code, body)
	} else {
		first := observationGrantView(t, observationGrantDevice(t, body), packageName)
		if first.GrantID == "" || !first.Available || first.BindingID != binding || first.Package != packageName {
			t.Fatalf("first grant=%#v", first)
		}
	}
	_, firstBody, _ := f.request("POST", path, request, true, true)
	first := observationGrantView(t, observationGrantDevice(t, firstBody), packageName)
	if code, body, _ := f.request("POST", path, request, true, true); code != 200 {
		t.Fatalf("idempotent grant=%d body=%s", code, body)
	} else if again := observationGrantView(t, observationGrantDevice(t, body), packageName); again.GrantID != first.GrantID {
		t.Fatalf("idempotent grant minted another token: first=%q again=%q", first.GrantID, again.GrantID)
	}

	if code, body, _ := f.request("POST", "/api/mobile/overview/synthetic/takeover", map[string]any{}, true, true); code != 200 {
		t.Fatalf("takeover=%d body=%s", code, body)
	}
	remove := map[string]any{"binding_id": binding, "package": packageName, "enabled": false}
	if code, body, _ := f.request("POST", path, remove, true, true); code != 200 {
		t.Fatalf("revoke=%d body=%s", code, body)
	} else {
		device := observationGrantDevice(t, body)
		if !device.Paused || !device.Takeover || len(device.ObservationApps) != 0 {
			t.Fatalf("grant revoke changed pause/takeover or retained grant: %#v", device)
		}
	}
	if code, body, _ := f.request("POST", path, request, true, true); code != 200 {
		t.Fatalf("regrant=%d body=%s", code, body)
	} else if next := observationGrantView(t, observationGrantDevice(t, body), packageName); next.GrantID == first.GrantID {
		t.Fatalf("regrant reused revoked token %q", next.GrantID)
	}
}

// The new route has its own protected+CSRF seal.  Existing controls cover
// similar routes, but adding an app grant must not accidentally be the first
// mutation endpoint that is reachable before login.
func TestMobileObservationAppGrantRouteRequiresSessionAndCSRF(t *testing.T) {
	f := setup(t)
	path := "/api/mobile/overview/synthetic/observation-apps"
	request := map[string]any{"binding_id": "fish-device-stale", "package": "com.example.home", "enabled": true}
	if code, _, _ := f.request("POST", path, request, true, false); code != 401 {
		t.Fatalf("anonymous grant status=%d, want 401", code)
	}
	f.login()
	if code, _, _ := f.request("POST", path, request, true, false); code != 403 {
		t.Fatalf("grant without csrf status=%d, want 403", code)
	}
}

// TestMobileObservationAppGrantProfileChangeAndCorruptionFailClosed keeps a
// now-unavailable grant visible only long enough to revoke it.  A broken host
// record must never be normalized into an effective permission.
func TestMobileObservationAppGrantProfileChangeAndCorruptionFailClosed(t *testing.T) {
	f, service, binding, packageName, profile := observationGrantFixture(t)
	path := "/api/mobile/overview/synthetic/observation-apps"
	enable := map[string]any{"binding_id": binding, "package": packageName, "enabled": true}
	if code, body, _ := f.request("POST", path, enable, true, true); code != 200 {
		t.Fatalf("initial grant=%d body=%s", code, body)
	}

	// Keep a valid profile file but remove this package and observe action. The
	// prior grant must become unavailable; it is not silently broadened or
	// discarded, because its owner still needs a revocation path.
	profileData, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	var profileJSON map[string]any
	if err = json.Unmarshal(profileData, &profileJSON); err != nil {
		t.Fatal(err)
	}
	devices := profileJSON["devices"].(map[string]any)
	device := devices["synthetic"].(map[string]any)
	device["packages"] = []string{"com.example.other"}
	device["actions"] = []string{"capture"}
	changed, _ := json.Marshal(profileJSON)
	if err = os.WriteFile(profile, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := f.request("GET", "/api/mobile/overview", nil, false, false); code != 200 {
		t.Fatalf("changed-profile overview=%d body=%s", code, body)
	} else if grant := observationGrantView(t, observationGrantDevice(t, body), packageName); grant.Available {
		t.Fatalf("removed profile privilege still effective: %#v", grant)
	}
	if code, _, _ := f.request("POST", path, enable, true, true); code != 409 {
		t.Fatalf("profile-removed grant re-enabled with status=%d", code)
	}
	disable := map[string]any{"binding_id": binding, "package": packageName, "enabled": false}
	if code, body, _ := f.request("POST", path, disable, true, true); code != 200 {
		t.Fatalf("profile-removed grant could not be revoked: %d %s", code, body)
	}

	// JSON corruption is rejected by the underlying host store before it can be
	// persisted. Its decoder remains an explicit fail-closed contract.
	if _, err := decodeMobileObservationAppGrants(json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed observation grant record decoded as valid")
	}

	// A structurally valid foreign-account record is also fail-closed. Unlike
	// malformed JSON it can be stored, so prove a rejected operation does not
	// overwrite it with a local account grant.
	snapshot, store, err := service.load(true)
	if err != nil {
		t.Fatal(err)
	}
	foreign := mobileObservationAppGrantState{Grants: []mobileObservationAppGrant{{
		GrantID: "observe-app-foreign", Account: "other-account", FishID: "fish-foreign", DeviceID: "synthetic", BindingID: "binding-foreign", Package: packageName, CreatedAt: time.Now().UTC(),
	}}}
	foreignRaw, _ := json.Marshal(foreign)
	if err = service.commit(snapshot, store, map[string]json.RawMessage{mobileObservationAppGrantsKey: foreignRaw}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := service.ObservationAppGrant("synthetic", packageName); err == nil || ok {
		t.Fatalf("foreign account grant record treated as effective: ok=%v err=%v", ok, err)
	}
	if code, _, _ := f.request("POST", path, enable, true, true); code != 500 {
		t.Fatalf("foreign account grant mutation status=%d, want 500", code)
	}
	if device := mobileFishDevice(t, service.Overview(), "synthetic"); device.ObservationAppsState != "unavailable" || len(device.ObservationApps) != 0 {
		t.Fatalf("foreign record was not marked unavailable: %#v", device)
	}
	snapshot, _, err = service.load(true)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.host.Records[mobileObservationAppGrantsKey]; string(got) != string(foreignRaw) {
		t.Fatalf("failed foreign-account operation overwrote host record: before=%s after=%s", foreignRaw, got)
	}
}

// TestMobileObservationAppGrantBindingGenerationAndPersistence proves that a
// grant is scoped to the exact current fish/device association.  An old page
// cannot use its token after rebind, and a state reload retains the current
// authorization without opening a device broker.
func TestMobileObservationAppGrantBindingGenerationAndPersistence(t *testing.T) {
	f, service, firstBinding, packageName, _ := observationGrantFixture(t)
	path := "/api/mobile/overview/synthetic/observation-apps"
	enable := map[string]any{"binding_id": firstBinding, "package": packageName, "enabled": true}
	if code, body, _ := f.request("POST", path, enable, true, true); code != 200 {
		t.Fatalf("grant=%d body=%s", code, body)
	}
	first := observationGrantView(t, mobileFishDevice(t, service.Overview(), "synthetic"), packageName)
	if grant, ok, err := service.ObservationAppGrant("synthetic", packageName); err != nil || !ok || grant.GrantID != first.GrantID {
		t.Fatalf("effective current grant=%#v ok=%v err=%v", grant, ok, err)
	}
	// Settings save uses the legacy-compatible repository write path.  It must
	// preserve unrelated host-owned grant records across ordinary configuration
	// changes, without needing to contact a device.
	settings := service.Settings()
	settings.Actions = append(settings.Actions, "launch")
	if err := service.Save(settings); err != nil {
		t.Fatalf("ordinary settings save: %v", err)
	}
	if grant, ok, err := service.ObservationAppGrant("synthetic", packageName); err != nil || !ok || grant.GrantID != first.GrantID {
		t.Fatalf("settings save lost app grant=%#v ok=%v err=%v", grant, ok, err)
	}
	reopened, err := NewManagerWithDeviceIO(f.dir, "", f.native, f.manager.deviceIO)
	if err != nil {
		t.Fatal(err)
	}
	restored, ok, readErr := reopened.mobile.(*mobileWorkflowService).ObservationAppGrant("synthetic", packageName)
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if readErr != nil || !ok || restored.GrantID != first.GrantID {
		t.Fatalf("reopened manager lost grant=%#v ok=%v err=%v", restored, ok, readErr)
	}

	if code, body, _ := f.request("POST", "/api/mobile/overview/synthetic/unbind", map[string]any{"binding_id": firstBinding}, true, true); code != 200 {
		t.Fatalf("unbind=%d body=%s", code, body)
	}
	if _, ok, err := service.ObservationAppGrant("synthetic", packageName); err != nil || ok {
		t.Fatalf("unbound grant remained current: ok=%v err=%v", ok, err)
	}
	if code, body, _ := f.request("POST", "/api/mobile/overview/synthetic/bind", map[string]any{}, true, true); code != 200 {
		t.Fatalf("rebind=%d body=%s", code, body)
	} else {
		second := observationGrantDevice(t, body)
		if second.BindingID == firstBinding {
			t.Fatalf("rebind reused binding %q", firstBinding)
		}
		if code, _, _ := f.request("POST", path, enable, true, true); code != 409 {
			t.Fatalf("stale binding grant status=%d", code)
		}
		if _, ok, err := service.ObservationAppGrant("synthetic", packageName); err != nil || ok {
			t.Fatalf("old grant revived on new binding: ok=%v err=%v", ok, err)
		}
		fresh := map[string]any{"binding_id": second.BindingID, "package": packageName, "enabled": true}
		if code, body, _ := f.request("POST", path, fresh, true, true); code != 200 {
			t.Fatalf("new binding grant=%d body=%s", code, body)
		} else if got := observationGrantView(t, observationGrantDevice(t, body), packageName); got.GrantID == first.GrantID {
			t.Fatalf("new binding reused old grant token %q", got.GrantID)
		}
	}
}
