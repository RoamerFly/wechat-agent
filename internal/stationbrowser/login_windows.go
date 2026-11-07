//go:build windows

package stationbrowser

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
	"wechat-bot/internal/hostfile"
)

const douyinLoginURL = "https://www.douyin.com/"

var (
	// ErrDouyinLoginActive is intentionally terse: callers must not infer a
	// profile path, browser command line, or a user's login state from it.
	ErrDouyinLoginActive = errors.New("抖音登录窗口已打开")
	// ErrDouyinLoginCloseUnconfirmed means the owned browser was force-closed
	// after a graceful Browser.close could not be confirmed.  It is never a
	// claim that profile changes were successfully persisted.
	ErrDouyinLoginCloseUnconfirmed = errors.New("抖音登录窗口退出未确认")
)

// DouyinLoginProfileLockPath returns the sidecar lock used by
// OpenDouyinLogin. Hosts that remove a dedicated profile must take this same
// lock first; it remains held until the owned Edge process actually exits.
func DouyinLoginProfileLockPath(profile string) (string, error) {
	abs, err := filepath.Abs(profile)
	if err != nil {
		return "", err
	}
	if filepath.Clean(abs) != abs {
		return "", fmt.Errorf("%w: invalid private profile", errCDPProtocol)
	}
	return abs + ".douyin-login.lock", nil
}

// LoginWindow owns one intentionally narrow, interactive browser process. It
// exposes neither cookies nor the DevTools transport to application code.
type LoginWindow struct {
	session *cdpSession
	exit    windows.Handle
	release func()

	active   atomic.Bool
	exited   atomic.Bool
	normal   atomic.Bool
	forced   atomic.Bool
	exitDone chan struct{}

	releaseOnce sync.Once
	closeOnce   sync.Once
	closeDone   chan struct{}
	closeErr    error
}

// OpenDouyinLogin opens the only production login target. It never reuses the
// normal Edge profile and never accepts a page URL from its caller.
func OpenDouyinLogin(ctx context.Context, profile string) (*LoginWindow, error) {
	return openDouyinLogin(ctx, profile, douyinLoginURL, true)
}

// openDouyinLogin is intentionally unexported so tests can use a local HTTP
// page in invisible mode. Production must enter through OpenDouyinLogin.
func openDouyinLogin(ctx context.Context, profile, target string, visible bool) (*LoginWindow, error) {
	edge, profile, err := cdpLaunchInputs(ctx, profile)
	if err != nil {
		return nil, err
	}
	lockPath, err := DouyinLoginProfileLockPath(profile)
	if err != nil {
		return nil, err
	}
	release, err := hostfile.TryLock(lockPath)
	if err != nil {
		return nil, ErrDouyinLoginActive
	}

	window := &LoginWindow{release: release, closeDone: make(chan struct{}), exitDone: make(chan struct{})}
	var process windows.Handle
	options := edgeLaunchOptions{
		visible: visible, appWindow: visible,
		onProcess: func(handle windows.Handle) error {
			return windows.DuplicateHandle(windows.CurrentProcess(), handle, windows.CurrentProcess(), &process, windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, 0)
		},
	}
	session, err := startEdgePipe(ctx, edge, profile, "", nil, options)
	if err != nil {
		if process != 0 {
			_ = windows.CloseHandle(process)
		}
		release()
		return nil, err
	}
	window.session, window.exit = session, process
	// Unlike silent readers, a user may close this visible app window directly.
	// EOF gets a short, bounded graceful shutdown path; service cancellation is
	// still marked separately by cdpSession.start and kills immediately.
	session.normalEOFGrace.Store(true)
	window.active.Store(true)
	go window.watchExit()

	if err := configureLoginTarget(ctx, session, target); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = window.Close(closeCtx)
		cancel()
		return nil, err
	}
	return window, nil
}

func configureLoginTarget(ctx context.Context, session *cdpSession, target string) error {
	// Edge itself was launched at about:blank. Reusing that page is essential
	// for --app mode: creating another target here would leave a blank app
	// window behind and open a second, ordinary tab/window.
	var targetID string
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for targetID == "" {
		var targets struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfos"`
		}
		if err := session.Call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
			return loginCallError(err)
		}
		for _, info := range targets.TargetInfos {
			if info.Type == "page" && info.URL == "about:blank" && info.TargetID != "" {
				targetID = info.TargetID
				break
			}
		}
		if targetID != "" {
			break
		}
		select {
		case <-ctx.Done():
			return loginCallError(ctx.Err())
		case <-deadline.C:
			return ErrBrowserUnavailable
		case <-time.After(25 * time.Millisecond):
		}
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := session.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &attached); err != nil || attached.SessionID == "" {
		return loginCallError(err)
	}
	if err := session.Call(ctx, "", "Browser.setDownloadBehavior", map[string]any{"behavior": "deny", "eventsEnabled": false}, nil); err != nil {
		return loginCallError(err)
	}
	if err := session.Call(ctx, attached.SessionID, "Page.enable", map[string]any{}, nil); err != nil {
		return loginCallError(err)
	}
	var navigated struct {
		ErrorText  string `json:"errorText"`
		IsDownload bool   `json:"isDownload"`
	}
	if err := session.Call(ctx, attached.SessionID, "Page.navigate", map[string]any{"url": target}, &navigated); err != nil || navigated.ErrorText != "" || navigated.IsDownload {
		return loginCallError(err)
	}
	return nil
}

func loginCallError(err error) error {
	if err == nil {
		return ErrBrowserUnavailable
	}
	return fmt.Errorf("%w: %v", ErrBrowserUnavailable, err)
}

func (w *LoginWindow) watchExit() {
	if w == nil {
		return
	}
	<-w.session.done
	w.active.Store(false)
	// done wakes CDP callers before platform teardown. Wait for the latter
	// before classifying an exit, otherwise gracefulExited can race this read.
	<-w.session.teardownDone
	if w.exit != 0 {
		_, _ = windows.WaitForSingleObject(w.exit, windows.INFINITE)
		var code uint32
		if (windows.GetExitCodeProcess(w.exit, &code) == nil && code == 0 || w.session.gracefulExited.Load()) && !w.forced.Load() {
			w.normal.Store(true)
		}
		_ = windows.CloseHandle(w.exit)
	}
	w.session.forceTerminate()
	w.releaseOnce.Do(func() {
		if w.release != nil {
			w.release()
		}
	})
	w.exited.Store(true)
	close(w.exitDone)
}

// Active reports whether the owned login browser has not started closing.
func (w *LoginWindow) Active() bool { return w != nil && w.active.Load() }

// Close requests Browser.close and waits for the actual browser process exit.
// If that cannot be confirmed before ctx expires it closes the owned Job tree
// but returns ErrDouyinLoginCloseUnconfirmed rather than a false success.
func (w *LoginWindow) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	// A user may have closed the app window before pressing the host's
	// "saved" button.  The watcher has already observed the actual process
	// exit in that case, so this is a confirmed, idempotent close.
	if w.exited.Load() {
		if w.normal.Load() {
			return nil
		}
		return ErrDouyinLoginCloseUnconfirmed
	}
	w.closeOnce.Do(func() {
		defer close(w.closeDone)
		if w.session == nil || w.exit == 0 {
			w.closeErr = ErrDouyinLoginCloseUnconfirmed
			return
		}
		// Browser.close is best effort because Edge normally tears down the CDP
		// pipe before it replies. Process-exit confirmation below is authoritative.
		w.session.requestGracefulClose()
		_ = w.session.Call(ctx, "", "Browser.close", map[string]any{}, nil)
		select {
		case <-w.exitDone:
			if !w.normal.Load() {
				w.closeErr = ErrDouyinLoginCloseUnconfirmed
			}
		case <-ctx.Done():
			w.forced.Store(true)
			w.session.forceTerminate()
			w.closeErr = fmt.Errorf("%w: %v", ErrDouyinLoginCloseUnconfirmed, ctx.Err())
		}
	})
	select {
	case <-w.closeDone:
		return w.closeErr
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrDouyinLoginCloseUnconfirmed, ctx.Err())
	}
}
