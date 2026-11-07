package stationbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type readerCall struct {
	sessionID string
	method    string
	params    map[string]any
}

type readerFixtureSession struct {
	events chan cdpEvent
	mu     sync.Mutex
	calls  []readerCall
	onCall func(string, map[string]any)
}

func newReaderFixtureSession() *readerFixtureSession {
	return &readerFixtureSession{events: make(chan cdpEvent, 32)}
}

func (s *readerFixtureSession) Call(_ context.Context, sessionID, method string, params any, result any) error {
	p, _ := params.(map[string]any)
	s.mu.Lock()
	s.calls = append(s.calls, readerCall{sessionID: sessionID, method: method, params: p})
	s.mu.Unlock()
	if method == "Target.createTarget" {
		putReaderResult(result, map[string]any{"targetId": "target-1"})
	}
	if method == "Target.attachToTarget" {
		putReaderResult(result, map[string]any{"sessionId": "session-1"})
	}
	if method == "Runtime.evaluate" {
		putReaderResult(result, map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{
			"title": "公开标题", "description": "公开简介", "url": fixtureTarget,
		}}})
	}
	if s.onCall != nil {
		s.onCall(method, p)
	}
	return nil
}

func (s *readerFixtureSession) Events() <-chan cdpEvent { return s.events }
func (s *readerFixtureSession) Close() error            { return nil }

func (s *readerFixtureSession) callsFor(method string) []readerCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []readerCall
	for _, call := range s.calls {
		if call.method == method {
			result = append(result, call)
		}
	}
	return result
}

func putReaderResult(target any, value any) {
	b, _ := json.Marshal(value)
	_ = json.Unmarshal(b, target)
}

const fixtureTarget = "http://127.0.0.1:38901/video"

func fixtureReader(session *readerFixtureSession) *Reader {
	return &Reader{
		profile: "test",
		opener:  func(context.Context, string) (readerSession, error) { return session, nil },
		allow:   func(target string) bool { return target == fixtureTarget },
	}
}

func TestReaderAllowsOnlyOneExactTopLevelDocument(t *testing.T) {
	session := newReaderFixtureSession()
	started := make(chan struct{})
	session.onCall = func(method string, _ map[string]any) {
		if method != "Page.navigate" {
			return
		}
		close(started)
		session.events <- cdpEvent{Method: "Fetch.requestPaused", SessionID: "session-1", Params: json.RawMessage(`{"requestId":"main","resourceType":"Document","request":{"url":"http://127.0.0.1:38901/video","method":"GET"}}`)}
		session.events <- cdpEvent{Method: "Fetch.requestPaused", SessionID: "session-1", Params: json.RawMessage(`{"requestId":"script","resourceType":"Script","request":{"url":"http://127.0.0.1:38901/script.js","method":"GET"}}`)}
		session.events <- cdpEvent{Method: "Fetch.requestPaused", SessionID: "session-1", Params: json.RawMessage(`{"requestId":"redirect","resourceType":"Document","request":{"url":"http://127.0.0.1:38901/redirect","method":"GET"}}`)}
		session.events <- cdpEvent{Method: "Network.responseReceived", SessionID: "session-1", Params: json.RawMessage(`{"type":"Document","response":{"url":"http://127.0.0.1:38901/video","status":200}}`)}
		session.events <- cdpEvent{Method: "Page.frameNavigated", SessionID: "session-1", Params: json.RawMessage(`{"frame":{"url":"http://127.0.0.1:38901/video"}}`)}
		session.events <- cdpEvent{Method: "Page.loadEventFired", SessionID: "session-1", Params: json.RawMessage(`{}`)}
	}
	page, err := fixtureReader(session).Read(t.Context(), fixtureTarget, func() error { return nil })
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if page != (Page{Title: "公开标题", Description: "公开简介"}) {
		t.Fatalf("Read() page = %#v", page)
	}
	<-started
	continued := session.callsFor("Fetch.continueRequest")
	if len(continued) != 1 || continued[0].params["requestId"] != "main" {
		t.Fatalf("continued requests = %#v, want only main document", continued)
	}
	denied := session.callsFor("Fetch.failRequest")
	if len(denied) != 2 {
		t.Fatalf("denied requests = %#v, want script and redirect", denied)
	}
	for _, call := range denied {
		if call.params["requestId"] == "main" {
			t.Fatalf("main document was denied: %#v", call)
		}
	}
}

func TestReaderCancellationStopsWaiting(t *testing.T) {
	session := newReaderFixtureSession()
	session.onCall = func(method string, _ map[string]any) {
		// Page.navigate intentionally receives no events.
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader := fixtureReader(session)
	done := make(chan error, 1)
	go func() {
		_, err := reader.Read(ctx, fixtureTarget, func() error { return nil })
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read() did not return after cancellation")
	}
}

func TestReaderReturnsAuthorizationErrorUnchanged(t *testing.T) {
	session := newReaderFixtureSession()
	want := errors.New("revoked")
	_, err := fixtureReader(session).Read(t.Context(), fixtureTarget, func() error { return want })
	if err != want {
		t.Fatalf("Read() error = %v, want original authorization error", err)
	}
}

func TestExactHTTPSTarget(t *testing.T) {
	for _, target := range []string{
		"https://www.bilibili.com/video/BV1xx411c7mD",
		"https://example.com/path",
	} {
		if !isExactHTTPSTarget(target) {
			t.Fatalf("isExactHTTPSTarget(%q) = false", target)
		}
	}
	for _, target := range []string{
		"http://example.com/path",
		"https://example.com/path?x=1",
		"https://user@example.com/path",
		"https://example.com:443/path",
	} {
		if isExactHTTPSTarget(target) {
			t.Fatalf("isExactHTTPSTarget(%q) = true", target)
		}
	}
}
