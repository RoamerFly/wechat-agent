//go:build !windows

package agenthost

// Keep the Windows request builder compilable for WSL's path-mapping tests.
// It is never used to launch the Windows helper outside a Windows binary.
func windowsParentIdentity() (int, uint64, error) { return 0, 0, nil }
