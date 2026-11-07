package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/billing"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediadecode"
	"wechat-bot/internal/mediaobserve"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/mediatranscribe"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/transcript"
)

type stationVideoObservationInput struct {
	Message     string  `json:"message_id"`
	Asset       string  `json:"asset_id"`
	Mode        string  `json:"mode"`
	Start       int     `json:"start_frame"`
	Step        int     `json:"step"`
	Page        int     `json:"page_size"`
	Batch       int     `json:"batch_size"`
	Dimension   int     `json:"max_dimension"`
	Tokens      int     `json:"analysis_tokens"`
	Question    string  `json:"question"`
	AudioStart  float64 `json:"start_seconds"`
	AudioLength float64 `json:"duration_seconds"`
	Timeout     int     `json:"timeout_seconds"`
}

func (h *stationShareHost) mediaBilling(dir string) (*billing.Store, error) {
	h.usageMu.Lock()
	defer h.usageMu.Unlock()
	if h.usage == nil {
		store, err := billing.Open(dir)
		if err != nil {
			return nil, errors.New("media_usage_record_failed")
		}
		h.usage = store
	}
	return h.usage, nil
}

func (t *stationAttachmentTools) observeVideo(ctx context.Context, raw json.RawMessage) (string, error) {
	in := stationVideoObservationInput{Step: 1, Page: 12, Batch: 6, Dimension: 960, Tokens: 1024, AudioLength: 90, Timeout: 240}
	if strictLocalMemoryDecode(raw, &in) != nil || in.Message == "" || len(in.Message) > 8192 || !stationAttachmentID(in.Asset) ||
		(in.Mode != "frames" && in.Mode != "audio") || in.Start < 0 || in.Step < 1 || in.Page < 1 || in.Batch < 1 || in.Dimension < 1 || in.Tokens < 1 ||
		in.AudioStart < 0 || in.AudioLength <= 0 || utf8.RuneCountInString(in.Question) > 4096 || in.Timeout < 0 || int64(in.Timeout) > int64((1<<63-1)/int64(time.Second)) {
		return "", errors.New("wechat_video_observation_parameters_invalid")
	}
	if in.Timeout != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(in.Timeout)*time.Second)
		defer cancel()
	}
	access, err := t.videoAccess(ctx, in.Message)
	if err != nil {
		return "", err
	}
	epoch, err := t.store.RecallEpoch(ctx)
	if err != nil {
		return "", err
	}
	source, err := access.source(ctx, access.thread, access.message)
	if err != nil || source.id() != in.Asset {
		return "", wechatapp.ErrAttachmentDenied
	}
	record, file, err := access.open(ctx, source)
	if errors.Is(err, os.ErrNotExist) {
		return "", observationFailure("wechat_video_original_required", "请先通过 wechat_video_read 核验此消息的视频原件。")
	}
	if err != nil {
		return "", err
	}
	defer file.Close()

	checkSource := func(ctx context.Context) error {
		return access.confirmObservationSource(ctx, source, epoch)
	}
	if t.shareHost == nil {
		return "", observationFailure("media_plugin_runtime_unavailable", "当前连接未准备媒体理解工具宿主。")
	}
	host, err := t.shareHost()
	if err != nil || host.store.Account() != source.Account {
		return "", observationFailure("media_plugin_runtime_unavailable", "无法打开当前连接的工具宿主。")
	}
	installed, err := host.store.Installed(ctx, localasr.PluginID)
	if err != nil || !installed.Enabled || installed.Removed {
		return "", observationFailure("media_plugin_not_enabled", "请在当前微信连接中安装并启用媒体理解工具。")
	}
	config, err := host.store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		return "", observationFailure("media_plugin_configuration_invalid", "无法读取当前连接的媒体理解设置。")
	}
	backend, validBackend := stationASRBackend(config.Values)
	if in.Mode == "audio" && !validBackend {
		return "", observationFailure("media_transcription_configuration_invalid", "请在当前微信连接中重新保存语音识别方式。")
	}
	localAudio := in.Mode == "audio" && backend == "local"
	var usage *billing.Store
	if !localAudio {
		usage, err = host.mediaBilling(t.dir)
		if err != nil {
			return "", observationFailure("media_usage_record_failed", "无法打开当前连接的模型用量记录。")
		}
	}
	post := host.postModel
	if post == nil {
		post = mcptools.PostPluginModel
	}
	purpose := "observe authorized WeChat video " + in.Mode
	contextFor := func(args json.RawMessage) pluginhost.Invocation {
		key := localCheckpointKey(t.trace.TraceID, t.trace.Sender, record.ID, record.SHA256, epoch, installed.Digest, installed.Revision, config.Revision, in.Mode, in.AudioStart, in.AudioLength, string(args))
		return pluginhost.Invocation{Account: source.Account, Thread: source.Thread, Requester: t.trace.Sender, Purpose: purpose, RequestKey: "station-media-" + key,
			SourceVersions: map[string]int{"native:" + record.ID: 1}, EvidenceEpoch: epoch, Authorization: record.ID, OriginSourceSender: source.Sender, RequestThread: t.trace.Thread}
	}
	check := func(ctx context.Context) error {
		if err := checkSource(ctx); err != nil {
			return err
		}
		p, err := host.store.Installed(ctx, localasr.PluginID)
		if err != nil || !p.Enabled || p.Removed || p.Revision != installed.Revision || p.Digest != installed.Digest {
			return observationFailure("media_plugin_version_changed", "读取期间工具版本、授权或启用状态已变更。")
		}
		c, err := host.store.Configuration(ctx, localasr.PluginID)
		if err != nil || c.Revision != config.Revision {
			return observationFailure("media_plugin_configuration_changed", "读取期间媒体理解配置已变更。")
		}
		return nil
	}
	origin := func(ctx context.Context, _ pluginhost.Job, _ pluginhost.Capability) error { return check(ctx) }

	dir, err := os.MkdirTemp(t.dir, "verified-video-")
	if err != nil {
		return "", observationFailure("media_staging_failed", "无法创建当前连接的私有解码目录。")
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "original.video")
	if err := stageStationMedia(ctx, path, file, record); err != nil {
		return "", err
	}
	meta, err := mediadecode.Probe(ctx, path)
	if err != nil {
		return "", stationProbeError(ctx, err)
	}
	out := map[string]any{"asset_id": record.ID, "message_id": source.Message, "sender_id": source.Sender, "occurred_at": source.Occurred,
		"sha256": record.SHA256, "mode": in.Mode, "untrusted_external_content": true}
	if in.Mode == "frames" {
		analyze := func(ctx context.Context, prompt string, images []deepseek.Image, tokens int) (string, error) {
			p, err := mediaobserve.NewVisual(ctx, host.store, 1, prompt, images, tokens)
			if err != nil {
				return "", err
			}
			invocation := contextFor(p.Arguments())
			p.Bind(mediaobserve.VisualBinding{
				Invocation:  func() pluginhost.Invocation { return invocation },
				CheckOrigin: origin,
				Controller:  func() (*pluginruntime.Controller, error) { return host.controller, nil },
				Billing:     usage,
				Transport:   post,
			})
			return p.Observe(ctx)
		}
		err = stationObserveFrames(ctx, path, dir, in, meta, analyze, out)
	} else if localAudio {
		out["asr_backend"] = "local"
		transcribe := func(ctx context.Context, wav []byte) (string, error) {
			grant := pluginhost.Grant{Account: source.Account, Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}
			if err := pluginhost.Authorize(installed.Package, grant, contextFor(nil), []string{"artifact.read", "model.invoke"}); err != nil {
				return "", observationFailure("media_plugin_permissions_changed", "当前微信连接缺少媒体处理或模型调用授权。")
			}
			if host.localASR.transcribe == nil {
				return "", observationFailure("station_asr_host_unavailable", "本机识别不可用。请核对已登记的资源及项目工具中的本地媒体识别。")
			}
			return host.localASR.transcribe(ctx, wav, func() error { return check(ctx) })
		}
		err = stationObserveAudio(ctx, path, dir, in.AudioStart, in.AudioLength, meta.Seconds, false, transcribe, out)
	} else {
		transcribe := func(ctx context.Context, wav []byte) (string, error) {
			var invocation pluginhost.Invocation
			p, err := mediatranscribe.New(ctx, mediatranscribe.Dependencies{
				Store:       host.store,
				Invocation:  func() pluginhost.Invocation { return invocation },
				OriginCheck: origin,
				Runtime: func(context.Context) (*pluginruntime.Controller, bool, error) {
					return host.controller, host.controller != nil, nil
				},
				Post:        post,
				RecordUsage: mediatranscribe.UsageRecorder(usage),
			}, 1, wav)
			if err != nil {
				return "", err
			}
			invocation = contextFor(p.Arguments())
			return p.Transcribe(ctx)
		}
		err = stationObserveAudio(ctx, path, dir, in.AudioStart, in.AudioLength, meta.Seconds, false, transcribe, out)
	}
	if checkErr := check(ctx); checkErr != nil {
		return "", checkErr
	}
	if err != nil {
		return "", err
	}
	result, err := json.Marshal(out)
	return string(result), err
}

func stageStationMedia(ctx context.Context, path string, original *os.File, record stationAttachmentRecord) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return observationFailure("media_staging_failed", "无法创建当前连接的私有解码副本。")
	}
	hash := sha256.New()
	writer := attachmentGrantWriter{allow: func() bool { return ctx.Err() == nil }, out: io.MultiWriter(out, hash)}
	count, err := io.Copy(writer, io.LimitReader(original, record.Bytes+1))
	closeErr := out.Close()
	if err != nil || closeErr != nil || count != record.Bytes || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
		return observationFailure("media_staging_integrity_failed", "解码副本未通过原件长度与哈希核对。")
	}
	return nil
}

func stationObserveFrames(ctx context.Context, path, dir string, in stationVideoObservationInput, meta mediadecode.Metadata, analyze func(context.Context, string, []deepseek.Image, int) (string, error), out map[string]any) error {
	count := min(in.Page, mediaobserve.RemainingFrames(meta.Frames, in.Start, in.Step))
	settings := mediaobserve.FrameOptions{Start: in.Start, Step: in.Step, BatchSize: in.Batch, MaxDimension: in.Dimension, Tokens: in.Tokens, Question: in.Question}
	rows, next, err := mediaobserve.InspectFrames(ctx, path, dir, settings, count, meta.Frames, analyze)
	if err != nil && len(rows) == 0 {
		return stationMediaFailure(err, "观察画面")
	}
	if err != nil {
		out["continuation_error"] = stationMediaFailure(err, "观察画面").Error()
	}
	coverage := "consecutive_frames"
	if in.Step > 1 {
		coverage = "sampled_frames"
	}
	out["coverage"], out["batches"], out["start_frame"], out["step"] = coverage, rows, in.Start, in.Step
	out["next_start_frame"], out["total_frames"], out["end_of_frame_selection"] = min(next, meta.Frames), meta.Frames, next >= meta.Frames
	out["requested_page_size"], out["requested_page_complete"] = in.Page, err == nil
	out["batch_size"], out["max_dimension"], out["analysis_tokens"] = in.Batch, in.Dimension, in.Tokens
	return nil
}

func stationObserveAudio(ctx context.Context, path, dir string, start, requestedLength, duration float64, voice bool, transcribe func(context.Context, []byte) (string, error), out map[string]any) error {
	const maxPCMPageSeconds = 240 // 16 kHz mono PCM16 fits the 8 MiB WAV envelope.
	requestedEnd := min(duration, start+requestedLength)
	out["start_seconds"], out["requested_duration_seconds"] = start, requestedLength
	extract, endState := mediadecode.ExtractAudioRange, "end_of_video"
	if voice {
		extract, endState = mediadecode.ExtractVoiceAudioRange, "end_of_voice"
		out["voice_duration_seconds"] = duration
	} else {
		out["video_duration_seconds"] = duration
	}
	finish := func(state string, next float64, terminal bool) {
		out["transcription_state"], out["next_start_seconds"] = state, next
		out["requested_range_complete"], out["audio_complete"] = terminal || next+.001 >= requestedEnd, terminal || next+.001 >= duration
	}
	if start >= duration {
		finish(endState, duration, true)
		return nil
	}
	length := min(requestedEnd-start, maxPCMPageSeconds)
	if length < requestedEnd-start {
		out["continuation_reason"] = "音频分段以满足 PCM 传输限制，可从 next_start_seconds 续读。"
	}
	wav, seconds, err := extract(ctx, path, filepath.Join(dir, "audio.wav"), start, length)
	switch {
	case errors.Is(err, mediadecode.ErrNoAudioTrack):
		finish("no_audio_track", start, true)
		return nil
	case errors.Is(err, mediadecode.ErrAudioRangeEmpty):
		finish("no_audio_in_range", start+length, false)
		return nil
	case err != nil:
		if voice {
			return observationFailure("incoming_voice_decode_failed", "已取得微信语音的可播放版本，但未能解码所选音频。")
		}
		return stationMediaFailure(err, "提取音轨")
	}
	text, err := transcribe(ctx, wav)
	if err != nil && !mediaspec.IsNoSpeech(err) {
		if voice {
			return err
		}
		return stationMediaFailure(err, "转写音轨")
	}
	state := "no_recognizable_speech"
	if err == nil {
		state = "transcribed"
		out["text"], out["text_format"] = transcript.Format(ctx, text)
	}
	out["effective_duration_seconds"], out["coverage"] = seconds, "decoded_audio_range"
	out["end_seconds"] = min(start+seconds, requestedEnd)
	// Seek by the requested decoder interval if it contained only partial
	// PCM, so a short or sparse stream cannot pin the caller to the same page.
	finish(state, min(start+length, requestedEnd), false)
	return nil
}

func observationFailure(code, message string) error {
	return mediaobserve.NewFailure(code, message, nil)
}

func stationMediaFailure(err error, stage string) error {
	var safe interface{ SafeToolFailure() (string, string) }
	if errors.As(err, &safe) || errors.Is(err, wechatapp.ErrAttachmentDenied) {
		return err
	}
	code := err.Error()
	if !wechatapp.SafeShareErrorCode(code) {
		code = "wechat_video_observation_incomplete"
	}
	return mediaobserve.NewFailure(code, fmt.Sprintf("视频读取停在%s阶段。请检查当前微信连接的媒体理解配置或从未读位置继续。", stage), err)
}
