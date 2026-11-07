package station

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/pluginhost"
)

// The origin is issued from the local chat event. It is not accepted in tool
// arguments or HTTP payloads, and survives a model or station restart.
type localPublicationOrigin struct {
	Request      string                        `json:"request_id"`
	Source       *companion.SourceRef          `json:"source"`
	Observations []localPublicationObservation `json:"observations,omitempty"`
}

type localPublicationObservation struct {
	apphost.ConnectionRef
	ContentID    string                   `json:"content_id"`
	Field        apphost.ObservationField `json:"field"`
	GrantVersion string                   `json:"grant_version"`
}

func localPublicationArtifact(a localArtifact) apphost.PublicationArtifact {
	return apphost.PublicationArtifact{ID: a.ID, SHA256: a.SHA256, Name: a.Name, Bytes: a.Bytes}
}

func (h *localChatService) validatePublication(ctx context.Context, job pluginhost.Job, draft apphost.PublicationDraft) error {
	access := &localWorkspaceAccess{home: h.home, fishID: job.Context.Account, ownerID: job.Context.Requester}
	grant, err := access.resolve()
	if err != nil || job.Context.Thread != job.Context.Requester || localCheckpointKey(grant) != job.Context.Authorization {
		return errors.New("原作品的家目录授权已变化")
	}
	var origin localPublicationOrigin
	if json.Unmarshal(draft.Origin, &origin) != nil || origin.Source == nil || origin.Request == "" ||
		origin.Source.Account != grant.FishID || origin.Source.Thread != grant.OwnerID || origin.Source.Sender != grant.OwnerID ||
		job.Context.SourceVersions[origin.Source.EventID] != origin.Source.Revision ||
		!h.home.artifacts.sourceValid(localArtifact{Fish: grant.FishID, Owner: grant.OwnerID, Request: origin.Request, Source: origin.Source}) {
		return errors.New("原作品请求的来源已变化")
	}
	for _, observation := range origin.Observations {
		if h.observations == nil || h.observations.Check(ctx, apphost.ObservationResult{
			ConnectionRef: observation.ConnectionRef, ContentID: observation.ContentID, Field: observation.Field, GrantVersion: observation.GrantVersion,
		}) != nil {
			return errors.New("原作品引用的应用资料已撤权或变更")
		}
	}
	refs := append([]apphost.PublicationArtifact{}, draft.Artifacts...)
	if draft.Project != nil {
		refs = append(refs, *draft.Project)
	}
	for _, ref := range refs {
		// Before the claim, verify the immutable bytes. After claiming, the
		// source fence checks records only. Each transfer verifies its own
		// bytes again, so an N-file upload does not rehash all N files per open.
		if job.State == "submitting" {
			if _, _, _, err := h.publicationArtifactAccess(ref); err != nil {
				return err
			}
			continue
		}
		file, err := h.openPublicationArtifact(ctx, ref)
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return errors.New("成品读取未完成")
		}
	}
	if !access.current(grant) {
		return errors.New("家目录授权已变化")
	}
	return ctx.Err()
}

func (h *localChatService) publicationArtifactAccess(ref apphost.PublicationArtifact) (localArtifact, *localWorkspaceAccess, localWorkspaceGrant, error) {
	store := h.home.artifacts
	artifact, err := store.readRecord(ref.ID)
	if err != nil || localPublicationArtifact(artifact) != ref {
		return localArtifact{}, nil, localWorkspaceGrant{}, errors.New("成品来源或内容已变化")
	}
	access, ok := store.access(artifact)
	if !ok {
		return localArtifact{}, nil, localWorkspaceGrant{}, errors.New("成品不属于当前本机身份或来源已变化")
	}
	grant, err := access.resolve()
	return artifact, access, grant, err
}

func (h *localChatService) openPublicationArtifact(ctx context.Context, ref apphost.PublicationArtifact) (io.ReadCloser, error) {
	artifact, access, grant, err := h.publicationArtifactAccess(ref)
	if err != nil {
		return nil, err
	}
	store := h.home.artifacts
	if !store.checkArtifactLineage(artifact, ctx, false) {
		return nil, errors.New("成品依赖的素材来源或内容已变化")
	}
	file, err := store.open(ctx, artifact, func() bool { return access.current(grant) })
	if err != nil {
		return nil, err
	}
	if !store.artifactLineageValid(artifact) {
		file.Close()
		return nil, errors.New("成品来源在校验期间已变化")
	}
	return file, nil
}
