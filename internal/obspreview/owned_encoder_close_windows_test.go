//go:build windows

package obspreview

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var gracefulEncoderHelperFile = flag.String("obspreview-graceful-encoder-file", "", "test-only graceful encoder readiness file")

func TestGracefulEncoderHelper(t *testing.T) {
	if *gracefulEncoderHelperFile == "" {
		return
	}
	if err := os.WriteFile(*gracefulEncoderHelperFile, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestEncoderCloseOnlyTargetsOwnedPIDOnEnumeratedDesktop(t *testing.T) {
	// These synthetic top-level windows share a desktop. Neither another
	// process's window nor a vanished window (PID zero) may receive WM_CLOSE.
	owners := map[uintptr]uint32{10: 92, 20: 93, 30: 92, 40: 0}
	var posted []uintptr
	postEncoderCloseForPID(92, []uintptr{0, 10, 20, 30, 40},
		func(window uintptr) uint32 { return owners[window] },
		func(window uintptr) { posted = append(posted, window) })
	if !reflect.DeepEqual(posted, []uintptr{10, 30}) {
		t.Fatalf("close targets = %v, want only encoder windows [10 30]", posted)
	}
	postEncoderCloseForPID(0, []uintptr{40},
		func(uintptr) uint32 { t.Fatal("queried a window without an owned PID"); return 0 },
		func(uintptr) { t.Fatal("posted without an owned PID") })
}

func TestEncoderStopGracefullyNonPortableUsesJobAndIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process := launchSyntheticEncoder(t, ctx, t.TempDir(), "")
	t.Cleanup(func() { _ = process.Stop(context.Background()) })
	_ = waitOwnedStatus(t, ctx, process)
	if !process.Running() {
		t.Fatal("running encoder reported unavailable")
	}
	if err := process.StopGracefully(ctx); err != nil {
		t.Fatal(err)
	}
	if process.Running() {
		t.Fatal("stopped encoder reported running")
	}
	if err := process.StopGracefully(ctx); err != nil {
		t.Fatalf("repeat graceful stop: %v", err)
	}
}

func TestPortableEncoderStopGracefullyCancellationAndConcurrentStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process := launchGracefulSyntheticPortable(t, ctx)
	desktopName := process.desktop.name
	stopping, stopCancel := context.WithCancel(context.Background())
	gracefulResult := make(chan error, 1)
	go func() { gracefulResult <- process.StopGracefully(stopping) }()
	// A short grace deadline interrupts the two-second wait. The concurrent
	// force stop must safely reuse the same handles and complete cleanup.
	time.AfterFunc(30*time.Millisecond, stopCancel)
	forcedResult := make(chan error, 1)
	go func() { forcedResult <- process.Stop(ctx) }()
	select {
	case err := <-gracefulResult:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled graceful stop: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("graceful and force stop deadlocked")
	}
	select {
	case err := <-forcedResult:
		if err != nil {
			t.Fatalf("concurrent force stop: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("concurrent force stop did not finish")
	}
	if err := process.StopGracefully(ctx); err != nil {
		t.Fatal(err)
	}
	if process.Running() || process.desktop.handle != 0 {
		t.Fatal("stopped portable process retained a process or desktop")
	}
	waitDesktopClosed(t, ctx, desktopName)
}

func TestPortableEncoderStopGracefullyFallsBackWithinTwoSeconds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process := launchGracefulSyntheticPortable(t, ctx)
	started := time.Now()
	if err := process.StopGracefully(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("graceful stop exceeded bounded grace and cleanup: %v", elapsed)
	}
	if process.Running() || process.desktop.handle != 0 {
		t.Fatal("fallback failed to reap the portable process and desktop")
	}
}

func TestPortableEncoderStopGracefullyHonorsContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process := launchGracefulSyntheticPortable(t, ctx)
	short, shortCancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer shortCancel()
	started := time.Now()
	if err := process.StopGracefully(short); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline stop: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("context did not interrupt graceful wait: %v", elapsed)
	}
	if process.Running() {
		t.Fatal("stopping process still admits work")
	}
	if err := process.Stop(ctx); err != nil {
		t.Fatalf("confirm cleanup after canceled wait: %v", err)
	}
	if process.desktop.handle != 0 {
		t.Fatal("desktop retained after confirmed stop")
	}
}

func launchGracefulSyntheticPortable(t *testing.T, ctx context.Context) *EncoderProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := encoderDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	ready := filepath.Join(directory, "desktop.txt")
	process, err := LaunchPortableEncoderProcess(ctx, EncoderLaunch{
		Executable: executable, Directory: directory, SHA256: digest,
		Arguments: []string{"-test.run=^TestGracefulEncoderHelper$", "-obspreview-graceful-encoder-file=" + ready},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Stop(context.Background()) })
	_ = waitDesktopName(t, ctx, ready)
	if !process.Running() {
		t.Fatal("synthetic portable process exited before the close test")
	}
	return process
}
