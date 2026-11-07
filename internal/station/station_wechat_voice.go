package station

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/mediadecode"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
)

type stationVoiceInput struct {
	Message  string  `json:"message_id"`
	Start    float64 `json:"start_seconds"`
	Duration float64 `json:"duration_seconds"`
	Timeout  int     `json:"timeout_seconds"`
}

type stationMediaObserver struct {
	tools      *stationAttachmentTools
	imageSince time.Time
	videoSince time.Time
}

func (a stationMediaObserver) HandlesType(kind int) bool {
	return kind == 3 || kind == 34 || kind == 43
}

func (a stationMediaObserver) SupportsMedia(ctx context.Context, m linuxnative.Message) bool {
	if !a.HandlesType(m.Type) || m.Direction != "incoming" || !a.tools.scopeAllowed(companion.Input{Account: m.AccountID, Thread: m.ThreadID, MessageTime: m.CreatedAt}, a.tools.grants) || a.tools.shareHost == nil {
		return false
	}
	if m.Type == 3 {
		at, err := time.Parse(time.RFC3339Nano, m.CreatedAt)
		return err == nil && !a.imageSince.IsZero() && !at.Before(a.imageSince) && a.tools.imageObservationAvailable(ctx, m.AccountID)
	}
	if m.Type == 43 {
		audio, vision := a.videoObservationModes(ctx, m)
		return audio || vision
	}
	host, err := a.tools.shareHost()
	if err != nil || host.store.Account() != m.AccountID {
		return false
	}
	p, err := host.store.Installed(ctx, localasr.PluginID)
	if err != nil || !p.Enabled || p.Removed {
		return false
	}
	c, err := host.store.Configuration(ctx, localasr.PluginID)
	backend, _ := stationASRBackend(c.Values)
	return err == nil && backend == "local"
}

func (a stationMediaObserver) videoObservationModes(ctx context.Context, m linuxnative.Message) (audio, vision bool) {
	at, err := time.Parse(time.RFC3339Nano, m.CreatedAt)
	if err != nil || a.videoSince.IsZero() || at.Before(a.videoSince) || a.tools.shareHost == nil {
		return false, false
	}
	host, err := a.tools.shareHost()
	if err != nil || host.store.Account() != m.AccountID {
		return false, false
	}
	p, err := host.store.Installed(ctx, localasr.PluginID)
	if err != nil || !p.Enabled || p.Removed {
		return false, false
	}
	c, err := host.store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		return false, false
	}
	backend, _ := stationASRBackend(c.Values)
	vision = host.controller != nil && stationVisionConfigured(c)
	if backend == "local" {
		return host.localASR.transcribe != nil, vision
	}
	var endpoint, model string
	audio = backend == "remote" && host.controller != nil && json.Unmarshal(c.Values["asr_endpoint"], &endpoint) == nil && strings.TrimSpace(endpoint) != "" &&
		json.Unmarshal(c.Values["asr_model"], &model) == nil && strings.TrimSpace(model) != "" && slices.Contains(c.ConfiguredSecrets, "asr_key")
	return audio, vision
}

func (a stationMediaObserver) Analyze(ctx context.Context, m linuxnative.Message) (string, error) {
	text, _, err := a.AnalyzeWithEvidence(ctx, m)
	return text, err
}

func (a stationMediaObserver) AnalyzeWithEvidence(ctx context.Context, m linuxnative.Message) (string, bot.MediaObservationEvidence, error) {
	ref, ok := companion.MediaSourceFromContext(ctx)
	if !ok || !a.HandlesType(ref.MessageType) || ref.MessageType != m.Type || ref.MessageID != m.ID || ref.Account != m.AccountID || ref.Thread != m.ThreadID || ref.Sender != m.SenderID {
		return "", bot.MediaObservationEvidence{}, wechatapp.ErrAttachmentDenied
	}
	t := *a.tools
	t.trace = companion.Input{Channel: "wechat", Account: ref.Account, Thread: ref.Thread, Sender: ref.Sender, TraceID: ref.MessageID, MessageTime: ref.OccurredAt, Source: &ref}
	if m.Type == 43 {
		if m.Direction != "incoming" || !t.allowed() {
			return "", bot.MediaObservationEvidence{}, wechatapp.ErrAttachmentDenied
		}
		audio, vision := a.videoObservationModes(ctx, m)
		if !audio && !vision {
			return "", bot.MediaObservationEvidence{}, observationFailure("video_observation_unavailable", "当前微信连接没有可用的视频观察方式。")
		}
		text, evidence, err := t.readAutomaticVideoEvidence(ctx, m.ID, 45, audio, vision)
		if err != nil {
			return "", bot.MediaObservationEvidence{}, err
		}
		return "[普通微信视频实际内容观察]\n覆盖：" + evidence.ObservedRange + "\n" + text, evidence, nil
	}
	if m.Type == 3 {
		if !a.SupportsMedia(ctx, m) {
			return "", bot.MediaObservationEvidence{}, wechatapp.ErrAttachmentDenied
		}
		observed, err := t.readImageObservation(ctx, stationImageInput{Message: m.ID})
		if err != nil {
			return "", bot.MediaObservationEvidence{}, err
		}
		return observed.Observation, bot.MediaObservationEvidence{ObservedRange: observed.Coverage, Excerpt: observed.Observation, Parts: []companion.ObservationPart{{Kind: companion.ObservationVisualFrames, Range: observed.Coverage, Excerpt: observed.Observation}}}, nil
	}
	out, err := t.transcribeVoice(ctx, stationVoiceInput{Message: ref.MessageID, Duration: 90, Timeout: 180})
	if err != nil {
		return "", bot.MediaObservationEvidence{}, err
	}
	if out["transcription_state"] == "no_recognizable_speech" {
		return "这条微信语音没有识别到可辨认的词句。", bot.MediaObservationEvidence{}, nil
	}
	text, ok := out["text"].(string)
	if !ok || text == "" {
		return "", bot.MediaObservationEvidence{}, observationFailure("incoming_voice_no_audio", "微信语音没有可读取的音频。")
	}
	coverage := fmt.Sprintf("微信语音音轨 %g–%g 秒", out["start_seconds"], out["end_seconds"])
	return text, bot.MediaObservationEvidence{ObservedRange: coverage, Excerpt: text, Parts: []companion.ObservationPart{{Kind: companion.ObservationAudioTranscript, Range: coverage, Excerpt: text}}}, nil
}

func (t *stationAttachmentTools) voiceTranscription(ctx context.Context, raw json.RawMessage) (string, error) {
	in := stationVoiceInput{Duration: 90, Timeout: 180}
	if strictLocalMemoryDecode(raw, &in) != nil || in.Message == "" || len(in.Message) > 8192 || in.Start < 0 || in.Duration <= 0 || in.Timeout < 0 || int64(in.Timeout) > int64((1<<63-1)/int64(time.Second)) {
		return "", errors.New("wechat_voice_parameters_invalid")
	}
	out, err := t.transcribeVoice(ctx, in)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(out)
	return string(data), err
}

func (t *stationAttachmentTools) transcribeVoice(ctx context.Context, in stationVoiceInput) (map[string]any, error) {
	if t.store == nil || !t.allowed() {
		return nil, wechatapp.ErrAttachmentDenied
	}
	if in.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(in.Timeout)*time.Second)
		defer cancel()
	}
	access, err := t.attachmentAccess(t.trace, in.Message, t.allowed)
	if err != nil || access.ref.MessageType != 34 {
		return nil, wechatapp.ErrAttachmentDenied
	}
	source, err := access.source(ctx, access.thread, access.message)
	if err != nil || source.Kind != "voice" {
		return nil, wechatapp.ErrAttachmentDenied
	}
	epoch, err := t.store.RecallEpoch(ctx)
	if err != nil {
		return nil, err
	}
	if t.shareHost == nil {
		return nil, observationFailure("media_plugin_runtime_unavailable", "当前微信连接未准备媒体理解工具。")
	}
	host, err := t.shareHost()
	if err != nil || host.store.Account() != source.Account {
		return nil, observationFailure("media_plugin_runtime_unavailable", "无法打开当前连接的媒体理解工具。")
	}
	installed, err := host.store.Installed(ctx, localasr.PluginID)
	if err != nil || !installed.Enabled || installed.Removed {
		return nil, observationFailure("media_plugin_not_enabled", "请在当前微信连接启用媒体理解工具。")
	}
	config, err := host.store.Configuration(ctx, localasr.PluginID)
	backend, _ := stationASRBackend(config.Values)
	if err != nil || backend != "local" || host.localASR.transcribe == nil {
		return nil, observationFailure("incoming_voice_local_asr_unavailable", "请为当前微信连接选择本机识别，并启用站点的本地媒体识别工具。")
	}
	invocation := pluginhost.Invocation{Account: source.Account, Thread: source.Thread, Requester: t.trace.Sender, Purpose: "transcribe authorized WeChat voice", RequestKey: "station-voice-" + source.id(), Authorization: source.id(), OriginSourceSender: source.Sender, RequestThread: t.trace.Thread, EvidenceEpoch: epoch}
	grant := pluginhost.Grant{Account: source.Account, Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}
	if pluginhost.Authorize(installed.Package, grant, invocation, []string{"artifact.read", "model.invoke"}) != nil {
		return nil, observationFailure("media_plugin_permissions_changed", "当前微信连接缺少媒体处理或模型调用授权。")
	}
	check := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := access.confirmObservationSource(ctx, source, epoch); err != nil {
			return err
		}
		p, pErr := host.store.Installed(ctx, localasr.PluginID)
		c, cErr := host.store.Configuration(ctx, localasr.PluginID)
		if pErr != nil || cErr != nil || !p.Enabled || p.Removed || p.Revision != installed.Revision || p.Digest != installed.Digest || c.Revision != config.Revision {
			return observationFailure("media_plugin_configuration_changed", "转写期间媒体理解工具或授权已变更。")
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(t.dir, "verified-voice-")
	if err != nil {
		return nil, observationFailure("media_staging_failed", "无法创建当前连接的私有解码目录。")
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "original.voice")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, observationFailure("media_staging_failed", "无法暂存微信语音。")
	}
	rendered, transferErr := t.native.StreamVerifiedPlayableVoice(ctx, source.Message, stationAttachmentLimit, attachmentGrantWriter{allow: func() bool { return access.Allowed(ctx, source.Thread) }, out: file})
	closeErr := file.Close()
	if err := check(); err != nil {
		return nil, err
	}
	if transferErr != nil || closeErr != nil {
		return nil, observationFailure("incoming_voice_playable_unavailable", "微信原生服务未能提供经核验的可播放版本。")
	}
	duration, err := mediadecode.ProbeVoiceDuration(ctx, path)
	if err != nil {
		return nil, observationFailure("incoming_voice_decode_failed", "可播放版本无法解码。请核对原生语音转换和本机 FFmpeg。")
	}
	out := map[string]any{"message_id": source.Message, "sender_id": source.Sender, "occurred_at": source.Occurred, "sha256": rendered.SHA256,
		"source_variant": rendered.Variant, "asr_backend": "local", "untrusted_external_content": true}
	err = stationObserveAudio(ctx, path, dir, in.Start, in.Duration, duration, true, func(ctx context.Context, wav []byte) (string, error) {
		text, err := host.localASR.transcribe(ctx, wav, check)
		if err != nil && !mediaspec.IsNoSpeech(err) {
			return "", observationFailure("incoming_voice_local_asr_failed", "语音已解码，但本机语音识别未完成。请核对已登记的本机识别资源。")
		}
		return text, err
	}, out)
	if checkErr := check(); checkErr != nil {
		return nil, checkErr
	}
	return out, err
}
