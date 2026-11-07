package adapter

// Owner-configured publication admission.  This deliberately only reads a
// private, local OAuth record: it neither starts OAuth nor contacts Bilibili.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/workspacefs"
)

const (
	publicationConnectionID     = "publish"
	publicationSettingsName     = "publication-settings.json"
	DefaultPublicationPartBytes = 10 << 20
	minPublicationPartBytes     = 1 << 20
	maxPublicationPartBytes     = 64 << 20
)

var errPublicationUnconfigured = errors.New("B站发布授权尚未配置")

// publicationSettings is the complete owner-managed JSON object in the
// application's private directory. account_id is an opaque stable local
// identity, never exposed directly; client_id, app_secret and access_token
// are only consumed by same-package publication transport code.
//
// part_bytes defaults to DefaultPublicationPartBytes when omitted or zero.
// Nonzero values are bounded explicitly rather than silently clamped.
type publicationSettings struct {
	SchemaVersion       int    `json:"schema_version"`
	Enabled             bool   `json:"enabled"`
	ClientID            string `json:"client_id"`
	AppSecret           string `json:"app_secret"`
	AccessToken         string `json:"access_token"`
	AccountID           string `json:"account_id"`
	ExpiresAt           string `json:"expires_at"`
	PartBytes           int    `json:"part_bytes,omitempty"`
	VerifiedOpenID      string `json:"verified_openid,omitempty"`
	VerifiedCredentials string `json:"verified_credentials,omitempty"`
	LegacyCredentials   string `json:"legacy_credentials,omitempty"`
	PolicyNonce         string `json:"policy_nonce,omitempty"`
	RefreshToken        string `json:"refresh_token,omitempty"`
	RefreshPending      bool   `json:"refresh_pending,omitempty"`
}

type loadedPublicationSettings struct {
	publicationSettings
	expiresAt time.Time
	version   string
}

// PublicationGrant admits a new host submission only when its exact,
// installation-scoped reference still names an enabled, unexpired owner
// credential. Its version changes on every credential/account/expiry edit.
func (p *PublicApp) PublicationGrant(ctx context.Context, ref apphost.ConnectionRef) (apphost.PublicationGrant, error) {
	if err := ctx.Err(); err != nil {
		return apphost.PublicationGrant{}, err
	}
	_, grant, err := p.publicationAccess(ref)
	if err != nil {
		if errors.Is(err, ErrOpenUploadInvalid) {
			return apphost.PublicationGrant{}, err
		}
		return apphost.PublicationGrant{}, apphost.ErrPublicationUnavailable
	}
	return grant, nil
}

// publicationAccess returns the locally configured credentials together with
// the current admission decision. It intentionally returns settings for an
// otherwise exact disabled or expired reference so Query/Cancel can still use
// valid credentials to reconcile a submission made before revocation.
func (p *PublicApp) publicationAccess(ref apphost.ConnectionRef) (publicationSettings, apphost.PublicationGrant, error) {
	loaded, err := p.loadPublicationSettings()
	if err != nil {
		return publicationSettings{}, apphost.PublicationGrant{}, err
	}
	settings := loaded.publicationSettings
	if !publicationRefMatches(ref, settings) {
		return settings, apphost.PublicationGrant{Allowed: false}, nil
	}
	if !settings.Enabled || settings.RefreshPending || !time.Now().UTC().Before(loaded.expiresAt) {
		return settings, apphost.PublicationGrant{Allowed: false, Version: loaded.version}, nil
	}
	return settings, apphost.PublicationGrant{Allowed: true, Version: loaded.version}, nil
}

func (p *PublicApp) publicationConnection() (apphost.Connection, bool) {
	loaded, err := p.loadPublicationSettings()
	if errors.Is(err, errPublicationUnconfigured) {
		return apphost.Connection{}, false
	}
	connection := apphost.Connection{ID: publicationConnectionID, Label: "B站创作发布", Configuration: "invalid"}
	if err != nil {
		return connection, true
	}
	connection.AccountID = publicationAccountID(loaded.AccountID)
	switch {
	case !loaded.Enabled:
		connection.Configuration = "disabled"
	case loaded.RefreshPending:
		connection.Configuration = "refresh-pending"
	case !time.Now().UTC().Before(loaded.expiresAt):
		connection.Configuration = "expired"
	default:
		connection.Configuration = "ready"
	}
	return connection, true
}

func (p *PublicApp) inspectPublication(ctx context.Context) (apphost.ConnectionStatus, error) {
	if err := ctx.Err(); err != nil {
		return apphost.ConnectionStatus{}, err
	}
	connection, configured := p.publicationConnection()
	if !configured {
		return apphost.ConnectionStatus{}, apphost.ErrNotFound
	}
	status := apphost.ConnectionStatus{Connection: connection, Login: "owner-configured OAuth", Runtime: "local", Worker: "disabled", CapabilityCheck: "publication_credentials_invalid", SupportedActions: []string{}}
	if connection.Configuration == "ready" {
		status.Worker = "ready"
		status.CapabilityCheck = "ready"
		status.SupportedActions = []string{"content.publish"}
	} else if connection.Configuration == "disabled" {
		status.CapabilityCheck = "publication_disabled"
	} else if connection.Configuration == "expired" {
		status.CapabilityCheck = "publication_credentials_expired"
	} else if connection.Configuration == "refresh-pending" {
		status.CapabilityCheck = "publication_refresh_unconfirmed"
	}
	return status, nil
}

func publicationRefMatches(ref apphost.ConnectionRef, settings publicationSettings) bool {
	return ref.AppID == ID && ref.InstallationID == installationID && ref.ConnectionID == publicationConnectionID && ref.AccountID == publicationAccountID(settings.AccountID)
}

func publicationAccountID(identity string) string {
	digest := sha256.Sum256([]byte("bilibili-publication-account-v1\x00" + identity))
	return "authorized-" + hex.EncodeToString(digest[:16])
}

func (p *PublicApp) loadPublicationSettings() (loadedPublicationSettings, error) {
	if p == nil || strings.TrimSpace(p.privateDirectory) == "" {
		return loadedPublicationSettings{}, errPublicationUnconfigured
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadPublicationSettingsLocked()
}

func (p *PublicApp) loadPublicationSettingsLocked() (loadedPublicationSettings, error) {
	root, err := workspacefs.OpenPath(p.privateDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return loadedPublicationSettings{}, errPublicationUnconfigured
		}
		return loadedPublicationSettings{}, ErrInvalid
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, publicationSettingsName)
	if errors.Is(err, os.ErrNotExist) {
		return loadedPublicationSettings{}, errPublicationUnconfigured
	}
	if err != nil {
		return loadedPublicationSettings{}, ErrInvalid
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(raw) > 64*1024 || !validPublicationSettingsShape(raw) {
		return loadedPublicationSettings{}, ErrInvalid
	}
	var settings publicationSettings
	if decodePublicationJSON(raw, &settings) != nil {
		return loadedPublicationSettings{}, ErrInvalid
	}
	expiresAt, err := validatePublicationSettings(&settings)
	if err != nil {
		// Resource bounds are a caller-actionable publication configuration
		// error. Preserve that stable error instead of disguising it as a
		// malformed public-observation policy.
		if errors.Is(err, ErrOpenUploadInvalid) {
			return loadedPublicationSettings{}, err
		}
		return loadedPublicationSettings{}, ErrInvalid
	}
	digest := sha256.Sum256(raw)
	return loadedPublicationSettings{publicationSettings: settings, expiresAt: expiresAt, version: "bilibili-publication-" + hex.EncodeToString(digest[:])}, nil
}

func validPublicationSettingsShape(raw []byte) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return false
	}
	for _, key := range []string{"schema_version", "enabled", "client_id", "app_secret", "access_token", "account_id", "expires_at"} {
		if value, ok := object[key]; !ok || string(value) == "null" {
			return false
		}
	}
	for _, key := range []string{"part_bytes", "verified_openid", "verified_credentials", "legacy_credentials", "policy_nonce", "refresh_token", "refresh_pending"} {
		if value, ok := object[key]; ok && string(value) == "null" {
			return false
		}
	}
	return true
}

func validatePublicationSettings(settings *publicationSettings) (time.Time, error) {
	if !safeHeaderValue(settings.ClientID, 256) || !safeHeaderValue(settings.AppSecret, 4096) || !safeHeaderValue(settings.AccessToken, 4096) || !safePublicationIdentity(settings.AccountID) {
		return time.Time{}, ErrInvalid
	}
	switch settings.SchemaVersion {
	case 1:
		if settings.VerifiedOpenID != "" || settings.VerifiedCredentials != "" || settings.LegacyCredentials != "" || settings.PolicyNonce != "" ||
			settings.RefreshToken != "" || settings.RefreshPending {
			return time.Time{}, ErrInvalid
		}
	case 2:
		if !safePublicationIdentity(settings.VerifiedOpenID) || settings.VerifiedCredentials != verifiedPublicationProof(*settings) ||
			!isPublicationDigest(settings.PolicyNonce, 16) || (settings.LegacyCredentials != "" && !isPublicationDigest(settings.LegacyCredentials, 32)) {
			return time.Time{}, ErrInvalid
		}
		if settings.RefreshToken != "" && (settings.RefreshPending || !safeHeaderValue(settings.RefreshToken, 4096)) {
			return time.Time{}, ErrInvalid
		}
	default:
		return time.Time{}, ErrInvalid
	}
	if settings.PartBytes == 0 {
		settings.PartBytes = DefaultPublicationPartBytes
	}
	if settings.PartBytes < minPublicationPartBytes || settings.PartBytes > maxPublicationPartBytes {
		return time.Time{}, ErrOpenUploadInvalid
	}
	expiresAt, err := time.Parse(time.RFC3339, settings.ExpiresAt)
	if err != nil || expiresAt.IsZero() {
		return time.Time{}, ErrInvalid
	}
	return expiresAt.UTC(), nil
}

func isPublicationDigest(value string, size int) bool {
	if len(value) != size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func safePublicationIdentity(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
