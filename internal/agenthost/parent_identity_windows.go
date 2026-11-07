//go:build windows

package agenthost

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// windowsParentIdentity returns the current process ID together with the
// immutable creation timestamp exposed by the kernel.  A PID alone is not a
// process identity: it may refer to an unrelated process after reuse.
func windowsParentIdentity() (int, uint64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user); err != nil {
		return 0, 0, fmt.Errorf("GetProcessTimes: %w", err)
	}
	value := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if value == 0 {
		return 0, 0, fmt.Errorf("GetProcessTimes returned zero creation time")
	}
	return int(windows.GetCurrentProcessId()), value, nil
}
