package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/fishidentity"
)

func TestLegacyHomeCommandImportsSelectedProjectWithoutStartingServices(t *testing.T) {
	base := t.TempDir()
	dataDir, workspace, source := filepath.Join(base, "station"), filepath.Join(base, "workspace"), filepath.Join(base, "old-project")
	for _, directory := range []string{dataDir, source} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := fishidentity.New(dataDir).InitializeLocal()
	if err != nil {
		t.Fatal(err)
	}
	const original = "print('synthetic persistent tool')\n"
	if err := os.WriteFile(filepath.Join(source, "tool.py"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--data", dataDir, "--workspace", workspace, "--legacy-home-source", source, "--legacy-home-target", "projects/old-project", "--legacy-home-max-bytes", "4096"}
	var output bytes.Buffer
	if err := run(t.Context(), args, &output); err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Digest string `json:"digest"`
		Status string `json:"status"`
	}
	if json.Unmarshal(output.Bytes(), &plan) != nil || len(plan.Digest) != 64 || plan.Status != "preview" || strings.Contains(output.String(), original) {
		t.Fatal("invalid project preview")
	}
	output.Reset()
	if err := run(t.Context(), append(args, "--legacy-home-digest", plan.Digest), &output); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(output.Bytes(), &plan) != nil || plan.Status != "applied" {
		t.Fatal("project import status missing")
	}
	target := filepath.Join(workspace, "fish_home", identity.FishID, "home", "projects", "old-project", "tool.py")
	content, err := os.ReadFile(target)
	if err != nil || string(content) != original {
		t.Fatal("selected project content changed", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "admin-password")); !os.IsNotExist(err) {
		t.Fatal("project import unexpectedly initialized a server", err)
	}
}

func TestLegacyHomeCommandRequiresExplicitSelection(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "station.exe")
	dataDir, source := t.TempDir(), t.TempDir()
	for _, args := range [][]string{
		{"--legacy-home-source", source, "--legacy-home-target", "projects/old"},
		{"--data", dataDir, "--legacy-home-target", "projects/old"},
		{"--data", dataDir, "--legacy-home-source", "relative", "--legacy-home-target", "projects/old"},
		{"--data", dataDir, "--legacy-home-source", source, "--legacy-home-target", "projects/old", "--desktop"},
		{"--data", dataDir, "--legacy-home-source", source, "--legacy-home-target", "projects/old", "--legacy-home-max-bytes", "0"},
		{"--data", dataDir, "--legacy-home-source", source, "--legacy-home-target", "projects/old", "--legacy-home-digest", "invalid"},
	} {
		if _, err := parseConfig(args, executable); err == nil {
			t.Fatal("ambiguous project import accepted", args)
		}
	}
}
