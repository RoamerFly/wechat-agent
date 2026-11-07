// Package rtmprelay provides a short-lived, loopback-only RTMP publish relay.
package rtmprelay

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

const (
	stateWaiting      = "waiting"
	stateConnecting   = "connecting"
	stateForwarding   = "forwarding"
	stateDisconnected = "disconnected"
	stateStopped      = "stopped"

	handshakeTimeout = 8 * time.Second
	verifyTimeout    = 5 * time.Second
	mediaIOTimeout   = 20 * time.Second
)

// Config describes the only permitted upstream destination and how it may be
// reached. Destination.Fragment is the stream key; it is deliberately not
// accepted from a path, query parameter, or userinfo field.
type Config struct {
	Destination *url.URL
	DialContext func(context.Context, string, string) (net.Conn, error)
	VerifyPeer  func(context.Context, *net.TCPConn) error
}

// Status is intentionally transport-only. It never exposes a destination,
// stream key, peer address, or a platform delivery result.
type Status struct {
	State         string
	MediaMessages uint64
}

// Relay accepts at most one authenticated loopback publisher and relays its
// media to one approved RTMP(S) destination.
type Relay struct {
	ctx    context.Context
	cancel context.CancelFunc

	listener net.Listener
	ticket   string

	mu            sync.Mutex
	state         string
	mediaMessages uint64
	claimed       bool
	closed        bool
	connections   map[net.Conn]struct{}
	done          chan struct{}
}

// Open validates cfg before opening a listener, then starts a loopback-only
// single-publisher session. The supplied context limits the session lifetime.
func Open(ctx context.Context, cfg Config) (*Relay, error) {
	if ctx == nil {
		return nil, errors.New("relay context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	destination := *cfg.Destination
	cfg.Destination = &destination

	ticket, err := newTicket()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("open loopback listener: %w", err)
	}

	relayCtx, cancel := context.WithCancel(ctx)
	r := &Relay{
		ctx:         relayCtx,
		cancel:      cancel,
		listener:    listener,
		ticket:      ticket,
		state:       stateWaiting,
		connections: make(map[net.Conn]struct{}),
		done:        make(chan struct{}),
	}

	go func() {
		defer close(r.done)
		r.acceptLoop(cfg)
	}()
	go func() {
		<-relayCtx.Done()
		r.closeInternal()
	}()

	return r, nil
}

// Port returns the loopback port allocated for this session.
func (r *Relay) Port() int {
	if r == nil || r.listener == nil {
		return 0
	}
	if tcp, ok := r.listener.Addr().(*net.TCPAddr); ok {
		return tcp.Port
	}
	return 0
}

// Ticket returns the random stream key required by the loopback publisher.
func (r *Relay) Ticket() string {
	if r == nil {
		return ""
	}
	return r.ticket
}

// Status reports only the state of this local relay and the number of media
// messages actually written to its approved destination.
func (r *Relay) Status() Status {
	if r == nil {
		return Status{State: stateStopped}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{State: r.state, MediaMessages: r.mediaMessages}
}

// Close fences future work first, closes every local socket, and waits for all
// workers or the caller context. It is safe to call more than once.
func (r *Relay) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("close context is required")
	}
	r.closeInternal()

	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validateConfig(cfg Config) error {
	if cfg.Destination == nil {
		return errors.New("relay destination is required")
	}
	if cfg.DialContext == nil {
		return errors.New("approved relay dialer is required")
	}
	if cfg.VerifyPeer == nil {
		return errors.New("relay peer verifier is required")
	}
	u := cfg.Destination
	if u.User != nil {
		return errors.New("relay destination userinfo is not permitted")
	}
	if u.Hostname() == "" {
		return errors.New("relay destination host is required")
	}
	if u.Scheme != "rtmp" && u.Scheme != "rtmps" {
		return errors.New("relay destination scheme is not supported")
	}
	if u.Opaque != "" || u.RawQuery != "" || u.ForceQuery {
		return errors.New("relay destination must not contain embedded credentials")
	}
	if !usableKey(u.Fragment) {
		return errors.New("relay destination stream key is invalid")
	}
	return nil
}

func usableKey(key string) bool {
	if key == "" || len(key) > 512 || strings.TrimSpace(key) != key {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func newTicket() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("create relay ticket: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (r *Relay) acceptLoop(cfg Config) {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			return
		}
		if r.isClosed() {
			_ = conn.Close()
			return
		}
		r.handleInbound(cfg, conn)
		if r.isClaimed() {
			return
		}
	}
}

func (r *Relay) handleInbound(cfg Config, conn net.Conn) {
	r.track(conn)
	defer r.untrackAndClose(conn)

	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	verifyCtx, cancel := context.WithTimeout(r.ctx, verifyTimeout)
	err := cfg.VerifyPeer(verifyCtx, tcpConn)
	cancel()
	if err != nil || r.isClosed() {
		return
	}

	_ = tcpConn.SetDeadline(time.Now().Add(handshakeTimeout))
	server := &gortmplib.ServerConn{RW: tcpConn}
	if err := server.Initialize(); err != nil {
		return
	}
	if err := server.AcceptConn(); err != nil {
		return
	}
	if !r.isExpectedPublisher(server) {
		_ = server.RejectAction()
		return
	}
	if !r.claim() {
		_ = server.RejectAction()
		return
	}
	// No later connection may consume the session, whether or not upstream
	// connection establishment succeeds.
	_ = r.listener.Close()
	_ = tcpConn.SetDeadline(time.Time{})

	r.setState(stateConnecting)
	if err := r.forward(cfg, server, tcpConn); err != nil && !r.isClosed() {
		r.setState(stateDisconnected)
	}
}

func (r *Relay) isExpectedPublisher(server *gortmplib.ServerConn) bool {
	if server == nil || !server.Publish || server.URL == nil {
		return false
	}
	if server.URL.Scheme != "rtmp" || server.URL.Hostname() != "127.0.0.1" ||
		server.URL.Port() != fmt.Sprint(r.Port()) || server.URL.RawQuery != "" {
		return false
	}
	expectedPath := "/live/" + r.ticket
	actualPath := server.URL.EscapedPath()
	if len(actualPath) != len(expectedPath) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actualPath), []byte(expectedPath)) == 1
}

func (r *Relay) forward(cfg Config, source *gortmplib.ServerConn, sourceConn net.Conn) error {
	var dialed atomic.Bool
	var outbound net.Conn
	defer func() {
		if outbound != nil {
			r.untrackAndClose(outbound)
		}
	}()
	client := &gortmplib.Client{
		URL:     cfg.Destination,
		Publish: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if err := r.ctx.Err(); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !dialed.CompareAndSwap(false, true) {
				return nil, errors.New("relay outbound attempt already used")
			}
			conn, err := cfg.DialContext(ctx, network, address)
			if err == nil && conn != nil {
				outbound = conn
				r.track(conn)
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetKeepAlive(true)
					_ = tcp.SetKeepAlivePeriod(mediaIOTimeout)
				}
			}
			return conn, err
		},
	}

	connectCtx, cancel := context.WithTimeout(r.ctx, handshakeTimeout)
	err := client.Initialize(connectCtx)
	cancel()
	if err != nil {
		return err
	}
	defer client.Close()

	_ = sourceConn.SetDeadline(time.Now().Add(mediaIOTimeout))
	if err := source.AcceptAction(); err != nil {
		return err
	}

	// Client.Read consumes downstream RTMP control/status messages while the
	// source loop writes media. Any downstream loss terminates both directions.
	downstreamLost := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			msg, readErr := client.Read()
			if readErr == nil && terminalDownstreamMessage(msg) {
				readErr = errors.New("relay downstream terminal status")
			}
			if readErr != nil {
				// A destination loss must release an idle source.Read too.
				_ = sourceConn.Close()
				select {
				case downstreamLost <- readErr:
				default:
				}
				return
			}
		}
	}()
	defer func() {
		// Wake and join the reader before the client is discarded.
		client.Close()
		<-readerDone
	}()

	for {
		select {
		case err := <-downstreamLost:
			return err
		default:
		}
		msg, err := source.Read()
		if err != nil {
			return err
		}
		if !forwardable(msg) {
			continue
		}
		if downstream := client.NetConn(); downstream != nil {
			_ = downstream.SetWriteDeadline(time.Now().Add(mediaIOTimeout))
		}
		if err := client.Write(msg); err != nil {
			return err
		}
		isMedia := mediaPayload(msg)
		if isMedia {
			r.recordForwardedMedia()
		}
		if downstream := client.NetConn(); downstream != nil {
			_ = downstream.SetWriteDeadline(time.Time{})
		}
		if isMedia {
			_ = sourceConn.SetDeadline(time.Now().Add(mediaIOTimeout))
		}
	}
}

func terminalDownstreamMessage(msg message.Message) bool {
	command, ok := msg.(*message.CommandAMF0)
	if !ok || command.Name != "onStatus" || len(command.Arguments) < 2 {
		return false
	}
	var status amf0.Object
	switch value := command.Arguments[1].(type) {
	case amf0.Object:
		status = value
	case amf0.ECMAArray:
		status = amf0.Object(value)
	default:
		return false
	}
	level, _ := status.GetString("level")
	code, _ := status.GetString("code")
	if level == "error" || strings.HasSuffix(code, ".Failed") || strings.HasSuffix(code, ".Error") {
		return true
	}
	switch code {
	case "NetStream.Unpublish.Success", "NetStream.Publish.Stop", "NetConnection.Connect.Closed":
		return true
	default:
		return false
	}
}

func forwardable(msg message.Message) bool {
	switch value := msg.(type) {
	case *message.Audio, *message.AudioExCodedFrames, *message.AudioExMultichannelConfig,
		*message.AudioExMultitrack, *message.AudioExSequenceEnd, *message.AudioExSequenceStart,
		*message.VideoExCodedFrames, *message.VideoExFramesX,
		*message.VideoExMetadata, *message.VideoExMultitrack, *message.VideoExSequenceEnd,
		*message.VideoExSequenceStart:
		return true
	case *message.DataAMF0:
		return metadataMessage(value)
	case *message.Video:
		return value.FrameType != message.VideoFrameTypeCommand
	default:
		return false
	}
}

func metadataMessage(msg *message.DataAMF0) bool {
	if msg == nil || len(msg.Payload) == 0 {
		return false
	}
	first, ok := msg.Payload[0].(string)
	if !ok {
		return false
	}
	if first == "onMetaData" {
		return true
	}
	if first != "@setDataFrame" || len(msg.Payload) < 2 {
		return false
	}
	second, ok := msg.Payload[1].(string)
	return ok && second == "onMetaData"
}

func mediaPayload(msg message.Message) bool {
	switch value := msg.(type) {
	case *message.Audio:
		return value != nil && len(value.AU) > 0
	case *message.Video:
		return value != nil && value.FrameType != message.VideoFrameTypeCommand &&
			value.PacketType == message.VideoPacketTypeAU && len(value.AU) > 0
	case *message.AudioExCodedFrames:
		return value != nil && len(value.Payload) > 0
	case *message.VideoExCodedFrames:
		return value != nil && len(value.Payload) > 0
	case *message.VideoExFramesX:
		return value != nil && len(value.Payload) > 0
	case *message.AudioExMultitrack:
		return value != nil && mediaPayload(value.Wrapped)
	case *message.VideoExMultitrack:
		return value != nil && mediaPayload(value.Wrapped)
	default:
		return false
	}
}

func (r *Relay) track(conn net.Conn) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = conn.Close()
		return
	}
	r.connections[conn] = struct{}{}
	r.mu.Unlock()
}

func (r *Relay) untrackAndClose(conn net.Conn) {
	r.mu.Lock()
	delete(r.connections, conn)
	r.mu.Unlock()
	_ = conn.Close()
}

func (r *Relay) claim() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.claimed {
		return false
	}
	r.claimed = true
	return true
}

func (r *Relay) isClaimed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.claimed
}

func (r *Relay) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func (r *Relay) setState(state string) {
	r.mu.Lock()
	if !r.closed {
		r.state = state
	}
	r.mu.Unlock()
}

func (r *Relay) recordForwardedMedia() {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A completed write is evidence even when shutdown raced with the write.
	r.mediaMessages++
	if !r.closed {
		r.state = stateForwarding
	}
}

func (r *Relay) closeInternal() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.state = stateStopped
	connections := make([]net.Conn, 0, len(r.connections))
	for conn := range r.connections {
		connections = append(connections, conn)
	}
	r.mu.Unlock()

	r.cancel()
	if r.listener != nil {
		_ = r.listener.Close()
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
}
