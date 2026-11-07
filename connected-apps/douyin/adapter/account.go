package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/workspacefs"
)

const (
	accountConnectionID  = "authorized-account"
	accountFile          = "authorization.json"
	accountScope         = "user_info"
	accountWorksScope    = "video.list.bind"
	accountPublishScope  = "video.create.bind"
	accountPostingScope  = "posting.behavior"
	accountOAuthLifetime = 10 * time.Minute
)

var (
	ErrAccountInput              = errors.New("请核对抖音应用凭据与已登记的 HTTPS 回调地址")
	ErrAccountCallback           = errors.New("抖音授权回跳与本次请求不符、已用过或已过期")
	ErrAccountScope              = errors.New("抖音账号未授予公开资料读取权限")
	ErrAccountRevision           = errors.New("抖音账号授权已变化，请刷新后重新操作")
	ErrAccountIdentity           = errors.New("这次授权对应另一个抖音应用或账号，请先解除现有连接")
	ErrAccountExchange           = errors.New("抖音授权码兑换失败，请重新发起授权")
	ErrAccountProfile            = errors.New("抖音账号身份核验失败，请重新发起授权")
	ErrAccountRefreshUnavailable = errors.New("抖音账号无法续期，请在桌面端重新扫码授权")
	ErrAccountRefreshFailed      = errors.New("抖音续期结果未确认，请核对账号状态后再试")
	ErrAccountRefreshBusy        = errors.New("抖音账号已有授权操作正在处理")
)

type AccountAuthorizationInput struct {
	Action       string `json:"action"`
	Revision     string `json:"revision"`
	ClientKey    string `json:"client_key"`
	ClientSecret string `json:"client_secret"`
	RedirectURI  string `json:"redirect_uri"`
	CallbackURL  string `json:"callback_url"`
	State        string `json:"state"`
	ReadWorks    bool   `json:"read_works"`
	PublishVideo bool   `json:"publish_video"`
}

type AccountAuthorizationView struct {
	Configured        bool   `json:"configured"`
	RefreshAvailable  bool   `json:"refresh_available"`
	WorksAuthorized   bool   `json:"works_authorized"`
	PublishAuthorized bool   `json:"publish_authorized"`
	Status            string `json:"status"`
	Label             string `json:"label"`
	ExpiresAt         string `json:"expires_at,omitempty"`
	Revision          string `json:"revision"`
}

type accountOAuthStart struct {
	AuthorizeURL string    `json:"authorize_url"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type accountAuthorization struct {
	SchemaVersion    int       `json:"schema_version"`
	Revision         string    `json:"revision"`
	ConsentRevision  string    `json:"consent_revision,omitempty"`
	ClientKey        string    `json:"client_key,omitempty"`
	OpenID           string    `json:"open_id,omitempty"`
	AccessToken      string    `json:"access_token,omitempty"`
	Label            string    `json:"label,omitempty"`
	ReadWorks        bool      `json:"read_works,omitempty"`
	PublishVideo     bool      `json:"publish_video,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitzero"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitzero"`
}

type accountAttempt struct {
	binding         string
	state           string
	revision        string
	settings        OAuthSettings
	redirect        *url.URL
	expires         time.Time
	consumed        bool
	readWorks       bool
	callbackWorks   bool
	publishVideo    bool
	callbackPublish bool
	timer           *time.Timer
}

func (p *SearchApp) AccountAuthorization() (AccountAuthorizationView, error) {
	record, err := p.currentAccount()
	return p.accountView(record), err
}

func (p *SearchApp) accountView(record accountAuthorization) AccountAuthorizationView {
	view := AccountAuthorizationView{Revision: record.Revision, Status: "unconfigured"}
	if record.AccessToken == "" {
		return view
	}
	view.Configured = true
	view.RefreshAvailable = record.refreshAvailable(p.nowUTC())
	view.WorksAuthorized = record.ReadWorks
	view.PublishAuthorized = record.PublishVideo
	view.Label = record.Label
	view.ExpiresAt = record.ExpiresAt.UTC().Format(time.RFC3339)
	view.Status = "ready"
	if !p.nowUTC().Before(record.ExpiresAt) {
		view.Status = "expired"
	}
	return view
}

func (p *SearchApp) accountConnection() apphost.Connection {
	record, err := p.currentAccount()
	view := p.accountView(record)
	if err != nil {
		view.Status = "invalid"
	}
	label := view.Label
	if label == "" {
		label = "抖音账号"
	}
	return apphost.Connection{ID: accountConnectionID, AccountID: accountWorksAccountID(record), Label: label, Configuration: view.Status}
}

func (p *SearchApp) AuthorizeAccount(ctx context.Context, binding string, in AccountAuthorizationInput) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if binding == "" {
		return nil, ErrAccountInput
	}
	switch in.Action {
	case "begin":
		return p.beginAccountAuthorization(binding, in)
	case "complete":
		if in.ReadWorks || in.PublishVideo || in.ClientKey != "" || in.ClientSecret != "" || in.RedirectURI != "" || in.Revision != "" || in.State != "" {
			return nil, ErrAccountInput
		}
		return p.completeAccountAuthorization(ctx, binding, in.CallbackURL)
	case "cancel":
		if in.ReadWorks || in.PublishVideo || in.ClientKey != "" || in.ClientSecret != "" || in.RedirectURI != "" || in.Revision != "" || in.CallbackURL != "" || len(in.State) != 48 {
			return nil, ErrAccountInput
		}
		return p.cancelAccountAuthorization(binding, in.State)
	case "disconnect", "refresh":
		if in.ReadWorks || in.PublishVideo || in.ClientKey != "" || in.ClientSecret != "" || in.RedirectURI != "" || in.CallbackURL != "" || in.State != "" {
			return nil, ErrAccountInput
		}
		if in.Action == "refresh" {
			return p.refreshAccountAuthorization(ctx, in.Revision)
		}
		return p.disconnectAccount(ctx, in.Revision)
	default:
		return nil, ErrAccountInput
	}
}

func (p *SearchApp) beginAccountAuthorization(binding string, in AccountAuthorizationInput) (accountOAuthStart, error) {
	redirect := accountRedirectURI(in.RedirectURI)
	if redirect == nil || in.CallbackURL != "" || in.State != "" {
		return accountOAuthStart{}, ErrAccountInput
	}
	state, err := accountNonce()
	if err != nil {
		return accountOAuthStart{}, err
	}
	settings := OAuthSettings{ClientKey: in.ClientKey, ClientSecret: in.ClientSecret, RedirectURI: redirect.String()}
	scopes := []string{accountScope}
	if in.ReadWorks {
		scopes = append(scopes, accountWorksScope)
	}
	if in.PublishVideo {
		scopes = append(scopes, accountPublishScope, accountPostingScope)
	}
	authorizeURL, err := AuthorizationURL(settings, scopes, state)
	if err != nil {
		return accountOAuthStart{}, ErrAccountInput
	}
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	record, err := p.readAccount()
	if err != nil {
		return accountOAuthStart{}, err
	}
	if record.Revision != in.Revision {
		return accountOAuthStart{}, ErrAccountRevision
	}
	if record.ClientKey != "" && record.ClientKey != in.ClientKey {
		return accountOAuthStart{}, ErrAccountIdentity
	}
	p.clearAccountAttempt()
	attempt := &accountAttempt{binding: binding, state: state, revision: record.Revision, settings: settings, redirect: redirect, readWorks: in.ReadWorks, publishVideo: in.PublishVideo, expires: p.nowUTC().Add(accountOAuthLifetime)}
	attempt.timer = time.AfterFunc(accountOAuthLifetime, func() {
		p.accountMu.Lock()
		defer p.accountMu.Unlock()
		if p.accountPending == attempt {
			p.accountPending = nil
		}
	})
	p.accountPending = attempt
	return accountOAuthStart{AuthorizeURL: authorizeURL, ExpiresAt: attempt.expires}, nil
}

func (p *SearchApp) clearAccountAttempt() {
	if p.accountPending != nil {
		p.accountPending.timer.Stop()
		p.accountPending = nil
	}
}

func (p *SearchApp) cancelAccountAuthorization(binding, state string) (AccountAuthorizationView, error) {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	if attempt := p.accountPending; attempt != nil && attempt.binding == binding &&
		subtle.ConstantTimeCompare([]byte(state), []byte(attempt.state)) == 1 {
		p.clearAccountAttempt()
	}
	record, err := p.readAccount()
	return p.accountView(record), err
}

func (p *SearchApp) consumeAccountCallback(binding, raw string) (*accountAttempt, string, error) {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	attempt := p.accountPending
	if attempt == nil || attempt.consumed || attempt.binding != binding || !p.nowUTC().Before(attempt.expires) {
		return nil, "", ErrAccountCallback
	}
	code, callbackWorks, callbackPublish, err := accountCallbackCode(raw, attempt)
	if err != nil {
		return nil, "", err
	}
	attempt.consumed = true
	attempt.callbackWorks = callbackWorks
	attempt.callbackPublish = callbackPublish
	return attempt, code, nil
}

func (p *SearchApp) completeAccountAuthorization(ctx context.Context, binding, callback string) (AccountAuthorizationView, error) {
	attempt, code, err := p.consumeAccountCallback(binding, callback)
	if err != nil {
		return AccountAuthorizationView{}, err
	}
	defer func() {
		p.accountMu.Lock()
		defer p.accountMu.Unlock()
		if p.accountPending == attempt {
			p.clearAccountAttempt()
		}
	}()
	grant, err := p.ExchangeAuthorizationCode(ctx, attempt.settings, code)
	if err != nil {
		return AccountAuthorizationView{}, ErrAccountExchange
	}
	if !slices.Contains(strings.Split(grant.Scope, ","), accountScope) {
		return AccountAuthorizationView{}, ErrAccountScope
	}
	profile, err := p.ReadAuthorizedProfile(ctx, grant)
	if err != nil || profile.OpenID == "" || profile.OpenID != grant.OpenID {
		return AccountAuthorizationView{}, ErrAccountProfile
	}
	revision, err := accountNonce()
	if err != nil {
		return AccountAuthorizationView{}, err
	}
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	if err := ctx.Err(); err != nil {
		return AccountAuthorizationView{}, err
	}
	record, err := p.readAccount()
	if err != nil {
		return AccountAuthorizationView{}, err
	}
	if p.accountPending != attempt || record.Revision != attempt.revision {
		return AccountAuthorizationView{}, ErrAccountRevision
	}
	if record.OpenID != "" && record.OpenID != grant.OpenID {
		return AccountAuthorizationView{}, ErrAccountIdentity
	}
	if !p.nowUTC().Before(grant.ExpiresAt) {
		return AccountAuthorizationView{}, ErrAccountProfile
	}
	next := accountAuthorization{SchemaVersion: 1, Revision: revision, ClientKey: attempt.settings.ClientKey, OpenID: grant.OpenID, AccessToken: grant.AccessToken, Label: accountLabel(profile.Nickname), ReadWorks: attempt.readWorks && attempt.callbackWorks && hasAccountWorksScope(grant.Scope), PublishVideo: attempt.publishVideo && attempt.callbackPublish && hasAccountPublishScopes(grant.Scope), ExpiresAt: grant.ExpiresAt.UTC(), RefreshToken: grant.RefreshToken, RefreshExpiresAt: grant.RefreshExpiresAt.UTC()}
	if err := p.writeAccount(next); err != nil {
		return AccountAuthorizationView{}, err
	}
	return p.accountView(next), nil
}

func (record accountAuthorization) grantRevision() string {
	if record.ConsentRevision != "" {
		return record.ConsentRevision
	}
	return record.Revision
}

func (record accountAuthorization) refreshAvailable(now time.Time) bool {
	return record.RefreshToken != "" && now.Before(record.RefreshExpiresAt)
}

func (p *SearchApp) refreshAccountAuthorization(ctx context.Context, revision string) (AccountAuthorizationView, error) {
	if !p.accountRefreshMu.TryLock() {
		return AccountAuthorizationView{}, ErrAccountRefreshBusy
	}
	defer p.accountRefreshMu.Unlock()
	p.accountMu.Lock()
	record, err := p.readAccount()
	pending := p.accountPending != nil
	p.accountMu.Unlock()
	if err != nil {
		return AccountAuthorizationView{}, err
	}
	if record.Revision != revision {
		return AccountAuthorizationView{}, ErrAccountRevision
	}
	if pending {
		return AccountAuthorizationView{}, ErrAccountRefreshBusy
	}
	if !record.refreshAvailable(p.nowUTC()) {
		return AccountAuthorizationView{}, ErrAccountRefreshUnavailable
	}
	grant, err := p.refreshOAuthGrant(ctx, record.ClientKey, record.RefreshToken)
	expired := errors.Is(err, errOAuthRefreshExpired)
	next := record
	if expired {
		next.RefreshToken, next.RefreshExpiresAt = "", time.Time{}
	} else {
		if err != nil || grant.OpenID != record.OpenID || !slices.Contains(strings.Split(grant.Scope, ","), accountScope) {
			return AccountAuthorizationView{}, ErrAccountRefreshFailed
		}
		profile, err := p.ReadAuthorizedProfile(ctx, grant)
		if err != nil {
			return AccountAuthorizationView{}, ErrAccountRefreshFailed
		}
		next.AccessToken, next.ExpiresAt = grant.AccessToken, grant.ExpiresAt.UTC()
		next.Label = accountLabel(profile.Nickname)
		next.ReadWorks = record.ReadWorks && hasAccountWorksScope(grant.Scope)
		next.PublishVideo = record.PublishVideo && hasAccountPublishScopes(grant.Scope)
		// Refreshing the access token cannot extend the renewal window.
		next.RefreshToken = grant.RefreshToken
		if grant.RefreshExpiresAt.Before(next.RefreshExpiresAt) {
			next.RefreshExpiresAt = grant.RefreshExpiresAt.UTC()
		}
	}
	next.Revision, err = accountNonce()
	if err != nil {
		return AccountAuthorizationView{}, err
	}
	// A token rotation is independent of the consent that authorized an original
	// upload or cursor. Explicit reauthorization and disconnection change it.
	next.ConsentRevision = record.grantRevision()

	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	if err := ctx.Err(); err != nil {
		return AccountAuthorizationView{}, err
	}
	current, err := p.readAccount()
	if err != nil {
		return AccountAuthorizationView{}, err
	}
	if current.Revision != revision || p.accountPending != nil {
		return AccountAuthorizationView{}, ErrAccountRevision
	}
	if !expired && !p.nowUTC().Before(next.ExpiresAt) {
		return AccountAuthorizationView{}, ErrAccountRefreshFailed
	}
	if err := p.writeAccount(next); err != nil {
		return AccountAuthorizationView{}, err
	}
	if expired {
		return AccountAuthorizationView{}, ErrAccountRefreshUnavailable
	}
	return p.accountView(next), nil
}

func (p *SearchApp) disconnectAccount(ctx context.Context, revision string) (AccountAuthorizationView, error) {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	if err := ctx.Err(); err != nil {
		return AccountAuthorizationView{}, err
	}
	record, err := p.readAccount()
	if err != nil {
		return AccountAuthorizationView{}, err
	}
	if revision != record.Revision {
		return AccountAuthorizationView{}, ErrAccountRevision
	}
	nextRevision, err := accountNonce()
	if err != nil {
		return AccountAuthorizationView{}, err
	}
	next := accountAuthorization{SchemaVersion: 1, Revision: nextRevision}
	if err := p.writeAccount(next); err != nil {
		return AccountAuthorizationView{}, err
	}
	p.clearAccountAttempt()
	return p.accountView(next), nil
}

func accountRedirectURI(raw string) *url.URL {
	if len(raw) > 2048 || strings.ContainsAny(raw, "#\\") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || strings.ContainsAny(u.Host, "%\\") {
		return nil
	}
	if u.Path != "" && u.Path != path.Clean(u.Path) && u.Path != path.Clean(u.Path)+"/" {
		return nil
	}
	return u
}

func accountCallbackCode(raw string, attempt *accountAttempt) (string, bool, bool, error) {
	if len(raw) > 8192 || strings.ContainsAny(raw, "#\\") {
		return "", false, false, ErrAccountCallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Scheme != attempt.redirect.Scheme ||
		u.Host != attempt.redirect.Host || oauthCallbackPath(u) != oauthCallbackPath(attempt.redirect) {
		return "", false, false, ErrAccountCallback
	}
	params, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(params) != 3 || len(params["code"]) != 1 || len(params["state"]) != 1 || len(params["scopes"]) != 1 ||
		subtle.ConstantTimeCompare([]byte(params.Get("state")), []byte(attempt.state)) != 1 {
		return "", false, false, ErrAccountCallback
	}
	code := params.Get("code")
	if code == "" || len(code) > 4096 || strings.ContainsFunc(code, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", false, false, ErrAccountCallback
	}
	if !slices.Contains(strings.Split(params.Get("scopes"), ","), accountScope) {
		return "", false, false, ErrAccountScope
	}
	return code, hasAccountWorksScope(params.Get("scopes")), hasAccountPublishScopes(params.Get("scopes")), nil
}

func hasAccountWorksScope(scopes string) bool {
	values := strings.Split(scopes, ",")
	return slices.Contains(values, accountWorksScope) || slices.Contains(values, "video.list")
}

func hasAccountPublishScopes(scopes string) bool {
	values := strings.Split(scopes, ",")
	return slices.Contains(values, accountPublishScope) && slices.Contains(values, accountPostingScope)
}

func oauthCallbackPath(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	return u.EscapedPath()
}

func accountNonce() (string, error) {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", apphost.ErrUnavailable
	}
	return hex.EncodeToString(random[:]), nil
}

func accountLabel(value string) string {
	runes := make([]rune, 0, 64)
	for _, r := range strings.TrimSpace(value) {
		if !unicode.IsControl(r) && !unicode.In(r, unicode.Cf) {
			runes = append(runes, r)
			if len(runes) == cap(runes) {
				break
			}
		}
	}
	label := strings.TrimSpace(string(runes))
	if label == "" {
		return "抖音账号"
	}
	return label
}

func (p *SearchApp) readAccount() (accountAuthorization, error) {
	var record accountAuthorization
	root, err := workspacefs.OpenPath(p.privateDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return record, apphost.ErrUnavailable
	}
	defer root.Close()
	info, err := root.Lstat(accountFile)
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return record, apphost.ErrUnavailable
	}
	file, err := workspacefs.OpenRegular(root, accountFile)
	if err != nil {
		return record, apphost.ErrUnavailable
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return record, apphost.ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxSettingsBytes+1))
	if err != nil || len(raw) > maxSettingsBytes {
		return record, apphost.ErrUnavailable
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var trailing any
	if dec.Decode(&record) != nil || dec.Decode(&trailing) != io.EOF || record.SchemaVersion != 1 {
		return accountAuthorization{}, apphost.ErrUnavailable
	}
	if decoded, err := hex.DecodeString(record.Revision); err != nil || len(decoded) != 24 {
		return accountAuthorization{}, apphost.ErrUnavailable
	}
	if record.AccessToken == "" {
		if record.ReadWorks || record.PublishVideo || record.ConsentRevision != "" || record.RefreshToken != "" || !record.RefreshExpiresAt.IsZero() || record.ClientKey != "" || record.OpenID != "" || record.Label != "" || !record.ExpiresAt.IsZero() {
			return accountAuthorization{}, apphost.ErrUnavailable
		}
	} else if record.ClientKey == "" || record.OpenID == "" || record.Label != accountLabel(record.Label) || record.ExpiresAt.IsZero() {
		return accountAuthorization{}, apphost.ErrUnavailable
	}
	if record.ConsentRevision != "" {
		if decoded, err := hex.DecodeString(record.ConsentRevision); err != nil || len(decoded) != 24 {
			return accountAuthorization{}, apphost.ErrUnavailable
		}
	}
	if (record.RefreshToken == "") != record.RefreshExpiresAt.IsZero() ||
		record.RefreshToken != "" && !safeOpaque(record.RefreshToken, 1, 1024) {
		return accountAuthorization{}, apphost.ErrUnavailable
	}
	return record, nil
}

func (p *SearchApp) writeAccount(record accountAuthorization) error {
	root, err := workspacefs.EnsurePath(p.privateDirectory)
	if err != nil {
		return apphost.ErrUnavailable
	}
	defer root.Close()
	raw, err := json.Marshal(record)
	if err != nil {
		return apphost.ErrUnavailable
	}
	name := "." + accountFile + "." + record.Revision + ".tmp"
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return apphost.ErrUnavailable
	}
	defer root.Remove(name)
	_, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || root.Rename(name, accountFile) != nil {
		return apphost.ErrUnavailable
	}
	return nil
}
