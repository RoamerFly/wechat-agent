//go:build windows

package obspreview

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

// ownedDesktop is a private, non-interactive Windows desktop used only for a
// host-owned encoder. It is never made visible with SwitchDesktop.
type ownedDesktop struct {
	name   string
	handle windows.Handle
	once   sync.Once
}

var (
	user32DLL      = windows.NewLazySystemDLL("user32.dll")
	createDesktopW = user32DLL.NewProc("CreateDesktopW")
	closeDesktop   = user32DLL.NewProc("CloseDesktop")
)

func createOwnedDesktop() (*ownedDesktop, error) {
	name := "AgentHomeStation-encoder-" + uuid.NewString()
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("%w: create private desktop name", ErrEncoderLaunch)
	}
	// The desktop is private to this process tree. GENERIC_ALL is required by
	// CreateDesktopW for the child GUI process to create its own windows there;
	// no caller ever receives this handle and we never switch to the desktop.
	r1, _, callErr := createDesktopW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		0,
		0,
		0,
		uintptr(windows.GENERIC_ALL),
		0,
	)
	if r1 == 0 {
		if callErr == nil || callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_INVALID_PARAMETER
		}
		return nil, fmt.Errorf("%w: create private desktop: %w", ErrEncoderLaunch, callErr)
	}
	return &ownedDesktop{name: name, handle: windows.Handle(r1)}, nil
}

func (d *ownedDesktop) Close() error {
	if d == nil || d.handle == 0 {
		return nil
	}
	var closeErr error
	d.once.Do(func() {
		r1, _, err := closeDesktop.Call(uintptr(d.handle))
		if r1 == 0 {
			if err == nil || err == windows.ERROR_SUCCESS {
				err = windows.ERROR_INVALID_PARAMETER
			}
			closeErr = fmt.Errorf("close private encoder desktop: %w", err)
			return
		}
		d.handle = 0
	})
	return closeErr
}
