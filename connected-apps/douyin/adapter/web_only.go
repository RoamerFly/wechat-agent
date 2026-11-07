package adapter

import (
	"context"

	"wechat-bot/internal/apphost"
)

// WebApp exposes the ordinary signed-in website connection. Its public method
// set deliberately contains no developer authorization or publication API.
type WebApp struct {
	reader      *SearchApp
	publication WebPublicationBridge
	interaction apphost.InteractionProvider
}

func NewWebOnly(web WebReadConnection) *WebApp {
	return &WebApp{reader: &SearchApp{webRead: web}}
}

// WithWebPublication attaches the host-owned ordinary-web creator bridge.
// The bridge receives a verified artifact stream through apphost, rather than
// a caller-provided file path.  Keeping it optional preserves a read-only
// ordinary web connection when the station has not enabled creator tools.
func (p *WebApp) WithWebPublication(bridge WebPublicationBridge) *WebApp {
	p.publication = bridge
	return p
}

func (p *WebApp) Registration() apphost.Registration {
	return apphost.Registration{Application: apphost.Application{
		ID: ID, Name: "抖音", InstallationID: installationID, Registration: "ordinary-web-v1",
	}, Provider: p}
}

func (p *WebApp) Connections(ctx context.Context) ([]apphost.Connection, error) {
	connection, present, err := p.reader.webReadConnection(ctx)
	if err != nil {
		return nil, err
	}
	if !present {
		return []apphost.Connection{}, nil
	}
	connection.Primary = true
	return []apphost.Connection{connection}, nil
}

func (p *WebApp) Inspect(ctx context.Context, id string) (apphost.ConnectionStatus, error) {
	if id != WebReadConnectionID {
		return apphost.ConnectionStatus{}, apphost.ErrNotFound
	}
	return p.reader.Inspect(ctx, id)
}

func (p *WebApp) ObservationTargets(ctx context.Context, id string) ([]apphost.ObservationTarget, error) {
	if id != WebReadConnectionID {
		return nil, apphost.ErrNotFound
	}
	return p.reader.webReadTargets(ctx)
}

func (p *WebApp) ObservationGrant(ctx context.Context, in apphost.ObservationAccess) (apphost.ObservationGrant, error) {
	if in.ConnectionID != WebReadConnectionID {
		return apphost.ObservationGrant{Allowed: false}, nil
	}
	return p.reader.ObservationGrant(ctx, in)
}

func (p *WebApp) ReadObservation(ctx context.Context, in apphost.ObservationRequest) (apphost.ObservationPage, error) {
	if in.ConnectionID != WebReadConnectionID {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	return p.reader.ReadObservation(ctx, in)
}
