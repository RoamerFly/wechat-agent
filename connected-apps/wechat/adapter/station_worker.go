package adapter

import (
	"context"

	"wechat-bot/internal/linuxnative"
)

// WorkerSender grants the station host an account-scoped native transport only
// after checking the current roster. Every subsequent request rechecks ownership.
func (p *stationNativeProvider) WorkerSender(ctx context.Context, connectionID string) (*linuxnative.Sender, error) {
	sender, err := p.loginAccount(ctx, connectionID, false)
	if err != nil {
		return nil, err
	}
	sender.GuardRequests(p.verify)
	return sender, nil
}
