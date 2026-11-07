package adapter

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"wechat-bot/internal/apphost"
)

// Seal the exact result and keyword to the current private search policy.
// A result remains usable after pagination or restart, and is revoked with
// the search policy. The client token is never sent to the player endpoint.
func playerFollowupID(policy loadedSettings, keywordID, videoID string) string {
	identity := "player." + keywordID + "." + videoID
	mac := hmac.New(sha256.New, []byte(policy.ClientToken))
	mac.Write([]byte("douyin-player-followup-v1\x00" + policy.version + "\x00" + identity))
	return identity + "." + hex.EncodeToString(mac.Sum(nil))
}

func playerFollowupVideo(policy loadedSettings, contentID string) string {
	if !policy.FollowPublicVideos || len(contentID) > 132 {
		return ""
	}
	parts := strings.Split(contentID, ".")
	if len(parts) != 4 || parts[0] != "player" || !decimalID.MatchString(parts[2]) {
		return ""
	}
	for _, keyword := range policy.Keywords {
		if parts[1] == keywordContentID(keyword) {
			if hmac.Equal([]byte(contentID), []byte(playerFollowupID(policy, parts[1], parts[2]))) {
				return parts[2]
			}
			break
		}
	}
	return ""
}

func (p *SearchApp) readPlayerEntry(ctx context.Context, request apphost.ObservationRequest) (apphost.ObservationPage, error) {
	if request.Cursor != "" || request.Limit != 0 {
		return apphost.ObservationPage{}, apphost.ErrObservationInvalid
	}
	policy, err := p.load()
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	if err := p.authorize(policy, request.ObservationAccess); err != nil {
		return apphost.ObservationPage{}, err
	}
	videoID := playerFollowupVideo(policy, request.ContentID)
	entry, err := p.fetchPlayer(ctx, videoID)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	if err := p.authorize(policy, request.ObservationAccess); err != nil {
		return apphost.ObservationPage{}, err
	}
	title := entry.Title
	if title == "" {
		title = "抖音作品 " + videoID
	}
	chunk := apphost.ObservationChunk{Text: title, SourceID: videoID, SourceURL: entry.URL}
	if policy.SamplePublicPlayback {
		chunk.FollowUpContentID = request.ContentID
		chunk.FollowUpField = apphost.ObservationPlayback
	}
	return apphost.ObservationPage{
		Chunks:   []apphost.ObservationChunk{chunk},
		Coverage: "已核对抖音官方播放器入口，尚未播放或读取音视频",
		Complete: true,
	}, nil
}
