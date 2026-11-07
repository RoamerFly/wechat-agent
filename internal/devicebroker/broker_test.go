package devicebroker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"wechat-bot/internal/adbserver"
	"wechat-bot/internal/devicespec"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
)

func TestOpenRejectsUnsafeLegacyInvocationAndPaths(t *testing.T) {
	options, paths := fixtureOptions(t)
	writeJSON(t, paths.config, validProfile(paths.adb))
	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{"python module flag", func(o *Options) { o.Legacy = legacyConfig("-m", paths.config, paths.storage) }},
		{"legacy environment", func(o *Options) {
			o.Legacy = legacyConfig(paths.script, paths.config, paths.storage)
			o.Legacy.Env = map[string]string{"SECRET": "no"}
		}},
		{"outside config", func(o *Options) {
			o.Legacy = legacyConfig(paths.script, filepath.Join(t.TempDir(), "profile.json"), paths.storage)
		}},
		{"fresh ledger outside state", func(o *Options) { o.Legacy = legacyConfig(paths.script, paths.config, paths.workspace) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := options
			tc.mutate(&o)
			if _, err := OpenIO(context.Background(), o); !errors.Is(err, ErrNeedsMigration) {
				t.Fatalf("Open error = %v, want needs migration", err)
			}
		})
	}
}

func TestOpenRequiresBoundedTrustedADBRoot(t *testing.T) {
	options, paths := fixtureOptions(t)
	writeJSON(t, paths.config, validProfile(paths.adb))
	options.Legacy = legacyConfig(paths.script, paths.config, paths.storage)
	outside := filepath.Join(paths.workspace, "untrusted", "adb.exe")
	if err := os.MkdirAll(filepath.Dir(outside), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*Options)
	}{
		{"no roots", func(o *Options) { o.ADBRoots = nil }},
		{"workspace is not a runtime root", func(o *Options) { o.ADBRoots = []string{paths.workspace} }},
		{"profile adb outside approved root", func(o *Options) { writeJSON(t, paths.config, validProfile(outside)) }},
		{"non-adb executable rejected", func(o *Options) {
			writeJSON(t, paths.config, validProfile(filepath.Join(paths.adbRoot, "other.exe")))
			_ = os.WriteFile(filepath.Join(paths.adbRoot, "other.exe"), []byte("other"), 0600)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeJSON(t, paths.config, validProfile(paths.adb))
			o := options
			tc.mutate(&o)
			if _, err := OpenIO(context.Background(), o); !errors.Is(err, ErrNeedsMigration) {
				t.Fatalf("Open error = %v, want needs migration", err)
			}
		})
	}
}

func TestPhysicalLeaseReleasesAfterCancellationAndProcessExit(t *testing.T) {
	if path := os.Getenv("DEVICEBROKER_LEASE_HELPER"); path != "" {
		lease, err := acquireLease(path)
		if err != nil {
			os.Exit(2)
		}
		defer lease.Close()
		fmt.Println("locked")
		select {}
	}
	path := filepath.Join(t.TempDir(), "physical.lock")
	lease, err := acquireLease(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = acquireLease(path); !errors.Is(err, errLeaseBusy) {
		t.Fatalf("same-process contention = %v, want busy", err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err = acquireLease(path)
	if err != nil {
		t.Fatalf("close did not release lease: %v", err)
	}
	_ = lease.Close()

	command := exec.Command(os.Args[0], "-test.run=^TestPhysicalLeaseReleasesAfterCancellationAndProcessExit$")
	command.Env = append(os.Environ(), "DEVICEBROKER_LEASE_HELPER="+path)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	if line, readErr := bufio.NewReader(output).ReadString('\n'); readErr != nil || line != "locked\n" {
		_ = command.Process.Kill()
		t.Fatalf("lease helper readiness = %q, %v", line, readErr)
	}
	if _, err = acquireLease(path); !errors.Is(err, errLeaseBusy) {
		_ = command.Process.Kill()
		t.Fatalf("child lock contention = %v, want busy", err)
	}
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for {
		lease, err = acquireLease(path)
		if err == nil {
			_ = lease.Close()
			break
		}
		if !errors.Is(err, errLeaseBusy) || time.Now().After(deadline) {
			t.Fatalf("process exit did not release lease: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fixturePaths struct {
	workspace  string
	state      string
	packageDir string
	adbRoot    string
	leaseDir   string
	python     string
	script     string
	config     string
	storage    string
	adb        string
}

func fixtureOptions(t *testing.T) (Options, fixturePaths) {
	t.Helper()
	root := t.TempDir()
	paths := fixturePaths{workspace: filepath.Join(root, "workspace"), state: filepath.Join(root, "state"), packageDir: filepath.Join(root, "package")}
	paths.adbRoot = filepath.Join(paths.workspace, "runtimes")
	paths.leaseDir = filepath.Join(paths.workspace, "device-leases")
	for _, directory := range []string{paths.workspace, paths.state, paths.packageDir, filepath.Join(paths.packageDir, "runtime"), paths.adbRoot, paths.leaseDir} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	paths.python = filepath.Join(paths.workspace, "python.exe")
	paths.script = filepath.Join(paths.packageDir, "runtime", "bridge.py")
	paths.config = filepath.Join(paths.workspace, "legacy-profile.json")
	paths.storage = filepath.Join(paths.state, "ledger")
	paths.adb = filepath.Join(paths.adbRoot, "adb.exe")
	for _, file := range []string{paths.python, paths.script, paths.adb} {
		if err := os.WriteFile(file, []byte("bridge"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(paths.storage, 0700); err != nil {
		t.Fatal(err)
	}
	fixturePackage(t, paths.packageDir)
	return Options{
		IOOptions: devicespec.IOOptions{Python: paths.python, PackageDirectory: paths.packageDir, WorkspaceDirectory: paths.workspace,
			StateDirectory: paths.state, GlobalLeaseDirectory: paths.leaseDir, ADBRoots: []string{paths.adbRoot},
			Account: "account_1", Devices: []string{"allowed"}, Actions: []string{"tap", "capture"}},
		ADBServer: fakeADBServer{},
	}, paths
}

type fakeADBServer struct{}

func (fakeADBServer) Acquire(context.Context, string) (adbserver.Lease, error) {
	return fakeADBLease{}, nil
}

type fakeADBLease struct{}

func (fakeADBLease) Endpoint() adbserver.Endpoint { return adbserver.DefaultEndpoint() }
func (fakeADBLease) Binding() string              { return "synthetic-adb-server" }
func (fakeADBLease) Close() error                 { return nil }

type trackedADBServer struct{ lease *trackedADBLease }

func (s trackedADBServer) Acquire(context.Context, string) (adbserver.Lease, error) {
	return s.lease, nil
}

type trackedADBLease struct {
	closes int
	err    error
}

func (l *trackedADBLease) Endpoint() adbserver.Endpoint { return adbserver.DefaultEndpoint() }
func (l *trackedADBLease) Binding() string              { return "tracked-server" }
func (l *trackedADBLease) Close() error                 { l.closes++; return l.err }

func legacyConfig(script, config, storage string) mcptools.ServerConfig {
	return mcptools.ServerConfig{Transport: "stdio", Args: []string{"-I", "-B", "-u", script, "--config", config, "--storage", storage}}
}

func validProfile(adb string) profile {
	return profile{ADB: adb, Devices: map[string]profileDevice{"allowed": {Serial: "emulator-5554", Enabled: true, Packages: []string{"com.example.browser"}, Actions: []string{"observe", "tap", "capture"}, Browsers: []string{"com.example.browser"}, WebHosts: []string{"example.test"}}}}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fixturePackage(t *testing.T, code string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(code, "runtime", "bridge.py"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	p := pluginhost.Package{SchemaVersion: 1, ID: "device-fixture", Name: "Synthetic device broker", Version: "1.0.0", HostProtocol: 1, Status: "ready", Platforms: []string{"windows-amd64", "linux-amd64"}, Runtime: pluginhost.Runtime{Transport: "mcp-stdio", Entrypoint: "runtime/bridge.py"}, Permissions: []string{"device"}, Files: map[string]string{"runtime/bridge.py": hex.EncodeToString(sum[:])}}
	for _, name := range []string{"config.schema.json", "ui.schema.json", "capabilities.json", "README.md", "PERMISSIONS.md", "MIGRATION.md", "CHANGELOG.md"} {
		body := []byte("synthetic fixture\n")
		if strings.HasSuffix(name, ".json") {
			body = []byte("{}")
		}
		if err := os.WriteFile(filepath.Join(code, name), body, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		p.Files[name] = hex.EncodeToString(sum[:])
	}
	data, _ := json.Marshal(p)
	if err = os.WriteFile(filepath.Join(code, "plugin.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

type countingADBServer struct{ calls int }

func (s *countingADBServer) Acquire(context.Context, string) (adbserver.Lease, error) {
	s.calls++
	return fakeADBLease{}, nil
}
