package pluginruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wechat-bot/internal/pluginhost"
)

type session struct {
	cmd      *exec.Cmd
	in       io.WriteCloser
	out      io.ReadCloser
	reader   *bufio.Reader
	gate     chan struct{}
	done     chan struct{}
	once     sync.Once
	dead     atomic.Bool
	next     int
	maxFrame int
	pack     pluginhost.Package
	grant    pluginhost.Grant
}

func startSession(cmd *exec.Cmd, cleanup func(), p pluginhost.Package, g pluginhost.Grant, maxFrame int) (*session, error) {
	if maxFrame == 0 {
		maxFrame = 8 << 20
	}
	if maxFrame < 1024 {
		cleanup()
		return nil, errors.New("plugin frame limit must be at least 1024 bytes")
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	// Own the read end: Cmd.Wait closes StdoutPipe early when a process exits
	// immediately after its last result, racing the reader of that result.
	out, childOut, err := os.Pipe()
	if err != nil {
		in.Close()
		cleanup()
		return nil, err
	}
	cmd.Stdout = childOut
	// Plugin stderr is untrusted and may contain credentials. Default is discard;
	// bounded/redacted diagnostic collection is a separate host responsibility.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		childOut.Close()
		cleanup()
		return nil, errors.New("plugin sandbox process start failed")
	}
	_ = childOut.Close()
	s := &session{cmd: cmd, in: in, out: out, reader: bufio.NewReaderSize(out, 32<<10), gate: make(chan struct{}, 1), done: make(chan struct{}), maxFrame: maxFrame, pack: p, grant: g}
	go func() { _ = cmd.Wait(); s.dead.Store(true); cleanup(); close(s.done) }()
	return s, nil
}
func (s *session) Close() error {
	s.once.Do(func() { s.dead.Store(true); _ = s.in.Close(); terminate(s.cmd); _ = s.out.Close() })
	select {
	case <-s.done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("plugin process termination unconfirmed")
	}
}
func (s *session) initialize(ctx context.Context) error {
	return s.initializeConfigured(ctx, nil, nil)
}
func (s *session) Alive() bool { return !s.dead.Load() }
func (s *session) initializeConfigured(ctx context.Context, config *pluginhost.RuntimeConfiguration, hostMetadata map[string]json.RawMessage) error {
	params := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "wechat-plugin-host", "version": "1"}}
	if config != nil || len(hostMetadata) > 0 {
		meta := map[string]any{}
		if config != nil {
			meta["wechat/configuration"] = config
		}
		if len(hostMetadata) > 0 {
			// Values have been JSON-validated by Executor.Start. Copy the raw
			// value so a resolver cannot mutate the launch metadata after start.
			copy := map[string]json.RawMessage{}
			for key, value := range hostMetadata {
				copy[key] = append(json.RawMessage(nil), value...)
			}
			meta["wechat/host_metadata"] = copy
		}
		params["_meta"] = meta
	}
	r, err := s.rpc(ctx, "initialize", params, true)
	if err != nil {
		return err
	}
	var reply struct {
		Protocol string `json:"protocolVersion"`
	}
	if json.Unmarshal(r, &reply) != nil || reply.Protocol != "2025-06-18" {
		return errors.New("unsupported plugin MCP protocol")
	}
	_, err = s.rpc(ctx, "notifications/initialized", map[string]any{}, false)
	return err
}
func (s *session) Health(ctx context.Context) error {
	_, err := s.rpc(ctx, "ping", map[string]any{}, true)
	return err
}
func (s *session) Call(ctx context.Context, c pluginhost.Invocation, tool string, args json.RawMessage) (json.RawMessage, error) {
	if err := pluginhost.Authorize(s.pack, s.grant, c, s.pack.Permissions); err != nil {
		return nil, err
	}
	if tool == "" || !json.Valid(args) {
		return nil, errors.New("invalid plugin tool call")
	}
	metadata, err := CallMetadata(ctx)
	if err != nil {
		return nil, err
	}
	meta := map[string]any{"wechat/context": rawCopy(c)}
	metadataBytes := 0
	for key, raw := range metadata {
		if !safeCallMetadataKey(key) || len(raw) == 0 || !json.Valid(raw) {
			return nil, errors.New("plugin call metadata invalid")
		}
		metadataBytes += len(key) + len(raw)
		if metadataBytes > maxHostMetadataBytes {
			return nil, errors.New("plugin call metadata exceeds limit")
		}
		// The context is host-authenticated and cannot be replaced by a policy.
		if key == "wechat/context" {
			return nil, errors.New("plugin call metadata cannot replace context")
		}
		meta[key] = json.RawMessage(append([]byte(nil), raw...))
	}
	// Host evidence is outside arguments; a plugin cannot override it with an
	// argument named account/thread. It still has no direct WeChat credentials.
	return s.rpc(ctx, "tools/call", map[string]any{"name": tool, "arguments": json.RawMessage(append([]byte(nil), args...)), "_meta": meta}, true)
}

func safeCallMetadataKey(key string) bool {
	if key == "wechat/document" || key == "station/audio" {
		return true
	}
	// Future host-owned metadata stays namespaced. Plugin code and arguments do
	// not have an API for injecting this map.
	return strings.HasPrefix(key, "wechat/") && safeMetadataKey(strings.TrimPrefix(key, "wechat/"))
}
func (s *session) rpc(ctx context.Context, method string, params any, response bool) (json.RawMessage, error) {
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.gate }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.dead.Load() {
		return nil, errors.New("plugin session ended; result may require reconciliation")
	}
	s.next++
	id := s.next
	request := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if response {
		request["id"] = id
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("plugin request serialization failed")
	}
	if len(data)+1 > s.maxFrame {
		return nil, fmt.Errorf("plugin request exceeds transport frame limit %d; use paged artifacts", s.maxFrame)
	}
	// Cancellation kills the session: subsequent calls must never consume a late
	// response as if it belonged to a new request. No automatic retry here.
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	if _, err = s.in.Write(append(data, '\n')); err != nil {
		_ = s.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("plugin request write failed; result unknown")
	}
	if !response {
		return nil, nil
	}
	for notifications := 0; notifications < 256; notifications++ {
		line, err := s.readFrame()
		if err != nil {
			_ = s.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		var frame struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &frame) != nil || frame.JSONRPC != "2.0" {
			_ = s.Close()
			return nil, errors.New("invalid plugin protocol frame")
		}
		if frame.Method != "" && len(frame.ID) == 0 {
			continue
		}
		if frame.Method != "" {
			if err = s.serviceReply(ctx, frame.ID, frame.Method, frame.Params); err != nil {
				_ = s.Close()
				return nil, err
			}
			continue
		}
		var received int
		if json.Unmarshal(frame.ID, &received) != nil || received != id || frame.Method != "" {
			_ = s.Close()
			return nil, errors.New("plugin response identity mismatch")
		}
		if len(frame.Error) > 0 && string(frame.Error) != "null" {
			return nil, errors.New("plugin reported a protocol error")
		}
		if len(frame.Result) == 0 {
			return nil, errors.New("plugin response missing result")
		}
		return frame.Result, nil
	}
	_ = s.Close()
	return nil, errors.New("plugin notification budget exhausted before response")
}
func (s *session) readFrame() ([]byte, error) {
	var out []byte
	for {
		part, err := s.reader.ReadSlice('\n')
		if len(out)+len(part) > s.maxFrame {
			return nil, fmt.Errorf("plugin response exceeds transport frame limit %d; result not accepted", s.maxFrame)
		}
		out = append(out, part...)
		if err == nil {
			return out, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, errors.New("plugin session disconnected; result unknown")
		}
	}
}
