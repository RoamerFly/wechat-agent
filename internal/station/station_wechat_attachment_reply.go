package station

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
)

func (t *stationAttachmentTools) Media(id string) (companion.GeneratedMedia, bool) {
	if asset, ok := strings.CutPrefix(id, "reply-"); ok && stationDocumentReplyID(asset) {
		r, err := t.readDocumentReply(asset)
		if err != nil {
			return companion.GeneratedMedia{}, false
		}
		_, err = t.documentReplyAccess(context.Background(), t.trace, r)
		return r.media(), err == nil
	}
	item, ok := t.prepared[id]
	return item, ok && t.replyAllowed(t.trace)
}

func (t *stationAttachmentTools) prepareReply(ctx context.Context, raw json.RawMessage) (string, error) {
	var req struct {
		Message string `json:"message_id"`
		Format  string `json:"format"`
	}
	if !t.replyAllowed(t.trace) || strictLocalMemoryDecode(raw, &req) != nil || req.Format != "" && req.Format != "original" && req.Format != "file" {
		return "", wechatapp.ErrAttachmentDenied
	}
	allowed := func() bool { return t.replyAllowed(t.trace) }
	access, err := t.attachmentAccess(t.trace, req.Message, allowed)
	if err != nil {
		return "", err
	}
	request, _ := json.Marshal(wechatapp.AttachmentRequest{Message: req.Message, Thread: t.trace.Thread})
	result, err := wechatapp.ReadAttachment(ctx, request, access)
	if err != nil {
		return "", err
	}
	asset, _ := result["asset_id"].(string)
	kind, _ := result["kind"].(string)
	filename, _ := result["filename"].(string)
	if req.Format == "file" {
		kind = "file"
	}
	if !stationAttachmentID(asset) || kind == "" || !allowed() {
		return "", wechatapp.ErrAttachmentDenied
	}
	media := companion.GeneratedMedia{ID: "reply-" + asset + "-" + kind, AssetID: asset, Kind: kind, Title: filename}
	if t.prepared == nil {
		t.prepared = make(map[string]companion.GeneratedMedia)
	}
	t.prepared[media.ID] = media
	data, err := json.Marshal(map[string]any{"media": []companion.GeneratedMedia{media}, "status": "prepared"})
	return string(data), err
}

func (t *stationAttachmentTools) stageReply(ctx context.Context, in companion.Input, kind, asset string) (string, error) {
	if stationDocumentReplyID(asset) {
		if kind != "file" {
			return "", wechatapp.ErrAttachmentDenied
		}
		return t.stageDocumentReply(ctx, in, asset)
	}
	if strings.HasPrefix(asset, "export-") {
		return t.stageWork(ctx, in, kind, asset)
	}
	allowed := func() bool { return t.replyAllowed(in) }
	if !allowed() || !stationAttachmentID(asset) {
		return "", wechatapp.ErrAttachmentDenied
	}
	for _, ref := range []*companion.SourceRef{in.Source, in.QuotedSource} {
		if !stationAttachmentRef(ref) {
			continue
		}
		access, err := t.attachmentAccess(in, ref.MessageID, allowed)
		if err != nil {
			continue
		}
		source, err := access.source(ctx, in.Thread, ref.MessageID)
		if err != nil || source.id() != asset || kind != source.Kind && kind != "file" {
			continue
		}
		record, original, err := access.open(ctx, source)
		if err != nil {
			return "", err
		}
		defer original.Close()
		if !allowed() {
			return "", wechatapp.ErrAttachmentDenied
		}
		upload, err := t.native.UploadVerified(ctx, source.Filename, record.MIME, original, record.Bytes, record.SHA256)
		if err != nil {
			return "", err
		}
		if !allowed() {
			return "", wechatapp.ErrAttachmentDenied
		}
		return upload.ID, nil
	}
	return "", errors.New("station_attachment_reply_source_unavailable")
}

func (t *stationAttachmentTools) canSubmitMedia(ctx context.Context, in companion.Input, kind, asset string) bool {
	if !stationDocumentReplyID(asset) {
		return true
	}
	r, err := t.readDocumentReply(asset)
	return kind == "file" && err == nil && t.verifyDocumentReplySource(ctx, in, r) == nil
}
