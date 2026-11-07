package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediadecode"
	"wechat-bot/internal/mediaobserve"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/transcript"
)

func (p *permittedTools) videoTools() []mcptools.Tool {
	if p.trace.Preview || p.trace.ReportOnly || p.trace.Account != p.manager.native.Account() || !permittedRecipient(p.manager.Config().Config, p.trace.Thread) || !p.manager.Config().Behavior.ReadVideos {
		return nil
	}
	return []mcptools.Tool{agentTool("video_inspect", "视频精读/逐帧读取: inspect actual video metadata, consecutive decoded frames, or arbitrary audio ranges. Automatic6-frame overview is a DEFAULT, not a capability ceiling. Choose ONE source: asset_id, message_id, or public url. Trusted owners may inspect another authorized conversation of THIS account: use source_thread_id with asset_id; a message_id resolves its actual source automatically. Ordinary members remain current-conversation only. This reads media, not private service directories. action defaults to info when omitted; info returns metadata only, then choose frames/audio for content. action=info returns actual frame count and timestamps metadata. action=frames defaults start_frame=0,step=1,page_size=24,batch_size=8,max_dimension=960,analysis_tokens=4096; ALL these are adjustable to user needs. page_size has no64-frame clamp; work is decoded/analyzed batch by batch. step>1 is sampling, never call it frame-by-frame. Return actual indexes/timestamps, effective settings and next_start_frame; continue on partial completion. action=audio defaults duration_seconds=90,audio_chunk_seconds=180,audio_timeout_seconds=60; longer requested ranges are processed completely in chunks. requested_range_complete describes that range; complete/video_audio_complete refer to the whole audio endpoint, so continue next_start_seconds when the user wants all audio. Adjust chunk size and timeout; if PCM payload must be split to fit transport, return actual size/reason instead of silently shortening the range. timeout_seconds defaults240 for the whole call;0 adds no local deadline but preserves caller cancellation. audio_timeout_seconds=0 also inherits caller cancellation. Provider-specific limits are actual feedback, not a project image-count quota. question customizes analysis. No WeChat send. For long work persist checkpoints and continue returned cursors. Input {action:info|frames|audio,source_thread_id?,asset_id?|message_id?|url?,start_frame?,page_size?,step?,batch_size?,max_dimension?,analysis_tokens?,timeout_seconds?,start_seconds?,duration_seconds?,audio_chunk_seconds?,audio_timeout_seconds?,question?}.", `{"type":"object","properties":{"action":{"type":"string","enum":["info","frames","audio"],"default":"info"},"source_thread_id":{"type":"string"},"asset_id":{"type":"string"},"message_id":{"type":"string"},"url":{"type":"string"},"start_frame":{"type":"integer","minimum":0},"page_size":{"type":"integer","minimum":1},"step":{"type":"integer","minimum":1},"batch_size":{"type":"integer","minimum":1},"max_dimension":{"type":"integer","minimum":1},"analysis_tokens":{"type":"integer","minimum":1},"timeout_seconds":{"type":"integer","minimum":0},"start_seconds":{"type":"number","minimum":0},"duration_seconds":{"type":"number","minimum":0},"audio_chunk_seconds":{"type":"number","minimum":0},"audio_timeout_seconds":{"type":"integer","minimum":0},"question":{"type":"string"}},"additionalProperties":false}`)}
}

type videoInspectionInput struct {
	sourceFingerprint string
	SourceThread      string  `json:"source_thread_id"`
	Action            string  `json:"action"`
	Asset             string  `json:"asset_id"`
	Message           string  `json:"message_id"`
	URL               string  `json:"url"`
	Start             int     `json:"start_frame"`
	PageSize          int     `json:"page_size"`
	Step              int     `json:"step"`
	AudioStart        float64 `json:"start_seconds"`
	AudioLength       float64 `json:"duration_seconds"`
	Question          string  `json:"question"`
	Tokens            int     `json:"analysis_tokens"`
	BatchSize         int     `json:"batch_size"`
	MaxDimension      int     `json:"max_dimension"`
	Timeout           *int    `json:"timeout_seconds"`
	AudioChunk        float64 `json:"audio_chunk_seconds"`
	AudioTimeout      *int    `json:"audio_timeout_seconds"`
}

type videoMetadata struct {
	Seconds float64 `json:"duration_seconds"`
	Frames  int     `json:"total_frames"`
	FPS     float64 `json:"average_fps"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
}

func probeInspectionVideo(ctx context.Context, path string) (videoMetadata, error) {
	decoded, err := mediadecode.Probe(ctx, path)
	return videoMetadata{Seconds: decoded.Seconds, Frames: decoded.Frames, FPS: decoded.FPS, Width: decoded.Width, Height: decoded.Height}, err
}

func (p *permittedTools) inspectionThreadAllowed(thread string) bool {
	if thread == "" || p.trace.Account != p.manager.native.Account() {
		return false
	}
	return p.conversationReadAllowed(thread)
}

// inspectionPublicURLPolicy binds an explicit URL to the actual incoming
// message that triggered this Agent turn. The URL is a purpose parameter, not
// a claim that the message itself was a native share card.
func (p *permittedTools) inspectionPublicURLPolicy(ctx context.Context, rawURL string) (*publicSharePolicy, error) {
	if !p.servicePluginsAllowed() || p.trace.Account != p.manager.native.Account() || !p.inspectionThreadAllowed(p.trace.Thread) {
		return nil, toolInputError("video_source_authorization_revoked")
	}
	messageID := p.trace.TraceID
	if p.trace.Source != nil && p.trace.Source.MessageID != "" {
		messageID = p.trace.Source.MessageID
	}
	if messageID == "" {
		return nil, toolInputError("video_public_url_source_required")
	}
	message, err := p.manager.native.Message(ctx, messageID)
	if err != nil {
		return nil, toolInputError("share_native_source_unavailable")
	}
	if message.AccountID == "" {
		message.AccountID = p.manager.native.Account()
	}
	if message.ID != messageID || message.AccountID != p.trace.Account || message.ThreadID != p.trace.Thread || message.Direction != "incoming" {
		return nil, toolInputError("video_public_url_source_required")
	}
	actorCtx := withSourceActor(ctx, p, agentID("video-inspect-public-url", p.trace.TaskID, p.trace.TraceID, message.ID, sharePublicURL(rawURL)), message.ThreadID)
	policy, err := p.manager.newExplicitPublicSharePolicy(actorCtx, message, rawURL, true)
	if err != nil {
		return nil, err
	}
	return policy, nil
}

func (p *permittedTools) inspectionSource(ctx context.Context, in *videoInspectionInput) ([]byte, string, error) {
	count := 0
	for _, s := range []string{in.Asset, in.Message, in.URL} {
		if s != "" {
			count++
		}
	}
	if count != 1 {
		return nil, "", toolInputError("请选择且仅选择asset_id、message_id或url中的一个来源")
	}
	m := p.manager
	explicitSource := in.SourceThread != ""
	if in.SourceThread == "" {
		in.SourceThread = p.trace.Thread
	}
	if !p.inspectionThreadAllowed(in.SourceThread) {
		return nil, "", toolInputError("video_source_not_authorized")
	}
	sourceTrace := p.trace
	sourceTrace.Thread = in.SourceThread
	if in.Asset != "" {
		m.assetMu.Lock()
		items, e := m.loadAssets()
		m.assetMu.Unlock()
		if e != nil {
			return nil, "", e
		}
		for _, a := range items {
			if a.ID == in.Asset && a.Account == p.trace.Account && a.Scope == assetScope(sourceTrace) {
				if time.Since(a.Updated) > time.Duration(m.Config().Companion.MemoryDays)*24*time.Hour {
					return nil, "", toolInputError("素材已超过当前保留时限")
				}
				if !strings.HasPrefix(a.MIME, "video/") {
					return nil, "", toolInputError("这不是原视频素材；请从media_recent找parent_asset_id对应的原视频")
				}
				b, e := m.cachedAssetData(a)
				return b, a.ID, e
			}
		}
		return nil, "", toolInputError("当前会话没有该视频素材")
	}
	if in.Message != "" {
		msg, e := m.native.Message(ctx, in.Message)
		if e != nil {
			return nil, "", e
		}
		if msg.ID != in.Message || msg.AccountID != p.trace.Account || explicitSource && msg.ThreadID != in.SourceThread || !p.inspectionThreadAllowed(msg.ThreadID) {
			return nil, "", toolInputError("消息不属于当前账号的已授权来源会话")
		}
		in.SourceThread = msg.ThreadID
		// Reuse the verified original already observed in its own conversation.
		// A private-chat inspection must not relocate or re-download that media.
		m.assetMu.Lock()
		assets, cacheErr := m.loadAssets()
		m.assetMu.Unlock()
		if cacheErr != nil {
			return nil, "", cacheErr
		}
		sourceTrace.Thread = msg.ThreadID
		in.sourceFingerprint = inboundFingerprint(msg)
		for _, asset := range assets {
			if asset.Account != p.trace.Account || asset.Scope != assetScope(sourceTrace) || !strings.HasPrefix(asset.MIME, "video/") || time.Since(asset.Updated) > time.Duration(m.Config().Companion.MemoryDays)*24*time.Hour {
				continue
			}
			if asset.hasOriginal(in.sourceFingerprint) {
				data, err := m.cachedAssetData(asset)
				if err == nil {
					return data, msg.ID, nil
				}
			}
		}
		if msg.Type == 43 {
			reader, ok := m.native.(incomingReader)
			if !ok {
				return nil, "", errors.New("incoming_media_backend_unavailable")
			}
			b, _, e := reader.IncomingMedia(ctx, msg.ID, 64<<20)
			return b, msg.ID, e
		}
		if wechatapp.IsChannelsShare(msg) {
			actorCtx := withSourceActor(ctx, p, agentID("video-inspect-channels", p.trace.TaskID, p.trace.TraceID, msg.ID), msg.ThreadID)
			video, err := (mediaUnderstanding{manager: m}).acquireChannelsVideo(actorCtx, msg)
			return video.data, msg.ID, err
		}
		if msg.Link == nil || msg.Link.URL == "" {
			return nil, "", toolInputError("该消息没有可读取的视频来源")
		}
		in.URL = msg.Link.URL
	}
	if !p.settings.Enabled || !p.settings.WebFetchEnabled {
		return nil, "", toolInputError("当前联网读取权限已关闭")
	}
	installed, stateErr := m.installedShareState(ctx)
	if stateErr != nil {
		return nil, "", stateErr
	}
	if !installed {
		return nil, "", toolInputError("share_plugin_migration_required")
	}
	policy, err := p.inspectionPublicURLPolicy(ctx, in.URL)
	if err != nil {
		return nil, "", err
	}
	v, err := policy.readShare(ctx, 64<<20)
	if err != nil {
		return nil, "", err
	}
	if v.Kind != "video" {
		return nil, "", toolInputError("分享链接没有返回实际视频")
	}
	return v.Data, policy.requestedURL, nil
}

func (p *permittedTools) callVideoInspection(ctx context.Context, _ string, args json.RawMessage) (result string, callErr error) {
	defer func() { callErr = videoInspectionError(callErr) }()
	var in videoInspectionInput
	if json.Unmarshal(args, &in) != nil || in.Start < 0 || in.PageSize < 0 || in.Step < 0 || in.AudioStart < 0 || in.AudioLength < 0 || in.Tokens < 0 || in.BatchSize < 0 || in.MaxDimension < 0 || in.AudioChunk < 0 {
		return "", toolInputError("视频精读参数无效")
	}
	// Discovery is a safe, read-only default. Validate explicit operations
	// before resolving/downloading a source; never guess frames vs audio.
	if in.Action == "" {
		in.Action = "info"
	}
	if in.Action != "info" && in.Action != "frames" && in.Action != "audio" {
		return "", toolInputError("action应为info、frames或audio")
	}
	seconds := 240
	if in.Timeout != nil {
		seconds = *in.Timeout
	}
	if seconds < 0 || int64(seconds) > int64(math.MaxInt64)/int64(time.Second) {
		return "", toolInputError("timeout_seconds超出可表示的执行范围")
	}
	if seconds > 0 {
		bounded, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		defer cancel()
		ctx = bounded
	}
	if in.BatchSize == 0 {
		in.BatchSize = 8
	}
	if in.MaxDimension == 0 {
		in.MaxDimension = 960
	}
	if in.Tokens == 0 {
		in.Tokens = 4096
	}
	data, source, err := p.inspectionSource(ctx, &in)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(p.manager.dir, "video-inspect-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "input.video")
	if err = os.WriteFile(path, data, 0600); err != nil {
		return "", err
	}
	meta, err := probeInspectionVideo(ctx, path)
	if err != nil {
		return "", err
	}
	out := map[string]any{"source_thread_id": in.SourceThread, "origin_thread_id": p.trace.Thread, "source": source, "metadata": meta, "read_only": true, "usage": "frames: start_frame=0,step=1; follow next_start_frame until complete. audio: follow next_start_seconds. Total pages are not limited."}
	out["effective_options"] = map[string]any{"action": in.Action, "batch_size": in.BatchSize, "max_dimension": in.MaxDimension, "analysis_tokens": in.Tokens, "timeout_seconds": seconds}
	encode := func() (string, error) {
		if !p.inspectionThreadAllowed(in.SourceThread) {
			return "", toolInputError("video_source_authorization_revoked")
		}
		b, e := json.Marshal(out)
		if e == nil {
			p.routeCrossConversationThread(in.SourceThread)
		}
		return string(b), e
	}
	if in.Action == "info" {
		return encode()
	}
	if in.Action == "audio" {
		installedMedia, installedErr := p.manager.installedServiceState(ctx, "media-understanding", "media")
		if installedErr != nil {
			return "", installedErr
		}
		if !installedMedia {
			return "", toolInputError("media_transcription_plugin_migration_required")
		}
		if !p.manager.mediaTranscriptionConfigured(ctx) {
			return "", toolInputError("media_transcription_configuration_required")
		}
		if in.AudioStart >= meta.Seconds {
			return "", toolInputError("音轨起点已超过视频时长")
		}
		length := in.AudioLength
		if length == 0 {
			length = 90
		}
		length = math.Min(length, meta.Seconds-in.AudioStart)
		message, e := p.inspectionTranscriptionMessage(ctx, in)
		if e != nil {
			return "", e
		}
		out["source_message_id"] = message.ID
		if e = p.inspectPluginAudioRange(ctx, data, in, length, meta.Seconds, message, out); e != nil {
			return "", e
		}
		return encode()
	}
	if in.Action != "frames" {
		return "", toolInputError("action应为info、frames或audio")
	}
	if in.Start > meta.Frames {
		return "", toolInputError("start_frame超出实际帧数")
	}
	if in.PageSize == 0 {
		in.PageSize = 24
	}
	if in.Step == 0 {
		in.Step = 1
	}
	count := min(in.PageSize, mediaobserve.RemainingFrames(meta.Frames, in.Start, in.Step))
	out["start_frame"], out["step"], out["requested_page_size"], out["effective_page_size"] = in.Start, in.Step, in.PageSize, count
	out["coverage"] = "contiguous_frames"
	if in.Step > 1 {
		out["coverage"] = "sampled_frames"
	}
	rows := []map[string]any{}
	next := in.Start
	if count > 0 {
		rows, next, err = p.inspectVideoFrameRange(ctx, path, dir, in, count, meta.Frames, source, data)
		if err != nil {
			if len(rows) == 0 {
				return "", err
			}
			out["continuation_reason"] = mcptools.FailureDescription(videoInspectionError(err))
		}
	}
	out["batches"], out["next_start_frame"], out["complete"], out["consecutive_frames"] = rows, min(next, meta.Frames), next >= meta.Frames, in.Step == 1
	return encode()
}

// inspectVideoFrameRange keeps decoded frames inside the host and asks the
// installed media-understanding plugin to observe them.  It deliberately does
// not use the chat model directly: video_inspect has the same source and
// package/permission boundary as every other visual observation.
func (p *permittedTools) inspectVideoFrameRange(ctx context.Context, path, dir string, in videoInspectionInput, count, total int, source string, data []byte) ([]map[string]any, int, error) {
	if err := p.manager.mediaImageObservationReady(ctx); err != nil {
		return nil, in.Start, err
	}
	if !p.servicePluginsAllowed() || !p.inspectionThreadAllowed(in.SourceThread) {
		return nil, in.Start, errors.New("video_source_authorization_revoked")
	}
	epoch, err := p.serviceEvidenceEpoch(ctx)
	if err != nil {
		return nil, in.Start, err
	}
	authorization := p.serviceAuthorization()
	if authorization == "" {
		return nil, in.Start, errors.New("video_source_authorization_revoked")
	}
	sum := sha256.Sum256(data)
	sourceID := "video-inspection:" + hex.EncodeToString(sum[:])
	sources := map[string]int{sourceID: 1}
	if p.trace.Source != nil && p.trace.Source.EventID != "" {
		sources[p.trace.Source.EventID] = p.trace.Source.Revision
	}
	if p.trace.ReplyContext != nil {
		for id, version := range p.trace.ReplyContext.SourceVersions {
			sources[id] = version
		}
	}
	baseKey := agentID("video-inspection-frames", p.trace.Account, p.trace.Thread, p.serviceRequester(), p.trace.TaskID, p.trace.TraceID, in.SourceThread, source, sourceID, strconv.Itoa(in.Start), strconv.Itoa(in.Step), strconv.Itoa(count), strconv.Itoa(in.BatchSize), strconv.Itoa(in.MaxDimension), strconv.Itoa(in.Tokens), in.Question)
	actorCtx := withSourceActor(ctx, p, baseKey, in.SourceThread)
	check := func(checkCtx context.Context) error {
		if !p.servicePluginsAllowed() || !p.inspectionThreadAllowed(in.SourceThread) || p.serviceAuthorization() != authorization {
			return errors.New("video_source_authorization_revoked")
		}
		currentEpoch, e := p.serviceEvidenceEpoch(checkCtx)
		if e != nil || currentEpoch != epoch {
			return errors.New("video_source_authorization_revoked")
		}
		return nil
	}
	analyze := func(analyzeCtx context.Context, prompt string, images []deepseek.Image, tokens int) (string, error) {
		if err := check(analyzeCtx); err != nil {
			return "", err
		}
		inv := pluginhost.Invocation{
			Account: p.trace.Account, Thread: p.trace.Thread, Requester: p.serviceRequester(),
			Purpose: "inspect authorized video frames", RequestKey: agentID(baseKey, prompt),
			EvidenceEpoch: epoch, Authorization: authorization, SourceVersions: sources,
			RequestThread: p.trace.Thread,
		}
		policy, e := p.manager.newBoundMediaVisualObservationPolicy(actorCtx, images, inv, 1, check, prompt, tokens)
		if e != nil {
			return "", e
		}
		return policy.Observe(analyzeCtx)
	}
	return mediaobserve.InspectFrames(actorCtx, path, dir, mediaobserve.FrameOptions{Start: in.Start, Step: in.Step, BatchSize: in.BatchSize, MaxDimension: in.MaxDimension, Tokens: in.Tokens, Question: in.Question}, count, total, analyze)
}

// inspectionTranscriptionMessage resolves only a canonical received message for
// the inspected original. An asset ID alone is not enough authority to submit
// audio: the retained first source must still be readable as native media.
func (p *permittedTools) inspectionTranscriptionMessage(ctx context.Context, in videoInspectionInput) (linuxnative.Message, error) {
	messageID := in.Message
	if messageID == "" && in.Asset != "" {
		p.manager.assetMu.Lock()
		assets, err := p.manager.loadAssets()
		p.manager.assetMu.Unlock()
		if err != nil {
			return linuxnative.Message{}, err
		}
		for _, asset := range assets {
			if asset.ID != in.Asset || asset.Account != p.trace.Account || asset.Scope != assetScope(companion.Input{Channel: "wechat", Account: p.trace.Account, Thread: in.SourceThread}) {
				continue
			}
			for _, r := range asset.Requests {
				if r.SourceFingerprint == "" {
					continue
				}
				current, err := p.manager.native.Message(ctx, r.TraceID)
				if err == nil && current.ID == r.TraceID && current.AccountID == p.trace.Account && current.ThreadID == in.SourceThread && p.inspectionThreadAllowed(current.ThreadID) && current.Direction == "incoming" && (current.Type == 43 || current.Type == 49) && inboundFingerprint(current) == r.SourceFingerprint {
					return current, nil
				}
				if ctx.Err() != nil {
					return linuxnative.Message{}, ctx.Err()
				}
			}
			break
		}
	}
	if messageID == "" {
		return linuxnative.Message{}, errors.New("video_audio_plugin_source_required")
	}
	message, err := p.manager.native.Message(ctx, messageID)
	if err != nil {
		return linuxnative.Message{}, errors.New("share_native_source_unavailable")
	}
	if message.AccountID == "" {
		message.AccountID = p.manager.native.Account()
	}
	if in.sourceFingerprint != "" && inboundFingerprint(message) != in.sourceFingerprint {
		return linuxnative.Message{}, errors.New("video_source_changed")
	}
	if message.ID != messageID || message.AccountID != p.trace.Account || message.ThreadID != in.SourceThread || !p.inspectionThreadAllowed(message.ThreadID) || message.Direction != "incoming" || (message.Type != 43 && message.Type != 49) {
		return linuxnative.Message{}, errors.New("share_native_source_required")
	}
	return message, nil
}

// inspectPluginAudioRange mirrors the continuation output while binding every
// PCM chunk to the original received message and its live source version.
func (p *permittedTools) inspectPluginAudioRange(ctx context.Context, data []byte, in videoInspectionInput, length, total float64, message linuxnative.Message, out map[string]any) error {
	requested := in.AudioChunk
	chunk := requested
	if chunk == 0 {
		chunk = 180
	}
	byteSeconds := float64((8<<20)-1024) / 32000
	chunk = math.Min(chunk, byteSeconds)
	next, end := in.AudioStart, in.AudioStart+length
	parts := []map[string]any{}
	texts := []string{}
	resultBytes := 0
	early := false
	timeout := 60
	if in.AudioTimeout != nil {
		timeout = *in.AudioTimeout
	}
	if timeout < 0 || int64(timeout) > math.MaxInt64/int64(1e9) {
		return toolInputError("audio_timeout_seconds超出可表示范围")
	}
	a := mediaUnderstanding{manager: p.manager}
	for next < end {
		count := math.Min(chunk, end-next)
		segmentStart := next
		text, seconds, err := a.videoAudioRangeTimedWith(ctx, data, segmentStart, count, timeout, func(ctx context.Context, wav []byte) (string, error) {
			actorCtx := withSourceActor(ctx, p, agentID("video-inspect-audio", p.trace.TaskID, p.trace.TraceID, message.ID, formatSeconds(segmentStart), formatSeconds(count)), message.ThreadID)
			policy, err := p.manager.newMediaTranscriptionPolicy(actorCtx, message, wav)
			if err != nil {
				return "", err
			}
			// Silence can encode to equal PCM bytes. Include the requested range in
			// the idempotency identity so continuation segments never collapse.
			policy.appendRequestSuffix(formatSeconds(segmentStart), formatSeconds(count))
			return policy.transcribe(ctx)
		})
		noSpeech := mediaspec.IsNoSpeech(err)
		if err != nil && !noSpeech {
			if len(parts) == 0 {
				return err
			}
			out["continuation_reason"] = mcptools.FailureDescription(videoInspectionError(err))
			break
		}
		stop := math.Min(end, next+math.Min(count, seconds))
		if stop <= next {
			return toolInputError("音轨读取未推进，已停止以保留可核对的起点")
		}
		text, method := transcript.Format(ctx, text)
		part := map[string]any{"start_seconds": next, "end_seconds": stop, "text": text, "text_format": method, "recognized_words_preserved": true}
		if noSpeech {
			part["state"] = "no_recognizable_speech"
			part["provider_code"] = "ASR_RESPONSE_HAVE_NO_WORDS"
		} else {
			part["state"] = "transcribed"
		}
		encoded, _ := json.Marshal(part)
		if resultBytes+len(encoded)*2 > 800<<10 {
			out["continuation_reason"] = "本次结果接近1MiB工具结果传输封套；所需范围未缩短，请从next_start_seconds继续读取"
			break
		}
		resultBytes += len(encoded) * 2
		parts = append(parts, part)
		if text != "" {
			texts = append(texts, text)
		}
		next = stop
		if seconds < count-.1 || end-next > 0 && end-next <= .05 {
			early = true
			break
		}
	}
	out["segments"], out["text"] = parts, strings.Join(texts, "\n")
	if len(texts) == 0 && len(parts) > 0 {
		out["transcription_state"] = "no_recognizable_speech"
		out["observation"] = "所选音轨范围已处理，识别服务未返回可转写的词；可能为音乐、环境声或不清晰人声，不代表提取或鉴权失败。"
	} else {
		out["transcription_state"] = "transcribed"
	}
	out["start_seconds"], out["end_seconds"], out["next_start_seconds"], out["complete"] = in.AudioStart, next, next, next >= total-.05 || early
	out["requested_range_complete"], out["video_audio_complete"] = next >= end-.0001 || early, next >= total-.05 || early
	out["audio_track_ended_early"], out["requested_duration_seconds"], out["effective_duration_seconds"] = early, in.AudioLength, length
	out["audio_endpoint_tolerance_seconds"] = .05
	out["requested_audio_chunk_seconds"], out["effective_audio_chunk_seconds"], out["audio_timeout_seconds"] = requested, chunk, timeout
	if requested > chunk {
		out["audio_chunk_adjustment_reason"] = "16kHz mono PCM16 must fit the8MiB ASR transport; the full requested range is processed in multiple chunks"
	}
	return nil
}
