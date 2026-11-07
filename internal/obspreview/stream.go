package obspreview

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
)

var (
	ErrStreamEndpoint         = errors.New("OBS encoder endpoint is not verified as owned")
	ErrStreamRelay            = errors.New("invalid local OBS relay")
	ErrStreamAlreadyRequested = errors.New("OBS encoder start was already requested")
	ErrStreamStopped          = errors.New("OBS encoder lease is stopping")
	ErrStreamBusy             = errors.New("OBS encoder already has an active or reconnecting stream")
	ErrStreamStartUnknown     = errors.New("OBS encoder start result is unknown")
	relayTicketPattern        = regexp.MustCompile("^[a-zA-Z0-9_-]{16,128}$")
)

// StreamStatus is an instantaneous observation of the local OBS encoder.
// It neither proves platform acceptance nor settles a pending start or stop.
type StreamStatus struct {
	Active       bool
	Reconnecting bool
}

// OwnedStream is the wire control for one dedicated encoder lease. The Windows
// process supervisor must own the encoder and isolate its WebSocket listener.
// It must end the lease to settle an in-flight start, including on host shutdown.
type OwnedStream struct {
	port     int
	password string
	verify   func(context.Context, *net.TCPConn) error

	mu              sync.Mutex
	startCancel     context.CancelFunc
	startDispatched bool
	stopping        bool
}

// NewOwnedStream requires a supervisor's ownership verifier. It runs against
// the connected socket before each WebSocket upgrade and must check that the
// peer belongs to this exact live lease. Port and password alone are insufficient.
func NewOwnedStream(port int, password string, verify func(context.Context, *net.TCPConn) error) (*OwnedStream, error) {
	if port < 1 || port > 65535 || password == "" || verify == nil {
		return nil, ErrStreamEndpoint
	}
	return &OwnedStream{port: port, password: password, verify: verify}, nil
}

func (s *OwnedStream) use(ctx context.Context, f func(context.Context, *websocket.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	dialer := websocket.Dialer{
		HandshakeTimeout: defaultTimeout,
		NetDialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			var dial net.Dialer
			conn, err := dial.DialContext(ctx, "tcp4", addr)
			if err != nil {
				return nil, err
			}
			tcp, ok := conn.(*net.TCPConn)
			if !ok || s.verify(ctx, tcp) != nil {
				_ = conn.Close()
				return nil, ErrStreamEndpoint
			}
			return conn, nil
		},
	}
	return withOBS(ctx, s.port, s.password, dialer, f)
}

func streamStatus(ctx context.Context, conn *websocket.Conn) (StreamStatus, error) {
	var result struct {
		Active       *bool `json:"outputActive"`
		Reconnecting *bool `json:"outputReconnecting"`
	}
	if err := request(ctx, conn, "GetStreamStatus", struct{}{}, &result); err != nil {
		return StreamStatus{}, err
	}
	if result.Active == nil || result.Reconnecting == nil {
		return StreamStatus{}, ErrProtocol
	}
	return StreamStatus{Active: *result.Active, Reconnecting: *result.Reconnecting}, nil
}

func (s *OwnedStream) Status(ctx context.Context) (status StreamStatus, err error) {
	err = s.use(ctx, func(ctx context.Context, conn *websocket.Conn) error {
		status, err = streamStatus(ctx, conn)
		return err
	})
	return status, err
}

// Start requests output to a host-owned loopback relay. OBS can persist its
// service settings, so the key must be an ephemeral relay ticket, never a
// platform credential. Success acknowledges the command; poll Status for activity.
func (s *OwnedStream) Start(ctx context.Context, relayPort int, relayTicket string) error {
	if relayPort < 1 || relayPort > 65535 || relayPort == s.port || !relayTicketPattern.MatchString(relayTicket) {
		return ErrStreamRelay
	}
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return ErrStreamStopped
	}
	if s.startCancel != nil || s.startDispatched {
		s.mu.Unlock()
		return ErrStreamAlreadyRequested
	}
	ctx, cancel := context.WithCancel(ctx)
	s.startCancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.startCancel = nil
		s.mu.Unlock()
	}()
	return s.use(ctx, func(ctx context.Context, conn *websocket.Conn) error {
		status, err := streamStatus(ctx, conn)
		if err != nil {
			return err
		}
		if status.Active || status.Reconnecting {
			return ErrStreamBusy
		}
		config := map[string]any{
			"streamServiceType": "rtmp_custom",
			"streamServiceSettings": map[string]string{
				"server": "rtmp://127.0.0.1:" + strconv.Itoa(relayPort) + "/live",
				"key":    relayTicket,
			},
		}
		if err := request(ctx, conn, "SetStreamServiceSettings", config, nil); err != nil {
			return err
		}
		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			return ErrStreamStopped
		}
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return err
		}
		s.startDispatched = true
		s.mu.Unlock()
		err = request(ctx, conn, "StartStream", struct{}{}, nil)
		if err != nil && !errors.Is(err, ErrRequestRejected) {
			return errors.Join(ErrStreamStartUnknown, err)
		}
		return err
	})
}

func (s *OwnedStream) fenceStart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping = true
	if s.startCancel != nil {
		s.startCancel()
	}
	return s.startDispatched
}

// RequestStop only requests a graceful stop. A supervisor handling emergency
// stop must fence further requests and close the owned process and relay.
func (s *OwnedStream) RequestStop(ctx context.Context) error {
	if !s.fenceStart() {
		return nil
	}
	return s.use(ctx, func(ctx context.Context, conn *websocket.Conn) error {
		return request(ctx, conn, "StopStream", struct{}{}, nil)
	})
}
