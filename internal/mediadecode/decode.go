// Package mediadecode owns the bounded local ffprobe/ffmpeg protocol shared by
// media understanding and the future media plugin adapter. It deliberately has
// no model, account, task, or WeChat dependency.
package mediadecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Image struct {
	Data []byte
	MIME string
}

type SampleResult struct {
	Frames  []Image
	Seconds float64
	Times   []float64
}

type Metadata struct {
	Seconds float64
	Frames  int
	FPS     float64
	Width   int
	Height  int
}

func FormatSeconds(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

func ParseDuration(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, errors.New("video_analysis_duration_limit")
	}
	return v, nil
}

func ensureDecoder() error {
	for _, program := range []string{"ffprobe", "ffmpeg"} {
		if _, err := exec.LookPath(program); err != nil {
			return errors.New("video_analysis_decoder_unavailable")
		}
	}
	return nil
}

// Available reports whether local decoding can be attempted.
func Available() bool { return ensureDecoder() == nil }

// ProbeDuration reads the local container duration without scanning frames.
func ProbeDuration(ctx context.Context, path string) (float64, error) {
	return probeDuration(ctx, path, videoAudioFormats)
}

func probeDuration(ctx context.Context, path, formats string) (float64, error) {
	probe := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file", "-format_whitelist", formats, "-show_entries", "format=duration", "-of", "csv=p=0", path)
	hideDecoderWindow(probe)
	duration, err := probe.Output()
	if err != nil {
		return 0, errors.New("video_analysis_probe_failed")
	}
	return ParseDuration(strings.TrimSpace(string(duration)))
}

// Sample writes the received bytes into an owner-provided private directory,
// probes its duration, then decodes the historical six evenly spaced frames.
// It preserves partial timing evidence on a later extraction error.
func Sample(ctx context.Context, dir string, data []byte) (SampleResult, error) {
	var sample SampleResult
	if err := ensureDecoder(); err != nil {
		return sample, err
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	tmp, err := os.MkdirTemp(dir, "video-analysis-")
	if err != nil {
		return sample, errors.New("video_analysis_staging_failed")
	}
	defer os.RemoveAll(tmp)
	input := filepath.Join(tmp, "input.bin")
	if os.WriteFile(input, data, 0600) != nil {
		return sample, errors.New("video_analysis_staging_failed")
	}
	seconds, err := ProbeDuration(ctx, input)
	if err != nil {
		return sample, err
	}
	sample.Seconds = seconds
	for i := 0; i < 6; i++ {
		at := seconds * (float64(i) + 0.5) / 6
		// A zero-frame decode can exit successfully. Never reuse a previous
		// iteration's file as evidence for this sampling time.
		output := filepath.Join(tmp, fmt.Sprintf("frame-%d.jpg", i))
		command := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-y", "-max_alloc", "67108864", "-protocol_whitelist", "file", "-format_whitelist", "mov,matroska,webm", "-ss", FormatSeconds(at), "-threads", "1", "-i", input, "-map", "0:v:0", "-frames:v", "1", "-threads", "1", "-filter_threads", "1", "-vf", "scale=960:960:force_original_aspect_ratio=decrease", "-q:v", "4", output)
		if command.Run() != nil {
			return sample, errors.New("video_frame_extraction_failed")
		}
		data, err := os.ReadFile(output)
		if err != nil || len(data) == 0 || len(data) > 2<<20 {
			return sample, errors.New("video_frame_invalid")
		}
		sample.Frames = append(sample.Frames, Image{Data: data, MIME: "image/jpeg"})
		sample.Times = append(sample.Times, at)
	}
	return sample, nil
}

// Probe inspects the first video stream using the existing local-only protocol.
func Probe(ctx context.Context, path string) (Metadata, error) {
	return probeVideo(ctx, path, true)
}

// ProbeMetadata reads the first video stream's container metadata without
// counting frames across the video. It does not prove complete decoding;
// callers can separately decode the frames needed for their operation.
func ProbeMetadata(ctx context.Context, path string) (Metadata, error) {
	return probeVideo(ctx, path, false)
}

func probeVideo(ctx context.Context, path string, countFrames bool) (Metadata, error) {
	arguments := []string{"-v", "error", "-protocol_whitelist", "file", "-format_whitelist", "mov,matroska,webm", "-select_streams", "v:0"}
	if countFrames {
		arguments = append(arguments, "-count_frames")
	}
	arguments = append(arguments, "-show_entries", "stream=nb_read_frames,avg_frame_rate,width,height:format=duration", "-of", "json", path)
	command := exec.CommandContext(ctx, "ffprobe", arguments...)
	hideDecoderWindow(command)
	raw, err := command.Output()
	if errors.Is(err, exec.ErrNotFound) {
		return Metadata{}, errors.New("video_analysis_decoder_unavailable")
	}
	if err != nil {
		return Metadata{}, errors.New("video_inspect_probe_failed")
	}
	return parseVideoMetadata(raw, countFrames)
}

func parseVideoMetadata(raw []byte, countFrames bool) (Metadata, error) {
	var result Metadata
	var info struct {
		Streams []struct {
			Count  string `json:"nb_read_frames"`
			Rate   string `json:"avg_frame_rate"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(raw, &info) != nil || len(info.Streams) != 1 {
		return result, errors.New("video_inspect_probe_failed")
	}
	stream := info.Streams[0]
	result.Frames, _ = strconv.Atoi(stream.Count)
	result.Seconds, _ = strconv.ParseFloat(info.Format.Duration, 64)
	result.Width, result.Height = stream.Width, stream.Height
	rate := strings.Split(stream.Rate, "/")
	if len(rate) == 2 {
		numerator, _ := strconv.ParseFloat(rate[0], 64)
		denominator, _ := strconv.ParseFloat(rate[1], 64)
		if denominator > 0 {
			result.FPS = numerator / denominator
		}
	}
	if result.Seconds <= 0 || math.IsNaN(result.Seconds) || math.IsInf(result.Seconds, 0) ||
		(countFrames && result.Frames < 1) ||
		(!countFrames && (result.Width < 1 || result.Height < 1 || result.FPS <= 0 || math.IsNaN(result.FPS) || math.IsInf(result.FPS, 0))) {
		return result, errors.New("video_inspect_metadata_invalid")
	}
	return result, nil
}

var framePTS = regexp.MustCompile(`pts_time:([0-9.+eE-]+)`)

// ExtractFramesSized decodes exactly the requested frame indexes. The caller
// owns dir and is responsible for removing its per-batch staging directory.
func ExtractFramesSized(ctx context.Context, path, dir string, start, count, step, dimension int) ([]Image, []float64, error) {
	end := start + (count-1)*step
	filter := fmt.Sprintf("select=between(n\\,%d\\,%d)*not(mod(n-%d\\,%d)),showinfo,scale=%d:%d:force_original_aspect_ratio=decrease", start, end, start, step, dimension, dimension)
	command := exec.CommandContext(ctx, "ffmpeg", "-v", "info", "-nostdin", "-y", "-max_alloc", "67108864", "-protocol_whitelist", "file", "-format_whitelist", "mov,matroska,webm", "-threads", "1", "-i", path, "-map", "0:v:0", "-vf", filter, "-fps_mode", "vfr", "-frames:v", strconv.Itoa(count), "-threads", "1", "-filter_threads", "1", "-q:v", "4", filepath.Join(dir, "frame-%06d.jpg"))
	hideDecoderWindow(command)
	log, err := command.CombinedOutput()
	if err != nil {
		return nil, nil, errors.New("video_inspect_decode_failed")
	}
	matches := framePTS.FindAllStringSubmatch(string(log), -1)
	images := []Image{}
	times := []float64{}
	for i := 0; i < count; i++ {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("frame-%06d.jpg", i+1)))
		if err != nil || len(data) == 0 || len(data) > 8<<20 || i >= len(matches) {
			return nil, nil, errors.New("video_inspect_frame_evidence_incomplete")
		}
		at, err := strconv.ParseFloat(matches[i][1], 64)
		if err != nil {
			return nil, nil, errors.New("video_inspect_timestamp_invalid")
		}
		images = append(images, Image{Data: data, MIME: "image/jpeg"})
		times = append(times, at)
	}
	return images, times, nil
}
