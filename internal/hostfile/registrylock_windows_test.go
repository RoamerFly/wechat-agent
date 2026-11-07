//go:build windows

package hostfile

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPinnedRegistryLockInteroperatesWithDesktopFileStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-cutover.lock")
	tryDesktopLock := func() (bool, error) {
		// Match the desktop FileStream constructor and byte range in a separate process.
		cmd := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `
$ErrorActionPreference = 'Stop'
$gate = [IO.File]::Open($env:AHS_SYNTHETIC_LOCK_PATH, 'OpenOrCreate', 'ReadWrite', 'ReadWrite')
try {
  try { $gate.Lock(0, 1) } catch [IO.IOException] { exit 37 }
} finally { $gate.Dispose() }
`)
		cmd.Env = append(os.Environ(), "AHS_SYNTHETIC_LOCK_PATH="+path)
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 37 {
			return false, nil
		}
		return err == nil, err
	}
	release, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := tryDesktopLock()
	release()
	if err != nil || acquired {
		t.Fatalf("desktop lock did not reject the Go-owned byte range: %v, %v", acquired, err)
	}
	if acquired, err = tryDesktopLock(); err != nil || !acquired {
		t.Fatalf("desktop lock did not open after Go released the byte range: %v, %v", acquired, err)
	}
}

func TestPinnedRegistryLockRejectsNonRegularPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.lock")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(path); err == nil {
		t.Fatal("directory accepted as identity lock")
	}
}

func TestPinnedRegistryLockBlocksReplacementAndStillSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.lock")
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	if err := os.Remove(path); err == nil {
		t.Fatal("locked identity sidecar was deleted")
	}

	acquired := make(chan func(), 1)
	failures := make(chan error, 1)
	go func() {
		next, err := Lock(path)
		if err != nil {
			failures <- err
			return
		}
		acquired <- next
	}()
	select {
	case next := <-acquired:
		next()
		t.Fatal("second lock did not wait for first owner")
	case err := <-failures:
		t.Fatalf("second lock failed instead of waiting: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	released = true
	select {
	case next := <-acquired:
		next()
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("second lock did not continue after release")
	}
}

func TestPinnedRegistryLockConcurrentFirstCreationStillSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.lock")
	start := make(chan struct{})
	acquired := make(chan func(), 2)
	failures := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			release, err := Lock(path)
			if err != nil {
				failures <- err
				return
			}
			acquired <- release
		}()
	}
	close(start)
	var first func()
	select {
	case first = <-acquired:
	case err := <-failures:
		t.Fatalf("first creator failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no creator acquired first empty sidecar")
	}
	select {
	case second := <-acquired:
		second()
		first()
		t.Fatal("two first creators owned lock concurrently")
	case err := <-failures:
		first()
		t.Fatalf("competing first creator failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	first()
	select {
	case second := <-acquired:
		second()
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("second first-creator did not continue after release")
	}
}

func TestPinnedRegistryLockRejectsSymlinkWhenSupported(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.lock")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "identity.lock")
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skip("Windows symlink creation is not available to this test user")
		}
		t.Fatal(err)
	}
	if _, err := Lock(link); err == nil {
		t.Fatal("symlink accepted as identity lock")
	}
}
