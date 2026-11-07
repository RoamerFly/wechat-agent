//go:build windows

package obspreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"wechat-bot/internal/workspacefs"
)

var ErrEncoderLaunch = errors.New("owned Windows encoder could not start")

// PROC_THREAD_ATTRIBUTE_JOB_LIST is ProcThreadAttributeValue(13, false, true, false).
const encoderJobListAttribute = 0x0002000d

// EncoderLaunch is a host-provisioned runtime, not a tool input. The caller
// must supply a trusted, isolated runtime and a private working directory.
// Arguments are nonsecret and must not contain credentials or push URLs.
type EncoderLaunch struct {
	Executable      string
	Directory       string
	SHA256          string
	Arguments       []string
	ControlPort     int
	ControlPassword string
}

// EncoderProcess owns one Windows job and its primary process handle. A job
// limits lifecycle, not filesystem or network access.
type EncoderProcess struct {
	mu       sync.Mutex
	job      windows.Handle
	process  windows.Handle
	pid      uint32
	stopping bool
	stream   *OwnedStream
	desktop  *ownedDesktop
	portable bool
}

// LaunchEncoder attaches the child to its private job at creation time, before
// it can run. It cannot attach to a running OBS instance or use preview settings.
func LaunchEncoder(ctx context.Context, config EncoderLaunch) (*EncoderProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := &EncoderProcess{}
	stream, err := NewOwnedStream(config.ControlPort, config.ControlPassword, p.verify)
	if err != nil {
		return nil, err
	}
	executable, err := verifyEncoderExecutable(config.Executable, config.SHA256)
	if err != nil {
		return nil, err
	}
	defer executable.Close()
	if !localEncoderPath(config.Directory) {
		return nil, fmt.Errorf("%w: invalid working directory", ErrEncoderLaunch)
	}
	directory, err := workspacefs.OpenPath(config.Directory)
	if err != nil {
		return nil, fmt.Errorf("%w: open working directory", ErrEncoderLaunch)
	}
	defer directory.Close()

	if err := p.launch(ctx, config, ""); err != nil {
		return nil, err
	}
	p.stream = stream
	return p, nil
}

// LaunchPortableEncoderProcess starts an independently configured encoder on a
// private Windows desktop. Unlike LaunchEncoder it deliberately has no OBS
// WebSocket control endpoint: the supplied runtime configuration owns stream
// start and output settings. Its window can therefore never appear on the
// user's interactive desktop.
func LaunchPortableEncoderProcess(ctx context.Context, config EncoderLaunch) (*EncoderProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.ControlPort != 0 || config.ControlPassword != "" {
		return nil, fmt.Errorf("%w: portable encoder cannot expose websocket control", ErrEncoderLaunch)
	}
	executable, err := verifyEncoderExecutable(config.Executable, config.SHA256)
	if err != nil {
		return nil, err
	}
	defer executable.Close()
	if !localEncoderPath(config.Directory) {
		return nil, fmt.Errorf("%w: invalid working directory", ErrEncoderLaunch)
	}
	directory, err := workspacefs.OpenPath(config.Directory)
	if err != nil {
		return nil, fmt.Errorf("%w: open working directory", ErrEncoderLaunch)
	}
	defer directory.Close()
	desktop, err := createOwnedDesktop()
	if err != nil {
		return nil, err
	}
	p := &EncoderProcess{desktop: desktop, portable: true}
	if err := p.launch(ctx, config, desktop.name); err != nil {
		_ = desktop.Close()
		return nil, err
	}
	return p, nil
}

func localEncoderPath(path string) bool {
	volume := filepath.VolumeName(path)
	return len(volume) == 2 && volume[1] == ':' && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func verifyEncoderExecutable(path, digest string) (*os.File, error) {
	if !localEncoderPath(path) || !strings.EqualFold(filepath.Ext(path), ".exe") {
		return nil, fmt.Errorf("%w: invalid executable path", ErrEncoderLaunch)
	}
	expected, err := hex.DecodeString(digest)
	if err != nil || len(expected) != sha256.Size {
		return nil, fmt.Errorf("%w: invalid executable digest", ErrEncoderLaunch)
	}
	root, err := workspacefs.OpenPath(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%w: open executable directory", ErrEncoderLaunch)
	}
	defer root.Close()
	info, err := root.Lstat(filepath.Base(path))
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: find executable", ErrEncoderLaunch)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid executable path", ErrEncoderLaunch)
	}
	// Disallow replacement and write handles while hashing and creating the process.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open executable: %w", ErrEncoderLaunch, err)
	}
	file := os.NewFile(uintptr(handle), path)
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: executable identity changed", ErrEncoderLaunch)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil || !bytes.Equal(hash.Sum(nil), expected) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: executable digest mismatch", ErrEncoderLaunch)
	}
	return file, nil
}

func (p *EncoderProcess) launch(ctx context.Context, config EncoderLaunch, desktopName string) error {
	app, err := windows.UTF16PtrFromString(config.Executable)
	if err != nil {
		return fmt.Errorf("%w: invalid executable path", ErrEncoderLaunch)
	}
	args := append([]string{config.Executable}, config.Arguments...)
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return fmt.Errorf("%w: invalid arguments", ErrEncoderLaunch)
	}
	directory, err := windows.UTF16PtrFromString(config.Directory)
	if err != nil {
		return fmt.Errorf("%w: invalid working directory", ErrEncoderLaunch)
	}
	env := narrowEncoderEnvironment(config.Directory)

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("%w: create job: %w", ErrEncoderLaunch, err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = windows.CloseHandle(job)
		}
	}()
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fmt.Errorf("%w: set job limits: %w", ErrEncoderLaunch, err)
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fmt.Errorf("%w: allocate process attributes: %w", ErrEncoderLaunch, err)
	}
	defer attributes.Delete()
	if err := attributes.Update(encoderJobListAttribute, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		return fmt.Errorf("%w: set job at creation: %w", ErrEncoderLaunch, err)
	}
	startup := windows.StartupInfoEx{
		StartupInfo:             windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE},
		ProcThreadAttributeList: attributes.List(),
	}
	if desktopName != "" {
		desktop, err := windows.UTF16PtrFromString(desktopName)
		if err != nil {
			return fmt.Errorf("%w: invalid private desktop", ErrEncoderLaunch)
		}
		startup.Desktop = desktop
	}
	var info windows.ProcessInformation
	if err := ctx.Err(); err != nil {
		return err
	}
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcess(app, command, nil, nil, false, flags, &env[0], directory, &startup.StartupInfo, &info); err != nil {
		return fmt.Errorf("%w: create owned process: %w", ErrEncoderLaunch, err)
	}
	defer windows.CloseHandle(info.Thread)
	if err := ctx.Err(); err != nil {
		_ = windows.CloseHandle(info.Process)
		return err
	}
	if _, err := windows.ResumeThread(info.Thread); err != nil {
		_ = windows.CloseHandle(info.Process)
		return fmt.Errorf("%w: resume owned process: %w", ErrEncoderLaunch, err)
	}
	p.job, p.process, p.pid = job, info.Process, info.ProcessId
	keep = true
	return nil
}

func narrowEncoderEnvironment(directory string) []uint16 {
	var entries []string
	for _, name := range []string{"SystemDrive", "SystemRoot"} {
		if value := os.Getenv(name); value != "" {
			entries = append(entries, name+"="+value)
		}
	}
	entries = append(entries, "TEMP="+directory, "TMP="+directory)
	if value := os.Getenv("WINDIR"); value != "" {
		entries = append(entries, "WINDIR="+value)
	}
	return append(utf16.Encode([]rune(strings.Join(entries, "\x00"))), 0, 0)
}

func (p *EncoderProcess) Stream() *OwnedStream { return p.stream }

// Start configures the owned encoder for the current host relay. The relay
// ticket is checked by OwnedStream and is never stored in this process object.
func (p *EncoderProcess) Start(ctx context.Context, port int, ticket string) error {
	p.mu.Lock()
	stream := p.stream
	running := !p.stopping && p.running()
	p.mu.Unlock()
	if !running || stream == nil {
		return ErrStreamEndpoint
	}
	return stream.Start(ctx, port, ticket)
}

func (p *EncoderProcess) verify(ctx context.Context, tcp *net.TCPConn) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopping || !p.running() {
		return ErrStreamEndpoint
	}
	if err := verifyEncoderPeer(ctx, tcp, p.pid); err != nil {
		return err
	}
	if !p.running() {
		return ErrStreamEndpoint
	}
	return nil
}

// VerifyPublisher admits a relay connection only when its opposite established
// socket belongs to this still-running, job-owned encoder process.
func (p *EncoderProcess) VerifyPublisher(ctx context.Context, socket *net.TCPConn) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopping || !p.running() {
		return ErrStreamEndpoint
	}
	// A portable process has no host-owned WebSocket control channel. It must
	// not expose any TCP listener before the host accepts it as an RTMP source.
	// The controlled-WebSocket strategy intentionally retains its listener and
	// therefore bypasses this portable-only rule.
	if p.portable && verifyEncoderHasNoTCPListeners(ctx, p.pid) != nil {
		return ErrStreamEndpoint
	}
	if err := verifyEncoderPublisher(ctx, socket, p.pid); err != nil {
		return err
	}
	if !p.running() {
		return ErrStreamEndpoint
	}
	return nil
}

func (p *EncoderProcess) running() bool {
	if p.process == 0 {
		return false
	}
	state, err := windows.WaitForSingleObject(p.process, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}

type encoderJobAccounting struct {
	totalUserTime, totalKernelTime, periodUserTime, periodKernelTime int64
	totalPageFaults, totalProcesses, activeProcesses, terminated     uint32
}

// Stop prevents subsequent control connections and stops the entire owned
// job. A timeout preserves the handles so the caller can confirm again.
func (p *EncoderProcess) Stop(ctx context.Context) error {
	if p.stream != nil {
		p.stream.fenceStart()
	}
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return nil
	}
	p.stopping = true
	if err := windows.TerminateJobObject(p.job, 1); err != nil {
		return fmt.Errorf("stop owned encoder job: %w", err)
	}
	// The root process handle is no longer needed for endpoint admission.
	// Release it before checking how many members remain in the job.
	if p.process != 0 {
		_ = windows.CloseHandle(p.process)
		p.process = 0
	}
	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()
	for {
		var accounting encoderJobAccounting
		if err := windows.QueryInformationJobObject(p.job, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
			return fmt.Errorf("query owned encoder job: %w", err)
		}
		if accounting.activeProcesses == 0 {
			if err := windows.CloseHandle(p.job); err != nil {
				return fmt.Errorf("close owned encoder job: %w", err)
			}
			p.job = 0
			if err := p.desktop.Close(); err != nil {
				return err
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
