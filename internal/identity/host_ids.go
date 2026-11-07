package identity

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

var legacyDeviceID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// ValidLegacyDeviceID checks identifiers persisted by the old device host.
func ValidLegacyDeviceID(id string) bool { return legacyDeviceID.MatchString(id) }

// ValidPathID accepts the historical account ID format used in private paths.
func ValidPathID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.@-", r)) {
			return false
		}
	}
	return id != "." && id != ".."
}

// RandomToken returns 32 cryptographically random bytes as lowercase hex.
func RandomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("system random source unavailable")
	}
	return hex.EncodeToString(b[:])
}
