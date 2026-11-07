package adapter

import (
	"context"

	"wechat-bot/internal/apphost"
)

// The adapter only binds an application capability. The trusted host owns the
// login profile, current grants, source versions and actual browser operation.
func (p *WebApp) WithWebInteraction(bridge apphost.InteractionProvider) *WebApp {
	p.interaction = bridge
	return p
}

func (p *WebApp) ResolveInteractionSubject(ctx context.Context, draft apphost.InteractionDraft) (string, error) {
	provider, ok := p.interaction.(apphost.InteractionSubjectProvider)
	if !ok {
		return "", apphost.ErrInteractionUnavailable
	}
	if draft.Target.AppID != ID || draft.Target.InstallationID != installationID || draft.Target.ConnectionID != WebReadConnectionID {
		return "", apphost.ErrInteractionDenied
	}
	return provider.ResolveInteractionSubject(ctx, draft)
}

func (p *WebApp) GrantInteraction(ctx context.Context, draft apphost.InteractionDraft) (apphost.PublicationGrant, error) {
	if p.interaction == nil {
		return apphost.PublicationGrant{}, apphost.ErrInteractionUnavailable
	}
	if draft.Target.AppID != ID || draft.Target.InstallationID != installationID || draft.Target.ConnectionID != WebReadConnectionID {
		return apphost.PublicationGrant{}, apphost.ErrInteractionDenied
	}
	return p.interaction.GrantInteraction(ctx, draft)
}

func (p *WebApp) PerformInteraction(ctx context.Context, in apphost.InteractionRequest) (apphost.InteractionReceipt, error) {
	if p.interaction == nil {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionUnavailable
	}
	if in.Reference.Target != in.Draft.Target || in.Draft.Target.AppID != ID || in.Draft.Target.InstallationID != installationID || in.Draft.Target.ConnectionID != WebReadConnectionID {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionDenied
	}
	return p.interaction.PerformInteraction(ctx, in)
}

func (p *WebApp) QueryInteraction(ctx context.Context, in apphost.InteractionQuery) (apphost.InteractionReceipt, error) {
	if p.interaction == nil {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionUnavailable
	}
	if in.Reference.Target != in.Draft.Target || in.Draft.Target.AppID != ID || in.Draft.Target.InstallationID != installationID || in.Draft.Target.ConnectionID != WebReadConnectionID {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionDenied
	}
	return p.interaction.QueryInteraction(ctx, in)
}
