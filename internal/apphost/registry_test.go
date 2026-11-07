package apphost

import "testing"

// The catalog can be built without querying or starting a provider.
type passiveProvider struct{ Provider }

func TestRegistryRejectsConflictingIdentity(t *testing.T) {
	wechat := Registration{
		Application: Application{ID: "wechat", Name: "微信", InstallationID: "wechat-legacy", Registration: "legacy"},
		Provider:    passiveProvider{},
	}
	other := wechat
	other.ID, other.InstallationID = "bilibili", "bilibili-local"
	r, err := New(wechat, other)
	if err != nil {
		t.Fatal(err)
	}
	apps := r.Applications()
	if len(apps) != 2 || apps[0].ID != other.ID || apps[1].ID != wechat.ID {
		t.Fatalf("unexpected host catalog: %+v", apps)
	}
	if _, ok := r.Lookup("unknown"); ok {
		t.Fatal("unknown application resolved to another registration")
	}
	if _, err := New(wechat, wechat); err == nil {
		t.Fatal("duplicate app ID overwrote the original provider")
	}
	other.InstallationID = wechat.InstallationID
	if _, err := New(wechat, other); err == nil {
		t.Fatal("different apps shared an installation ID")
	}
}
