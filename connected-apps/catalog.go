// Package catalog binds bundled application sources to host-approved digests.
package catalog

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"wechat-bot/internal/pluginhost"
)

// Only descriptors are embedded. Runtime source stays in the distribution.
//
//go:embed */app.json
var descriptors embed.FS

type Contract struct {
	Package pluginhost.Package
	Digest  string
}

func IDs() []string { return []string{"wechat", "bilibili", "douyin"} }

func Lookup(id string) (Contract, error) {
	if !slices.Contains(IDs(), id) {
		return Contract{}, errors.New("应用不在本机发行目录中")
	}
	data, err := descriptors.ReadFile(id + "/app.json")
	var p pluginhost.Package
	if err != nil || json.Unmarshal(data, &p) != nil || p.ID != id {
		return Contract{}, errors.New("应用发行声明不可用")
	}
	hash := sha256.Sum256(data)
	return Contract{Package: p, Digest: hex.EncodeToString(hash[:])}, nil
}
