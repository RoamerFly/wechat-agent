//go:build windows

package obspreview

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const encoderGracefulCloseTimeout = 2 * time.Second

var (
	encoderEnumDesktopWindows = user32DLL.NewProc("EnumDesktopWindows")
	encoderGetWindowPID       = user32DLL.NewProc("GetWindowThreadProcessId")
	encoderPostMessageW       = user32DLL.NewProc("PostMessageW")
	encoderWindowCollectors   sync.Map
	encoderWindowSequence     atomic.Uintptr
	// Allocate one callback for the lifetime of the host. The callback only
	// collects handles; PID queries and asynchronous posts happen afterwards.
	encoderCollectWindow = windows.NewCallback(func(window, data uintptr) uintptr {
		if value, ok := encoderWindowCollectors.Load(data); ok {
			collected := value.(*[]uintptr)
			*collected = append(*collected, window)
		}
		return 1
	})
)

// Running reports whether the owned root process is alive and still admits
// operations. A process being stopped is never reported as available.
func (p *EncoderProcess) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.stopping && p.running()
}

// StopGracefully first asks only this portable encoder's top-level windows on
// its owned private desktop to close. It never switches desktops. After at most
// two seconds (or context cancellation), Stop reaps and verifies the whole job,
// including children left behind by a normally exiting root process. A caller
// receiving a timeout can call Stop again to finish confirming cleanup.
func (p *EncoderProcess) StopGracefully(ctx context.Context) error {
	if p.stream != nil {
		p.stream.fenceStart()
	}
	p.mu.Lock()
	if p.portable && p.desktop != nil && p.desktop.handle != 0 &&
		p.job != 0 && !p.stopping && p.running() {
		// Keep desktop/process handles pinned until enumeration and waiting
		// finish. Concurrent Stop therefore cannot close or recycle them.
		p.stopping = true
		if ctx.Err() == nil {
			postOwnedEncoderClose(p.desktop.handle, p.pid)
			grace, cancel := context.WithTimeout(ctx, encoderGracefulCloseTimeout)
			ticker := time.NewTicker(15 * time.Millisecond)
			for p.running() && grace.Err() == nil {
				select {
				case <-grace.Done():
				case <-ticker.C:
				}
			}
			ticker.Stop()
			cancel()
		}
	}
	p.mu.Unlock()
	// Stop takes the same mutex. Release it before invoking the existing job
	// fallback; another Stop winning this race is harmless and idempotent.
	return p.Stop(ctx)
}

func postOwnedEncoderClose(desktop windows.Handle, pid uint32) {
	var candidates []uintptr
	// Pass an integer lookup key through LPARAM rather than round-tripping a
	// Go pointer through uintptr in the callback. Concurrent encoders retain
	// independent collectors, removed immediately after synchronous enumeration.
	key := encoderWindowSequence.Add(1)
	encoderWindowCollectors.Store(key, &candidates)
	ok, _, _ := encoderEnumDesktopWindows.Call(uintptr(desktop), encoderCollectWindow, key)
	encoderWindowCollectors.Delete(key)
	if ok == 0 {
		return // Job termination remains available if window enumeration fails.
	}
	postEncoderCloseForPID(pid, candidates, func(window uintptr) uint32 {
		var owner uint32
		encoderGetWindowPID.Call(window, uintptr(unsafe.Pointer(&owner)))
		return owner
	}, func(window uintptr) {
		encoderPostMessageW.Call(window, 0x0010, 0, 0) // WM_CLOSE is asynchronous.
	})
}

// candidates must come exclusively from EnumDesktopWindows on the still-owned
// desktop. Query each current PID immediately before posting, so windows owned
// by another process on that desktop are never intentionally targeted.
func postEncoderCloseForPID(pid uint32, candidates []uintptr, windowPID func(uintptr) uint32, post func(uintptr)) {
	if pid == 0 {
		return
	}
	for _, window := range candidates {
		if window != 0 && windowPID(window) == pid {
			post(window)
		}
	}
}
