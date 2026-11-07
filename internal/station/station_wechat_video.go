package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediadecode"
)

type stationVideoFetch func(context.Context, string, int64) ([]byte, string, error)

type stationVideoError struct{ code, stage string }

func (e *stationVideoError) Error() string { return e.code }
func (e *stationVideoError) SafeToolFailure() (string, string) {
	return e.code, "视频原件读取未完成。当前阶段为" + e.stage + "。"
}
func videoReadError(code, stage string) error { return &stationVideoError{code: code, stage: stage} }

func channelsVideoBinding(message linuxnative.Message, source linuxnative.ChannelsSource) string {
	if source.ObjectID == "" {
		// Some incoming cards supply only an exact locator. Never reuse those
		// cached bytes if the locator or key changes.
		return localCheckpointKey(wechatapp.NativeShareFingerprint(message), source.MediaURL, source.DecodeKey, source.ObjectNonceID)
	}
	return localCheckpointKey(wechatapp.NativeShareFingerprint(message), source.ObjectID, source.ObjectNonceID)
}

func (a stationAttachmentAccess) downloadChannelsVideo(ctx context.Context, original stationAttachmentSource, file *os.File) (linuxnative.VerifiedIncomingMedia, error) {
	limit := a.maxBytes
	if limit == 0 {
		limit = 50 << 20
	}
	message, err := a.native.Message(ctx, original.Message)
	if err != nil {
		return linuxnative.VerifiedIncomingMedia{}, err
	}
	source, err := wechatapp.ReadChannelsSource(ctx, message, a.native.Account(), a.native)
	if err != nil {
		return linuxnative.VerifiedIncomingMedia{}, err
	}
	if channelsVideoBinding(message, source) != original.Binding || !a.Allowed(ctx, original.Thread) {
		return linuxnative.VerifiedIncomingMedia{}, wechatapp.ErrAttachmentDenied
	}
	result, directErr := a.fetchChannelsVideo(ctx, original, source, limit, file)
	var stage *stationVideoError
	if !errors.As(directErr, &stage) || ctx.Err() != nil || !a.Allowed(ctx, original.Thread) || a.shareHost == nil || source.ObjectID == "" {
		return result, directErr
	}
	switch stage.code {
	case "channels_media_locator_missing", "channels_video_download_failed", "channels_decode_key_invalid", "wechat_video_probe_failed", "channels_http_status":
	default:
		return result, directErr
	}
	host, err := a.shareHost()
	if err != nil {
		return result, videoReadError("share_reader_store_unavailable", "打开当前连接的工具宿主")
	}
	resolved, err := host.resolve(ctx, a, original, message, source)
	if err != nil {
		return result, err
	}
	if err = file.Truncate(0); err != nil {
		return result, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	return a.fetchChannelsVideo(ctx, original, resolved, limit, file)
}

func (a stationAttachmentAccess) fetchChannelsVideo(ctx context.Context, original stationAttachmentSource, source linuxnative.ChannelsSource, limit int64, file *os.File) (linuxnative.VerifiedIncomingMedia, error) {
	if !a.Allowed(ctx, original.Thread) {
		return linuxnative.VerifiedIncomingMedia{}, wechatapp.ErrAttachmentDenied
	}
	if source.State != "available" || source.MediaURL == "" {
		return linuxnative.VerifiedIncomingMedia{}, videoReadError("channels_media_locator_missing", "定位视频号原件")
	}
	fetch := a.fetchVideo
	if fetch == nil {
		fetch = mcptools.GetPublicMedia
	}
	data, _, err := fetch(ctx, source.MediaURL, limit)
	if err != nil {
		if ctx.Err() != nil {
			return linuxnative.VerifiedIncomingMedia{}, ctx.Err()
		}
		code := "channels_video_download_failed"
		var web *mcptools.WebError
		if errors.As(err, &web) {
			code = "channels_" + web.Code
		}
		return linuxnative.VerifiedIncomingMedia{}, videoReadError(code, "下载原件")
	}
	if int64(len(data)) > limit || len(data) == 0 {
		return linuxnative.VerifiedIncomingMedia{}, videoReadError("channels_video_size_invalid", "下载原件")
	}
	if err = wechatapp.DecodeChannelsPayload(data, source.DecodeKey); err != nil {
		return linuxnative.VerifiedIncomingMedia{}, videoReadError("channels_decode_key_invalid", "解密原件")
	}
	writer := attachmentGrantWriter{allow: func() bool { return a.Allowed(ctx, original.Thread) }, out: file}
	if n, err := writer.Write(data); err != nil || n != len(data) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return linuxnative.VerifiedIncomingMedia{}, err
	}
	if _, err := mediadecode.Probe(ctx, file.Name()); err != nil {
		return linuxnative.VerifiedIncomingMedia{}, stationProbeError(ctx, err)
	}
	hash := sha256.Sum256(data)
	return linuxnative.VerifiedIncomingMedia{Bytes: int64(len(data)), MIME: "video/mp4", SHA256: hex.EncodeToString(hash[:]), Variant: "channels_original"}, nil
}

func stationProbeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err.Error() == "video_analysis_decoder_unavailable" {
		return videoReadError("wechat_video_decoder_unavailable", "查找 FFprobe")
	}
	return videoReadError("wechat_video_probe_failed", "探测原件")
}

func (t *stationAttachmentTools) videoAccess(ctx context.Context, message string) (stationAttachmentAccess, error) {
	if t.store == nil || !t.allowed() {
		return stationAttachmentAccess{}, wechatapp.ErrAttachmentDenied
	}
	trace := t.trace
	if trace.Source != nil && trace.Source.MessageID == message || trace.QuotedSource != nil && trace.QuotedSource.MessageID == message {
		if trace.Source != nil && trace.Source.MessageID == message {
			trace.TraceID = message
		}
		access, err := t.attachmentAccess(trace, message, t.allowed)
		access.videoOnly = true
		return access, err
	}
	m, visible, err := t.historyMessage(ctx, message)
	if err != nil || !visible {
		return stationAttachmentAccess{}, wechatapp.ErrAttachmentDenied
	}
	hash := sha256.Sum256([]byte(m.Body))
	ref := &companion.SourceRef{Account: m.AccountID, Thread: m.ThreadID, MessageID: m.ID, Sender: m.SenderID, ServerID: m.ServerID, OccurredAt: m.CreatedAt, MessageType: m.Type, BodyHash: hex.EncodeToString(hash[:])}
	return stationAttachmentAccess{native: t.native, dir: t.dir, thread: t.trace.Thread, message: message, grant: t.grants[t.trace.Thread],
		allowed: func() bool { return t.allowed() && t.historyAllowed(ctx) }, ref: ref, store: t.store, history: true, videoOnly: true, fetchVideo: t.fetchVideo}, nil
}

func (t *stationAttachmentTools) readVideo(ctx context.Context, raw json.RawMessage) (string, error) {
	var in struct {
		Message string `json:"message_id"`
		Max     *int64 `json:"max_bytes"`
		Timeout *int   `json:"timeout_seconds"`
	}
	if strictLocalMemoryDecode(raw, &in) != nil || in.Message == "" || len(in.Message) > 8192 || in.Max != nil && (*in.Max < 1 || *in.Max > stationAttachmentLimit) || in.Timeout != nil && (*in.Timeout < 0 || int64(*in.Timeout) > int64((1<<63-1)/int64(time.Second))) {
		return "", errors.New("wechat_video_parameters_invalid")
	}
	timeout := 240
	if in.Timeout != nil {
		timeout = *in.Timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	access, err := t.videoAccess(ctx, in.Message)
	if err != nil {
		return "", err
	}
	access.shareHost, access.requester, access.requestTrace = t.shareHost, t.trace.Sender, t.trace.TraceID
	access.maxBytes = 50 << 20
	if in.Max != nil {
		access.maxBytes = *in.Max
	}
	epoch, err := t.store.RecallEpoch(ctx)
	if err != nil {
		return "", err
	}
	source, err := access.source(ctx, access.thread, access.message)
	if err != nil {
		return "", err
	}
	record, file, err := access.open(ctx, source)
	if errors.Is(err, os.ErrNotExist) {
		err = access.DownloadAttachment(ctx, access.thread, access.message)
		if err == nil {
			record, file, err = access.open(ctx, source)
		}
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	if record.Bytes > access.maxBytes {
		return "", videoReadError("wechat_video_size_exceeds_request", "原件大小超过本次 max_bytes")
	}
	meta, err := mediadecode.Probe(ctx, file.Name())
	if err != nil {
		return "", stationProbeError(ctx, err)
	}
	current, err := access.source(ctx, access.thread, access.message)
	if err != nil || current != source || !access.Allowed(ctx, access.thread) {
		return "", wechatapp.ErrAttachmentDenied
	}
	currentEpoch, err := t.store.RecallEpoch(ctx)
	if err != nil {
		return "", err
	}
	if currentEpoch != epoch {
		return "", wechatapp.ErrAttachmentDenied
	}
	value := map[string]any{"asset_id": record.ID, "message_id": source.Message, "sender_id": source.Sender, "occurred_at": source.Occurred,
		"bytes": record.Bytes, "mime": record.MIME, "sha256": record.SHA256, "variant": record.Variant, "requested_max_bytes": access.maxBytes,
		"metadata": map[string]any{"duration_seconds": meta.Seconds, "total_frames": meta.Frames, "average_fps": meta.FPS, "width": meta.Width, "height": meta.Height},
		"coverage": "original_and_metadata", "content_observed": false, "audio_transcribed": false, "content_analysis_tool": "wechat_video_observe"}
	result, err := json.Marshal(value)
	return string(result), err
}
