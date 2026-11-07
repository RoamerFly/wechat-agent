package mediadecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

var ErrNoAudioTrack = errors.New("video_audio_track_absent")
var ErrAudioRangeEmpty = errors.New("video_audio_range_empty")

const videoAudioFormats = "mov,matroska,webm"
const voiceAudioFormats = "wav"

// ProbeVoiceDuration accepts only local audio containers supported by the
// native playable-voice rendition, never playlists or the raw SILK original.
func ProbeVoiceDuration(ctx context.Context, path string) (float64, error) {
	return probeDuration(ctx, path, voiceAudioFormats)
}

func ExtractVoiceAudioRange(ctx context.Context, path, out string, start, length float64) ([]byte, float64, error) {
	return extractAudioRange(ctx, path, out, start, length, voiceAudioFormats)
}

// ExtractAudioRange decodes one page of the first audio stream. The caller
// owns the private staging directory and controls the requested page length.
func ExtractAudioRange(ctx context.Context, path, out string, start, length float64) ([]byte, float64, error) {
	return extractAudioRange(ctx, path, out, start, length, videoAudioFormats)
}

func extractAudioRange(ctx context.Context, path, out string, start, length float64, formats string) ([]byte, float64, error) {
	if err := ensureDecoder(); err != nil {
		return nil, 0, err
	}
	probe := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file", "-format_whitelist", formats, "-select_streams", "a:0", "-show_entries", "stream=index", "-of", "json", path)
	hideDecoderWindow(probe)
	var tracks struct {
		Streams []json.RawMessage `json:"streams"`
	}
	raw, err := probe.Output()
	if err != nil || json.Unmarshal(raw, &tracks) != nil || tracks.Streams == nil {
		return nil, 0, errors.New("video_audio_probe_failed")
	}
	if len(tracks.Streams) == 0 {
		return nil, 0, ErrNoAudioTrack
	}
	sec := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
	decode := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-y", "-max_alloc", "67108864", "-protocol_whitelist", "file", "-format_whitelist", formats, "-threads", "1", "-ss", sec(start), "-i", path, "-map", "0:a:0", "-vn", "-t", sec(length), "-threads", "1", "-filter_threads", "1", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", out)
	hideDecoderWindow(decode)
	if err := decode.Run(); err != nil {
		return nil, 0, errors.New("video_audio_decode_failed")
	}
	wav, err := os.ReadFile(out)
	if err != nil || len(wav) == 0 || len(wav) > 8<<20 {
		return nil, 0, errors.New("video_audio_limit")
	}
	duration := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file", "-format_whitelist", "wav", "-show_entries", "format=duration", "-of", "csv=p=0", out)
	hideDecoderWindow(duration)
	raw, err = duration.Output()
	if err != nil {
		return nil, 0, errors.New("video_audio_duration_invalid")
	}
	value := strings.TrimSpace(string(raw))
	if value == "N/A" || value == "0.000000" {
		return nil, 0, ErrAudioRangeEmpty
	}
	seconds, err := ParseDuration(value)
	if err != nil || seconds > length+.001 {
		return nil, 0, errors.New("video_audio_duration_invalid")
	}
	return wav, seconds, nil
}
