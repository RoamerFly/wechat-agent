package fishidentity

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func fishTestID() string { return "fish-" + strings.Repeat("a", 48) }

func TestFishDeviceBindingLifecycle(t *testing.T) {
	registry := New(t.TempDir())
	if _, _, err := registry.BindDevice("account-a", "emulator-5554"); err == nil {
		t.Fatal("device binding succeeded before the account joined the fish")
	}
	fishID, err := registry.Bind("account-a")
	if err != nil {
		t.Fatalf("bind account-a: %v", err)
	}
	if fishID == "" {
		t.Fatal("empty fish ID")
	}
	if _, _, err := registry.BindDevice("account-a", "device.with.dot"); err == nil {
		t.Fatal("device binding accepted an ID rejected by the mobile device specification")
	}

	gotFish, firstBinding, err := registry.BindDevice("account-a", "emulator-5554")
	if err != nil {
		t.Fatalf("first device bind: %v", err)
	}
	if gotFish != fishID || !validFishDeviceBindingID(firstBinding) {
		t.Fatalf("unexpected first device binding: fish=%q binding=%q", gotFish, firstBinding)
	}
	if gotFish, again, err := registry.BindDevice("account-a", "emulator-5554"); err != nil || gotFish != fishID || again != firstBinding {
		t.Fatalf("idempotent device bind = (%q, %q, %v), want (%q, %q, nil)", gotFish, again, err, fishID, firstBinding)
	}
	if err := registry.UnbindDevice("account-a", "emulator-5554", "fish-device-"+strings.Repeat("b", 48)); err == nil {
		t.Fatal("stale page unbound the current device binding")
	}
	if err := registry.UnbindDevice("account-a", "emulator-5554", firstBinding); err != nil {
		t.Fatalf("unbind current device: %v", err)
	}
	_, rebound, err := registry.BindDevice("account-a", "emulator-5554")
	if err != nil {
		t.Fatalf("rebind device: %v", err)
	}
	if rebound == firstBinding {
		t.Fatal("unbind/rebind reused a device binding ID")
	}
}

func TestFishAccountUnbindOnlyRemovesItsDeviceBindings(t *testing.T) {
	registry := New(t.TempDir())
	fishA, err := registry.Bind("account-a")
	if err != nil {
		t.Fatal(err)
	}
	fishB, err := registry.Bind("account-b")
	if err != nil || fishA != fishB {
		t.Fatalf("bind account-b: fish=%q err=%v", fishB, err)
	}
	_, bindingA, err := registry.BindDevice("account-a", "emulator-5554")
	if err != nil {
		t.Fatal(err)
	}
	_, bindingB, err := registry.BindDevice("account-b", "emulator-5556")
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Unbind("account-a"); err != nil {
		t.Fatalf("unbind account-a: %v", err)
	}
	if _, _, err := registry.DeviceBindings("account-a"); err == nil {
		t.Fatal("unbound account still exposed device binding metadata")
	}
	gotFish, bindings, err := registry.DeviceBindings("account-b")
	if err != nil {
		t.Fatalf("read account-b bindings: %v", err)
	}
	if gotFish != fishA || len(bindings) != 1 || bindings[0].BindingID != bindingB {
		t.Fatalf("unbind account-a changed account-b binding: fish=%q bindings=%#v", gotFish, bindings)
	}
	if err := registry.UnbindDevice("account-a", "emulator-5554", bindingA); err == nil {
		t.Fatal("former account could modify a revoked device binding")
	}
}

func TestFishDeviceBindingV1CompatibilityAndFailClosedValidation(t *testing.T) {
	t.Run("read v1 but do not auto-authorize devices", func(t *testing.T) {
		registry := New(t.TempDir())
		v1 := fmt.Sprintf(`{"version":1,"fish_id":%q,"bindings":[{"platform":"wechat","account_id":"account-a"}]}`, fishTestID())
		if err := os.WriteFile(registry.path, []byte(v1), 0600); err != nil {
			t.Fatal(err)
		}
		fishID, bindings, err := registry.DeviceBindings("account-a")
		if err != nil || fishID != fishTestID() || len(bindings) != 0 {
			t.Fatalf("v1 read = fish=%q bindings=%#v err=%v", fishID, bindings, err)
		}
		_, bindingID, err := registry.BindDevice("account-a", "emulator-5554")
		if err != nil || !validFishDeviceBindingID(bindingID) {
			t.Fatalf("v1 migration by explicit bind: id=%q err=%v", bindingID, err)
		}
		record, err := registry.Read()
		if err != nil || record.Version != fishIdentityCurrentVersion || len(record.DeviceBindings) != 1 {
			t.Fatalf("explicit device bind did not safely upgrade v1: %#v err=%v", record, err)
		}
	})

	for name, document := range map[string]string{
		"v1 carrying device field":         fmt.Sprintf(`{"version":1,"fish_id":%q,"bindings":[{"platform":"wechat","account_id":"account-a"}],"device_bindings":[]}`, fishTestID()),
		"v2 device for unbound account":    fmt.Sprintf(`{"version":2,"fish_id":%q,"bindings":[{"platform":"wechat","account_id":"account-a"}],"device_bindings":[{"account_id":"account-b","device_id":"emulator-5554","binding_id":%q}]}`, fishTestID(), "fish-device-"+strings.Repeat("c", 48)),
		"v2 device uses account ID syntax": fmt.Sprintf(`{"version":2,"fish_id":%q,"bindings":[{"platform":"wechat","account_id":"account-a"}],"device_bindings":[{"account_id":"account-a","device_id":"device.with.dot","binding_id":%q}]}`, fishTestID(), "fish-device-"+strings.Repeat("d", 48)),
		"v2 duplicate device":              fmt.Sprintf(`{"version":2,"fish_id":%q,"bindings":[{"platform":"wechat","account_id":"account-a"}],"device_bindings":[{"account_id":"account-a","device_id":"emulator-5554","binding_id":%q},{"account_id":"account-a","device_id":"emulator-5554","binding_id":%q}]}`, fishTestID(), "fish-device-"+strings.Repeat("e", 48), "fish-device-"+strings.Repeat("f", 48)),
	} {
		t.Run(name, func(t *testing.T) {
			registry := New(t.TempDir())
			if err := os.WriteFile(registry.path, []byte(document), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := registry.DeviceBindings("account-a"); err == nil {
				t.Fatalf("corrupt registry %q was accepted", name)
			}
			data, err := os.ReadFile(registry.path)
			if err != nil || string(data) != document {
				t.Fatalf("corrupt registry %q was overwritten: %v %q", name, err, data)
			}
		})
	}
}
