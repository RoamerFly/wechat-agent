package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacySettingsCommandPreviewsAndImportsWithoutStartingStation(t *testing.T) {
	base := t.TempDir()
	source, target := filepath.Join(base, "old-admin"), filepath.Join(base, "station")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	const key = "synthetic-legacy-command-api-key"
	config, _ := json.Marshal(map[string]any{
		"provider": "deepseek", "base_url": "https://api.deepseek.com", "model": "deepseek-flash",
		"api_key": key, "max_tokens": 4096, "thinking_enabled": true, "reasoning_effort": "low",
		"companion": map[string]any{"persona": "合成测试角色"},
	})
	if err := os.WriteFile(filepath.Join(source, "config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	args := []string{"--data", target, "--legacy-settings-source", source, "--listen", listener.Addr().String()}
	var output bytes.Buffer
	if err := run(t.Context(), args, &output); err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Digest string `json:"digest"`
		Status string `json:"status"`
	}
	if json.Unmarshal(output.Bytes(), &plan) != nil || plan.Status != "preview" || len(plan.Digest) != 64 || strings.Contains(output.String(), key) {
		t.Fatal("invalid or private preview output")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("preview created station data", err)
	}
	output.Reset()
	if err := run(t.Context(), append(args, "--legacy-settings-digest", plan.Digest), &output); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(output.Bytes(), &plan) != nil || plan.Status != "applied" || strings.Contains(output.String(), key) {
		t.Fatal("invalid or private import output")
	}
	data, err := os.ReadFile(filepath.Join(target, "local-model.json"))
	if err != nil || !bytes.Contains(data, []byte(key)) {
		t.Fatal("private model settings were not imported", err)
	}
	if _, err := os.Stat(filepath.Join(target, "admin-password")); !os.IsNotExist(err) {
		t.Fatal("settings import started owner authentication", err)
	}
}

func TestLegacySettingsCommandRejectsAmbiguousModes(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "agent-home-station-server.exe")
	source, target := filepath.Join(t.TempDir(), "old-admin"), filepath.Join(t.TempDir(), "station")
	digest := strings.Repeat("a", 64)
	for _, args := range [][]string{
		{"--legacy-settings-source", source},
		{"--data", target, "--legacy-settings-source", "relative"},
		{"--data", target, "--legacy-settings-digest", digest},
		{"--data", target, "--legacy-settings-source", source, "--legacy-settings-digest", "invalid"},
		{"--data", target, "--legacy-settings-source", source, "--desktop"},
		{"--data", target, "--legacy-settings-source", source, "--runtime-config", source},
	} {
		if _, err := parseConfig(args, executable); err == nil {
			t.Fatal("accepted ambiguous settings import", args)
		}
	}
}
