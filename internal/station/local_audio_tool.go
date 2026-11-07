package station

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"

	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

const (
	localAudioPackage    = "audio-workshop"
	localAudioToolName   = "project_audio_prepare"
	localAudioCapability = "audio.prepare"
)

func (t *localToolSet) localAudioTools(ctx context.Context) []mcptools.Tool {
	host := t.projectHostForDiscovery()
	if host == nil {
		return nil
	}
	if _, err := installedLocalProject(ctx, host, projectOperation(localAudioToolName)); err != nil {
		return nil
	}
	return []mcptools.Tool{mcptools.NewTool(localAudioToolName, "从已登记的整数 PCM WAV 成品截取指定时间段，可混为单声道，输出原采样率的 16 位 PCM WAV。用成品编号 artifact_id，无需传路径；返回实际覆盖及可下载的新成品编号。", `{"type":"object","properties":{"artifact_id":{"type":"string"},"filename":{"type":"string","maxLength":120},"start_seconds":{"type":"number","minimum":0},"duration_seconds":{"type":"number","exclusiveMinimum":0},"channels":{"type":"string","enum":["preserve","mono"]}},"required":["artifact_id"],"additionalProperties":false}`)}
}

func (t *localToolSet) prepareLocalAudio(ctx context.Context, raw json.RawMessage) (string, error) {
	var input struct {
		ArtifactID string   `json:"artifact_id"`
		Filename   string   `json:"filename,omitempty"`
		Start      float64  `json:"start_seconds,omitempty"`
		Duration   *float64 `json:"duration_seconds,omitempty"`
		Channels   string   `json:"channels"`
	}
	if strictLocalMemoryDecode(raw, &input) != nil || !artifactID(input.ArtifactID, "work-") {
		return "", mcptools.ArgumentError("$.artifact_id", "请选择已登记的 WAV 成品编号，并使用音频工具声明的参数")
	}
	if input.Filename != "" && (!filepath.IsLocal(input.Filename) || !mediaspec.SafeFilename(input.Filename) || len([]rune(input.Filename)) > 120 || !strings.EqualFold(filepath.Ext(input.Filename), ".wav")) {
		return "", mcptools.ArgumentError("$.filename", "请使用以 .wav 结尾的单个文件名")
	}
	if input.Start < 0 || input.Duration != nil && *input.Duration <= 0 {
		return "", mcptools.ArgumentError("$.start_seconds", "起点须为非负数，指定的时长须大于零")
	}
	if input.Channels == "" {
		input.Channels = "preserve"
	}
	if input.Channels != "preserve" && input.Channels != "mono" {
		return "", mcptools.ArgumentError("$.channels", "声道只支持 preserve 或 mono")
	}
	arguments, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return t.invokeLocalProjectTool(ctx, localAudioToolName, arguments, input.ArtifactID)
}

type localAudioVerification struct {
	Format        string `json:"format"`
	SampleRate    uint32 `json:"sample_rate"`
	Channels      uint16 `json:"channels"`
	Frames        uint32 `json:"frames"`
	BitsPerSample uint16 `json:"bits_per_sample"`
}

type localAudioCoverage struct {
	StartSeconds     float64  `json:"start_seconds"`
	RequestedSeconds *float64 `json:"requested_duration_seconds"`
	DurationSeconds  float64  `json:"actual_duration_seconds"`
	StartFrame       uint32   `json:"start_frame"`
	Frames           uint32   `json:"frames"`
	EOF              bool     `json:"eof"`
}

func (p *localProjectPolicy) validateAudio(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	var result pluginruntime.VerifiedResult
	if err := p.Check(ctx, job, cap); err != nil {
		return result, err
	}
	var out struct {
		OK          bool                `json:"ok"`
		Error       string              `json:"error"`
		InputSHA256 string              `json:"input_sha256"`
		Coverage    *localAudioCoverage `json:"coverage"`
		Outputs     []struct {
			Path         string                 `json:"path"`
			Name         string                 `json:"name"`
			MIMEType     string                 `json:"mime_type"`
			SHA256       string                 `json:"sha256"`
			Size         int64                  `json:"size"`
			Verification localAudioVerification `json:"verification"`
		} `json:"outputs"`
	}
	if len(raw) > 32<<10 || json.Unmarshal(raw, &out) != nil {
		return result, errors.New("音频工坊返回格式无效")
	}
	if !out.OK && len(out.Outputs) == 0 && safeLocalAudioCode(out.Error) {
		result.Failed = true
		result.Result, _ = json.Marshal(map[string]string{"reason": out.Error})
		return result, nil
	}
	if !out.OK || p.input == nil || out.InputSHA256 != p.input.artifact.SHA256 || len(out.Outputs) != 1 {
		return result, errors.New("音频工坊未返回与输入成品匹配的结果")
	}
	entry := out.Outputs[0]
	if entry.MIMEType != "audio/wav" || !artifactID("sha-"+entry.SHA256, "sha-") {
		return result, errors.New("音频工坊输出格式或摘要无效")
	}
	info := entry.Verification
	var requested struct {
		Start    float64  `json:"start_seconds"`
		Duration *float64 `json:"duration_seconds"`
		Channels string   `json:"channels"`
	}
	coverage := out.Coverage
	rate := float64(info.SampleRate)
	if json.Unmarshal(job.Arguments, &requested) != nil || coverage == nil || rate == 0 ||
		requested.Channels == "mono" && info.Channels != 1 ||
		coverage.Frames != info.Frames || float64(coverage.StartFrame) != math.Floor(requested.Start*rate) ||
		coverage.StartSeconds != float64(coverage.StartFrame)/rate || coverage.DurationSeconds != float64(info.Frames)/rate ||
		(coverage.RequestedSeconds == nil) != (requested.Duration == nil) {
		return result, errors.New("音频工坊的声道或覆盖回执与请求不一致")
	}
	if requested.Duration != nil {
		limit := math.Ceil(*requested.Duration * rate)
		if *coverage.RequestedSeconds != *requested.Duration || float64(info.Frames) > limit || !coverage.EOF && float64(info.Frames) != limit {
			return result, errors.New("音频工坊的实际时长与请求不一致")
		}
	} else if !coverage.EOF {
		return result, errors.New("音频工坊未处理到源音频末尾")
	}
	artifact, err := p.publishOutput(ctx, job, cap, entry.Path, entry.Name, ".wav", entry.SHA256, func(file *os.File, size int64) error {
		return verifyLocalPCM(file, size, entry.Size, info)
	})
	if err != nil {
		return result, err
	}
	result.Assets = []string{artifact.ID}
	result.Result, _ = json.Marshal(struct {
		localAudioVerification
		Coverage *localAudioCoverage `json:"coverage"`
	}{info, coverage})
	return result, nil
}

// wave writes a canonical 44-byte PCM header. Recheck the actual file, length,
// byte rate and number of frames before accepting the worker's claimed result.
func verifyLocalPCM(file *os.File, size, reportedSize int64, info localAudioVerification) error {
	var header [44]byte
	_, err := file.ReadAt(header[:], 0)
	u16, u32 := binary.LittleEndian.Uint16, binary.LittleEndian.Uint32
	dataBytes := int64(info.Frames) * int64(info.Channels) * 2
	blockBytes := uint32(info.Channels) * 2
	if err != nil || size != reportedSize || size != dataBytes+44 || size > stationAttachmentLimit ||
		info.Format != "wav" || info.BitsPerSample != 16 || info.Channels == 0 || info.SampleRate == 0 || info.Frames == 0 ||
		string(header[:4]) != "RIFF" || int64(u32(header[4:8])) != size-8 || string(header[8:16]) != "WAVEfmt " ||
		u32(header[16:20]) != 16 || u16(header[20:22]) != 1 || u16(header[22:24]) != info.Channels ||
		u32(header[24:28]) != info.SampleRate || uint64(u32(header[28:32])) != uint64(info.SampleRate)*uint64(blockBytes) ||
		uint32(u16(header[32:34])) != blockBytes || u16(header[34:36]) != 16 || string(header[36:40]) != "data" ||
		int64(u32(header[40:44])) != dataBytes {
		return errors.New("成品的 PCM WAV 格式、帧数或大小与回执不一致")
	}
	return nil
}

func safeLocalAudioCode(code string) bool {
	switch code {
	case "audio_pcm_wav_required", "audio_input_hash_mismatch", "audio_range_empty",
		"audio_wav_format_not_supported", "audio_output_not_authorized", "audio_input_not_authorized",
		"audio_artifact_not_authorized", "audio_input_missing", "audio_input_too_large",
		"audio_wav_compression_not_supported", "audio_wav_invalid", "audio_wav_unexpected_eof",
		"audio_wav_frame_count_invalid", "audio_wav_truncated", "audio_output_unwritable", "audio_output_failed",
		"audio_arguments_invalid", "audio_start_seconds_invalid", "audio_duration_seconds_invalid",
		"audio_channels_invalid", "audio_filename_invalid", "audio_output_too_large":
		return true
	default:
		return false
	}
}
