package admin

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	mobile "wechat-bot/internal/devicespec"
)

func mobileFishDevice(t *testing.T, overview mobileOverview, deviceID string) mobileDeviceOverview {
	t.Helper()
	for _, device := range overview.Devices {
		if device.DeviceID == deviceID {
			return device
		}
	}
	t.Fatalf("device %q absent from overview %#v", deviceID, overview)
	return mobileDeviceOverview{}
}

func decodeMobileFishOverview(t *testing.T, raw []byte) mobileOverview {
	t.Helper()
	var overview mobileOverview
	if err := json.Unmarshal(raw, &overview); err != nil {
		t.Fatalf("decode mobile overview: %v; body=%s", err, raw)
	}
	return overview
}

// TestMobileFishBindingManagementAPI requires the ordinary protected management
// surface rather than calling registry mutation directly.  In particular, an
// old page cannot unbind a replacement binding, and bind never doubles as a
// hidden resume action.
func TestMobileFishBindingManagementAPI(t *testing.T) {
	f := setup(t)
	service := f.manager.mobile.(*mobileWorkflowService)
	settings := service.Settings()
	settings.Enabled, settings.Devices = true, []string{"synthetic"}
	if err := service.Save(settings); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.fish.Bind(f.native.Account()); err != nil {
		t.Fatal(err)
	}

	bindPath := "/api/mobile/overview/synthetic/bind"
	if code, _, _ := f.request("POST", bindPath, map[string]any{}, true, false); code != 401 {
		t.Fatalf("unauthenticated bind status=%d, want 401", code)
	}
	f.login()
	if code, _, _ := f.request("POST", bindPath, map[string]any{}, true, false); code != 403 {
		t.Fatalf("bind without CSRF status=%d, want 403", code)
	}
	code, raw, _ := f.request("POST", bindPath, map[string]any{}, true, true)
	if code != 200 {
		t.Fatalf("bind status=%d body=%s", code, raw)
	}
	firstOverview := decodeMobileFishOverview(t, raw)
	first := mobileFishDevice(t, firstOverview, "synthetic")
	if firstOverview.FishID == "" || first.FishID != firstOverview.FishID || first.BindingID == "" {
		t.Fatalf("bind response lacks current fish/device binding: %#v", firstOverview)
	}
	if code, raw, _ = f.request("POST", bindPath, map[string]any{}, true, true); code != 200 {
		t.Fatalf("idempotent bind status=%d body=%s", code, raw)
	}
	if again := mobileFishDevice(t, decodeMobileFishOverview(t, raw), "synthetic"); again.BindingID != first.BindingID {
		t.Fatalf("same binding changed on idempotent bind: first=%q again=%q", first.BindingID, again.BindingID)
	}
	if code, raw, _ = f.request("POST", "/api/mobile/overview/synthetic/takeover", map[string]any{}, true, true); code != 200 {
		t.Fatalf("takeover status=%d body=%s", code, raw)
	}

	unbindPath := "/api/mobile/overview/synthetic/unbind"
	if code, raw, _ = f.request("POST", unbindPath, map[string]string{"binding_id": "fish-device-000000000000000000000000000000000000000000000000"}, true, true); code != 409 {
		t.Fatalf("stale binding unbind status=%d body=%s, want 409", code, raw)
	}
	if code, raw, _ = f.request("POST", unbindPath, map[string]string{"binding_id": first.BindingID}, true, true); code != 200 {
		t.Fatalf("current binding unbind status=%d body=%s", code, raw)
	}
	if gone := mobileFishDevice(t, decodeMobileFishOverview(t, raw), "synthetic"); gone.BindingID != "" || gone.FishID != "" || !gone.Paused || !gone.Takeover {
		t.Fatalf("unbind did not revoke just the device association: %#v", gone)
	}
	if code, raw, _ = f.request("POST", bindPath, map[string]any{}, true, true); code != 200 {
		t.Fatalf("rebind status=%d body=%s", code, raw)
	}
	if rebound := mobileFishDevice(t, decodeMobileFishOverview(t, raw), "synthetic"); rebound.BindingID == "" || rebound.BindingID == first.BindingID {
		t.Fatalf("rebind did not mint a fresh binding: first=%q rebound=%q", first.BindingID, rebound.BindingID)
	}
}

// TestMobileFishBindingFenceKeepsLegacyOwnerControlButRevokesAutonomy checks
// the host-side fence, independently of a plugin runtime.  A pre-identity
// owner task remains controllable, but the instant an account joins a fish it
// cannot continue without the explicit device binding frozen into its task.
func TestMobileFishBindingFenceKeepsLegacyOwnerControlButRevokesAutonomy(t *testing.T) {
	f := setup(t)
	legacy := mobile.Task{
		ID: "legacy-owner-task", DeviceID: "synthetic",
		Source: mobile.Source{Admin: true},
	}
	if err := f.manager.mobileTaskBindingCurrent(legacy); err != nil {
		t.Fatalf("legacy owner task blocked before account joins fish: %v", err)
	}

	fishID, err := f.manager.fish.Bind(f.native.Account())
	if err != nil {
		t.Fatalf("bind test account to fish: %v", err)
	}
	if err := f.manager.mobileTaskBindingCurrent(legacy); err == nil {
		t.Fatal("legacy tokenless task continued after account joined fish")
	}
	_, first, err := f.manager.fish.BindDevice(f.native.Account(), "synthetic")
	if err != nil {
		t.Fatalf("bind synthetic device: %v", err)
	}
	current := legacy
	current.FishID, current.DeviceBindingID = fishID, first
	if err := f.manager.mobileTaskBindingCurrent(current); err != nil {
		t.Fatalf("current fish task rejected: %v", err)
	}

	if err := f.manager.fish.UnbindDevice(f.native.Account(), "synthetic", first); err != nil {
		t.Fatalf("unbind synthetic device: %v", err)
	}
	if err := f.manager.mobileTaskBindingCurrent(current); err == nil {
		t.Fatal("unbound task remained authorized")
	}
	_, second, err := f.manager.fish.BindDevice(f.native.Account(), "synthetic")
	if err != nil {
		t.Fatalf("rebind synthetic device: %v", err)
	}
	if second == first {
		t.Fatal("rebind reused old binding token")
	}
	if err := f.manager.mobileTaskBindingCurrent(current); err == nil {
		t.Fatal("old task token resumed after rebind")
	}
	current.DeviceBindingID = second
	if err := f.manager.mobileTaskBindingCurrent(current); err != nil {
		t.Fatalf("replacement task binding rejected: %v", err)
	}
}

// TestMobileFishBindingIsAccountScopedAndPersistent proves the registry does
// not use a device alias alone as authority and that a restarted manager sees
// the same current binding, rather than silently reconstructing one.
func TestMobileFishBindingIsAccountScopedAndPersistent(t *testing.T) {
	dir := t.TempDir()
	native := &fakeNative{}
	m, err := NewManager(filepath.Join(dir, "admin"), "", native)
	if err != nil {
		t.Fatal(err)
	}
	account := native.Account()
	fishID, err := m.fish.Bind(account)
	if err != nil {
		t.Fatal(err)
	}
	_, binding, err := m.fish.BindDevice(account, "emulator-5554")
	if err != nil {
		t.Fatal(err)
	}
	otherFish, err := m.fish.Bind("other-account")
	if err != nil || otherFish != fishID {
		t.Fatalf("bind other account = (%q, %v), want same fish", otherFish, err)
	}
	if _, _, err := m.fish.DeviceBindings("other-account"); err != nil {
		t.Fatalf("other account read its empty binding set: %v", err)
	}
	if err := m.fish.UnbindDevice("other-account", "emulator-5554", binding); err == nil {
		t.Fatal("another account removed this account's device binding")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewManager(filepath.Join(dir, "admin"), "", native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	gotFish, bindings, err := reopened.fish.DeviceBindings(account)
	if err != nil || gotFish != fishID || len(bindings) != 1 || bindings[0].BindingID != binding {
		t.Fatalf("reopened device binding = fish %q bindings %#v err %v", gotFish, bindings, err)
	}
}

// TestMobileFishBindingPauseCannotBeBypassedByBinding exercises the state
// before any plugin/broker is opened.  Rebinding metadata must not resume a
// paused or owner-taken-over device; only the explicit resume control may do
// that.
func TestMobileFishBindingPauseCannotBeBypassedByBinding(t *testing.T) {
	f := setup(t)
	service := f.manager.mobile.(*mobileWorkflowService)
	settings := service.Settings()
	settings.Enabled, settings.Devices = true, []string{"synthetic"}
	if err := service.Save(settings); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.fish.Bind(f.native.Account()); err != nil {
		t.Fatal(err)
	}
	_, binding, err := f.manager.fish.BindDevice(f.native.Account(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetObservationPaused("synthetic", true, true); err != nil {
		t.Fatal(err)
	}
	if _, again, err := f.manager.fish.BindDevice(f.native.Account(), "synthetic"); err != nil || again != binding {
		t.Fatalf("idempotent bind during takeover = (%q, %v)", again, err)
	}
	if _, err := service.Start(mobile.Source{Admin: true, MessageID: "paused-binding-check"}, "synthetic", "read only"); err == nil || err.Error() != "device_observation_paused" {
		t.Fatalf("binding bypassed takeover: %v", err)
	}
	if overview := service.Overview(); len(overview.Devices) != 1 || !overview.Devices[0].Paused || !overview.Devices[0].Takeover {
		t.Fatalf("binding changed owner takeover state: %#v", overview)
	}
}

// TestMobileFishBindingStartFreezesGenerationAndTickStopsRevokedTask uses the
// normal installed-workflow fixture.  It proves the task itself carries the
// binding generation, and a later unlink stops it before a planner or broker
// transition can revive it.
func TestMobileFishBindingStartFreezesGenerationAndTickStopsRevokedTask(t *testing.T) {
	x := newWorkflowFixture(t)
	fishID, err := x.f.manager.fish.Bind(x.f.native.Account())
	if err != nil {
		t.Fatal(err)
	}
	_, binding, err := x.f.manager.fish.BindDevice(x.f.native.Account(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	task := x.start(t)
	if task.FishID != fishID || task.DeviceBindingID != binding {
		t.Fatalf("start did not freeze current fish binding: %#v", task)
	}
	if err := x.f.manager.fish.UnbindDevice(x.f.native.Account(), "synthetic", binding); err != nil {
		t.Fatal(err)
	}
	x.tick(t)
	stored, ok := x.service.Get(task.ID)
	if !ok || stored.State != "stopped" || stored.Reason != "device_fish_binding_changed" {
		t.Fatalf("revoked task was not retired before workflow dispatch: %#v present=%v", stored, ok)
	}
	if x.actions != 0 || x.model.chatCalls != 0 {
		t.Fatalf("revoked task reached planner or device I/O: actions=%d model=%d", x.actions, x.model.chatCalls)
	}
}

// TestMobileFishBindingUnlinkDuringAdvanceStopsLaterTransitions covers the
// race that matters in practice: an authorized native action can already be
// in flight when its owner unlinks the device.  Its late result may be
// recorded for diagnostics, but it must not schedule another action, planner
// call, or successful completion under the revoked binding.
func TestMobileFishBindingUnlinkDuringAdvanceStopsLaterTransitions(t *testing.T) {
	x := newWorkflowFixture(t)
	if _, err := x.f.manager.fish.Bind(x.f.native.Account()); err != nil {
		t.Fatal(err)
	}
	_, binding, err := x.f.manager.fish.BindDevice(x.f.native.Account(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	task := x.start(t)
	for i := 0; i < 7; i++ {
		x.tick(t)
	}
	x.entered, x.block = make(chan struct{}), make(chan struct{})
	x.service.tick()
	select {
	case <-x.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("test action did not enter broker")
	}
	if err := x.f.manager.fish.UnbindDevice(x.f.native.Account(), "synthetic", binding); err != nil {
		t.Fatal(err)
	}
	close(x.block)
	deadline := time.Now().Add(3 * time.Second)
	for {
		x.service.mu.Lock()
		active := len(x.service.active)
		x.service.mu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late action did not settle after unlink")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		x.tick(t)
	}
	current, ok := x.service.Get(task.ID)
	if !ok || current.State != "stopped" || current.Reason != "device_fish_binding_changed" {
		t.Fatalf("unlinked in-flight task continued: %#v present=%v", current, ok)
	}
	if x.actions != 1 {
		t.Fatalf("unlink scheduled another native action: got %d", x.actions)
	}
}
