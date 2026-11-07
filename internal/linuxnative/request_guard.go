package linuxnative

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

type guardedTransport struct {
	next  http.RoundTripper
	check func(*http.Request) (func(), error)
}

func (t guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	release, err := t.check(req)
	if err != nil {
		return nil, err
	}
	res, err := t.next.RoundTrip(req)
	if release != nil {
		if err != nil || res.Body == nil {
			release()
		} else {
			res.Body = &guardedResponseBody{ReadCloser: res.Body, release: release}
		}
	}
	return res, err
}

type guardedResponseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *guardedResponseBody) Close() error {
	defer b.once.Do(b.release)
	return b.ReadCloser.Close()
}

// GuardRequests binds a fresh account sender to its verified native instance.
// Call before publishing the sender. The guard must use an independent client.
func (s *Sender) GuardRequests(check func(context.Context) error) {
	s.guardHTTPRequests(func(req *http.Request) (func(), error) { return nil, check(req.Context()) })
}

// ErrWriteNotAdmitted proves the request did not reach the native transport.
var ErrWriteNotAdmitted = errors.New("native write was not admitted")

// GuardChanges checks account writes at the transport boundary. Reads,
// inbound downloads, and stopping an existing delivery remain available.
func (s *Sender) GuardChanges(check func(context.Context) (func(), error)) {
	prefix := s.accountPath() + "/"
	s.guardHTTPRequests(func(req *http.Request) (func(), error) {
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
			return nil, nil
		}
		path, scoped := strings.CutPrefix(req.URL.EscapedPath(), prefix)
		if scoped && req.Method == http.MethodPost {
			parts := strings.Split(path, "/")
			if len(parts) == 3 && parts[1] != "" && (parts[0] == "jobs" && parts[2] == "cancel" ||
				parts[0] == "messages" && (parts[2] == "media-transfers" || parts[2] == "channels-share-url")) {
				return nil, nil
			}
		}
		release, err := check(req.Context())
		if err != nil {
			return nil, ErrWriteNotAdmitted
		}
		return release, nil
	})
}

func (s *Sender) guardHTTPRequests(check func(*http.Request) (func(), error)) {
	client := *s.client
	client.Transport = guardedTransport{next: client.Transport, check: check}
	s.client = &client
}
