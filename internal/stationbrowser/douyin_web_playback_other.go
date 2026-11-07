//go:build !windows

package stationbrowser

import (
	"context"
	"time"
)

func SampleDouyinWebVideo(context.Context, string, string, time.Duration, func() error) (PlaybackSample, error) {
	return PlaybackSample{}, ErrBrowserUnavailable
}
