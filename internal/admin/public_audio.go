package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/plugins"
)

func preparePublicAudio(ctx context.Context, dir string, data []byte) (plugins.Output, error) {
	return preparePublicAudioPreview(ctx, dir, data, 0)
}
func defaultAudioPreview(request string) int {
	if strings.Contains(request, "试听") {
		return 45
	}
	for _, p := range []string{"整首", "全曲", "完整", "全首", "发完"} {
		if strings.Contains(request, p) {
			return 0
		}
	}
	return 0
}
func preparePublicAudioPreview(ctx context.Context, dir string, data []byte, preview int) (plugins.Output, error) {
	conversion := (&voiceConversion{DurationSeconds: float64(preview)}).effective()
	return preparePublicAudioRange(ctx, dir, data, conversion)
}
func preparePublicAudioRange(ctx context.Context, dir string, data []byte, conversion voiceConversion) (plugins.Output, error) {
	conversion = conversion.effective()
	if err := conversion.validate("voice"); err != nil {
		return plugins.Output{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(conversion.TimeoutSeconds*float64(time.Second)))
	defer cancel()
	tmp, err := os.MkdirTemp(dir, "public-audio-")
	if err != nil {
		return plugins.Output{}, err
	}
	defer os.RemoveAll(tmp)
	input := filepath.Join(tmp, "input.bin")
	if err = os.WriteFile(input, data, 0600); err != nil {
		return plugins.Output{}, err
	}
	args := []string{"-v", "error", "-protocol_whitelist", "file", "-format_whitelist", "wav,mp3,flac,ogg,aac,mov,matroska,webm", "-show_entries", "format=duration", "-of", "json", input}
	b, err := exec.CommandContext(ctx, "ffprobe", args...).Output()
	if err != nil || len(b) > 65536 {
		return plugins.Output{}, &mcptools.WebError{Code: "audio_prepare_failed"}
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	_ = json.Unmarshal(b, &probe)
	seconds, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return plugins.Output{}, &mcptools.WebError{Code: "audio_duration_limit"}
	}
	if conversion.StartSeconds >= seconds {
		return plugins.Output{}, toolInputError("start_seconds is outside the actual recording")
	}
	sendingSeconds := seconds - conversion.StartSeconds
	if conversion.DurationSeconds > sendingSeconds+0.01 {
		return plugins.Output{}, toolInputError("requested duration exceeds remaining recording; inspect actual duration and adjust")
	}
	if conversion.DurationSeconds > 0 {
		sendingSeconds = math.Min(sendingSeconds, conversion.DurationSeconds)
	}
	count := math.Ceil(sendingSeconds / conversion.SegmentSeconds)
	if count >= float64(int(^uint(0)>>1)) {
		return plugins.Output{}, toolInputError("requested segment count exceeds platform index range")
	}
	chunks := int(count)
	output := plugins.Output{Text: fmt.Sprintf("已从缓存原音频 %.3f 秒处准备 %.3f 秒，按每段最多 %.3f 秒分为 %d 条语音；尚未发送。", conversion.StartSeconds, sendingSeconds, conversion.SegmentSeconds, chunks)}
	output.Text += "原音频完整保留，后续要原文件可直接复用。"
	for i := 0; i < chunks; i++ {
		filename := fmt.Sprintf("audio-part-%d-of-%d.wav", i+1, chunks)
		dest := filepath.Join(tmp, filename)
		length := math.Min(conversion.SegmentSeconds, sendingSeconds-float64(i)*conversion.SegmentSeconds)
		cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-y", "-protocol_whitelist", "file", "-format_whitelist", "wav,mp3,flac,ogg,aac,mov,matroska,webm", "-threads", "1", "-ss", formatSeconds(conversion.StartSeconds+float64(i)*conversion.SegmentSeconds), "-i", input, "-vn", "-ac", "1", "-ar", "16000", "-t", formatSeconds(length), "-c:a", "pcm_s16le", "-f", "wav", dest)
		if err = cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return plugins.Output{}, ctx.Err()
			}
			return plugins.Output{}, &mcptools.WebError{Code: "audio_prepare_failed"}
		}
		part, err := os.ReadFile(dest)
		if err != nil || len(part) < 44 || len(part) > 10<<20 {
			return plugins.Output{}, &mcptools.WebError{Code: "audio_prepare_failed"}
		}
		output.Media = append(output.Media, plugins.MediaResult{Kind: "voice", Data: part, MIMEType: "audio/wav", Filename: filename})
	}
	return output, nil
}

func safeMediaFilename(name string) bool { return mediaspec.SafeFilename(name) }

func extractPublicAudioFile(ctx context.Context, dir string, data []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	tmp, err := os.MkdirTemp(dir, "public-audio-extract-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	input, output := filepath.Join(tmp, "source.mp4"), filepath.Join(tmp, "audio.m4a")
	if err = os.WriteFile(input, data, 0600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-y", "-protocol_whitelist", "file", "-format_whitelist", "mov", "-i", input, "-map", "0:a:0", "-vn", "-c:a", "copy", "-f", "ipod", output)
	if err = cmd.Run(); err != nil {
		return nil, &mcptools.WebError{Code: "audio_prepare_failed"}
	}
	info, err := os.Stat(output)
	if err != nil || info.Size() < 32 || info.Size() > toolsLimit("voice") {
		return nil, &mcptools.WebError{Code: "audio_prepare_failed"}
	}
	return os.ReadFile(output)
}
