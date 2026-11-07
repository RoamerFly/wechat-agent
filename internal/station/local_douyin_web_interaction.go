package station

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

type localDouyinWebInteraction struct {
	home         *localHomeService
	installation string
}
type localDouyinInteractionCheckpoint struct {
	Version   int    `json:"version"`
	TaskID    string `json:"task_id"`
	Manifest  string `json:"manifest"`
	VideoID   string `json:"video_id"`
	Submitted bool   `json:"submitted"`
	RemoteID  string `json:"remote_id,omitempty"`
}

func (p *localDouyinWebInteraction) access(ctx context.Context) (localDouyinWebAccess, error) {
	return (&localDouyinWebPublication{home: p.home, installation: p.installation}).access(ctx)
}
func (p *localDouyinWebInteraction) ref(a localDouyinWebAccess) apphost.ConnectionRef {
	return apphost.ConnectionRef{AppID: douyinapp.ID, InstallationID: p.installation, ConnectionID: a.connection.ID, AccountID: a.connection.AccountID}
}

func (p *localDouyinWebInteraction) ResolveInteractionSubject(ctx context.Context, d apphost.InteractionDraft) (string, error) {
	a, err := p.access(ctx)
	if err != nil || p.ref(a) != d.Target || d.Kind != "reply_comment" || !a.record.RepliesEnabled || !a.record.CommentsEnabled {
		return "", apphost.ErrInteractionDenied
	}
	r := &localDouyinWebObservation{home: p.home, installation: p.installation}
	target, ok := r.resolveTarget(a, d.ContentID)
	if !ok || target.videoID == "" {
		return "", apphost.ErrInteractionDenied
	}
	proof, err := r.commentProof(a, target.videoID, d.CommentID)
	if err != nil {
		return "", apphost.ErrInteractionDenied
	}
	return proof.SHA256, nil
}

func (p *localDouyinWebInteraction) GrantInteraction(ctx context.Context, d apphost.InteractionDraft) (apphost.PublicationGrant, error) {
	a, err := p.access(ctx)
	if err != nil {
		return apphost.PublicationGrant{}, apphost.ErrInteractionDenied
	}
	allowed := d.Kind == "like" && a.record.LikesEnabled || d.Kind == "reply_comment" && a.record.RepliesEnabled && a.record.CommentsEnabled
	available := (&localDouyinWebPublication{home: p.home, installation: p.installation}).profileAvailable(a.record)
	if d.Target != p.ref(a) || !allowed || !available || !a.record.ReadEnabled || localDouyinWebScope(a.record) != "home_and_videos" {
		return apphost.PublicationGrant{Allowed: false}, nil
	}
	if d.ContentID != "" {
		reader := &localDouyinWebObservation{home: p.home, installation: p.installation}
		target, ok := reader.resolveTarget(a, d.ContentID)
		if !ok || target.videoID == "" {
			return apphost.PublicationGrant{Allowed: false}, nil
		}
		if d.Kind == "reply_comment" {
			proof, err := reader.commentProof(a, target.videoID, d.CommentID)
			if err != nil || proof.SHA256 != d.CommentVersion {
				return apphost.PublicationGrant{Allowed: false}, nil
			}
		}
	}
	return apphost.PublicationGrant{Allowed: true, Version: a.version}, nil
}

func (p *localDouyinWebInteraction) checkpoint(raw json.RawMessage, ref apphost.InteractionReference) (localDouyinInteractionCheckpoint, error) {
	if len(raw) == 0 {
		return localDouyinInteractionCheckpoint{Version: 1, TaskID: ref.TaskID, Manifest: ref.ManifestSHA256}, nil
	}
	var c localDouyinInteractionCheckpoint
	if strictLocalMemoryDecode(raw, &c) != nil || c.Version != 1 || c.TaskID != ref.TaskID || c.Manifest != ref.ManifestSHA256 ||
		(c.VideoID != "" && !validLocalDouyinCommentID(c.VideoID)) || strings.ContainsAny(c.RemoteID, "\x00\r\n") || c.Submitted && c.VideoID == "" {
		return c, apphost.ErrInteractionConflict
	}
	if ref.RemoteID != "" {
		if c.RemoteID != "" && c.RemoteID != ref.RemoteID {
			return c, apphost.ErrInteractionReceipt
		}
		c.RemoteID = ref.RemoteID
	}
	return c, nil
}

func (p *localDouyinWebInteraction) PerformInteraction(ctx context.Context, in apphost.InteractionRequest) (apphost.InteractionReceipt, error) {
	if in.CheckActive == nil || in.SaveCheckpoint == nil || in.Reference.Target != in.Draft.Target {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionInvalid
	}
	c, err := p.checkpoint(in.Checkpoint, in.Reference)
	if err != nil {
		return apphost.InteractionReceipt{}, err
	}
	if c.Submitted {
		return p.QueryInteraction(ctx, apphost.InteractionQuery{Reference: in.Reference, Draft: in.Draft, Checkpoint: in.Checkpoint})
	}
	if err := in.CheckActive(ctx); err != nil {
		return apphost.InteractionReceipt{}, err
	}
	a, err := p.access(ctx)
	if err != nil || p.ref(a) != in.Reference.Target {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionDenied
	}
	r := &localDouyinWebObservation{home: p.home, installation: p.installation}
	target, ok := r.resolveTarget(a, in.Draft.ContentID)
	if !ok || target.videoID == "" {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionDenied
	}
	if in.Draft.Kind == "reply_comment" {
		proof, err := r.commentProof(a, target.videoID, in.Draft.CommentID)
		if err != nil || proof.SHA256 != in.Draft.CommentVersion {
			return apphost.InteractionReceipt{}, apphost.ErrInteractionDenied
		}
	}
	c.VideoID = target.videoID
	if err := in.SaveCheckpoint(ctx, mustLocalDouyinJSON(c)); err != nil {
		return apphost.InteractionReceipt{}, err
	}
	request := stationbrowser.DouyinInteractionRequest{VideoID: target.videoID, Kind: in.Draft.Kind, CommentID: in.Draft.CommentID, Text: in.Draft.Text, Desired: in.Draft.Desired, ExpectedCommentSHA256: in.Draft.CommentVersion,
		BeforeSubmit: func(callCtx context.Context) error {
			if err := in.CheckActive(callCtx); err != nil {
				return err
			}
			c.Submitted = true
			return in.SaveCheckpoint(callCtx, mustLocalDouyinJSON(c))
		}}
	result, err := p.run(ctx, a, request, func() error { return in.CheckActive(ctx) })
	result.Code = localInteractionBrowserCode(result.Code, err)
	// Preserve late verified identity independently of a cancelled HTTP request.
	if result.RemoteID != "" {
		c.RemoteID = result.RemoteID
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if e := in.SaveCheckpoint(saveCtx, mustLocalDouyinJSON(c)); e != nil {
			return apphost.InteractionReceipt{}, e
		}
	}
	definitiveFailure := result.State == "failed" && (errors.Is(err, stationbrowser.ErrDouyinInteractionInput) || errors.Is(err, stationbrowser.ErrDouyinInteractionRejected))
	if err != nil && result.State != "applied" && !definitiveFailure {
		return apphost.InteractionReceipt{Reference: in.Reference, State: "unknown", Code: localInteractionCode(result.Code)}, nil
	}
	return localDouyinInteractionReceipt(in.Reference, in.Draft.Kind, target.videoID, result)
}

func (p *localDouyinWebInteraction) QueryInteraction(ctx context.Context, in apphost.InteractionQuery) (apphost.InteractionReceipt, error) {
	if in.Reference.Target != in.Draft.Target {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionInvalid
	}
	c, err := p.checkpoint(in.Checkpoint, in.Reference)
	if err != nil {
		return apphost.InteractionReceipt{}, err
	}
	if !c.Submitted && c.RemoteID == "" {
		state := "unknown"
		if in.Cancel {
			state = "cancelled"
		}
		return apphost.InteractionReceipt{Reference: in.Reference, State: state, Code: "not_submitted"}, nil
	}
	a, err := p.access(ctx)
	if err != nil || p.ref(a) != in.Reference.Target {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionDenied
	}
	authorize := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := p.access(ctx)
		if err != nil || p.ref(current) != in.Reference.Target || current.profile != a.profile || !(&localDouyinWebPublication{home: p.home, installation: p.installation}).profileAvailable(current.record) {
			return apphost.ErrInteractionDenied
		}
		return nil
	}
	if err := authorize(); err != nil {
		return apphost.InteractionReceipt{}, err
	}
	request := stationbrowser.DouyinInteractionRequest{VideoID: c.VideoID, Kind: in.Draft.Kind, CommentID: in.Draft.CommentID, Text: in.Draft.Text, Desired: in.Draft.Desired, Submitted: true, RemoteID: c.RemoteID}
	result, err := p.run(ctx, a, request, authorize)
	result.Code = localInteractionBrowserCode(result.Code, err)
	if err != nil && result.State != "applied" {
		return apphost.InteractionReceipt{Reference: in.Reference, State: "unknown", Code: localInteractionCode(result.Code)}, nil
	}
	return localDouyinInteractionReceipt(in.Reference, in.Draft.Kind, c.VideoID, result)
}

func localInteractionBrowserCode(code string, err error) string {
	if code != "" || err == nil {
		return localInteractionCode(code)
	}
	switch {
	case errors.Is(err, stationbrowser.ErrDouyinWebLoginRequired):
		return "login_required"
	case errors.Is(err, stationbrowser.ErrDouyinLoginActive):
		return "profile_busy"
	case errors.Is(err, context.DeadlineExceeded):
		return "operation_timeout"
	case errors.Is(err, context.Canceled):
		return "operation_cancelled"
	case errors.Is(err, stationbrowser.ErrDouyinInteractionInput):
		return "invalid_request"
	case errors.Is(err, stationbrowser.ErrDouyinInteractionUnavailable):
		return "controls_unavailable"
	default:
		return "operation_unavailable"
	}
}

func (p *localDouyinWebInteraction) run(ctx context.Context, a localDouyinWebAccess, in stationbrowser.DouyinInteractionRequest, authorize func() error) (stationbrowser.DouyinInteractionResult, error) {
	m := p.home.douyinWeb
	m.mu.Lock()
	run, serviceCtx := m.interact, m.ctx
	m.mu.Unlock()
	if run == nil {
		return stationbrowser.DouyinInteractionResult{State: "unknown", Code: "browser_unavailable"}, apphost.ErrInteractionUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	stop := context.AfterFunc(serviceCtx, cancel)
	defer stop()
	return run(callCtx, a.profile, in, authorize)
}

func localDouyinInteractionReceipt(ref apphost.InteractionReference, kind, video string, r stationbrowser.DouyinInteractionResult) (apphost.InteractionReceipt, error) {
	if r.State == "applied" && (r.RemoteID == "" || (kind == "like" && r.RemoteID != "like:"+video) || (kind == "reply_comment" && !validLocalDouyinCommentID(r.RemoteID))) {
		return apphost.InteractionReceipt{}, apphost.ErrInteractionReceipt
	}
	if r.RemoteID != "" {
		if ref.RemoteID != "" && ref.RemoteID != r.RemoteID {
			return apphost.InteractionReceipt{}, apphost.ErrInteractionReceipt
		}
		ref.RemoteID = r.RemoteID
	}
	state := r.State
	if state != "applied" && state != "failed" && state != "cancelled" {
		state = "unknown"
	}
	return apphost.InteractionReceipt{Reference: ref, State: state, Code: localInteractionCode(r.Code)}, nil
}

func localInteractionCode(code string) string {
	if code == "" {
		return ""
	}
	if len(code) > 64 {
		return "interaction_failed"
	}
	for _, c := range code {
		if c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "interaction_failed"
		}
	}
	return code
}
