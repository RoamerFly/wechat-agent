package adapter

// This file owns the narrow, server-to-server OAuth token exchange used by
// the publication enrollment flow. It intentionally does not create an
// authorization URL or follow a user-agent redirect.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"wechat-bot/internal/strictjson"
)

const (
	publicationOAuthTokenURL        = "https://api.bilibili.com/x/account-oauth2/v1/token"
	publicationOAuthRefreshTokenURL = "https://api.bilibili.com/x/account-oauth2/v1/refresh_token"
	publicationOAuthResponseLimit   = 64 << 10
	publicationOAuthTimeout         = 30 * time.Second
)

// publicationOAuthToken is deliberately private: callers retain only the
// fields needed to create their own private connection record.
type publicationOAuthToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scopes       []string
}

type publicationOAuthTransport interface {
	Exchange(context.Context, OpenCredentials, string) (publicationOAuthToken, error)
	Refresh(context.Context, OpenCredentials, string) (publicationOAuthToken, error)
}

type publicationOAuthClient struct {
	client *http.Client
	now    func() time.Time
}

func newPublicationOAuthClient() publicationOAuthTransport {
	return &publicationOAuthClient{
		now: time.Now,
		client: &http.Client{
			Timeout:   publicationOAuthTimeout,
			Jar:       nil,
			Transport: openUploadTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *publicationOAuthClient) Exchange(ctx context.Context, credentials OpenCredentials, code string) (publicationOAuthToken, error) {
	if !validPublicationOAuthCredentials(credentials) || !safePublicationOAuthValue(code) {
		return publicationOAuthToken{}, ErrOpenUploadInvalid
	}
	return c.request(ctx, publicationOAuthTokenURL, credentials, "authorization_code", "code", code, true)
}

func (c *publicationOAuthClient) Refresh(ctx context.Context, credentials OpenCredentials, refreshToken string) (publicationOAuthToken, error) {
	if !validPublicationOAuthCredentials(credentials) || !safePublicationOAuthValue(refreshToken) {
		return publicationOAuthToken{}, ErrOpenUploadInvalid
	}
	return c.request(ctx, publicationOAuthRefreshTokenURL, credentials, "refresh_token", "refresh_token", refreshToken, false)
}

func validPublicationOAuthCredentials(credentials OpenCredentials) bool {
	// AccessToken is intentionally not required here: neither documented token
	// endpoint uses it, and accepting it would incorrectly turn a renewal into
	// a dependency on an expired access token.
	return safeHeaderValue(credentials.ClientID, 256) && safeHeaderValue(credentials.AppSecret, 4096)
}

func safePublicationOAuthValue(value string) bool {
	return safeHeaderValue(value, 4096)
}

func (c *publicationOAuthClient) request(ctx context.Context, endpoint string, credentials OpenCredentials, grantType, tokenKey, token string, requireScopes bool) (publicationOAuthToken, error) {
	if c == nil || c.client == nil || c.now == nil {
		return publicationOAuthToken{}, ErrOpenUploadInvalid
	}
	u, err := fixedPublicationOAuthURL(endpoint)
	if err != nil {
		return publicationOAuthToken{}, ErrOpenUploadInvalid
	}
	query := u.Query()
	query.Set("client_id", credentials.ClientID)
	query.Set("client_secret", credentials.AppSecret)
	query.Set("grant_type", grantType)
	query.Set(tokenKey, token)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(nil))
	if err != nil {
		return publicationOAuthToken{}, requestError(ctx)
	}
	// Bilibili specifies a form POST, but its fields belong in the query string.
	// Keep the body exactly empty so no secret is duplicated in a request body.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := c.client.Do(req)
	if err != nil {
		return publicationOAuthToken{}, requestError(ctx)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return publicationOAuthToken{}, classifyOpenAPIError(response.StatusCode)
		}
		return publicationOAuthToken{}, ErrOpenUploadUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, publicationOAuthResponseLimit+1))
	if err != nil {
		return publicationOAuthToken{}, requestError(ctx)
	}
	if len(body) > publicationOAuthResponseLimit {
		return publicationOAuthToken{}, ErrOpenUploadResponse
	}
	return decodePublicationOAuthToken(body, c.now(), requireScopes)
}

func fixedPublicationOAuthURL(raw string) (*url.URL, error) {
	if raw != publicationOAuthTokenURL && raw != publicationOAuthRefreshTokenURL {
		return nil, ErrOpenUploadInvalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "api.bilibili.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrOpenUploadInvalid
	}
	if (raw == publicationOAuthTokenURL && u.Path != "/x/account-oauth2/v1/token") || (raw == publicationOAuthRefreshTokenURL && u.Path != "/x/account-oauth2/v1/refresh_token") {
		return nil, ErrOpenUploadInvalid
	}
	return u, nil
}

func decodePublicationOAuthToken(body []byte, now time.Time, requireScopes bool) (publicationOAuthToken, error) {
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if _, err := strictjson.Decode(body); err != nil {
		return publicationOAuthToken{}, ErrOpenUploadResponse
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Code == nil {
		return publicationOAuthToken{}, ErrOpenUploadResponse
	}
	if *envelope.Code != 0 {
		return publicationOAuthToken{}, classifyOpenAPIError(*envelope.Code)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data, &fields); err != nil || fields == nil {
		return publicationOAuthToken{}, ErrOpenUploadResponse
	}
	accessRaw, accessOK := fields["access_token"]
	refreshRaw, refreshOK := fields["refresh_token"]
	expiresRaw, expiresOK := fields["expires_in"]
	if !accessOK || !refreshOK || !expiresOK {
		return publicationOAuthToken{}, ErrOpenUploadResponse
	}
	var token publicationOAuthToken
	var expires int64
	if json.Unmarshal(accessRaw, &token.AccessToken) != nil || json.Unmarshal(refreshRaw, &token.RefreshToken) != nil || json.Unmarshal(expiresRaw, &expires) != nil ||
		!safePublicationOAuthValue(token.AccessToken) || !safePublicationOAuthValue(token.RefreshToken) || expires <= now.Unix() {
		return publicationOAuthToken{}, ErrOpenUploadResponse
	}
	token.ExpiresAt = time.Unix(expires, 0).UTC()
	if token.ExpiresAt.Unix() != expires || token.ExpiresAt.Year() > 9999 {
		return publicationOAuthToken{}, ErrOpenUploadResponse
	}
	scopesRaw, hasScopes := fields["scopes"]
	if !hasScopes {
		if requireScopes {
			return publicationOAuthToken{}, ErrOpenUploadResponse
		}
		return token, nil
	}
	if json.Unmarshal(scopesRaw, &token.Scopes) != nil || token.Scopes == nil || len(token.Scopes) > 128 {
		return publicationOAuthToken{}, ErrOpenUploadResponse
	}
	for _, scope := range token.Scopes {
		if !safeHeaderValue(scope, 256) {
			return publicationOAuthToken{}, ErrOpenUploadResponse
		}
	}
	return token, nil
}
