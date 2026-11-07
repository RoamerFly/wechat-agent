//go:build windows

package obspreview

import (
	"context"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ownedDesktopHelperFile = flag.String("obspreview-owned-desktop-file", "", "test-only private desktop name file")
var ownedDesktopPublisherAddress = flag.String("obspreview-owned-desktop-publisher-address", "", "test-only private desktop publisher address")

var (
	getThreadDesktop          = user32DLL.NewProc("GetThreadDesktop")
	getUserObjectInformationW = user32DLL.NewProc("GetUserObjectInformationW")
	openDesktopW              = user32DLL.NewProc("OpenDesktopW")
)

func TestOwnedDesktopHelper(t *testing.T) {
	if *ownedDesktopHelperFile == "" {
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	name, err := currentOwnedDesktopName()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*ownedDesktopHelperFile, []byte(name), 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestOwnedDesktopPublisherHelper(t *testing.T) {
	if *ownedDesktopPublisherAddress == "" {
		return
	}
	connection, err := net.DialTimeout("tcp4", *ownedDesktopPublisherAddress, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {}
}

func TestLaunchPortableEncoderProcessUsesPrivateDesktop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := encoderDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	desktopFile := filepath.Join(directory, "desktop-name.txt")
	process, err := LaunchPortableEncoderProcess(ctx, EncoderLaunch{
		Executable: executable,
		Directory:  directory,
		SHA256:     digest,
		Arguments: []string{
			"-test.run=^TestOwnedDesktopHelper$",
			"-obspreview-owned-desktop-file=" + desktopFile,
		},
	})
	if err != nil {
		t.Fatalf("launch portable encoder: %v", err)
	}
	t.Cleanup(func() { _ = process.Stop(context.Background()) })
	if process.Stream() != nil {
		t.Fatal("portable encoder unexpectedly exposes a websocket stream")
	}
	if err := process.Start(ctx, 1935, "abcdefghijklmnop"); !errors.Is(err, ErrStreamEndpoint) {
		t.Fatalf("portable process start = %v, want %v", err, ErrStreamEndpoint)
	}
	desktopName := waitDesktopName(t, ctx, desktopFile)
	if !strings.HasPrefix(desktopName, "AgentHomeStation-encoder-") {
		t.Fatalf("child desktop = %q, want private encoder desktop", desktopName)
	}
	if process.desktop == nil || desktopName != process.desktop.name {
		t.Fatalf("child desktop = %q, process desktop = %#v", desktopName, process.desktop)
	}
	if err := process.Stop(ctx); err != nil {
		t.Fatalf("stop portable encoder: %v", err)
	}
	if process.desktop.handle != 0 {
		t.Fatal("private desktop handle remains open after owned job stops")
	}
	waitDesktopClosed(t, ctx, desktopName)
}

func TestPortableEncoderPublisherRejectsListenersAndAcceptsListenerFreeSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan *net.TCPConn, 1)
	go func() {
		connection, acceptErr := listener.(*net.TCPListener).AcceptTCP()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := encoderDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	portable, err := LaunchPortableEncoderProcess(ctx, EncoderLaunch{
		Executable: executable,
		Directory:  directory,
		SHA256:     digest,
		Arguments: []string{
			"-test.run=^TestOwnedDesktopPublisherHelper$",
			"-obspreview-owned-desktop-publisher-address=" + listener.Addr().String(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = portable.Stop(context.Background()) })
	var source *net.TCPConn
	select {
	case source = <-accepted:
		defer source.Close()
	case <-ctx.Done():
		t.Fatal("portable encoder did not connect to local relay")
	}
	if err := portable.VerifyPublisher(ctx, source); err != nil {
		t.Fatalf("listener-free portable source rejected: %v", err)
	}

	self, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(self)
	listenerProcess := &EncoderProcess{process: self, pid: uint32(os.Getpid()), portable: true}
	local := acceptedLoopbackEndpoint(t, "127.0.0.1:0")
	defer local.Close()
	if err := listenerProcess.VerifyPublisher(ctx, local); !errors.Is(err, ErrStreamEndpoint) {
		t.Fatalf("portable process with listener = %v, want %v", err, ErrStreamEndpoint)
	}
}

func currentOwnedDesktopName() (string, error) {
	handle, _, err := getThreadDesktop.Call(uintptr(windows.GetCurrentThreadId()))
	if handle == 0 {
		return "", err
	}
	var required uint32
	_, _, err = getUserObjectInformationW.Call(handle, 2, 0, 0, uintptr(unsafe.Pointer(&required))) // UOI_NAME
	if required == 0 {
		return "", err
	}
	buffer := make([]uint16, (required+1)/2)
	r1, _, err := getUserObjectInformationW.Call(handle, 2, uintptr(unsafe.Pointer(&buffer[0])), uintptr(required), uintptr(unsafe.Pointer(&required)))
	if r1 == 0 {
		return "", err
	}
	return windows.UTF16ToString(buffer), nil
}

func waitDesktopName(t *testing.T, ctx context.Context, path string) string {
	t.Helper()
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(contents))
		}
		if ctx.Err() != nil {
			t.Fatalf("portable child did not report private desktop: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func openOwnedDesktop(name string) (windows.Handle, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	r1, _, callErr := openDesktopW.Call(uintptr(unsafe.Pointer(namePtr)), 0, 0, 1) // DESKTOP_READOBJECTS
	if r1 == 0 {
		return 0, callErr
	}
	return windows.Handle(r1), nil
}

func waitDesktopClosed(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	for {
		handle, err := openOwnedDesktop(name)
		if err != nil {
			return
		}
		_ = closeDesktopHandle(handle)
		if ctx.Err() != nil {
			t.Fatalf("private desktop %q remains after process stop", name)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func closeDesktopHandle(handle windows.Handle) error {
	r1, _, err := closeDesktop.Call(uintptr(handle))
	if r1 == 0 {
		return err
	}
	return nil
}
