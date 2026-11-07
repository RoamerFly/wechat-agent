//go:build windows

package obspreview

import (
	"context"
	"errors"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// OBS's WebSocket service does not authenticate the local peer identity. A
// password plus a loopback address is therefore insufficient for the encoder
// path. Before upgrading the connection, verify the concrete connected socket
// and its listener against Windows' owner table.
var (
	obsIPHelper       = windows.NewLazySystemDLL("iphlpapi.dll")
	obsExtendedTCP    = obsIPHelper.NewProc("GetExtendedTcpTable")
	errOBSOwnerLookup = errors.New("owned OBS TCP peer is unavailable")
)

const (
	obsAFInet              = 2
	obsAFInet6             = 23
	obsTCPTableOwnerPIDAll = 5
	obsTCPStateListen      = 2
	obsTCPStateEstablished = 5
	obsLoopbackIPv4        = 0x0100007f
)

type obsTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwnerPID   uint32
}

type obsTCP6RowOwnerPID struct {
	LocalAddr     [16]byte
	LocalScopeID  uint32
	LocalPort     uint32
	RemoteAddr    [16]byte
	RemoteScopeID uint32
	RemotePort    uint32
	State         uint32
	OwnerPID      uint32
}

// verifyEncoderPeer permits only an established IPv4 loopback connection to a
// listener owned by pid. It rejects wildcard and IPv6 listeners on the same
// port. That makes a port/password collision fail closed instead of letting a
// separate local process impersonate this encoder lease.
func verifyEncoderPeer(ctx context.Context, conn *net.TCPConn, pid uint32) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil || conn == nil || pid == 0 {
		return ErrStreamEndpoint
	}
	local, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok || local.IP == nil || !local.IP.IsLoopback() || local.IP.To4() == nil || local.Port < 1 {
		return ErrStreamEndpoint
	}
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || remote.IP == nil || !remote.IP.IsLoopback() || remote.IP.To4() == nil || remote.Port < 1 {
		return ErrStreamEndpoint
	}
	// A client socket's local end is the server connection's remote end.
	// Windows reports port fields in network byte order in MIB rows.
	clientPort := hostPort(local.Port)
	serverPort := hostPort(remote.Port)
	rows, err := obsTCPRows(obsAFInet)
	if err != nil {
		return ErrStreamEndpoint
	}
	listener := false
	connected := false
	for _, row := range rows {
		if row.LocalPort != serverPort {
			continue
		}
		switch row.State {
		case obsTCPStateListen:
			// A loopback control endpoint must not share its port with any
			// wildcard or other interface listener.
			if row.LocalAddr != obsLoopbackIPv4 || row.OwnerPID != pid {
				return ErrStreamEndpoint
			}
			listener = true
		case obsTCPStateEstablished:
			if row.LocalAddr == obsLoopbackIPv4 && row.RemoteAddr == obsLoopbackIPv4 &&
				row.RemotePort == clientPort && row.OwnerPID == pid {
				connected = true
			}
		}
	}
	if !listener || !connected {
		return ErrStreamEndpoint
	}
	rows6, err := obsTCP6Rows()
	if err != nil {
		return ErrStreamEndpoint
	}
	for _, row := range rows6 {
		if row.State == obsTCPStateListen && row.LocalPort == serverPort {
			return ErrStreamEndpoint
		}
	}
	return nil
}

// verifyEncoderPublisher verifies the opposite direction. The host owns the
// loopback relay listener and conn is its accepted endpoint. The encoder must
// own the matching client-side established socket. No listener lookup is made
// for the encoder because it is publishing as a client.
func verifyEncoderPublisher(ctx context.Context, conn *net.TCPConn, pid uint32) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil || conn == nil || pid == 0 {
		return ErrStreamEndpoint
	}
	local, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok || local.IP == nil || !local.IP.IsLoopback() || local.IP.To4() == nil || local.Port < 1 {
		return ErrStreamEndpoint
	}
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || remote.IP == nil || !remote.IP.IsLoopback() || remote.IP.To4() == nil || remote.Port < 1 {
		return ErrStreamEndpoint
	}
	rows, err := obsTCPRows(obsAFInet)
	if err != nil {
		return ErrStreamEndpoint
	}
	// The accepted host-side connection is reversed in the encoder process.
	encoderPort := hostPort(remote.Port)
	relayPort := hostPort(local.Port)
	for _, row := range rows {
		if row.State == obsTCPStateEstablished && row.LocalAddr == obsLoopbackIPv4 &&
			row.RemoteAddr == obsLoopbackIPv4 && row.LocalPort == encoderPort &&
			row.RemotePort == relayPort && row.OwnerPID == pid {
			return nil
		}
	}
	return ErrStreamEndpoint
}

// verifyEncoderHasNoTCPListeners is the additional admission rule for a
// portable encoder with no host-owned WebSocket endpoint. A normal OBS
// control process deliberately listens on loopback and must not use this
// check. The portable runtime has no reason to listen at all; failing closed
// prevents a bundled configuration from exposing an accidental control or
// service port while it publishes to the host relay.
func verifyEncoderHasNoTCPListeners(ctx context.Context, pid uint32) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil || pid == 0 {
		return ErrStreamEndpoint
	}
	rows, err := obsTCPRows(obsAFInet)
	if err != nil {
		return ErrStreamEndpoint
	}
	for _, row := range rows {
		if row.State == obsTCPStateListen && row.OwnerPID == pid {
			return ErrStreamEndpoint
		}
	}
	rows6, err := obsTCP6Rows()
	if err != nil {
		return ErrStreamEndpoint
	}
	for _, row := range rows6 {
		if row.State == obsTCPStateListen && row.OwnerPID == pid {
			return ErrStreamEndpoint
		}
	}
	if ctx.Err() != nil {
		return ErrStreamEndpoint
	}
	return nil
}

func hostPort(port int) uint32 {
	value := uint16(port)
	return uint32(value<<8 | value>>8)
}

func obsTCPRows(family uint32) ([]obsTCPRowOwnerPID, error) {
	buffer, err := obsTCPTable(family)
	if err != nil {
		return nil, err
	}
	count := *(*uint32)(unsafe.Pointer(&buffer[0]))
	rowSize := int(unsafe.Sizeof(obsTCPRowOwnerPID{}))
	if count > uint32((len(buffer)-4)/rowSize) {
		return nil, errOBSOwnerLookup
	}
	rows := make([]obsTCPRowOwnerPID, 0, count)
	for index := uint32(0); index < count; index++ {
		offset := 4 + int(index)*rowSize
		rows = append(rows, *(*obsTCPRowOwnerPID)(unsafe.Pointer(&buffer[offset])))
	}
	return rows, nil
}

func obsTCPTable(family uint32) ([]byte, error) {
	var size uint32
	r1, _, _ := obsExtendedTCP.Call(0, uintptr(unsafe.Pointer(&size)), 0, uintptr(family), obsTCPTableOwnerPIDAll, 0)
	if syscall.Errno(r1) != windows.ERROR_INSUFFICIENT_BUFFER || size < 4 || size > 16<<20 {
		return nil, errOBSOwnerLookup
	}
	for range 3 {
		buffer := make([]byte, size)
		r1, _, callErr := obsExtendedTCP.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(family), obsTCPTableOwnerPIDAll, 0)
		if r1 == 0 {
			return buffer, nil
		}
		if syscall.Errno(r1) != windows.ERROR_INSUFFICIENT_BUFFER || size < 4 || size > 16<<20 {
			if callErr != nil {
				return nil, callErr
			}
			return nil, errOBSOwnerLookup
		}
	}
	return nil, errOBSOwnerLookup
}

func obsTCP6Rows() ([]obsTCP6RowOwnerPID, error) {
	buffer, err := obsTCPTable(obsAFInet6)
	if err != nil {
		return nil, err
	}
	count := *(*uint32)(unsafe.Pointer(&buffer[0]))
	rowSize := int(unsafe.Sizeof(obsTCP6RowOwnerPID{}))
	if count > uint32((len(buffer)-4)/rowSize) {
		return nil, errOBSOwnerLookup
	}
	rows := make([]obsTCP6RowOwnerPID, 0, count)
	for index := uint32(0); index < count; index++ {
		offset := 4 + int(index)*rowSize
		rows = append(rows, *(*obsTCP6RowOwnerPID)(unsafe.Pointer(&buffer[offset])))
	}
	return rows, nil
}
