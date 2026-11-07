//go:build windows

package obspreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows"
)

var (
	ownedEncoderHelperPort       = flag.Int("obspreview-owned-encoder-port", 0, "test-only owned encoder port")
	ownedEncoderHelperPIDFile    = flag.String("obspreview-owned-encoder-pid-file", "", "test-only owned encoder child PID file")
	ownedEncoderHelperGrandchild = flag.Bool("obspreview-owned-encoder-grandchild", false, "test-only owned encoder grandchild")
	ownedEncoderCrashHostFile    = flag.String("obspreview-owned-encoder-crash-host-file", "", "test-only outer encoder host readiness file")
)

type crashHostReady struct {
	Port          int    `json:"port"`
	EncoderPID    uint32 `json:"encoder_pid"`
	GrandchildPID uint32 `json:"grandchild_pid"`
}

func TestOwnedEncoderHelper(t *testing.T) {
	if *ownedEncoderHelperGrandchild {
		select {}
	}
	if *ownedEncoderHelperPort == 0 {
		return
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(*ownedEncoderHelperPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if *ownedEncoderHelperPIDFile != "" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(exe, "-test.run=^TestOwnedEncoderHelper$", "-obspreview-owned-encoder-grandchild")
		child.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(*ownedEncoderHelperPIDFile, []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		writeHelperWire(conn, 0, map[string]any{"rpcVersion": 1, "authentication": map[string]string{
			"salt": base64Test("salt"), "challenge": base64Test("challenge"),
		}})
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		writeHelperWire(conn, 2, map[string]int{"negotiatedRpcVersion": 1})
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var envelope struct {
			Op int             `json:"op"`
			D  json.RawMessage `json:"d"`
		}
		var request struct {
			RequestType string `json:"requestType"`
			RequestID   string `json:"requestId"`
		}
		if json.Unmarshal(raw, &envelope) != nil || envelope.Op != 6 ||
			json.Unmarshal(envelope.D, &request) != nil || request.RequestType != "GetStreamStatus" {
			return
		}
		// The parent sets this marker before launch. A true value proves the
		// narrow child environment leaked an unrelated host variable.
		leaked := os.Getenv("OBSPREVIEW_TEST_PARENT_SECRET") != ""
		writeHelperWire(conn, 7, map[string]any{
			"requestType": request.RequestType, "requestId": request.RequestID,
			"requestStatus": map[string]any{"result": true, "code": 100},
			"responseData":  map[string]bool{"outputActive": leaked, "outputReconnecting": false},
		})
	})}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

// TestOwnedEncoderCrashHost is an isolated test executable used only by the
// parent crash test below. The parent terminates this host without calling
// Stop, then verifies that KILL_ON_JOB_CLOSE reaps the encoder process tree.
func TestOwnedEncoderCrashHost(t *testing.T) {
	if *ownedEncoderCrashHostFile == "" {
		return
	}
	directory := filepath.Dir(*ownedEncoderCrashHostFile)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	process := launchSyntheticEncoder(t, ctx, directory, filepath.Join(directory, "crash-grandchild.pid"))
	_ = waitOwnedStatus(t, ctx, process)
	grandchild := waitPIDFile(t, ctx, filepath.Join(directory, "crash-grandchild.pid"))
	encoded, err := json.Marshal(crashHostReady{Port: process.stream.port, EncoderPID: process.pid, GrandchildPID: grandchild})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*ownedEncoderCrashHostFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func writeHelperWire(conn *websocket.Conn, operation int, data any) {
	_ = conn.WriteJSON(map[string]any{"op": operation, "d": data})
}

func base64Test(value string) string {
	return "Y2hhbmdlLW1l" // syntactically valid challenge material is sufficient for test authentication.
}

func TestOwnedEncoderLaunchControlsExactChildAndStopsTree(t *testing.T) {
	t.Setenv("OBSPREVIEW_TEST_PARENT_SECRET", "must-not-reach-child")
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "grandchild.pid")
	process := launchSyntheticEncoder(t, ctx, directory, pidFile)
	t.Cleanup(func() { _ = process.Stop(context.Background()) })

	status := waitOwnedStatus(t, ctx, process)
	if status.Active || status.Reconnecting {
		t.Fatalf("unexpected status indicates leaked parent environment: %+v", status)
	}
	grandchildPID := waitPIDFile(t, ctx, pidFile)
	if !process.running() {
		t.Fatal("owned encoder process exited before stop")
	}
	unrelated, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer unrelated.Close()

	if err := process.Stop(ctx); err != nil {
		t.Fatalf("stop owned encoder: %v", err)
	}
	if process.running() {
		t.Fatal("owned encoder is still running after job stop")
	}
	assertProcessStopped(t, grandchildPID)
	if err := waitPortClosed(ctx, process.stream.port); err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialTimeout("tcp4", unrelated.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("stopping an owned job affected unrelated helper: %v", err)
	}
	_ = connection.Close()
}

func TestOwnedEncoderLaunchRejectsDigestBeforeChildCreation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	port := reserveEncoderPort(t)
	_, err = LaunchEncoder(ctx, EncoderLaunch{
		Executable: executable, Directory: t.TempDir(), SHA256: strings.Repeat("0", sha256.Size*2),
		Arguments:   []string{"-test.run=^TestOwnedEncoderHelper$", "-obspreview-owned-encoder-port=" + strconv.Itoa(port)},
		ControlPort: port, ControlPassword: "synthetic-control-password",
	})
	if !errors.Is(err, ErrEncoderLaunch) {
		t.Fatalf("digest mismatch error = %v", err)
	}
	if connection, dialErr := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 150*time.Millisecond); dialErr == nil {
		_ = connection.Close()
		t.Fatal("digest-rejected launch opened a control listener")
	}
}

func TestOwnedEncoderProcessRejectsUnrelatedControlPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	directory := t.TempDir()
	process := launchSyntheticEncoder(t, ctx, directory, "")
	t.Cleanup(func() { _ = process.Stop(context.Background()) })
	_ = waitOwnedStatus(t, ctx, process)

	unrelated, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer unrelated.Close()
	accepted := make(chan *net.TCPConn, 1)
	go func() {
		connection, acceptErr := unrelated.(*net.TCPListener).AcceptTCP()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	client, err := net.DialTimeout("tcp4", unrelated.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()
	if err := process.verify(ctx, client.(*net.TCPConn)); !errors.Is(err, ErrStreamEndpoint) {
		t.Fatalf("unrelated control peer = %v", err)
	}
}

func TestOwnedEncoderHostCrashClosesJobTree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
	defer cancel()
	directory := t.TempDir()
	readyFile := filepath.Join(directory, "crash-host-ready.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	host := exec.Command(executable, "-test.run=^TestOwnedEncoderCrashHost$", "-obspreview-owned-encoder-crash-host-file="+readyFile)
	host.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var hostOutput bytes.Buffer
	host.Stdout, host.Stderr = &hostOutput, &hostOutput
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped && host.Process != nil {
			_ = host.Process.Kill()
			_, _ = host.Process.Wait()
		}
	})
	ready, readyErr := readCrashHostReady(ctx, readyFile)
	if readyErr != nil {
		t.Fatalf("crash host did not become ready: %v\n%s", readyErr, hostOutput.String())
	}
	if ready.Port < 1 || ready.EncoderPID == 0 || ready.GrandchildPID == 0 {
		t.Fatalf("incomplete crash host readiness: %+v", ready)
	}
	encoderHandle := openProcessForStop(t, ready.EncoderPID)
	defer windows.CloseHandle(encoderHandle)
	grandchildHandle := openProcessForStop(t, ready.GrandchildPID)
	defer windows.CloseHandle(grandchildHandle)
	if err := host.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := host.Wait(); err == nil {
		t.Fatal("crash host unexpectedly reported a clean exit")
	}
	stopped = true
	assertStoppedHandle(t, encoderHandle)
	assertStoppedHandle(t, grandchildHandle)
	if err := waitPortClosed(ctx, ready.Port); err != nil {
		t.Fatal(err)
	}
}

func launchSyntheticEncoder(t *testing.T, ctx context.Context, directory, pidFile string) *EncoderProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := encoderDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	port := reserveEncoderPort(t)
	args := []string{"-test.run=^TestOwnedEncoderHelper$", "-obspreview-owned-encoder-port=" + strconv.Itoa(port)}
	if pidFile != "" {
		args = append(args, "-obspreview-owned-encoder-pid-file="+pidFile)
	}
	process, err := LaunchEncoder(ctx, EncoderLaunch{
		Executable: executable, Directory: directory, SHA256: digest, Arguments: args,
		ControlPort: port, ControlPassword: "synthetic-control-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func encoderDigest(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}

func reserveEncoderPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func waitOwnedStatus(t *testing.T, ctx context.Context, process *EncoderProcess) StreamStatus {
	t.Helper()
	for {
		status, err := process.Stream().Status(ctx)
		if err == nil {
			return status
		}
		if ctx.Err() != nil {
			t.Fatalf("owned encoder never became available: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func waitPIDFile(t *testing.T, ctx context.Context, path string) uint32 {
	t.Helper()
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			value, parseErr := strconv.ParseUint(strings.TrimSpace(string(contents)), 10, 32)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			return uint32(value)
		}
		if ctx.Err() != nil {
			t.Fatalf("owned encoder did not record grandchild: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func waitCrashHostReady(t *testing.T, ctx context.Context, path string) crashHostReady {
	t.Helper()
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			var ready crashHostReady
			if err := json.Unmarshal(contents, &ready); err != nil {
				t.Fatal(err)
			}
			return ready
		}
		if ctx.Err() != nil {
			t.Fatalf("crash host did not become ready: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readCrashHostReady(ctx context.Context, path string) (crashHostReady, error) {
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			var ready crashHostReady
			if err := json.Unmarshal(contents, &ready); err != nil {
				return crashHostReady{}, err
			}
			return ready, nil
		}
		if ctx.Err() != nil {
			return crashHostReady{}, err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func waitPortClosed(ctx context.Context, port int) error {
	for {
		connection, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err != nil {
			return nil
		}
		_ = connection.Close()
		if ctx.Err() != nil {
			return errors.New("owned encoder control port remained open")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func assertProcessStopped(t *testing.T, pid uint32) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return // The process has already been reaped.
		}
		t.Fatalf("open owned encoder child: %v", err)
	}
	defer windows.CloseHandle(handle)
	assertStoppedHandle(t, handle)
}

func openProcessForStop(t *testing.T, pid uint32) windows.Handle {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		t.Fatalf("open running owned process %d: %v", pid, err)
	}
	return handle
}

func assertStoppedHandle(t *testing.T, handle windows.Handle) {
	t.Helper()
	state, err := windows.WaitForSingleObject(handle, 2000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("owned encoder child is still alive: state=%d err=%v", state, err)
	}
}
