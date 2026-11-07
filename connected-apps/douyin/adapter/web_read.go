package adapter

import (
	"context"
	"strings"

	"wechat-bot/internal/apphost"
)

const (
	// WebReadConnectionID is the separately opt-in connection for the trusted
	// host's saved web login profile. It is not an OAuth or platform-account
	// connection.
	WebReadConnectionID = "web-page-text"
	// WebReadContentID is the fixed home-page text target offered by that
	// connection.
	WebReadContentID = "home"
	// WebVideoContentPrefix identifies an opaque video-page target minted by
	// the host from a link actually discovered on the owner's home page.
	WebVideoContentPrefix = "web-video-"
)

// IsWebReadContentID performs syntax validation only. In particular, a
// syntactically valid video ID has no authority by itself: the trusted host
// must still verify its origin, owner scope, and grant in WebReadConnection.
func IsWebReadContentID(contentID string) bool {
	if contentID == WebReadContentID {
		return true
	}
	if !strings.HasPrefix(contentID, WebVideoContentPrefix) {
		return false
	}
	encoded := contentID[len(WebVideoContentPrefix):]
	if len(encoded) < 1 || len(encoded) > 220 {
		return false
	}
	for i := 0; i < len(encoded); i++ {
		character := encoded[i]
		if !(character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

// WebReadConnection is supplied by the trusted station before startup. The
// station owns the browser, profile, grants, source checks, and pagination;
// this adapter only exposes its narrowly validated callback results.
type WebReadConnection interface {
	Connection(context.Context) (apphost.Connection, bool, error)
	Targets(context.Context) ([]apphost.ObservationTarget, error)
	Grant(context.Context, apphost.ObservationAccess) (apphost.ObservationGrant, error)
	Read(context.Context, apphost.ObservationRequest) (apphost.ObservationPage, error)
}

// SetWebReadConnection sets the trusted web-read callback. It is intended for
// startup wiring only and must not be changed while the app is in use.
func (p *SearchApp) SetWebReadConnection(web WebReadConnection) {
	if p != nil {
		p.webRead = web
	}
}

// WithWebReadConnection returns an independent startup instance with web
// reading wired to web. It deliberately does not copy mutexes or pending
// authorization state, so separately constructed station homes cannot replace
// one another's host-owned browser callback through a shared SearchApp.
func (p *SearchApp) WithWebReadConnection(web WebReadConnection) *SearchApp {
	if p == nil {
		return nil
	}
	return &SearchApp{
		privateDirectory: p.privateDirectory,
		client:           p.client,
		now:              p.now,
		samplePlayer:     p.samplePlayer,
		webRead:          web,
	}
}

func (p *SearchApp) webReadConnection(ctx context.Context) (apphost.Connection, bool, error) {
	if p == nil || p.webRead == nil {
		return apphost.Connection{}, false, nil
	}
	connection, present, err := p.webRead.Connection(ctx)
	if err != nil || !present {
		return apphost.Connection{}, present, err
	}
	if connection.ID != WebReadConnectionID {
		return apphost.Connection{}, false, apphost.ErrNotFound
	}
	return connection, true, nil
}

func (p *SearchApp) webReadRef(ctx context.Context) (apphost.ConnectionRef, bool, error) {
	connection, present, err := p.webReadConnection(ctx)
	if err != nil || !present {
		return apphost.ConnectionRef{}, present, err
	}
	return apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: WebReadConnectionID, AccountID: connection.AccountID}, true, nil
}

func (p *SearchApp) validWebReadAccess(ref apphost.ConnectionRef, contentID string, field apphost.ObservationField) bool {
	if ref.AppID != ID || ref.InstallationID != installationID || ref.ConnectionID != WebReadConnectionID || !IsWebReadContentID(contentID) {
		return false
	}
	// The homepage is text-only. Playback, audio and visual analysis each
	// require a host-minted video target and their own current grant checked by
	// the trusted callback.
	return field == apphost.ObservationPageText || contentID != WebReadContentID && (field == apphost.ObservationPlayback || field == apphost.ObservationAudio || field == apphost.ObservationVisual || field == apphost.ObservationComments)
}

func (p *SearchApp) webReadTargets(ctx context.Context) ([]apphost.ObservationTarget, error) {
	ref, present, err := p.webReadRef(ctx)
	if err != nil || !present {
		if err != nil {
			return nil, err
		}
		return nil, apphost.ErrNotFound
	}
	targets, err := p.webRead.Targets(ctx)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		if target.ConnectionRef != ref || target.ContentID != WebReadContentID || len(target.Fields) != 1 || target.Fields[0] != apphost.ObservationPageText {
			return nil, apphost.ErrObservationForbidden
		}
	}
	return targets, nil
}

func (p *SearchApp) webReadAccess(ctx context.Context, access apphost.ObservationAccess) (bool, error) {
	ref, present, err := p.webReadRef(ctx)
	if err != nil || !present {
		return false, err
	}
	return p.validWebReadAccess(access.ConnectionRef, access.ContentID, access.Field) && access.ConnectionRef == ref, nil
}
