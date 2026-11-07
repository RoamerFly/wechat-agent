package main

import (
	"path/filepath"
	"testing"
)

func TestParseLegacyWechatReleaseArgs(t *testing.T) {
	data := t.TempDir()
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	parsed, err := parseLegacyWechatReleaseArgs([]string{"--data", data, "--connection", "wechat-primary", "--audit-digest", digest})
	if err != nil {
		t.Fatalf("preflight arguments: %v", err)
	}
	if parsed.dataDir != filepath.Clean(data) || parsed.connectionID != "wechat-primary" || parsed.auditDigest != digest || parsed.acceptUnknown || parsed.applyDigest != "" {
		t.Fatalf("preflight arguments = %#v", parsed)
	}
	parsed, err = parseLegacyWechatReleaseArgs([]string{"--data", data, "--connection", "wechat-primary", "--audit-digest", digest, "--accept-unknown", "--apply", "preview-token"})
	if err != nil || !parsed.acceptUnknown || parsed.applyDigest != "preview-token" {
		t.Fatalf("apply arguments = %#v, %v", parsed, err)
	}
}

func TestParseLegacyWechatReleaseArgsRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, args := range [][]string{
		{"--data", "relative", "--connection", "wechat-primary", "--audit-digest", digest},
		{"--data", t.TempDir(), "--audit-digest", digest},
		{"--data", t.TempDir(), "--connection", "wechat-primary", "--audit-digest", "not-a-sha256"},
		{"--data", t.TempDir(), "--connection", "wechat-primary", "--audit-digest", digest, "unexpected"},
	} {
		if _, err := parseLegacyWechatReleaseArgs(args); err == nil {
			t.Fatalf("unsafe release CLI arguments accepted: %#v", args)
		}
	}
}
