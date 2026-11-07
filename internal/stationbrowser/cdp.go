package stationbrowser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

type cdpEvent struct {
	Method, SessionID string
	Params            json.RawMessage
}

var (
	errCDPUnavailable = errors.New("private Edge DevTools is unavailable")
	errCDPClosed      = errors.New("private Edge DevTools is closed")
	errCDPProtocol    = errors.New("private Edge DevTools protocol error")
)

const maxCDPMessage = 4 << 20

type cdpFailure struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpFailure) Error() string {
	return fmt.Sprintf("DevTools command failed (%d): %s", e.Code, e.Message)
}

type cdpReply struct {
	result  json.RawMessage
	failure *cdpFailure
}
type cdpSession struct {
	in, out       *os.File
	platformClose func() error
	// platformGracefulClose only releases handles after a browser requested to
	// exit normally. It must never terminate the Job tree.
	platformGracefulClose func() error
	platformForceClose    func() error
	gracefulClose         atomic.Bool
	// normalEOFGrace is only enabled by the interactive login window. It gives
	// a user closing that window a short opportunity to flush profile state;
	// headless readers keep the immediate-kill behavior.
	normalEOFGrace atomic.Bool
	contextClosing atomic.Bool
	gracefulExited atomic.Bool
	writeMu        sync.Mutex
	mu             sync.Mutex
	pending        map[uint64]chan cdpReply
	err            error
	events         chan cdpEvent
	done           chan struct{}
	teardownDone   chan struct{}
	closed         sync.Once
	stopContext    func() bool
	next           atomic.Uint64
}

func newCDPSession(in, out *os.File, closePlatform func() error) *cdpSession {
	return &cdpSession{in: in, out: out, platformClose: closePlatform, pending: map[uint64]chan cdpReply{}, events: make(chan cdpEvent, 512), done: make(chan struct{}), teardownDone: make(chan struct{})}
}

func (s *cdpSession) start(ctx context.Context) {
	s.mu.Lock()
	s.stopContext = context.AfterFunc(ctx, func() { s.contextClosing.Store(true); _ = s.Close() })
	s.mu.Unlock()
	go s.readLoop()
}

func (s *cdpSession) requestGracefulClose() {
	if s != nil {
		s.gracefulClose.Store(true)
	}
}

func (s *cdpSession) forceTerminate() {
	if s != nil && s.platformForceClose != nil {
		_ = s.platformForceClose()
	}
}
func (s *cdpSession) Events() <-chan cdpEvent { return s.events }
func (s *cdpSession) Call(ctx context.Context, sessionID, method string, params any, result any) error {
	if s == nil {
		return errCDPClosed
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	id := s.next.Add(1)
	reply := make(chan cdpReply, 1)
	s.mu.Lock()
	if s.err != nil {
		e := s.err
		s.mu.Unlock()
		return e
	}
	s.pending[id] = reply
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	req := struct {
		ID        uint64 `json:"id"`
		Method    string `json:"method"`
		Params    any    `json:"params,omitempty"`
		SessionID string `json:"sessionId,omitempty"`
	}{id, method, params, sessionID}
	body, e := json.Marshal(req)
	if e != nil {
		return e
	}
	if len(body) > maxCDPMessage {
		return fmt.Errorf("%w: request too large", errCDPProtocol)
	}
	s.writeMu.Lock()
	if e = ctx.Err(); e == nil {
		s.mu.Lock()
		e = s.err
		s.mu.Unlock()
	}
	if e != nil {
		s.writeMu.Unlock()
		return e
	}
	_, e = s.in.Write(append(body, 0))
	s.writeMu.Unlock()
	if e != nil {
		s.fail(errCDPClosed)
		return errCDPClosed
	}
	select {
	case r := <-reply:
		if r.failure != nil {
			return r.failure
		}
		if result != nil && len(r.result) > 0 {
			return json.Unmarshal(r.result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		s.mu.Lock()
		e := s.err
		s.mu.Unlock()
		if e == nil {
			e = errCDPClosed
		}
		return e
	}
}
func (s *cdpSession) readLoop() {
	// readLoop is the sole owner that closes events, so cancellation cannot race
	// an event send with an external Close call.
	defer close(s.events)
	r := bufio.NewReader(s.out)
	for {
		frame, e := readCDPFrame(r)
		if e != nil {
			s.fail(fmt.Errorf("%w: %v", errCDPClosed, e))
			return
		}
		var m struct {
			ID        uint64          `json:"id"`
			Method    string          `json:"method"`
			SessionID string          `json:"sessionId"`
			Params    json.RawMessage `json:"params"`
			Result    json.RawMessage `json:"result"`
			Error     *cdpFailure     `json:"error"`
		}
		if json.Unmarshal(frame, &m) != nil {
			s.fail(fmt.Errorf("%w: invalid message", errCDPProtocol))
			return
		}
		if m.ID != 0 {
			s.mu.Lock()
			ch := s.pending[m.ID]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- cdpReply{m.Result, m.Error}:
				default:
				}
			}
			continue
		}
		if m.Method != "" {
			select {
			case s.events <- cdpEvent{m.Method, m.SessionID, m.Params}:
			default:
				s.fail(fmt.Errorf("%w: event queue full", errCDPProtocol))
				return
			}
		}
	}
}
func readCDPFrame(r *bufio.Reader) ([]byte, error) {
	b := make([]byte, 0, 4096)
	for {
		x, e := r.ReadByte()
		if e != nil {
			return nil, e
		}
		if x == 0 {
			return b, nil
		}
		if len(b) == maxCDPMessage {
			return nil, fmt.Errorf("%w: message too large", errCDPProtocol)
		}
		b = append(b, x)
	}
}
func (s *cdpSession) fail(e error) {
	s.closed.Do(func() {
		defer close(s.teardownDone)
		s.mu.Lock()
		s.err = e
		p := s.pending
		s.pending = map[uint64]chan cdpReply{}
		stop := s.stopContext
		s.mu.Unlock()
		close(s.done) // callers must stop before teardown can block on Windows I/O.
		if stop != nil {
			stop()
		}
		for _, ch := range p {
			select {
			case ch <- cdpReply{failure: &cdpFailure{-1, "DevTools connection closed"}}:
			default:
			}
		}
		// Kill the owned process tree before closing the pipe endpoints. A blocked
		// synchronous Read/Write then unblocks without allowing a renderer to live.
		if !s.contextClosing.Load() && (s.gracefulClose.Load() || s.normalEOFGrace.Load()) && s.platformGracefulClose != nil {
			_ = s.platformGracefulClose()
		} else if s.platformClose != nil {
			_ = s.platformClose()
		}
		_ = s.in.Close()
		_ = s.out.Close()
	})
}
func (s *cdpSession) Close() error {
	if s != nil {
		s.fail(errCDPClosed)
	}
	return nil
}
