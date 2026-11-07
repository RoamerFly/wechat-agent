package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type wechatMediaHost struct {
	p          *permittedTools
	raw        json.RawMessage
	selected   crossMediaInput
	verified   *linuxnative.Message
	authorized bool
}

func (h *wechatMediaHost) CurrentThread() string        { return h.p.trace.Thread }
func (h *wechatMediaHost) Invalid(message string) error { return toolInputError(message) }
func (h *wechatMediaHost) Authorize(ctx context.Context, in wechatapp.MediaRequest) error {
	if err := h.p.manager.checkWechatSubmission(ctx); err != nil {
		return err
	}
	expected, err := wechatapp.ParseMediaRequest(h.raw, h.CurrentThread())
	if err != nil || !reflect.DeepEqual(in, expected) {
		return toolInputError("media_request_identity_changed")
	}
	encoded, _ := json.Marshal(in)
	var request crossMediaInput
	if json.Unmarshal(encoded, &request) != nil {
		return toolInputError("invalid_media_request")
	}
	p := h.p
	if !p.crossAllowed(request.Source, request.Target) || !companion.MediaPermitted(p.manager.Config().Companion.Permissions, request.Kind) || !p.manager.agentTaskActive(p.trace) {
		return toolInputError("来源、目标或跨会话媒体发送未授权")
	}
	h.verified = nil
	if request.Message != "" {
		msg, err := p.manager.native.Message(ctx, request.Message)
		if err != nil || msg.ID != request.Message || msg.AccountID != p.trace.Account || msg.ThreadID != request.Source || msg.FileAttachmentName() == "" {
			return toolInputError("source_message_not_owned_file")
		}
		h.verified = &msg
	}
	h.selected = request
	h.authorized = true
	return nil
}
func (h *wechatMediaHost) ClaimMedia(ctx context.Context, in wechatapp.MediaRequest, fn func(wechatapp.MediaSession) (any, error)) (string, error) {
	encoded, _ := json.Marshal(in)
	var request crossMediaInput
	if json.Unmarshal(encoded, &request) != nil || !h.authorized || !reflect.DeepEqual(request, h.selected) {
		return "", toolInputError("media_request_identity_changed")
	}
	p := h.p
	args, _ := json.Marshal(request)
	return p.privilegedCall(ctx, "wechat_send_media", args, func() (any, error) {
		p.manager.crossSendMu.Lock()
		defer p.manager.crossSendMu.Unlock()
		session := &wechatMediaSession{p: p, in: request, args: args, verified: h.verified, ctx: ctx}
		defer func() { session.closed = true }()
		if err := session.check(ctx); err != nil {
			return nil, err
		}
		return fn(session)
	})
}

type wechatMediaSession struct {
	p                                                     *permittedTools
	in                                                    crossMediaInput
	args                                                  json.RawMessage
	verified                                              *linuxnative.Message
	ctx                                                   context.Context
	closed, resolved, inspected, reused, prepared, queued bool
	identity                                              string
	origin                                                *companion.Input
	outputs                                               []companion.GeneratedMedia
}

func (s *wechatMediaSession) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return toolInputError("media_preparation_session_closed")
	}
	p := s.p
	if !p.crossAllowed(s.in.Source, s.in.Target) || !companion.MediaPermitted(p.manager.Config().Companion.Permissions, s.in.Kind) || !p.manager.agentTaskActive(p.trace) {
		return rejectOutboundEvidence(errors.New("cross_media_authorization_revoked"))
	}
	return p.manager.checkWechatSubmission(ctx)
}
func (s *wechatMediaSession) Resolve(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if s.resolved {
		return toolInputError("media_source_already_resolved")
	}
	p, m := s.p, s.p.manager
	in := &s.in
	verified := s.verified
	identity := p.trace.TraceID
	if p.trace.OriginTraceID != "" {
		identity = p.trace.OriginTraceID
	}
	if p.trace.TaskID != "" {
		identity = "task:" + p.trace.TaskID
	}

	if in.Message != "" {
		msg := *verified
		var e error
		if _, e = (mediaUnderstanding{manager: m}).receiveFile(ctx, msg, msg.FileAttachmentName()); e != nil {
			return e
		}
		m.assetMu.Lock()
		assets, e := m.loadAssets()
		m.assetMu.Unlock()
		if e != nil {
			return e
		}
		fingerprint := inboundFingerprint(msg)
		for _, a := range assets {
			if a.Account != p.trace.Account || a.Scope != assetScope(companion.Input{Channel: "wechat", Account: p.trace.Account, Thread: in.Source}) {
				continue
			}
			if a.hasOriginal(fingerprint) {
				in.Asset = a.ID
				break
			}
		}
	}

	s.identity = identity
	s.resolved = true
	return nil
}

func (s *wechatMediaSession) Existing(ctx context.Context) (result any, exists bool, err error) {
	if err := s.check(ctx); err != nil {
		return nil, false, err
	}
	if !s.resolved || s.prepared || s.queued {
		return nil, false, toolInputError("media_preparation_order_invalid")
	}
	defer func() {
		if err == nil {
			s.inspected, s.reused = true, exists
		}
	}()
	p, m := s.p, s.p.manager
	in := &s.in
	identity := s.identity
	if identity != "" && in.Asset != "" {
		entries, e := os.ReadDir(filepath.Join(m.dir, "cross-media"))
		if e != nil && !os.IsNotExist(e) {
			return nil, false, e
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			old, e := m.readCrossBatch(strings.TrimSuffix(entry.Name(), ".json"))
			if e != nil || old.SupersededBy != "" || !old.CancelledAt.IsZero() || old.RequestIdentity != identity && crossBatchAccepted(old) || old.Origin != p.trace.Thread || old.Sender != p.trace.Sender || old.Source != in.Source || old.Target != in.Target || old.Asset != in.Asset || old.Parts[0].Media.Kind != in.Kind || !sameVoiceConversion(old.Conversion, in.Conversion) {
				continue
			}
			state := "queued"
			if crossBatchAccepted(old) {
				state = "accepted"
			} else {
				if !m.crossSeedAllowed(old.Parts[0]) {
					return nil, false, errors.New("cross_media_origin_evidence_requires_review")
				}
				for _, part := range old.Parts {
					if e := p.validateSelectedMediaVoice(part.Media); e != nil {
						return nil, false, e
					}
				}
			}
			return map[string]any{"operation_id": old.ID, "state": state, "source_thread_id": old.Source, "target_thread_id": old.Target, "parts": len(old.Parts), "effective_conversion": old.Conversion.effective(), "reused_existing_operation": true, "instruction": "同一素材目标已有投递批次；核对原operation_id，不创建新的发送队列。"}, true, nil
		}
	}

	return nil, false, nil
}

func (s *wechatMediaSession) Prepare(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if !s.resolved || !s.inspected || s.reused || s.prepared || s.queued {
		return toolInputError("media_preparation_order_invalid")
	}
	p, m := s.p, s.p.manager
	in := &s.in
	origin, err := p.prepareCrossMediaOrigin(ctx)
	if err != nil {
		return err
	}
	sourceTrace := p.trace
	sourceTrace.Thread = in.Source
	access := &permittedTools{manager: m, trace: sourceTrace, media: map[string]companion.GeneratedMedia{}}
	var outputs []companion.GeneratedMedia
	if in.Kind == "file" {
		m.assetMu.Lock()
		assets, e := m.loadAssets()
		m.assetMu.Unlock()
		if e != nil {
			return e
		}
		var selected *mediaAsset
		for i := range assets {
			a := assets[i]
			if a.ID == in.Asset && a.Account == p.trace.Account && a.Scope == assetScope(sourceTrace) {
				selected = &a
				break
			}
		}
		if selected == nil {
			return errors.New("asset_not_in_source_conversation")
		}
		a := *selected
		if len(a.Hash) != 64 {
			return errors.New("original_file_not_cached")
		}
		n := a.Bytes
		native, ok := m.native.(nativeTools)
		if !ok {
			return errors.New("native_upload_unavailable")
		}
		caps, e := native.Capabilities(ctx)
		if e != nil {
			return e
		}
		if limit := caps.MediaSendMaxBytes["file"]; limit <= 0 || n > limit {
			return errors.New("native_file_size_limit")
		}
		upload, e := m.uploadVerifiedAsset(ctx, native, a)
		if e != nil {
			return e
		}
		if e = m.tools.upload(stagedUpload{UploadSource: upload, Account: p.trace.Account, Kind: "file"}); e != nil {
			return e
		}
		outputs = []companion.GeneratedMedia{{ID: a.ID + "-file", SourceID: upload.ID, AssetID: a.ID, Kind: "file", Title: a.Title, CachedMIME: a.MIME, VoiceID: a.VoiceID, VoiceEngine: a.VoiceEngine, VoiceProfile: a.VoiceProfile}}
	} else {
		raw, e := access.reuseAssetConverted(ctx, in.Asset, in.Kind, in.Conversion)
		if e != nil {
			return e
		}
		var prepared struct {
			Media []companion.GeneratedMedia `json:"media"`
		}
		if json.Unmarshal([]byte(raw), &prepared) != nil || len(prepared.Media) == 0 {
			return errors.New("requested_media_format_unavailable")
		}
		outputs = prepared.Media
		for i := range outputs {
			if staged, ok := access.media[outputs[i].ID]; ok {
				outputs[i].SourceID = staged.SourceID
			}
			if outputs[i].SourceID == "" {
				outputs[i].SourceID = outputs[i].ID
			}
		}
	}
	for _, media := range outputs {
		if e := p.validateSelectedMediaVoice(media); e != nil {
			return e
		}
	}

	s.origin, s.outputs = origin, outputs
	s.prepared = true
	return nil
}

func (s *wechatMediaSession) Queue(ctx context.Context) (any, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if !s.prepared || s.queued {
		return nil, toolInputError("media_preparation_order_invalid")
	}
	s.queued = true
	p, m := s.p, s.p.manager
	in := &s.in
	identity, origin, outputs, args := s.identity, s.origin, s.outputs, s.args
	for _, media := range outputs {
		if err := p.validateSelectedMediaVoice(media); err != nil {
			return nil, err
		}
	}
	if !p.crossAllowed(in.Source, in.Target) {
		return nil, rejectOutboundEvidence(errors.New("cross_media_authorization_revoked"))
	}
	id := p.operationID("wechat_send_media", args)
	if err := p.validateCrossMediaOrigin(ctx, origin); err != nil {
		return nil, err
	}
	if err := m.checkWechatSubmission(ctx); err != nil {
		return nil, err
	}
	batch := crossMediaBatch{OriginInput: origin, Conversion: in.Conversion, RequestIdentity: identity, TaskID: p.trace.TaskID, ID: id, Account: p.trace.Account, Origin: p.trace.Thread, Source: in.Source, Target: in.Target, Sender: p.trace.Sender, Asset: in.Asset, Created: time.Now().UTC()}
	for i, media := range outputs {
		input := p.trace
		input.ExpectedVoiceID, input.ExpectedVoiceProfile = p.SelectedVoiceIdentity()
		input.Thread = in.Target
		input.TraceID = id
		input.OriginTraceID = p.trace.TraceID
		input.TaskID = ""
		input.IsGroup = strings.HasSuffix(in.Target, "@chatroom")
		input.QuoteServerID = ""
		key := agentID(id, in.Target, in.Asset, media.SourceID, string(rune(i+1)))
		batch.Parts = append(batch.Parts, bot.MediaSeed{Key: key, Trace: id, Input: input, OriginInput: origin, SourceID: media.SourceID, Media: media, OriginThread: p.trace.Thread, SourceThread: in.Source, Requester: p.trace.Sender, OriginTask: p.trace.TaskID})
	}
	if e := ensureManaged(filepath.Join(m.dir, "cross-media")); e != nil {
		return nil, e
	}
	b, _ := json.Marshal(batch)
	if e := writePrivate(filepath.Join(m.dir, "cross-media", id+".json"), b); e != nil {
		return nil, e
	}
	if batch.TaskID != "" {
		if e := m.bindCrossTask(batch); e != nil {
			return nil, e
		}
	}
	if e := m.queueCrossBatch(batch); e != nil {
		return nil, e
	}
	m.record(bot.Record{Event: "cross_media_queued", Category: "decision", ThreadID: in.Target, Sender: p.trace.Sender, MessageID: p.trace.TraceID, Content: "[" + in.Kind + "] " + outputs[0].Title, Reason: "explicit_destination_persisted", Status: "queued", Origin: "agent"})
	return map[string]any{"operation_id": id, "state": "queued", "source_thread_id": in.Source, "target_thread_id": in.Target, "asset_id": in.Asset, "parts": len(batch.Parts), "effective_conversion": in.Conversion.effective(), "instruction": "附件已排入明确目标会话的持久队列，尚未确认送达。用wechat_operation_status核对。不要把附件再加入当前私聊reply.media_ids，不重复生成或发送。"}, nil
}
