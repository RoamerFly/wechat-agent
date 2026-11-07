// Package adapter owns the WeChat application identity. Private connections
// and credentials are resolved by the station host.
package adapter

import (
	"crypto/sha256"
	"encoding/hex"

	"wechat-bot/internal/apphost"
)

const ID = "wechat"
const LegacyInstallationID = "wechat-legacy"

func Application() apphost.Application {
	return apphost.Application{ID: ID, Name: "微信", InstallationID: LegacyInstallationID, Registration: "legacy"}
}

func ConnectionID(account string) string {
	sum := sha256.Sum256([]byte(account))
	return ID + "-" + hex.EncodeToString(sum[:])
}

// AccountDirectory is the stable directory component used by the former
// standalone WeChat host. Keep it with the application adapter so migration
// code can read an existing account layout without importing the old host's
// account registry.
func AccountDirectory(account string) string {
	sum := sha256.Sum256([]byte(account))
	return hex.EncodeToString(sum[:])
}

// LegacyAccountRecord is the on-disk account-directory record written by the
// previous standalone WeChat host. It is intentionally a data-only projection:
// application migrations must not construct or retain its old Manager runtime.
type LegacyAccountRecord struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	Removed  bool   `json:"removed,omitempty"`
	Deleting bool   `json:"deleting,omitempty"`
	Purged   bool   `json:"purged,omitempty"`
}

// LegacyAccountRegistered preserves the previous directory's visibility rule.
// Removed, deleting and purged accounts are never imported as active
// connections, even when their old files still exist for recovery.
func LegacyAccountRegistered(record LegacyAccountRecord) bool {
	return !record.Removed && !record.Purged && !record.Deleting
}

// LegacyGroupRule is the complete JSON-compatible group-rule projection from
// the former standalone configuration. The import currently interprets only
// ID, Mode and Prefix, but preserving all fields keeps the compatibility
// boundary faithful and makes future migration decisions explicit.
type LegacyGroupRule struct {
	ID                      string `json:"id"`
	Name                    string `json:"name"`
	Mode                    string `json:"mode"`
	Prefix                  string `json:"prefix"`
	Persona                 string `json:"persona"`
	ForceReplyWhenMentioned bool   `json:"force_reply_when_mentioned"`
}

func ConnectionRef(account string) apphost.ConnectionRef {
	return apphost.ConnectionRef{AppID: ID, InstallationID: LegacyInstallationID, ConnectionID: ConnectionID(account), AccountID: account}
}
