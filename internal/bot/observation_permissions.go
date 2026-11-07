package bot

import "wechat-bot/internal/linuxnative"

// File acquisition is not image/video understanding. A known attachment must
// remain available for the session's file tools even with vision disabled.
// Conversation eligibility and source identity are checked independently.
func (w *Worker) observationAllowed(m linuxnative.Message) bool {
	if w.config.Options == nil || w.hasScopedMedia(m) {
		return true
	}
	if m.FileAttachmentName() != "" {
		return true
	}
	switch m.Type {
	case 3, 47:
		return w.config.Options.ReadImages
	case 34:
		return true
	case 43, 49:
		return w.config.Options.ReadVideos
	default:
		return false
	}
}
