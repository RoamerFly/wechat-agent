package adapter

import (
	"context"
	"errors"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

func (p *stationNativeProvider) ProvisionConnection(ctx context.Context, requestID string) (apphost.Connection, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := p.verify(ctx); err != nil {
		return apphost.Connection{}, err
	}
	account, err := p.client.ForAccount("account-" + requestID)
	if err != nil {
		return apphost.Connection{}, apphost.ErrUnavailable
	}
	item, err := account.EnsureManagedAccount(ctx)
	if err != nil {
		return apphost.Connection{}, apphost.ErrUnavailable
	}
	if err := p.verify(ctx); err != nil {
		return apphost.Connection{}, err
	}
	return stationConnection(item), nil
}

func (p *stationNativeProvider) loginAccount(ctx context.Context, connectionID string, start bool) (*linuxnative.Sender, error) {
	items, err := p.accounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if ConnectionID(item.ID) == connectionID {
			if start && (item.Authenticated || item.Connection == "online" || item.Connection == "reconnecting") {
				return nil, apphost.ErrLifecycleConflict
			}
			return p.client.ForAccount(item.ID)
		}
	}
	return nil, apphost.ErrNotFound
}

func (p *stationNativeProvider) loginView(ctx context.Context, op linuxnative.LoginOperation, err error) (apphost.LoginChallenge, error) {
	switch {
	case errors.Is(err, linuxnative.ErrAccountLoginBusy):
		return apphost.LoginChallenge{}, apphost.ErrLifecycleConflict
	case errors.Is(err, linuxnative.ErrAccountLoginMissing):
		return apphost.LoginChallenge{}, apphost.ErrNotFound
	case err != nil:
		return apphost.LoginChallenge{}, apphost.ErrUnavailable
	}
	if err := p.verify(ctx); err != nil {
		return apphost.LoginChallenge{}, err
	}
	view := apphost.LoginChallenge{ID: op.ID, State: op.State}
	switch op.State {
	case "starting", "awaiting_scan", "awaiting_approval", "succeeded", "failed", "expired", "cancelled", "rejected", "needs_login", "challenge_required", "approval_required", "needs_review":
	default:
		view.State = "unknown"
	}
	if op.State == "awaiting_scan" && op.QRAvailable && op.QRGeneration >= 0 && op.QRGeneration <= 100 {
		view.QRAvailable = true
		view.QRGeneration = op.QRGeneration
	}
	return view, nil
}

func (p *stationNativeProvider) StartConnectionLogin(ctx context.Context, id, requestID string) (apphost.LoginChallenge, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	account, err := p.loginAccount(ctx, id, true)
	if err != nil {
		return apphost.LoginChallenge{}, err
	}
	op, err := account.StartAccountQR(ctx, "station-login-"+requestID)
	return p.loginView(ctx, op, err)
}

func (p *stationNativeProvider) ReadConnectionLogin(ctx context.Context, id, operationID string) (apphost.LoginChallenge, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	account, err := p.loginAccount(ctx, id, false)
	if err != nil {
		return apphost.LoginChallenge{}, err
	}
	op, err := account.ReadAccountLogin(ctx, operationID)
	return p.loginView(ctx, op, err)
}

func (p *stationNativeProvider) ReadConnectionQR(ctx context.Context, id, operationID string, generation int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	account, err := p.loginAccount(ctx, id, false)
	if err != nil {
		return nil, err
	}
	op, err := account.ReadAccountLogin(ctx, operationID)
	view, err := p.loginView(ctx, op, err)
	if err != nil {
		return nil, err
	}
	if !view.QRAvailable || view.QRGeneration != generation {
		return nil, apphost.ErrNotFound
	}
	data, err := account.LoginQR(ctx, operationID, generation)
	if err != nil {
		return nil, apphost.ErrUnavailable
	}
	if err := p.verify(ctx); err != nil {
		return nil, err
	}
	return data, nil
}
