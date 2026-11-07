package adapter

import (
	"context"
	"errors"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

func (p *SearchApp) readPlayerPlayback(ctx context.Context, request apphost.ObservationRequest) (apphost.ObservationPage, error) {
	seconds := request.Limit
	if seconds == 0 {
		seconds = 6
	}
	if request.Cursor != "" {
		return apphost.ObservationPage{}, apphost.ErrObservationInvalid
	}
	if seconds < 1 || seconds > 20 {
		return apphost.ObservationPage{}, apphost.ErrObservationPlaybackLimit
	}
	policy, err := p.load()
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	authorize := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return p.authorize(policy, request.ObservationAccess)
	}
	if err := authorize(); err != nil {
		return apphost.ObservationPage{}, err
	}
	videoID := playerFollowupVideo(policy, request.ContentID)
	entry, err := p.fetchPlayer(ctx, videoID)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	if err := authorize(); err != nil {
		return apphost.ObservationPage{}, err
	}
	sample, err := p.samplePlayer(ctx, entry.URL, time.Duration(seconds)*time.Second, authorize)
	if authErr := authorize(); authErr != nil {
		return apphost.ObservationPage{}, authErr
	}
	if err != nil {
		if errors.Is(err, stationbrowser.ErrBrowserUnavailable) {
			return apphost.ObservationPage{}, apphost.ErrObservationBrowserUnavailable
		}
		return apphost.ObservationPage{}, apphost.ErrObservationRemotePlayerUnavailable
	}
	intervals := make([]apphost.PlaybackInterval, 0, len(sample.Intervals))
	for _, interval := range sample.Intervals {
		intervals = append(intervals, apphost.PlaybackInterval{StartSeconds: interval.StartSeconds, EndSeconds: interval.EndSeconds})
	}
	return apphost.ObservationPage{
		Playback: intervals,
		PlaybackProof: &apphost.PlaybackProof{
			RequestedSeconds: seconds, MediaDurationSeconds: sample.DurationSeconds,
			Ready: sample.Ready, Ended: sample.Ended, Paused: sample.Paused,
		},
		Coverage: "独立 Edge 对已核对的抖音官方播放器进行静音采样，只记录实际播放区间",
		Complete: true,
	}, nil
}
