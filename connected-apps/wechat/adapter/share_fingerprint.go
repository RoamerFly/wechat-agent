package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"

	"wechat-bot/internal/linuxnative"
)

var shareErrorCode = regexp.MustCompile(`^[a-z_]{1,96}$`)

// SafeShareErrorCode accepts only opaque stable codes for user-visible status.
// Provider text, URLs and credentials must remain outside the activity log.
func SafeShareErrorCode(code string) bool { return shareErrorCode.MatchString(code) }

// NativeShareFingerprint binds a task to immutable native message identity and
// content, never to observations or display names.
func NativeShareFingerprint(m linuxnative.Message) string {
	b, _ := json.Marshal([]any{m.AccountID, m.ThreadID, m.ID, m.SenderID, m.ServerID, m.CreatedAt, m.Direction, m.Type, m.AppMessageType, m.Body, m.Link})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ReceivedSourceFingerprint also includes attachment identity for media reads.
func ReceivedSourceFingerprint(m linuxnative.Message, tool string) string {
	if tool != "media.observe" && tool != "media.transcribe" && tool != "media.listen" {
		return NativeShareFingerprint(m)
	}
	b, _ := json.Marshal([]any{NativeShareFingerprint(m), m.Attachments})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
