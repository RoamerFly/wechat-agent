package station

import (
	"context"
	"errors"
	"time"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

func localDouyinWebFieldAllowed(record localDouyinWebRecord, contentID string, field apphost.ObservationField) bool {
	if field == apphost.ObservationPageText {
		return true
	}
	if contentID == douyinapp.WebReadContentID || localDouyinWebScope(record) != "home_and_videos" {
		return false
	}
	return field == apphost.ObservationPlayback && record.PlaybackEnabled || field == apphost.ObservationAudio && record.AudioEnabled || field == apphost.ObservationVisual && record.VisualEnabled || field == apphost.ObservationComments && record.CommentsEnabled
}

func (p *localDouyinWebObservation) readPlayback(ctx context.Context, a localDouyinWebAccess, in apphost.ObservationRequest, target localDouyinWebTarget, authorize func() error) (apphost.ObservationPage, error) {
	if in.Cursor != "" || target.videoID == "" {
		return apphost.ObservationPage{}, apphost.ErrObservationInvalid
	}
	seconds := in.Limit
	if seconds == 0 {
		seconds = 6
	}
	if seconds < 1 || seconds > 20 {
		return apphost.ObservationPage{}, apphost.ErrObservationPlaybackLimit
	}
	m := p.home.douyinWeb
	m.mu.Lock()
	sampleVideo, serviceCtx := m.sampleVideo, m.ctx
	m.mu.Unlock()
	if sampleVideo == nil {
		return apphost.ObservationPage{}, apphost.ErrObservationBrowserUnavailable
	}
	playCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second+25*time.Second)
	defer cancel()
	stop := context.AfterFunc(serviceCtx, cancel)
	defer stop()
	if err := authorize(); err != nil {
		return apphost.ObservationPage{}, err
	}
	sample, err := sampleVideo(playCtx, a.profile, target.videoID, time.Duration(seconds)*time.Second, authorize)
	// Revocation wins even if the browser returned a seemingly valid sample.
	if authErr := authorize(); authErr != nil {
		return apphost.ObservationPage{}, authErr
	}
	if err != nil {
		if errors.Is(err, stationbrowser.ErrPlaybackUnavailable) {
			return apphost.ObservationPage{}, apphost.ErrObservationWebPlaybackUnavailable
		}
		return apphost.ObservationPage{}, localDouyinWebReadError(err)
	}
	intervals := make([]apphost.PlaybackInterval, 0, len(sample.Intervals))
	for _, interval := range sample.Intervals {
		intervals = append(intervals, apphost.PlaybackInterval{StartSeconds: interval.StartSeconds, EndSeconds: interval.EndSeconds})
	}
	coverage := "独立 Edge 对已核对原视频编号及媒体来源的网页播放器静音采样，仅表示实际播放区间，不包含画面理解或音轨转写"
	if len(intervals) == 0 {
		coverage = "已核对视频及媒体来源，但未记录到实际播放区间；本次不计为观看，可重试"
		if !sample.Ready {
			coverage = "已核对视频及媒体来源，但播放器尚未就绪，未记录观看；可稍后重试"
		}
	}
	return apphost.ObservationPage{
		Playback: intervals,
		PlaybackProof: &apphost.PlaybackProof{
			RequestedSeconds: seconds, MediaDurationSeconds: sample.DurationSeconds,
			Ready: sample.Ready, Ended: sample.Ended, Paused: sample.Paused,
		},
		Coverage: coverage,
		Complete: true,
	}, nil
}
