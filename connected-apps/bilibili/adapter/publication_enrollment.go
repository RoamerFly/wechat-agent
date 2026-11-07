package adapter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrPublicationAuthorizationConflict = errors.New("B站发布授权已更新")
	ErrPublicationAuthorizationIdentity = errors.New("B站授权不属于已绑定的应用和账号")
	ErrPublicationAuthorizationLegacy   = errors.New("请先核验现有发布凭据，再更换令牌")
	ErrPublicationAuthorizationVerify   = errors.New("B站账号核验失败")
)

const unconfiguredPublicationRevision = "unconfigured"

// PublicationAuthorizationView is owner-facing. Raw OpenID and credentials
// are never returned by this interface.
type PublicationAuthorizationView struct {
	Revision         string `json:"revision"`
	State            string `json:"state"`
	Verified         bool   `json:"verified"`
	Enabled          bool   `json:"enabled"`
	ExpiresAt        string `json:"expires_at,omitempty"`
	RefreshAvailable bool   `json:"refresh_available"`
	RefreshPending   bool   `json:"refresh_pending"`
}

type PublicationAuthorizationInput struct {
	Action      string `json:"action"`
	Revision    string `json:"revision"`
	ClientID    string `json:"client_id"`
	AppSecret   string `json:"app_secret"`
	AccessToken string `json:"access_token"`
	ExpiresAt   string `json:"expires_at"`
	Enabled     *bool  `json:"enabled"`
}

func (p *PublicApp) PublicationAuthorization() (PublicationAuthorizationView, error) {
	value, err := p.loadPublicationSettings()
	if errors.Is(err, errPublicationUnconfigured) {
		return p.publicationAuthorizationView(loadedPublicationSettings{}), nil
	}
	if err != nil {
		return PublicationAuthorizationView{}, err
	}
	return p.publicationAuthorizationView(value), nil
}

func (p *PublicApp) publicationAuthorizationView(value loadedPublicationSettings) PublicationAuthorizationView {
	if value.version == "" {
		return PublicationAuthorizationView{Revision: unconfiguredPublicationRevision, State: "unconfigured"}
	}
	mac := hmac.New(sha256.New, p.publicationUIKey[:])
	_, _ = mac.Write([]byte(value.version))
	view := PublicationAuthorizationView{
		Revision: hex.EncodeToString(mac.Sum(nil)), Verified: value.SchemaVersion == 2, Enabled: value.Enabled, ExpiresAt: value.ExpiresAt,
		RefreshAvailable: value.RefreshToken != "" && time.Now().Before(value.expiresAt), RefreshPending: value.RefreshPending,
	}
	switch {
	case !value.Enabled:
		view.State = "disabled"
	case !time.Now().Before(value.expiresAt):
		view.State = "expired"
	case !view.Verified:
		view.State = "legacy"
	default:
		view.State = "ready"
	}
	return view
}

func (p *PublicApp) SavePublicationAuthorization(ctx context.Context, in PublicationAuthorizationInput) (PublicationAuthorizationView, error) {
	current, err := p.loadPublicationSettings()
	if err != nil && !errors.Is(err, errPublicationUnconfigured) {
		return PublicationAuthorizationView{}, err
	}
	view := p.publicationAuthorizationView(current)
	if in.Revision != view.Revision {
		return view, ErrPublicationAuthorizationConflict
	}
	next := current.publicationSettings
	switch in.Action {
	case "disable":
		if current.version == "" || in.Enabled != nil || in.ClientID != "" || in.AppSecret != "" || in.AccessToken != "" || in.ExpiresAt != "" {
			return view, ErrInvalid
		}
		if !next.Enabled {
			return view, nil
		}
		next.Enabled = false
	case "verify":
		next, err = p.verifyPublicationAuthorization(ctx, current.publicationSettings, in)
		if err != nil {
			return view, err
		}
	default:
		return view, ErrInvalid
	}
	saved, err := p.commitPublicationAuthorization(ctx, current.version, next)
	if err != nil {
		return view, err
	}
	return p.publicationAuthorizationView(saved), nil
}

func (p *PublicApp) commitPublicationAuthorization(ctx context.Context, expected string, next publicationSettings) (loadedPublicationSettings, error) {
	var err error
	if next.SchemaVersion == 2 {
		next.PolicyNonce, err = openUploadNonce()
		if err != nil {
			return loadedPublicationSettings{}, ErrPublicationAuthorizationVerify
		}
	}
	expiry, err := validatePublicationSettings(&next)
	if err != nil {
		return loadedPublicationSettings{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return loadedPublicationSettings{}, err
	}
	latest, readErr := p.loadPublicationSettingsLocked()
	if readErr != nil && !errors.Is(readErr, errPublicationUnconfigured) {
		return loadedPublicationSettings{}, readErr
	}
	if latest.version != expected {
		return loadedPublicationSettings{}, ErrPublicationAuthorizationConflict
	}
	if err := p.savePrivateJSON(publicationSettingsName, next); err != nil {
		return loadedPublicationSettings{}, err
	}
	raw, _ := json.Marshal(next)
	digest := sha256.Sum256(raw)
	return loadedPublicationSettings{
		publicationSettings: next, expiresAt: expiry, version: "bilibili-publication-" + hex.EncodeToString(digest[:]),
	}, nil
}

func (p *PublicApp) verifyPublicationAuthorization(ctx context.Context, current publicationSettings, in PublicationAuthorizationInput) (publicationSettings, error) {
	next := current
	expiry, err := time.Parse(time.RFC3339, in.ExpiresAt)
	if err != nil || !time.Now().Before(expiry) || in.Enabled == nil {
		return next, ErrInvalid
	}
	credentials := OpenCredentials{ClientID: in.ClientID, AppSecret: in.AppSecret, AccessToken: in.AccessToken}
	if !safeHeaderValue(credentials.ClientID, 256) || !safeHeaderValue(credentials.AppSecret, 4096) || !safeHeaderValue(credentials.AccessToken, 4096) {
		return next, ErrInvalid
	}
	if current.SchemaVersion == 1 && (in.ClientID != current.ClientID || in.AppSecret != current.AppSecret || in.AccessToken != current.AccessToken) {
		return next, ErrPublicationAuthorizationLegacy
	}
	if current.SchemaVersion == 2 && in.ClientID != current.ClientID {
		return next, ErrPublicationAuthorizationIdentity
	}
	verify := p.verifyPublicationAccount
	if verify == nil {
		verify = func(ctx context.Context, creds OpenCredentials) (string, error) {
			client, err := NewOpenUploadClient(creds)
			if err != nil {
				return "", err
			}
			return client.AccountOpenID(ctx)
		}
	}
	openID, err := verify(ctx, credentials)
	if err != nil || !safePublicationIdentity(openID) {
		return next, ErrPublicationAuthorizationVerify
	}
	if current.SchemaVersion == 2 && openID != current.VerifiedOpenID {
		return next, ErrPublicationAuthorizationIdentity
	}
	next.SchemaVersion, next.Enabled = 2, *in.Enabled
	next.ClientID, next.AppSecret, next.AccessToken = in.ClientID, in.AppSecret, in.AccessToken
	next.ExpiresAt, next.VerifiedOpenID = expiry.UTC().Format(time.RFC3339), openID
	next.RefreshToken, next.RefreshPending = "", false
	if current.SchemaVersion == 0 {
		next.AccountID = publicationReadIdentity(next)
	}
	if current.SchemaVersion == 1 {
		next.LegacyCredentials = publicationCredentialFingerprint(current)
	}
	next.VerifiedCredentials = verifiedPublicationProof(next)
	return next, nil
}

func publicationReadIdentity(settings publicationSettings) string {
	if settings.SchemaVersion != 2 {
		return ""
	}
	raw, _ := json.Marshal([]string{"bilibili-verified-account-v1", settings.ClientID, settings.VerifiedOpenID})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func verifiedPublicationProof(settings publicationSettings) string {
	raw, _ := json.Marshal([]string{publicationCredentialFingerprint(settings), publicationReadIdentity(settings)})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
