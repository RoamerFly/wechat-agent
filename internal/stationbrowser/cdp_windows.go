//go:build windows

package stationbrowser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"wechat-bot/internal/workspacefs"
)

// Available only locates Edge in the two protected Program Files locations.
// It intentionally never consults PATH, registry overrides, or environment
// executable variables: those would turn browser discovery into code loading.
func Available() bool { _, err := trustedEdge(); return err == nil }
func trustedEdge() (string, error) {
	for _, p := range []string{`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`, `C:\Program Files\Microsoft\Edge\Application\msedge.exe`} {
		if i, e := os.Stat(p); e == nil && !i.IsDir() {
			return p, nil
		}
	}
	return "", errCDPUnavailable
}

func openCDP(ctx context.Context, profile string) (*cdpSession, error) {
	edge, profile, e := cdpLaunchInputs(ctx, profile)
	if e != nil {
		return nil, e
	}
	return startEdgePipe(ctx, edge, profile, "", nil, edgeLaunchOptions{})
}

// openRestrictedCDP starts a separate, ephemeral Edge profile whose only
// network egress is the per-session CONNECT proxy. It is intentionally not a
// replacement for openCDP: callers must opt in with an explicit host policy.
func openRestrictedCDP(ctx context.Context, profile string, allowHost func(string) bool) (*cdpSession, error) {
	return openRestrictedCDPWithCapacity(ctx, profile, allowHost, playerProxyMaxClients)
}

func openRestrictedCDPWithCapacity(ctx context.Context, profile string, allowHost func(string) bool, capacity int) (*cdpSession, error) {
	edge, profile, e := cdpLaunchInputs(ctx, profile)
	if e != nil {
		return nil, e
	}
	proxy, e := openPlayerProxyWithCapacity(ctx, allowHost, capacity)
	if e != nil {
		return nil, e
	}
	session, e := startEdgePipe(ctx, edge, profile, proxy.address(), proxy.Close, edgeLaunchOptions{})
	if e != nil {
		_ = proxy.Close()
		return nil, e
	}
	return session, nil
}

func cdpLaunchInputs(ctx context.Context, profile string) (string, string, error) {
	if e := ctx.Err(); e != nil {
		return "", "", e
	}
	edge, e := trustedEdge()
	if e != nil {
		return "", "", e
	}
	profile, e = filepath.Abs(profile)
	if e != nil {
		return "", "", e
	}
	if filepath.Clean(profile) != profile {
		return "", "", fmt.Errorf("%w: invalid private profile", errCDPProtocol)
	}
	r, e := workspacefs.EnsurePath(profile)
	if e != nil {
		return "", "", fmt.Errorf("%w: private profile: %v", errCDPProtocol, e)
	}
	_ = r.Close()
	return edge, profile, nil
}

// edgeLaunchOptions remains private: ordinary readers retain the invisible
// defaults while the dedicated login entry point can ask for one app window.
type edgeLaunchOptions struct {
	visible   bool
	appWindow bool
	onProcess func(windows.Handle) error
}

func startEdgePipe(ctx context.Context, edge, profile, proxyAddress string, onClose func() error, options edgeLaunchOptions) (*cdpSession, error) {
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var browserRead, hostWrite, hostRead, browserWrite windows.Handle
	if e := windows.CreatePipe(&browserRead, &hostWrite, sa, 0); e != nil {
		return nil, e
	}
	defer windows.CloseHandle(browserRead)
	if e := windows.CreatePipe(&hostRead, &browserWrite, sa, 0); e != nil {
		_ = windows.CloseHandle(hostWrite)
		return nil, e
	}
	defer windows.CloseHandle(browserWrite)
	if e := windows.SetHandleInformation(hostWrite, windows.HANDLE_FLAG_INHERIT, 0); e != nil {
		_ = windows.CloseHandle(hostWrite)
		_ = windows.CloseHandle(hostRead)
		return nil, e
	}
	if e := windows.SetHandleInformation(hostRead, windows.HANDLE_FLAG_INHERIT, 0); e != nil {
		_ = windows.CloseHandle(hostWrite)
		_ = windows.CloseHandle(hostRead)
		return nil, e
	}
	nul, e := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if e != nil {
		_ = windows.CloseHandle(hostWrite)
		_ = windows.CloseHandle(hostRead)
		return nil, e
	}
	defer windows.CloseHandle(nul)
	// The extended handle list prevents this child inheriting arbitrary handles
	// from the desktop process (including its API, login, and admin handles).
	for _, handle := range []windows.Handle{browserRead, browserWrite, nul} {
		if e = windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); e != nil {
			_ = windows.CloseHandle(hostWrite)
			_ = windows.CloseHandle(hostRead)
			return nil, e
		}
	}
	job, e := windows.CreateJobObject(nil, nil)
	if e != nil {
		_ = windows.CloseHandle(hostWrite)
		_ = windows.CloseHandle(hostRead)
		return nil, e
	}
	var lim windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	lim.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, e = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&lim)), uint32(unsafe.Sizeof(lim))); e != nil {
		_ = windows.CloseHandle(job)
		_ = windows.CloseHandle(hostWrite)
		_ = windows.CloseHandle(hostRead)
		return nil, e
	}
	args := []string{edge, "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-extensions", "--metrics-recording-only", "--remote-debugging-pipe", fmt.Sprintf("--remote-debugging-io-pipes=%d,%d", browserRead, browserWrite), "--user-data-dir=" + profile}
	if !options.visible {
		// Mute before page scripts run: setting a selected video's muted flag
		// later cannot prevent an earlier autoplay from reaching system audio.
		args = append(args, "--headless=new", "--mute-audio")
	}
	if options.appWindow {
		// App mode hides the normal omnibox.  The initial page stays blank until
		// download policy is installed by the login entry point.
		args = append(args, "--app=about:blank")
	}
	if proxyAddress != "" {
		args = append(args,
			"--proxy-server=http://"+proxyAddress,
			"--proxy-bypass-list=<-loopback>",
			"--disable-quic",
			"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
			"--webrtc-ip-handling-policy=disable_non_proxied_udp",
		)
	}
	args = append(args, "about:blank")
	cmd, e := windows.UTF16PtrFromString(joinWindowsArgs(args))
	if e != nil {
		return nil, closeStart(job, hostWrite, hostRead, e)
	}
	app, _ := windows.UTF16PtrFromString(edge)
	cwd, _ := windows.UTF16PtrFromString(filepath.Dir(edge))
	env := narrowEnvironment()
	attributes, e := windows.NewProcThreadAttributeList(1)
	if e != nil {
		return nil, closeStart(job, hostWrite, hostRead, e)
	}
	defer attributes.Delete()
	handles := []windows.Handle{browserRead, browserWrite, nul}
	if e = attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); e != nil {
		return nil, closeStart(job, hostWrite, hostRead, e)
	}
	si := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES, StdInput: nul, StdOutput: nul, StdErr: nul}, ProcThreadAttributeList: attributes.List()}
	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	if !options.visible {
		flags |= windows.CREATE_NO_WINDOW
	}
	if e = windows.CreateProcess(app, cmd, nil, nil, true, flags, &env[0], cwd, &si.StartupInfo, &pi); e != nil {
		return nil, closeStart(job, hostWrite, hostRead, e)
	}
	defer windows.CloseHandle(pi.Thread)
	if e = windows.AssignProcessToJobObject(job, pi.Process); e != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		_ = windows.CloseHandle(pi.Process)
		return nil, closeStart(job, hostWrite, hostRead, e)
	}
	if options.onProcess != nil {
		if e = options.onProcess(pi.Process); e != nil {
			_ = windows.TerminateJobObject(job, 1)
			_ = windows.CloseHandle(pi.Process)
			return nil, closeStart(job, hostWrite, hostRead, e)
		}
	}
	if _, e = windows.ResumeThread(pi.Thread); e != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(pi.Process)
		return nil, closeStart(job, hostWrite, hostRead, e)
	}
	if e = ctx.Err(); e != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(pi.Process)
		return nil, closeStart(job, hostWrite, hostRead, e)
	}
	in := os.NewFile(uintptr(hostWrite), "edge-cdp-in")
	out := os.NewFile(uintptr(hostRead), "edge-cdp-out")
	var cleanupOnce sync.Once
	cleanup := func(force bool) (closeErr error) {
		cleanupOnce.Do(func() {
			if onClose != nil {
				_ = onClose()
			}
			if force {
				_ = windows.TerminateJobObject(job, 1)
			}
			_, _ = windows.WaitForSingleObject(pi.Process, 2000)
			_ = windows.CloseHandle(pi.Process)
			closeErr = windows.CloseHandle(job)
		})
		return closeErr
	}
	session := newCDPSession(in, out, func() error {
		return cleanup(true)
	})
	session.platformForceClose = func() error { return cleanup(true) }
	session.platformGracefulClose = func() error {
		// Interactive login windows may be closed by the user rather than by a
		// CDP Browser.close. Give Edge a bounded chance to finish its profile
		// write before killing the Job; ordinary reader sessions never take this
		// branch.
		if result, _ := windows.WaitForSingleObject(pi.Process, 1000); result == windows.WAIT_OBJECT_0 {
			session.gracefulExited.Store(true)
			return cleanup(false)
		}
		return cleanup(true)
	}
	session.start(ctx)
	return session, nil
}
func closeStart(job, one, two windows.Handle, e error) error {
	_ = windows.CloseHandle(job)
	_ = windows.CloseHandle(one)
	_ = windows.CloseHandle(two)
	return e
}
func narrowEnvironment() []uint16 {
	names := []string{"SystemRoot", "WINDIR", "SystemDrive", "COMSPEC", "PATH", "PATHEXT", "TEMP", "TMP", "LOCALAPPDATA", "APPDATA", "PROGRAMDATA", "USERPROFILE", "HOMEDRIVE", "HOMEPATH"}
	lines := make([]string, 0, len(names))
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok && v != "" {
			lines = append(lines, n+"="+v)
		}
	}
	sort.Strings(lines)
	// UTF16FromString forbids embedded NULs. CreateProcess requires a sequence
	// of NUL-terminated NAME=value entries followed by one extra NUL.
	return append(utf16.Encode([]rune(strings.Join(lines, "\x00"))), 0, 0)
}
func joinWindowsArgs(a []string) string {
	for i, v := range a {
		a[i] = quoteWindowsArg(v)
	}
	return strings.Join(a, " ")
}
func quoteWindowsArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slash := 0
	for _, r := range s {
		if r == '\\' {
			slash++
			continue
		}
		if r == '"' {
			b.WriteString(strings.Repeat("\\", slash*2+1))
			b.WriteRune(r)
			slash = 0
			continue
		}
		if slash > 0 {
			b.WriteString(strings.Repeat("\\", slash))
			slash = 0
		}
		b.WriteRune(r)
	}
	if slash > 0 {
		b.WriteString(strings.Repeat("\\", slash*2))
	}
	b.WriteByte('"')
	return b.String()
}
