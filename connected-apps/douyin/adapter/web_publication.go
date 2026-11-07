package adapter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"wechat-bot/internal/apphost"
)

// WebPublicationBridge is deliberately implemented by the local station, not
// by this application package.  The station owns the confirmed browser
// profile, temporary staging and creator-center automation.  Consequently an
// adapter never receives an arbitrary local path or profile credential.
type WebPublicationBridge interface {
	PublicationGrant(context.Context, apphost.ConnectionRef) (apphost.PublicationGrant, error)
	ContinuePublication(context.Context, apphost.PublicationRequest) (apphost.PublicationReceipt, error)
	ReconcilePublication(context.Context, apphost.PublicationReference, json.RawMessage, bool) (apphost.PublicationReceipt, error)
}

var _ apphost.CheckpointedPublicationProvider = (*WebApp)(nil)

func (p *WebApp) RecoverPublication(ctx context.Context, in apphost.PublicationRecoveryRequest) (apphost.PublicationReceipt, error) {
	provider, ok := p.publication.(apphost.PublicationRecoveryProvider)
	if !ok {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationUnavailable
	}
	return provider.RecoverPublication(ctx, in)
}

func (p *WebApp) PublicationCheckpointProgress(raw json.RawMessage) (*apphost.PublicationProgress, error) {
	progress, ok := p.publication.(apphost.PublicationCheckpointProgressProvider)
	if !ok {
		return nil, apphost.ErrPublicationUnavailable
	}
	return progress.PublicationCheckpointProgress(raw)
}

// ValidatePublicationDraft validates only the ordinary creator-center
// contract.  Account/profile authorization is checked separately by the
// station immediately before staging and immediately before native submit.
func (p *WebApp) ValidatePublicationDraft(draft apphost.PublicationDraft) error {
	if draft.Target.AppID != ID || draft.Target.InstallationID != installationID ||
		draft.Target.ConnectionID != WebReadConnectionID || len(draft.Artifacts) != 1 {
		return apphost.ErrPublicationInvalidDraft
	}
	artifact := draft.Artifacts[0]
	extension := strings.ToLower(filepath.Ext(artifact.Name))
	if !safePublicationFileName(artifact.Name) || artifact.Bytes < 12 || artifact.Bytes > douyinMaxVideoBytes ||
		(extension != ".mp4" && extension != ".webm") {
		return apphost.ErrPublicationInvalidDraft
	}
	var options struct {
		Visibility string `json:"visibility"`
	}
	if decodeDouyinPublication(draft.Options, &options) != nil ||
		(options.Visibility != "public" && options.Visibility != "private" && options.Visibility != "friends") {
		return apphost.ErrPublicationInvalidDraft
	}
	text := strings.TrimSpace(draft.Title)
	if body := strings.TrimSpace(draft.Body); body != "" {
		if text != "" {
			text += "\n"
		}
		text += body
	}
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 1000 || strings.ContainsRune(text, 0) {
		return apphost.ErrPublicationInvalidDraft
	}
	return nil
}

func (p *WebApp) PublicationGrant(ctx context.Context, target apphost.ConnectionRef) (apphost.PublicationGrant, error) {
	if p.publication == nil {
		return apphost.PublicationGrant{Allowed: false}, apphost.ErrPublicationDenied
	}
	if target.AppID != ID || target.InstallationID != installationID || target.ConnectionID != WebReadConnectionID {
		return apphost.PublicationGrant{Allowed: false}, apphost.ErrPublicationDenied
	}
	return p.publication.PublicationGrant(ctx, target)
}

func (p *WebApp) ContinuePublication(ctx context.Context, request apphost.PublicationRequest) (apphost.PublicationReceipt, error) {
	if p.publication == nil {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationUnavailable
	}
	if err := p.ValidatePublicationDraft(request.Draft); err != nil {
		return apphost.PublicationReceipt{}, err
	}
	return p.publication.ContinuePublication(ctx, request)
}

func (p *WebApp) ReconcilePublication(ctx context.Context, reference apphost.PublicationReference, checkpoint json.RawMessage, cancel bool) (apphost.PublicationReceipt, error) {
	if p.publication == nil {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationUnavailable
	}
	if reference.Target.AppID != ID || reference.Target.InstallationID != installationID || reference.Target.ConnectionID != WebReadConnectionID {
		return apphost.PublicationReceipt{}, apphost.ErrPublicationDenied
	}
	return p.publication.ReconcilePublication(ctx, reference, checkpoint, cancel)
}
