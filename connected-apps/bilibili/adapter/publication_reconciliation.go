package adapter

import (
	"context"
	"encoding/json"

	"wechat-bot/internal/apphost"
)

func (p *PublicApp) ReadPublicationCandidates(ctx context.Context, ref apphost.PublicationReference, checkpoint json.RawMessage, query apphost.PublicationReconciliationQuery) (apphost.PublicationReconciliation, error) {
	state, err := loadUploadCheckpoint(checkpoint)
	if err != nil || state.Stage != "posting" || ref.RemoteID != "" {
		return apphost.PublicationReconciliation{}, apphost.ErrPublicationConflict
	}
	switch query.Action {
	case "candidates":
		if query.BVID != "" || query.Page < 0 || query.PageSize < 0 || query.PageSize > 50 {
			return apphost.PublicationReconciliation{}, apphost.ErrPublicationReconciliationInput
		}
		if query.Page == 0 {
			query.Page = 1
		}
		if query.PageSize == 0 {
			query.PageSize = 10
		}
	case "verify":
		if !safeArchiveResourceID(query.BVID) || query.Page != 0 || query.PageSize != 0 {
			return apphost.PublicationReconciliation{}, apphost.ErrPublicationReconciliationInput
		}
	default:
		return apphost.PublicationReconciliation{}, apphost.ErrPublicationReconciliationInput
	}
	settings, grant, err := p.publicationAccess(ref.Target)
	if err != nil || !grant.Allowed || settings.SchemaVersion != 2 || !state.permitsRead(settings) {
		return apphost.PublicationReconciliation{}, apphost.ErrPublicationDenied
	}
	client, err := p.newPublicationTransport(settings)
	if err != nil {
		return apphost.PublicationReconciliation{}, apphost.ErrPublicationReconciliationRead
	}
	var result apphost.PublicationReconciliation
	if query.Action == "verify" {
		status, err := client.View(ctx, query.BVID)
		if err != nil {
			return apphost.PublicationReconciliation{}, apphost.ErrPublicationReconciliationRead
		}
		result.Item = &apphost.PublicationCandidateObservation{BVID: query.BVID, Published: status.Published}
	} else {
		result, err = listPublicationCandidates(ctx, client, query.Page, query.PageSize)
		if err != nil {
			return apphost.PublicationReconciliation{}, err
		}
	}
	// Do not disclose newly observed account data after concurrent revocation,
	// expiry or rotation, even if the provider completed the read.
	_, latest, err := p.publicationAccess(ref.Target)
	if err != nil || !latest.Allowed || latest.Version != grant.Version {
		return apphost.PublicationReconciliation{}, apphost.ErrPublicationDenied
	}
	if err := ctx.Err(); err != nil {
		return apphost.PublicationReconciliation{}, err
	}
	return result, nil
}
