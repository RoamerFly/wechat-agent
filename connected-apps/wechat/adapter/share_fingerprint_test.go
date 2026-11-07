package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"wechat-bot/internal/linuxnative"
)

func TestNativeShareFingerprintPreservesSourceIdentityContract(t *testing.T) {
	kind := 51
	message := linuxnative.Message{
		AccountID: "account", ThreadID: "room", ID: "message", SenderID: "sender", ServerID: "server",
		CreatedAt: "2026-11-06T00:00:00Z", Direction: "incoming", Type: 49, AppMessageType: &kind,
		Body: "original card", Link: &linuxnative.LinkCard{URL: "https://example.test/card", Title: "title"},
	}
	raw, err := json.Marshal([]any{message.AccountID, message.ThreadID, message.ID, message.SenderID, message.ServerID, message.CreatedAt, message.Direction, message.Type, message.AppMessageType, message.Body, message.Link})
	if err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(raw)
	if got, want := NativeShareFingerprint(message), hex.EncodeToString(wantSum[:]); got != want {
		t.Fatalf("source identity contract changed: got %s want %s", got, want)
	}
	message.Body = "edited card"
	if NativeShareFingerprint(message) == hex.EncodeToString(wantSum[:]) {
		t.Fatal("edited native source retained old fingerprint")
	}
}

func TestReceivedSourceFingerprintBindsMediaAttachments(t *testing.T) {
	message := linuxnative.Message{AccountID: "account", ThreadID: "room", ID: "message", SenderID: "sender", Attachments: []linuxnative.Attachment{{Kind: "video", Filename: "one.mp4", MessageID: "message"}}}
	share := ReceivedSourceFingerprint(message, "share.resolve_channels")
	media := ReceivedSourceFingerprint(message, "media.observe")
	if share == media {
		t.Fatal("media read did not bind attachment identity")
	}
	message.Attachments[0].Filename = "replaced.mp4"
	if ReceivedSourceFingerprint(message, "media.observe") == media {
		t.Fatal("attachment change retained old media fingerprint")
	}
}
