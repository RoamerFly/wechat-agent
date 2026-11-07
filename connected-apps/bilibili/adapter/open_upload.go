package adapter

// This file implements the small, protocol-only portion of Bilibili's Open
// Platform upload flow.  It deliberately does not create an archive or expose
// any publishing capability: the host owns OAuth, policy and the publication
// ledger.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wechat-bot/internal/strictjson"
)

const (
	openUploadInitURL     = "https://member.bilibili.com/arcopen/fn/archive/video/init"
	openUploadPartURL     = "https://openupos.bilivideo.com/video/v2/part/upload"
	openUploadCompleteURL = "https://member.bilibili.com/arcopen/fn/archive/video/complete"
	openArchivePostURL    = "https://member.bilibili.com/arcopen/fn/archive/add-by-utoken"
	openArchiveViewURL    = "https://member.bilibili.com/arcopen/fn/archive/view"
	openArchiveListURL    = "https://member.bilibili.com/arcopen/fn/archive/viewlist"
	openAccountInfoURL    = "https://member.bilibili.com/arcopen/fn/user/account/info"

	openUploadResponseLimit = 1 << 20
	openUploadTimeout       = 2 * time.Minute
)

var (
	// ErrOpenUploadInvalid is returned for a locally invalid credential or
	// protocol parameter. Its text intentionally contains no provider data.
	ErrOpenUploadInvalid = errors.New("B站上传参数无效")
	// ErrOpenUploadResponse is returned for a malformed or over-sized response.
	ErrOpenUploadResponse = errors.New("B站上传响应无效")
	// ErrOpenUploadUnavailable is returned for a transport or HTTP service error.
	ErrOpenUploadUnavailable = errors.New("B站上传服务暂不可用")
)

// OpenCredentials must be supplied by a host-authorized OAuth connection.
// They are never placed in a URL, error, or upload-part request.
type OpenCredentials struct {
	ClientID    string
	AppSecret   string
	AccessToken string
}

// OpenUploadOptions contains host-controlled transport defaults. A zero timeout
// uses the default; callers can request longer transfers without a silent cap.
type OpenUploadOptions struct {
	Timeout time.Duration
}

// OpenAPIError reports only an Open Platform code and a stable category. It
// intentionally does not include the provider's message, response body, URL,
// upload token, or credentials.
type OpenAPIError struct {
	Code int
	Kind string
}

func (e *OpenAPIError) Error() string {
	if e == nil {
		return "B站开放平台错误"
	}
	return "B站开放平台" + e.Kind + "错误(" + strconv.Itoa(e.Code) + ")"
}

// OpenUploadClient is safe for concurrent use after construction. The two
// function fields are intentionally replaceable in same-package tests only.
type OpenUploadClient struct {
	client      *http.Client
	credentials OpenCredentials
	now         func() time.Time
	nonce       func() (string, error)
}

// NewOpenUploadClient creates a direct, cookie-less, non-redirecting client.
// The caller retains the OAuth lifecycle; this constructor merely validates
// that required values are present and safe to put in HTTP headers.
func NewOpenUploadClient(credentials OpenCredentials) (*OpenUploadClient, error) {
	return NewOpenUploadClientWithOptions(credentials, OpenUploadOptions{})
}

func NewOpenUploadClientWithOptions(credentials OpenCredentials, options OpenUploadOptions) (*OpenUploadClient, error) {
	if !safeHeaderValue(credentials.ClientID, 256) || !safeHeaderValue(credentials.AppSecret, 4096) || !safeHeaderValue(credentials.AccessToken, 4096) {
		return nil, ErrOpenUploadInvalid
	}
	if options.Timeout < 0 {
		return nil, ErrOpenUploadInvalid
	}
	if options.Timeout == 0 {
		options.Timeout = openUploadTimeout
	}
	return &OpenUploadClient{
		credentials: credentials,
		now:         time.Now,
		nonce:       openUploadNonce,
		client: &http.Client{
			Timeout:   options.Timeout,
			Jar:       nil,
			Transport: openUploadTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func openUploadTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}

// Initialize asks Bilibili for an opaque upload token for a safe basename.
func (c *OpenUploadClient) Initialize(ctx context.Context, name string) (string, error) {
	if c == nil || !safeUploadName(name) {
		return "", ErrOpenUploadInvalid
	}
	body, err := json.Marshal(struct {
		Name       string `json:"name"`
		UploadType string `json:"utype"`
	}{Name: name, UploadType: "0"})
	if err != nil { // kept for future-proofing if the request shape changes.
		return "", ErrOpenUploadInvalid
	}
	data, err := c.signedCall(ctx, openUploadInitURL, nil, body)
	if err != nil {
		return "", err
	}
	var result struct {
		UploadToken string `json:"upload_token"`
	}
	if err := json.Unmarshal(data, &result); err != nil || !safeUploadToken(result.UploadToken) {
		return "", ErrOpenUploadResponse
	}
	return result.UploadToken, nil
}

// UploadPart uploads exactly one already-bounded part. It deliberately accepts
// []byte instead of an io.Reader so a caller cannot accidentally claim one
// length while sending another. The upload endpoint receives no OAuth or
// X-Bili signing headers; its query token is URL-encoded and never surfaced in
// an error.
func (c *OpenUploadClient) UploadPart(ctx context.Context, uploadToken string, part int64, data []byte) error {
	if c == nil || !safeUploadToken(uploadToken) || part < 1 || len(data) == 0 {
		return ErrOpenUploadInvalid
	}
	u, err := fixedOpenUploadURL(openUploadPartURL)
	if err != nil {
		return ErrOpenUploadInvalid
	}
	q := u.Query()
	q.Set("upload_token", uploadToken)
	q.Set("part_number", strconv.FormatInt(part, 10))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return requestError(ctx)
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/octet-stream")
	return c.doResponse(req)
}

// Complete finishes a successfully uploaded set of parts. The request body is
// exactly empty but is signed with the MD5 of that empty body.
func (c *OpenUploadClient) Complete(ctx context.Context, uploadToken string) error {
	if c == nil || !safeUploadToken(uploadToken) {
		return ErrOpenUploadInvalid
	}
	q := make(url.Values)
	q.Set("upload_token", uploadToken)
	_, err := c.signedCall(ctx, openUploadCompleteURL, q, nil)
	return err
}

func (c *OpenUploadClient) signedCall(ctx context.Context, endpoint string, query url.Values, body []byte) (json.RawMessage, error) {
	return c.signedRequest(ctx, http.MethodPost, endpoint, query, body)
}

func (c *OpenUploadClient) signedRequest(ctx context.Context, method, endpoint string, query url.Values, body []byte) (json.RawMessage, error) {
	if c == nil || c.client == nil || c.now == nil || c.nonce == nil {
		return nil, ErrOpenUploadInvalid
	}
	readOnly := endpoint == openArchiveViewURL || endpoint == openArchiveListURL || endpoint == openAccountInfoURL
	if endpoint == openUploadPartURL || (readOnly && (method != http.MethodGet || len(body) != 0)) || (!readOnly && method != http.MethodPost) {
		return nil, ErrOpenUploadInvalid
	}
	u, err := fixedOpenUploadURL(endpoint)
	if err != nil {
		return nil, ErrOpenUploadInvalid
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	nonce, err := c.nonce()
	if err != nil || !safeHeaderValue(nonce, 256) {
		return nil, ErrOpenUploadUnavailable
	}
	bodyHash := md5.Sum(body)
	headers := map[string]string{
		"x-bili-accesskeyid":       c.credentials.ClientID,
		"x-bili-content-md5":       hex.EncodeToString(bodyHash[:]),
		"x-bili-signature-method":  "HMAC-SHA256",
		"x-bili-signature-nonce":   nonce,
		"x-bili-signature-version": "2.0",
		"x-bili-timestamp":         strconv.FormatInt(c.now().Unix(), 10),
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, requestError(ctx)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Access-Token", c.credentials.AccessToken)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Authorization", openUploadSignature(c.credentials.AppSecret, headers))
	return c.doJSON(req)
}

func (c *OpenUploadClient) doJSON(req *http.Request) (json.RawMessage, error) {
	body, err := c.doResponseRaw(req)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if _, err := strictjson.Decode(body); err != nil {
		return nil, ErrOpenUploadResponse
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Code == nil {
		return nil, ErrOpenUploadResponse
	}
	if *envelope.Code != 0 {
		return nil, classifyOpenAPIError(*envelope.Code)
	}
	return envelope.Data, nil
}

func (c *OpenUploadClient) doResponse(req *http.Request) error {
	_, err := c.doJSON(req)
	return err
}

func (c *OpenUploadClient) doResponseRaw(req *http.Request) ([]byte, error) {
	if c == nil || c.client == nil {
		return nil, ErrOpenUploadInvalid
	}
	response, err := c.client.Do(req)
	if err != nil {
		return nil, requestError(req.Context())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, classifyOpenAPIError(response.StatusCode)
		}
		return nil, ErrOpenUploadUnavailable
	}
	limited := io.LimitReader(response.Body, openUploadResponseLimit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, requestError(req.Context())
	}
	if len(body) > openUploadResponseLimit {
		return nil, ErrOpenUploadResponse
	}
	return body, nil
}

func fixedOpenUploadURL(raw string) (*url.URL, error) {
	if raw != openUploadInitURL && raw != openUploadPartURL && raw != openUploadCompleteURL && raw != openArchivePostURL && raw != openArchiveViewURL && raw != openArchiveListURL && raw != openAccountInfoURL {
		return nil, ErrOpenUploadInvalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrOpenUploadInvalid
	}
	switch raw {
	case openUploadInitURL, openUploadCompleteURL, openArchivePostURL, openArchiveViewURL, openArchiveListURL, openAccountInfoURL:
		if u.Host != "member.bilibili.com" || u.Path == "" {
			return nil, ErrOpenUploadInvalid
		}
	case openUploadPartURL:
		if u.Host != "openupos.bilivideo.com" || u.Path != "/video/v2/part/upload" {
			return nil, ErrOpenUploadInvalid
		}
	}
	return u, nil
}

func openUploadSignature(secret string, headers map[string]string) string {
	keys := []string{
		"x-bili-accesskeyid", "x-bili-content-md5", "x-bili-signature-method",
		"x-bili-signature-nonce", "x-bili-signature-version", "x-bili-timestamp",
	}
	// keys are already lexicographically sorted. Keeping the exact six entries
	// prevents accidental signing of access-token or arbitrary request headers.
	var canonical strings.Builder
	for index, key := range keys {
		if index > 0 {
			canonical.WriteByte('\n')
		}
		canonical.WriteString(key)
		canonical.WriteByte(':')
		canonical.WriteString(headers[key])
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical.String()))
	return hex.EncodeToString(mac.Sum(nil))
}

func openUploadNonce() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func safeHeaderValue(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func safeUploadName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || strings.TrimSpace(name) == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\:") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func safeUploadToken(token string) bool {
	if token == "" || len(token) > 4096 || strings.TrimSpace(token) != token {
		return false
	}
	for _, r := range token {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func classifyOpenAPIError(code int) error {
	kind := "请求被拒绝"
	switch {
	case code == 429 || code == 127009 || code == 127306:
		kind = "限流"
	case code == 401 || code == 127001 || code == 127002:
		kind = "鉴权"
	case code == 403 || code == 127007 || code == 127011 || code == 127304 || code == 127305:
		kind = "权限"
	case code == 123028:
		kind = "处理中"
	case (code >= 4000 && code <= 4009) || code == 4012 || code == 127022:
		kind = "参数"
	case (code >= 500 && code < 600) || code == 4010 || code == 4011:
		kind = "服务暂不可用"
	}
	return &OpenAPIError{Code: code, Kind: kind}
}

func requestError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrOpenUploadUnavailable
}
