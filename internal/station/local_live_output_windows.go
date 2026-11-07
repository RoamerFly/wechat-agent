//go:build windows

package station

import (
	"context"
	"errors"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/obsportable"
)

func stationLiveOutput(stationData, workspace string, programs *localLivePrograms) (apphost.LiveOutput, apphost.LiveSourceOutput, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	installation, err := obsportable.Open(ctx, stationData, workspace)
	if errors.Is(err, obsportable.ErrMissing) {
		return nil, nil, "missing"
	}
	if err != nil {
		return nil, nil, "invalid"
	}
	stream := apphost.NewManagedLiveOutputWithPreflight(func(ctx context.Context, ref apphost.LiveReference) (apphost.LiveEncoder, error) {
		media, err := programs.open(ctx, ref)
		if err != nil {
			return nil, err
		}
		if media == nil {
			return installation.Encoder(ctx)
		}
		return installation.EncoderWithMedia(ctx, &obsportable.Media{Source: media.Source, SHA256: media.SHA256,
			Bytes: media.Bytes, Width: media.Width, Height: media.Height, Loop: ref.ProgramLoop})
	}, programs.check)
	source := apphost.NewManagedLiveSource(func(ctx context.Context, ref apphost.LiveReference) (apphost.LiveLocalSource, error) {
		media, err := programs.open(ctx, ref)
		if err != nil {
			return nil, err
		}
		if media == nil {
			return nil, errLiveProgramSource
		}
		return installation.CameraWithMedia(ctx, &obsportable.Media{Source: media.Source, SHA256: media.SHA256,
			Bytes: media.Bytes, Width: media.Width, Height: media.Height, Loop: ref.ProgramLoop})
	}, programs.check)
	return stream, source, "ready"
}
