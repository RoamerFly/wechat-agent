//go:build windows

package station

// Windows has no authenticated loopback sockets.  We therefore query the OS
// TCP owner table and require the listener PID to be the exact process started
// by pluginruntime before talking to the local ComfyUI HTTP server.

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	localComfyIPHelper      = windows.NewLazySystemDLL("iphlpapi.dll")
	localComfyExtendedTCP   = localComfyIPHelper.NewProc("GetExtendedTcpTable")
	errLocalComfyNoListener = errors.New("station_comfyui_listener_missing")
	errLocalComfyWrongOwner = errors.New("station_comfyui_listener_owner_invalid")
)

const (
	localComfyAFInet            = 2
	localComfyTCPTableOwnerPID  = 3
	localComfyTCPStateListening = 2
)

type localComfyTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwnerPID   uint32
}

func localComfyListenerOwner(port, expectedPID int) error {
	if port < 1 || port > 65535 || expectedPID <= 0 {
		return errLocalComfyWrongOwner
	}
	var size uint32
	r1, _, _ := localComfyExtendedTCP.Call(0, uintptr(unsafe.Pointer(&size)), 0, localComfyAFInet, localComfyTCPTableOwnerPID, 0)
	if syscall.Errno(r1) != windows.ERROR_INSUFFICIENT_BUFFER || size < 4 {
		return errLocalComfyNoListener
	}
	buffer := make([]byte, size)
	r1, _, callErr := localComfyExtendedTCP.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)), 0, localComfyAFInet, localComfyTCPTableOwnerPID, 0)
	if r1 != 0 {
		return callErr
	}
	count := *(*uint32)(unsafe.Pointer(&buffer[0]))
	rowSize := int(unsafe.Sizeof(localComfyTCPRowOwnerPID{}))
	if count > uint32((len(buffer)-4)/rowSize) {
		return errLocalComfyNoListener
	}
	for index := uint32(0); index < count; index++ {
		offset := 4 + int(index)*rowSize
		row := (*localComfyTCPRowOwnerPID)(unsafe.Pointer(&buffer[offset]))
		if row.State != localComfyTCPStateListening || row.LocalAddr != 0x0100007f || int(syscall.Ntohs(uint16(row.LocalPort))) != port {
			continue
		}
		if int(row.OwnerPID) != expectedPID {
			return errLocalComfyWrongOwner
		}
		return nil
	}
	return errLocalComfyNoListener
}
