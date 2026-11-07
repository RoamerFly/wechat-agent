package admin

import (
	"context"
	"errors"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

type channelsReader = wechatapp.ChannelsReader

type receivedChannelsVideo struct {
	data   []byte
	sample videoSample
}

func (a mediaUnderstanding) understandChannels(ctx context.Context, msg linuxnative.Message) (string, error) {
	video, err := a.acquireChannelsVideo(ctx, msg)
	if err != nil {
		return "", err
	}
	return a.observeVideo(ctx, msg, video.data, video.sample, true)
}

func (a mediaUnderstanding) acquireChannelsVideo(ctx context.Context, msg linuxnative.Message) (receivedChannelsVideo, error) {
	if err := checkChannelsActor(ctx); err != nil {
		return receivedChannelsVideo{}, err
	}
	native, ok := a.manager.native.(channelsReader)
	if !ok {
		return receivedChannelsVideo{}, errors.New("channels_source_backend_missing")
	}
	return wechatapp.ReadChannelsVideo(ctx, msg, a.manager.native.Account(), native,
		func(ctx context.Context, source linuxnative.ChannelsSource) (receivedChannelsVideo, error) {
			if err := checkChannelsActor(ctx); err != nil {
				return receivedChannelsVideo{}, err
			}
			data, sample, err := a.readChannelsVideo(ctx, source)
			if denied := checkChannelsActor(ctx); denied != nil {
				return receivedChannelsVideo{}, denied
			}
			return receivedChannelsVideo{data: data, sample: sample}, err
		},
		func(ctx context.Context, source linuxnative.ChannelsSource) (linuxnative.ChannelsSource, error) {
			return a.manager.resolveReceivedChannels(ctx, msg, source)
		},
	)
}

func checkChannelsActor(ctx context.Context) error {
	if actor, explicit := sourceActorFromContext(ctx); explicit && !actor.active() {
		return errors.New("share_request_authorization_changed")
	}
	return nil
}

func (a mediaUnderstanding) readChannelsVideo(ctx context.Context, source linuxnative.ChannelsSource) ([]byte, videoSample, error) {
	var sample videoSample
	if source.State != "available" || source.MediaURL == "" {
		return nil, sample, errors.New("channels_media_locator_missing")
	}
	data, _, err := mcptools.GetPublicMedia(ctx, source.MediaURL, 50<<20)
	if err != nil {
		return nil, sample, errors.New("channels_video_download_failed")
	}
	if err := wechatapp.DecodeChannelsPayload(data, source.DecodeKey); err != nil {
		return nil, sample, err
	}
	sample, err = sampleVideo(ctx, a.manager.dir, data)
	if err != nil && (sample.Seconds == 0 || ctx.Err() != nil) {
		if source.DecodeKey == "" && err.Error() == "video_analysis_probe_failed" {
			return nil, sample, errors.New("channels_decode_key_missing")
		}
		return nil, sample, err
	}
	return data, sample, nil
}
