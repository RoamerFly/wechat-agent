package mediaobserve

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"math"

	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/mediadecode"
)

type SampledFrame struct {
	Index       int
	TimeSeconds float64
	Image       deepseek.Image
}

type FrameSamplePage struct {
	Frames                      []SampledFrame
	FrameCount, Step, NextIndex int
	DurationSeconds             float64
}

var ErrFrameSamplePosition = errors.New("video_frame_sample_position_invalid")

// SampleFramePage selects about one frame per five seconds at the average
// frame rate, with a denser selection for short clips. Times are decoded PTS,
// never inferred from the average rate. The caller owns the staging directory.
func SampleFramePage(ctx context.Context, path, dir string, start, limit int) (FrameSamplePage, error) {
	var page FrameSamplePage
	if start < 0 || limit < 1 {
		return page, ErrFrameSamplePosition
	}
	meta, err := mediadecode.Probe(ctx, path)
	if err != nil {
		return page, err
	}
	step := max(1, meta.Frames/6)
	if fps := meta.FPS; fps > 0 && fps*5 < float64(step) {
		step = max(1, int(math.Round(fps*5)))
	}
	if start >= meta.Frames || start%step != 0 {
		return page, ErrFrameSamplePosition
	}
	count := min(limit, RemainingFrames(meta.Frames, start, step))
	images, times, err := ExtractFrames(ctx, path, dir, start, count, step, 960)
	if err != nil {
		return page, err
	}
	if len(images) != count || len(times) != count {
		return page, errors.New("video_frame_sample_incomplete")
	}
	page = FrameSamplePage{FrameCount: meta.Frames, DurationSeconds: meta.Seconds, Step: step}
	for i, image := range images {
		at := times[i]
		if math.IsNaN(at) || math.IsInf(at, 0) || at < 0 || at > meta.Seconds || (i > 0 && at < times[i-1]) {
			return FrameSamplePage{}, errors.New("video_frame_sample_timeline_invalid")
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(image.Data))
		if err != nil || config.Width < 1 || config.Width > 960 || config.Height < 1 || config.Height > 960 {
			return FrameSamplePage{}, errors.New("video_frame_sample_jpeg_invalid")
		}
		page.Frames = append(page.Frames, SampledFrame{
			Index: start + i*step, TimeSeconds: at, Image: image,
		})
	}
	last := page.Frames[len(page.Frames)-1].Index
	page.NextIndex = last + min(step, meta.Frames-last)
	return page, nil
}
