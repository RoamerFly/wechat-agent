// Package linuxnative adapts the local sender API to the Linux-native
// agent-wechat HTTP service. It deliberately accepts only a literal loopback
// endpoint: this prevents a deployment configuration from turning a bearer
// token into an arbitrary outbound HTTP credential.
package linuxnative

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/sending"
)

const (
	maxResponseBytes = 64 << 10
	defaultPoll      = 250 * time.Millisecond
	defaultReadyWait = 2 * time.Second
)

// Config configures the separately deployed Linux-native service. TokenFile
// is intentionally a filename rather than a token value, so process listings
// and ordinary environment dumps do not contain the bearer credential.
type Config struct {
	URL          string
	TokenFile    string
	Account      string
	PollInterval time.Duration
	ReadyTimeout time.Duration
}

// Sender implements sending.Engine.
type Sender struct {
	namesMu      sync.Mutex
	names        map[string]nameCacheEntry
	base         url.URL
	token        string
	account      string
	pollInterval time.Duration
	readyTimeout time.Duration
	client       *http.Client
}

type accountStatus struct {
	Authenticated *bool `json:"authenticated"`
}

type sendIntent struct {
	ID          string `json:"id"`
	Destination string `json:"destination"`
	State       string `json:"state"`
	ClientID    string `json:"clientId"`
	ServerID    string `json:"serverId"`
	Reason      string `json:"reason"`
	ErrorCode   string `json:"errorCode"`
}

// New validates immutable configuration and reads the explicit token file.
// No network request is made here.
func New(config Config) (*Sender, error) {
	base, err := loopbackURL(config.URL)
	if err != nil {
		return nil, errors.New("linux native URL is invalid")
	}
	if !safeID(config.Account) {
		return nil, errors.New("linux native account is invalid")
	}
	token, err := readToken(config.TokenFile)
	if err != nil {
		return nil, err
	}
	if config.PollInterval <= 0 {
		config.PollInterval = defaultPoll
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = defaultReadyWait
	}

	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true, // Transport retries are possible on reused connections.
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, _, splitErr := net.SplitHostPort(address)
			if splitErr != nil || !literalLoopback(host) {
				return nil, errors.New("non-loopback dial denied")
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	return &Sender{
		base: base, token: token, account: config.Account,
		pollInterval: config.PollInterval, readyTimeout: config.ReadyTimeout,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("redirect denied")
			},
		},
	}, nil
}

// Ready checks the upstream's authenticated state. Account existence alone is
// never interpreted as logged in.
func (s *Sender) Ready() bool {
	ctx, cancel := context.WithTimeout(context.Background(), s.readyTimeout)
	defer cancel()
	return s.authenticated(ctx)
}

// SendText creates a single idempotent send intent and polls only its safe,
// read-only status endpoint until the context expires or final state arrives.
func (s *Sender) SendText(ctx context.Context, request sending.TextRequest) (sending.Receipt, error) {
	if !s.authenticated(ctx) {
		return sending.Receipt{}, sending.ErrNotReady
	}
	body, err := json.Marshal(struct {
		Kind     string `json:"kind"`
		ThreadID string `json:"threadId"`
		Text     string `json:"text"`
	}{Kind: "text", ThreadID: request.Recipient, Text: request.Text})
	if err != nil {
		return sending.Receipt{}, sending.ErrRejected
	}

	post, err := s.newRequest(ctx, http.MethodPost, s.sendsPath(), bytes.NewReader(body))
	if err != nil {
		return sending.Receipt{}, sending.ErrOutcomeUnknown
	}
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Authorization", "Bearer "+s.token)
	post.Header.Set("Idempotency-Key", randomKey())
	post.GetBody = nil                 // Do not give Transport a replayable POST body.
	response, err := s.client.Do(post) // Never retry a write: it may have succeeded.
	if err != nil {
		if errors.Is(err, ErrWriteNotAdmitted) {
			return sending.Receipt{}, ErrWriteNotAdmitted
		}
		return sending.Receipt{}, sending.ErrOutcomeUnknown
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return sending.Receipt{}, sending.ErrNotReady
	}
	if response.StatusCode != http.StatusAccepted {
		if response.StatusCode >= 400 && response.StatusCode < 500 {
			return sending.Receipt{}, sending.ErrRejected
		}
		return sending.Receipt{}, sending.ErrOutcomeUnknown
	}
	var intent sendIntent
	if !decodeBounded(response.Body, &intent) || intent.ID == "" || !safeID(intent.ID) {
		return sending.Receipt{}, sending.ErrOutcomeUnknown
	}
	if receipt, final, err := intentReceipt(intent, request.Recipient); final {
		return receipt, err
	}

	id := intent.ID
	for {
		select {
		case <-ctx.Done():
			return sending.Receipt{}, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: "receipt_wait_timeout", IntentID: id}
		case <-time.After(s.pollInterval):
		}
		intent, err = s.getIntent(ctx, id)
		if err != nil {
			return sending.Receipt{}, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: "receipt_lookup_failed", IntentID: id}
		}
		if receipt, final, outcome := intentReceipt(intent, request.Recipient); final {
			return receipt, outcome
		}
	}
}

func (s *Sender) authenticated(ctx context.Context) bool {
	request, err := s.newRequest(ctx, http.MethodGet, s.accountPath(), nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	response, err := s.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var status accountStatus
	return decodeBounded(response.Body, &status) && status.Authenticated != nil && *status.Authenticated
}

func (s *Sender) getIntent(ctx context.Context, id string) (sendIntent, error) {
	request, err := s.newRequest(ctx, http.MethodGet, s.sendsPath()+"/"+url.PathEscape(id), nil)
	if err != nil {
		return sendIntent{}, err
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	response, err := s.client.Do(request)
	if err != nil {
		return sendIntent{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return sendIntent{}, errors.New("intent status unavailable")
	}
	var intent sendIntent
	if !decodeBounded(response.Body, &intent) || intent.ID != id {
		return sendIntent{}, errors.New("intent status unavailable")
	}
	return intent, nil
}

func (s *Sender) accountPath() string { return "/v1/accounts/" + url.PathEscape(s.account) }
func (s *Sender) sendsPath() string   { return s.accountPath() + "/sends" }

func (s *Sender) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u := s.base
	u.Path = path
	return http.NewRequestWithContext(ctx, method, u.String(), body)
}

func loopbackURL(raw string) (url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return url.URL{}, errors.New("invalid")
	}
	if !literalLoopback(u.Hostname()) {
		return url.URL{}, errors.New("invalid")
	}
	if port := u.Port(); port != "" {
		p, portErr := strconv.Atoi(port)
		if portErr != nil || p < 1 || p > 65535 {
			return url.URL{}, errors.New("invalid")
		}
	}
	u.Path = ""
	return *u, nil
}

func literalLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func readToken(filename string) (string, error) {
	if filename == "" {
		return "", errors.New("linux native token file is required")
	}
	f, err := os.Open(filename)
	if err != nil {
		return "", errors.New("linux native token file is unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil || len(data) > 1024 {
		return "", errors.New("linux native token file is invalid")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || !utf8.ValidString(token) || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("linux native token file is invalid")
	}
	return token, nil
}

func safeID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return value != "." && value != ".."
}

func decodeBounded(body io.Reader, value any) bool {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return false
	}
	var extra any
	return errors.Is(decoder.Decode(&extra), io.EOF)
}

func intentReceipt(intent sendIntent, recipient string) (sending.Receipt, bool, error) {
	if intent.Reason == "" {
		intent.Reason = intent.ErrorCode
	}
	switch intent.State {
	case "accepted":
		if intent.Destination != recipient || !positiveDecimal(intent.ClientID) || !positiveDecimal(intent.ServerID) {
			return sending.Receipt{}, true, sending.ErrOutcomeUnknown
		}
		return sending.Receipt{Recipient: recipient, ClientMessageID: intent.ClientID, ServerMessageID: intent.ServerID}, true, nil
	case "failed":
		return sending.Receipt{}, true, &sending.OutcomeError{Cause: sending.ErrRejected, Reason: safeSendReason(intent.Reason), IntentID: intent.ID}
	case "needs_review":
		return sending.Receipt{}, true, &sending.OutcomeError{Cause: sending.ErrOutcomeUnknown, Reason: safeSendReason(intent.Reason), IntentID: intent.ID}
	case "queued", "sending", "submitting":
		return sending.Receipt{}, false, nil
	default:
		return sending.Receipt{}, true, sending.ErrOutcomeUnknown
	}
}

func safeSendReason(reason string) string {
	switch reason {
	case "submission_outcome_unknown", "service_rejected", "transport_unavailable", "quote_target_unavailable", "media_processing_failed", "unsupported_media", "media_encoder_unavailable", "voice_duration_limit", "auth_required", "media_service_rejected", "media_cancelled", "media_disconnected", "media_worker_stopping", "worker_interrupted", "media_worker_cancelled":
		return reason
	default:
		return "send_outcome_unknown"
	}
}

func positiveDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	v, err := strconv.ParseUint(value, 10, 64)
	return err == nil && v > 0
}

func randomKey() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}
