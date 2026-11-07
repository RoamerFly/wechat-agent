package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStationBuildExcludesLegacyManagementAndDevices(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("station dependency graph: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		switch dependency {
		case "wechat-bot/internal/admin", "wechat-bot/internal/devicebroker",
			"wechat-bot/internal/adbserver", "wechat-bot/internal/devicespec":
			t.Errorf("standalone station imports retired host package %s", dependency)
		}
	}
}

func TestDefaultPathsUseExecutableLocation(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "bin", "Agent-home-station.exe")
	cfg, err := parseConfig(nil, executable)
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}
	wantData := filepath.Join(filepath.Dir(executable), "data", "station")
	if cfg.dataDir != wantData {
		t.Fatalf("dataDir = %q, want %q", cfg.dataDir, wantData)
	}
	wantWorkspace := filepath.Join(wantData, "managed-workspaces")
	if cfg.workspaceRoot != wantWorkspace {
		t.Fatalf("workspaceRoot = %q, want %q", cfg.workspaceRoot, wantWorkspace)
	}
}

func TestExplicitPathsMustBeAbsolute(t *testing.T) {
	_, err := parseConfig([]string{"-data", "relative"}, `C:\\Program Files\\Agent Home Station\\Agent-home-station.exe`)
	if err == nil {
		t.Fatal("relative data path was accepted")
	}
	_, err = parseConfig([]string{"-workspace", "relative"}, `C:\\Program Files\\Agent Home Station\\Agent-home-station.exe`)
	if err == nil {
		t.Fatal("relative workspace path was accepted")
	}
}

func TestLocalListenAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8082", "localhost:8082", "[::1]:8082", "127.0.0.1:0"} {
		if _, err := localListenAddress(address); err != nil {
			t.Errorf("localListenAddress(%q) unexpected error: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:8082", "[::]:8082", "192.168.1.5:8082", "example.com:8082", "localhost", ":8082", "127.0.0.1:bad"} {
		if _, err := localListenAddress(address); err == nil {
			t.Errorf("localListenAddress(%q) unexpectedly succeeded", address)
		}
	}
}

func TestDesktopWechatAdmissionRequiresSeparatePrivateRoot(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "agent-home-station.exe")
	both := []string{"--desktop", "--wechat-native-url", "http://127.0.0.1:18808", "--wechat-native-pid", "42"}
	cfg, err := parseConfig(both, exe)
	if err != nil || cfg.wechatNativePID != 42 || cfg.wechatNativeURL != "http://127.0.0.1:18808" {
		t.Fatal(cfg, err)
	}
	for _, args := range [][]string{
		both[1:],
		both[:3],
		{"--desktop", "--wechat-native-url", "http://localhost:18808", "--wechat-native-pid", "42"},
		{"--desktop", "--wechat-native-url", "http://127.0.0.1:0", "--wechat-native-pid", "42"},
		{"--desktop", "--wechat-native-url", "http://127.0.0.1:8082", "--wechat-native-pid", "42"},
		append(append([]string(nil), both...), "--workspace", filepath.Join(cfg.dataDir, "apps")),
	} {
		if _, err := parseConfig(args, exe); err == nil {
			t.Fatal("unsafe daemon admission", args)
		}
	}
}
