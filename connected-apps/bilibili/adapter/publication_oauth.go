package adapter

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	bilibiliAuthorizeURL     = "https://account.bilibili.com/pc/account-pc/auth/oauth"
	publicationOAuthLifetime = 10 * time.Minute
)

var (
	ErrPublicationOAuthCallback           = errors.New("B站授权回跳地址无效或已过期")
	ErrPublicationOAuthScope              = errors.New("B站授权缺少 USER_INFO 或 ARC_BASE")
	ErrPublicationOAuthExchange           = errors.New("B站授权码兑换失败")
	ErrPublicationOAuthRefresh            = errors.New("B站续期结果未确认，请重新网页授权")
	ErrPublicationOAuthRefreshUnavailable = errors.New("当前发布授权不能续期")
)

type PublicationOAuthInput struct {
	Action      string `json:"action"`
	Revision    string `json:"revision"`
	ClientID    string `json:"client_id"`
	AppSecret   string `json:"app_secret"`
	RedirectURI string `json:"redirect_uri"`
	CallbackURL string `json:"callback_url"`
	Enabled     *bool  `json:"enabled"`
}

type PublicationOAuthStart struct {
	AuthorizeURL string `json:"authorize_url"`
	ExpiresAt    string `json:"expires_at"`
}

// An attempt is private and process-local. Restarting the station invalidates
// its state and discards the pending app secret. A callback is consumed before
// exchanging its code, including when the exchange result is unknown.
type publicationOAuthAttempt struct {
	ownerSession    string
	state           string
	redirect        *url.URL
	credentials     OpenCredentials
	enabled         bool
	expectedVersion string
	expires         time.Time
	timer           *time.Timer
}

func (p *PublicApp) PublicationOAuth(ctx context.Context, ownerSession string, in PublicationOAuthInput) (any, error) {
	if ownerSession == "" || ctx.Err() != nil {
		return nil, ErrInvalid
	}
	switch in.Action {
	case "begin":
		return p.beginPublicationOAuth(ownerSession, in)
	case "complete":
		if in.Revision != "" || in.ClientID != "" || in.AppSecret != "" || in.RedirectURI != "" || in.Enabled != nil {
			return nil, ErrInvalid
		}
		return p.completePublicationOAuth(ctx, ownerSession, in.CallbackURL)
	case "refresh":
		if in.ClientID != "" || in.AppSecret != "" || in.RedirectURI != "" || in.CallbackURL != "" || in.Enabled != nil {
			return nil, ErrInvalid
		}
		return p.refreshPublicationOAuth(ctx, in.Revision)
	default:
		return nil, ErrInvalid
	}
}

func (p *PublicApp) beginPublicationOAuth(ownerSession string, in PublicationOAuthInput) (PublicationOAuthStart, error) {
	redirect := publicationRedirectURI(in.RedirectURI)
	if redirect == nil || in.CallbackURL != "" || in.Enabled == nil || !safeHeaderValue(in.ClientID, 256) || !safeHeaderValue(in.AppSecret, 4096) {
		return PublicationOAuthStart{}, ErrInvalid
	}
	state, err := openUploadNonce()
	if err != nil {
		return PublicationOAuthStart{}, ErrPublicationAuthorizationVerify
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	current, err := p.loadPublicationSettingsLocked()
	if err != nil && !errors.Is(err, errPublicationUnconfigured) {
		return PublicationOAuthStart{}, err
	}
	if in.Revision != p.publicationAuthorizationView(current).Revision {
		return PublicationOAuthStart{}, ErrPublicationAuthorizationConflict
	}
	if current.SchemaVersion == 1 {
		return PublicationOAuthStart{}, ErrPublicationAuthorizationLegacy
	}
	if current.SchemaVersion == 2 && in.ClientID != current.ClientID {
		return PublicationOAuthStart{}, ErrPublicationAuthorizationIdentity
	}
	if previous := p.publicationOAuthAttempt; previous != nil {
		previous.timer.Stop()
	}
	attempt := &publicationOAuthAttempt{
		ownerSession: ownerSession, state: state, redirect: redirect, enabled: *in.Enabled,
		credentials:     OpenCredentials{ClientID: in.ClientID, AppSecret: in.AppSecret},
		expectedVersion: current.version, expires: time.Now().Add(publicationOAuthLifetime),
	}
	attempt.timer = time.AfterFunc(publicationOAuthLifetime, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.publicationOAuthAttempt == attempt {
			p.publicationOAuthAttempt = nil
		}
	})
	p.publicationOAuthAttempt = attempt
	query := url.Values{"client_id": {in.ClientID}, "gourl": {redirect.String()}, "state": {state}}
	return PublicationOAuthStart{AuthorizeURL: bilibiliAuthorizeURL + "?" + query.Encode(), ExpiresAt: attempt.expires.UTC().Format(time.RFC3339)}, nil
}

func publicationRedirectURI(raw string) *url.URL {
	if len(raw) > 4096 {
		return nil
	}
	uri, err := url.Parse(raw)
	if err != nil || uri.Opaque != "" || uri.User != nil || uri.Hostname() == "" || uri.Path == "" ||
		uri.RawQuery != "" || uri.ForceQuery || strings.Contains(raw, "#") || strings.Contains(uri.Path, "\\") ||
		strings.Contains(uri.Host, "%") {
		return nil
	}
	if port := uri.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil
		}
	}
	if uri.Path != path.Clean(uri.Path) && uri.Path != path.Clean(uri.Path)+"/" {
		return nil
	}
	if uri.Scheme == "https" {
		return uri
	}
	ip := net.ParseIP(uri.Hostname())
	if uri.Scheme == "http" && (strings.EqualFold(uri.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())) {
		return uri
	}
	return nil
}

func publicationCallbackCode(raw string, attempt *publicationOAuthAttempt) string {
	if len(raw) > 8192 || strings.Contains(raw, "#") {
		return ""
	}
	callback, err := url.Parse(raw)
	if err != nil || callback.Opaque != "" || callback.User != nil || callback.Scheme != attempt.redirect.Scheme ||
		callback.Host != attempt.redirect.Host || callback.EscapedPath() != attempt.redirect.EscapedPath() {
		return ""
	}
	params, err := url.ParseQuery(callback.RawQuery)
	if err != nil || len(params) != 2 || len(params["code"]) != 1 || len(params["state"]) != 1 ||
		subtle.ConstantTimeCompare([]byte(params.Get("state")), []byte(attempt.state)) != 1 || !safeHeaderValue(params.Get("code"), 4096) {
		return ""
	}
	return params.Get("code")
}

func (p *PublicApp) consumePublicationCallback(ownerSession, callbackURL string) (*publicationOAuthAttempt, loadedPublicationSettings, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	attempt := p.publicationOAuthAttempt
	if attempt == nil || attempt.ownerSession != ownerSession || !time.Now().Before(attempt.expires) {
		return nil, loadedPublicationSettings{}, "", ErrPublicationOAuthCallback
	}
	code := publicationCallbackCode(callbackURL, attempt)
	if code == "" {
		return nil, loadedPublicationSettings{}, "", ErrPublicationOAuthCallback
	}
	attempt.timer.Stop()
	p.publicationOAuthAttempt = nil
	current, err := p.loadPublicationSettingsLocked()
	if err != nil && !errors.Is(err, errPublicationUnconfigured) {
		return nil, loadedPublicationSettings{}, "", err
	}
	if current.version != attempt.expectedVersion {
		return nil, loadedPublicationSettings{}, "", ErrPublicationAuthorizationConflict
	}
	return attempt, current, code, nil
}

func (p *PublicApp) publicationOAuthClient() publicationOAuthTransport {
	if p.publicationOAuth != nil {
		return p.publicationOAuth
	}
	return newPublicationOAuthClient()
}

func (p *PublicApp) completePublicationOAuth(ctx context.Context, ownerSession, callbackURL string) (PublicationAuthorizationView, error) {
	attempt, current, code, err := p.consumePublicationCallback(ownerSession, callbackURL)
	if err != nil {
		return PublicationAuthorizationView{}, err
	}
	token, err := p.publicationOAuthClient().Exchange(ctx, attempt.credentials, code)
	if err != nil {
		return PublicationAuthorizationView{}, ErrPublicationOAuthExchange
	}
	if !canPublishWithOAuthScopes(token.Scopes) {
		return PublicationAuthorizationView{}, ErrPublicationOAuthScope
	}
	return p.savePublicationOAuthToken(ctx, current, attempt.credentials, token, attempt.enabled)
}

func canPublishWithOAuthScopes(scopes []string) bool {
	return slices.Contains(scopes, "USER_INFO") && slices.Contains(scopes, "ARC_BASE")
}

func (p *PublicApp) refreshPublicationOAuth(ctx context.Context, revision string) (PublicationAuthorizationView, error) {
	current, err := p.loadPublicationSettings()
	if errors.Is(err, errPublicationUnconfigured) {
		return PublicationAuthorizationView{}, ErrPublicationOAuthRefreshUnavailable
	}
	if err != nil {
		return PublicationAuthorizationView{}, err
	}
	view := p.publicationAuthorizationView(current)
	if revision != view.Revision {
		return view, ErrPublicationAuthorizationConflict
	}
	if !view.RefreshAvailable {
		return view, ErrPublicationOAuthRefreshUnavailable
	}

	// Refresh tokens are single use. Commit a tombstone before contacting the
	// platform so restart, cancellation and lost responses cannot replay it.
	pending := current.publicationSettings
	pending.RefreshToken, pending.RefreshPending = "", true
	claimed, err := p.commitPublicationAuthorization(ctx, current.version, pending)
	if err != nil {
		return view, err
	}
	creds := OpenCredentials{ClientID: current.ClientID, AppSecret: current.AppSecret}
	token, err := p.publicationOAuthClient().Refresh(ctx, creds, current.RefreshToken)
	if err != nil || token.RefreshToken == current.RefreshToken {
		return p.publicationAuthorizationView(claimed), ErrPublicationOAuthRefresh
	}
	if token.Scopes != nil && !canPublishWithOAuthScopes(token.Scopes) {
		return p.publicationAuthorizationView(claimed), ErrPublicationOAuthScope
	}
	return p.savePublicationOAuthToken(ctx, claimed, creds, token, current.Enabled)
}

func (p *PublicApp) savePublicationOAuthToken(ctx context.Context, current loadedPublicationSettings, credentials OpenCredentials, token publicationOAuthToken, enabled bool) (PublicationAuthorizationView, error) {
	view := p.publicationAuthorizationView(current)
	if !safeHeaderValue(token.RefreshToken, 4096) {
		return view, ErrPublicationOAuthExchange
	}
	next, err := p.verifyPublicationAuthorization(ctx, current.publicationSettings, PublicationAuthorizationInput{
		ClientID: credentials.ClientID, AppSecret: credentials.AppSecret, AccessToken: token.AccessToken,
		ExpiresAt: token.ExpiresAt.UTC().Format(time.RFC3339), Enabled: &enabled,
	})
	if err != nil {
		return view, err
	}
	next.RefreshToken = token.RefreshToken
	saved, err := p.commitPublicationAuthorization(ctx, current.version, next)
	if err != nil {
		return view, err
	}
	return p.publicationAuthorizationView(saved), nil
}
