package fishidentity

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestGuardHomeChecksCurrentOwnerAndRevision(t *testing.T) {
	dir := t.TempDir()
	registry, other := New(dir), New(dir)
	initial, err := registry.InitializeLocal()
	if err != nil {
		t.Fatal(err)
	}
	owner := initial.LocalOwner
	check := func(fishID, ownerID string, revision uint64, permitted bool) {
		t.Helper()
		called := false
		err := registry.GuardHome(fishID, ownerID, revision, func(record Record) error {
			called = true
			if record.FishID != fishID || record.LocalOwner == nil || record.LocalOwner.ID != ownerID || record.LocalOwner.Revision != revision {
				t.Fatalf("mutation received an unexpected identity: %#v", record)
			}
			return nil
		})
		if called != permitted || permitted && err != nil || !permitted && !errors.Is(err, ErrHomeRevisionConflict) {
			t.Fatalf("guard permission = %t, callback = %t, err = %v", permitted, called, err)
		}
	}
	check(initial.FishID, owner.ID, owner.Revision, true)
	check(initial.FishID, "other-owner", owner.Revision, false)
	paused, err := other.SetHomeAccess(owner.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	check(initial.FishID, owner.ID, paused.LocalOwner.Revision, false)
	resumed, err := other.SetHomeAccess(paused.LocalOwner.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	check(initial.FishID, owner.ID, owner.Revision, false)
	check(initial.FishID, owner.ID, resumed.LocalOwner.Revision, true)
}

func TestFishLocalIdentityInitializesWithoutApplicationAndPersists(t *testing.T) {
	dir := t.TempDir()
	registry := New(dir)
	before, err := registry.Read()
	if err != nil {
		t.Fatal(err)
	}
	if before.FishID != "" || before.LocalOwner != nil {
		t.Fatalf("a read of an unconfigured registry must not initialize it: %#v", before)
	}
	if _, err := os.Stat(filepath.Join(dir, "fish-identity.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read unexpectedly wrote identity registry: %v", err)
	}

	created, err := registry.InitializeLocal()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidFishID(created.FishID) || created.LocalOwner == nil || !ValidLocalOwner(*created.LocalOwner) || !created.LocalOwner.HomeAccess || created.LocalOwner.Revision != 1 {
		t.Fatalf("unexpected initialized identity: %#v", created)
	}
	if len(created.Bindings) != 0 {
		t.Fatalf("local initialization unexpectedly bound an application: %#v", created.Bindings)
	}

	afterRestart, err := New(dir).Read()
	if err != nil {
		t.Fatal(err)
	}
	if afterRestart.FishID != created.FishID || afterRestart.LocalOwner == nil || *afterRestart.LocalOwner != *created.LocalOwner {
		t.Fatalf("local identity did not survive restart: got %#v, want %#v", afterRestart, created)
	}
}

func TestFishLocalIdentityPauseIsNotReenabledAndUsesCAS(t *testing.T) {
	registry := New(t.TempDir())
	created, err := registry.InitializeLocal()
	if err != nil {
		t.Fatal(err)
	}
	paused, err := registry.SetHomeAccess(created.LocalOwner.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	if paused.LocalOwner.HomeAccess || paused.LocalOwner.Revision != 2 {
		t.Fatalf("pause not recorded: %#v", paused.LocalOwner)
	}
	retried, err := registry.InitializeLocal()
	if err != nil {
		t.Fatal(err)
	}
	if retried.LocalOwner.HomeAccess || retried.LocalOwner.Revision != paused.LocalOwner.Revision {
		t.Fatalf("initialize silently reenabled paused grant: %#v", retried.LocalOwner)
	}
	if _, err := registry.SetHomeAccess(1, true); !errors.Is(err, ErrHomeRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	resumed, err := registry.SetHomeAccess(2, true)
	if err != nil || !resumed.LocalOwner.HomeAccess || resumed.LocalOwner.Revision != 3 {
		t.Fatalf("resume = %#v, %v", resumed.LocalOwner, err)
	}
	if _, err := New(t.TempDir()).SetHomeAccess(1, true); !errors.Is(err, ErrHomeNotInitialized) {
		t.Fatalf("uninitialized mutation error = %v", err)
	}
}

func TestFishLocalIdentitySurvivesWechatUnbind(t *testing.T) {
	registry := New(t.TempDir())
	created, err := registry.InitializeLocal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Bind("local-owner-wechat"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Unbind("local-owner-wechat"); err != nil {
		t.Fatal(err)
	}
	got, err := registry.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.FishID != created.FishID || got.LocalOwner == nil || *got.LocalOwner != *created.LocalOwner || len(got.Bindings) != 0 {
		t.Fatalf("WeChat unbind changed local owner: %#v", got)
	}
}

func TestFishLocalIdentityReadsLegacyWithoutAutoAuthorization(t *testing.T) {
	for _, version := range []int{fishIdentityVersionV1, fishIdentityVersionV2} {
		t.Run("v"+string(rune('0'+version)), func(t *testing.T) {
			dir := t.TempDir()
			fixture := `{"version":` + string(rune('0'+version)) + `,"fish_id":"fish-0123456789abcdef0123456789abcdef0123456789abcdef","bindings":[]}`
			if err := os.WriteFile(filepath.Join(dir, "fish-identity.json"), []byte(fixture), 0600); err != nil {
				t.Fatal(err)
			}
			registry := New(dir)
			read, err := registry.Read()
			if err != nil {
				t.Fatal(err)
			}
			if read.Version != version || read.LocalOwner != nil {
				t.Fatalf("legacy read auto-migrated or authorized: %#v", read)
			}
			created, err := registry.InitializeLocal()
			if err != nil {
				t.Fatal(err)
			}
			if created.Version != fishIdentityCurrentVersion || created.LocalOwner == nil || !created.LocalOwner.HomeAccess {
				t.Fatalf("explicit legacy initialization failed: %#v", created)
			}
		})
	}
}

func TestFishLocalIdentityRejectsDamagedOwnerWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fish-identity.json")
	damaged := []byte(`{"version":3,"fish_id":"fish-0123456789abcdef0123456789abcdef0123456789abcdef","bindings":[],"local_owner":{"id":"owner-bad","revision":0,"home_access":true}}`)
	if err := os.WriteFile(path, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	registry := New(dir)
	if _, err := registry.InitializeLocal(); err == nil {
		t.Fatal("damaged registry initialized")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(damaged) {
		t.Fatalf("damaged registry overwritten: %s", got)
	}
}

func TestFishLocalIdentityAndWechatBindingDoNotLoseConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	local := New(dir)
	wechat := New(dir) // simulates the legacy manager process.
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := local.InitializeLocal()
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := wechat.Bind("concurrent-wechat")
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := New(dir).Read()
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalOwner == nil || !got.LocalOwner.HomeAccess || !fishAccountIsBound(got, "concurrent-wechat") {
		t.Fatalf("concurrent update was lost: %#v", got)
	}
}
