package admin

import (
	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

// publisherDescriptionObservation keeps card copy separate from anything the
// local reader decoded. It is useful context, but it is never firsthand
// evidence and therefore must not be used to form an experience reflection.
func publisherDescriptionObservation(m linuxnative.Message) (companion.ObservationPart, bool) {
	part, ok := wechatapp.PublisherCardDescription(m)
	if !ok {
		return companion.ObservationPart{}, false
	}
	return companion.ObservationPart{Kind: companion.ObservationPublisherDescription, Range: part.Range, Excerpt: part.Excerpt}, true
}

func shareMessageDescription(m linuxnative.Message) string {
	return wechatapp.CardDescription(m)
}
func sharePublicURL(raw string) string {
	return wechatapp.PublicShareURL(raw)
}
