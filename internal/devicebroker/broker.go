// Package devicebroker binds original device authority to fixed host I/O.
// High-level native protocol runs only in the installed isolated mobile plugin.
package devicebroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"wechat-bot/internal/adbserver"
	"wechat-bot/internal/devicespec"
	"wechat-bot/internal/mcptools"
)

var (
	ErrNeedsMigration = devicespec.ErrNeedsMigration
	ErrDeviceBusy     = errors.New("device_busy")
	errChanged        = errors.New("device_broker_changed")
	errToolDenied     = errors.New("device_broker_tool_not_permitted")
	identifier        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	serial            = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)
	packageName       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(?:\.[A-Za-z][A-Za-z0-9_]*)+$`)
	hostName          = regexp.MustCompile(`^[a-z0-9.-]{1,253}$`)
)

// Options contains host-trusted paths. PackageDirectory is an already
// verified package snapshot; this package does not accept a model-supplied
// directory, command, script, environment, or endpoint.
type Options struct {
	devicespec.IOOptions
	inspectOnly bool
	Runner      IORunner // host-only bounded native executor; never JSON or plugin arguments
	// ADBServer is a host-owned foreground server manager. Nil uses the
	// process-wide managed server, never an MCP-session child.
	ADBServer interface {
		Acquire(context.Context, string) (adbserver.Lease, error)
	}
}

// Host adapts the concrete broker for the legacy management service.
type Host struct{}

func (Host) Inspect(ctx context.Context, options devicespec.IOOptions) error {
	return InspectIOOptions(ctx, Options{IOOptions: options})
}

func (Host) Grants(ctx context.Context, options devicespec.IOOptions) ([]devicespec.IOGrant, error) {
	return InspectIOGrant(ctx, Options{IOOptions: options})
}

func (Host) Open(ctx context.Context, options devicespec.IOOptions) (devicespec.IOBroker, error) {
	return OpenIO(ctx, Options{IOOptions: options})
}

func (Host) Cleanup() error {
	return errors.Join(CleanupQuarantined(), adbserver.CleanupQuarantined())
}

// quarantinedRegistry keeps only brokers whose native-child cleanup was not
// confirmed. It intentionally has no public identifiers or task data. Without
// this strong reference, a discarded wrapper could let GC close the retained
// lease file while a native child outcome was still unknown.
type cleanupOwner interface{ Close() error }

var quarantinedRegistry = struct {
	sync.Mutex
	brokers map[cleanupOwner]struct{}
}{brokers: map[cleanupOwner]struct{}{}}

type resourceProof struct {
	seals       []fileSeal
	dirs        []string
	privateDirs []string
}

type resolved struct {
	profile        profile
	adb            string
	storage        string
	seals          []fileSeal
	dirs           []string
	privateDirs    []string
	leaseDirectory string
	leases         map[string]string
	adbLease       adbserver.Lease
}

type fileSeal struct {
	path string
	hash string
}

// CleanupQuarantined retries only cleanup for brokers whose prior cleanup
// failed. It never replays an operation or starts a new process.
func CleanupQuarantined() error {
	quarantinedRegistry.Lock()
	items := make([]cleanupOwner, 0, len(quarantinedRegistry.brokers))
	for item := range quarantinedRegistry.brokers {
		items = append(items, item)
	}
	quarantinedRegistry.Unlock()
	var first error
	for _, item := range items {
		if err := item.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func registerQuarantine(item cleanupOwner) {
	quarantinedRegistry.Lock()
	quarantinedRegistry.brokers[item] = struct{}{}
	quarantinedRegistry.Unlock()
}

func unregisterQuarantine(item cleanupOwner) {
	quarantinedRegistry.Lock()
	delete(quarantinedRegistry.brokers, item)
	quarantinedRegistry.Unlock()
}

func (b *resourceProof) verified() bool {
	for _, seal := range b.seals {
		if !seal.matches() {
			return false
		}
	}
	for _, dir := range b.dirs {
		if _, err := canonicalDirectory(dir); err != nil {
			return false
		}
	}
	for _, dir := range b.privateDirs {
		if err := verifyPrivateDirectory(dir); err != nil {
			return false
		}
	}
	return true
}

func (s fileSeal) matches() bool {
	path, err := canonicalFile(s.path)
	if err != nil || path != s.path {
		return false
	}
	hash, err := hashFile(path)
	return err == nil && hash == s.hash
}

func resolve(ctx context.Context, options Options) (resolved, error) {
	if !identifier.MatchString(options.Account) {
		return resolved{}, ErrNeedsMigration
	}
	python, err := canonicalFile(options.Python)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	workspace, err := canonicalDirectory(options.WorkspaceDirectory)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	state, err := canonicalDirectory(options.StateDirectory)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	adbRoots, err := canonicalADBRoots(options.ADBRoots, workspace)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	packageDir, err := canonicalDirectory(options.PackageDirectory)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	runtimeBridge, err := canonicalFile(filepath.Join(packageDir, "runtime", "bridge.py"))
	if err != nil || !within(packageDir, runtimeBridge) {
		return resolved{}, ErrNeedsMigration
	}

	compatibility := ""
	if options.CompatibilityEntry != "" {
		compatibility, err = canonicalFile(options.CompatibilityEntry)
		if err != nil || !within(workspace, compatibility) {
			return resolved{}, ErrNeedsMigration
		}
	}
	legacyConfig, legacyStorage, err := legacyPaths(options.Legacy, runtimeBridge, compatibility)
	if err != nil || !withinAny(legacyConfig, workspace, state) || !within(state, legacyStorage) {
		return resolved{}, ErrNeedsMigration
	}
	legacyConfig, err = canonicalFile(legacyConfig)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	legacyStorage, err = canonicalDirectory(legacyStorage)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	legacyHash, err := hashFile(legacyConfig)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	filtered, adb, err := filteredProfile(legacyConfig, options.Devices, options.Actions)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	if after, hashErr := hashFile(legacyConfig); hashErr != nil || after != legacyHash {
		return resolved{}, ErrNeedsMigration
	}
	adb, err = canonicalADB(adb, adbRoots)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	leaseCandidate, err := absoluteLeaf(options.GlobalLeaseDirectory)
	if err != nil || !within(workspace, leaseCandidate) || samePath(leaseCandidate, workspace) || within(state, leaseCandidate) || within(leaseCandidate, state) {
		return resolved{}, ErrNeedsMigration
	}
	parent, err := canonicalDirectory(filepath.Dir(leaseCandidate))
	if err != nil || !within(workspace, parent) || !samePath(leaseCandidate, filepath.Join(parent, filepath.Base(leaseCandidate))) {
		return resolved{}, ErrNeedsMigration
	}
	if options.inspectOnly {
		// Inspection intentionally ends before creating protected state or
		// acquiring ADB, but it retains the filtered public profile for
		// InspectIOGrant. Callers expose only package/action aliases from it.
		return resolved{profile: filtered}, nil
	}
	// Do not alter the ACL/mode of the historical account state directory.
	// Instead, create one protected leaf for the broker-derived profile.
	profileDirectory, err := ensurePrivateDirectory(filepath.Join(state, "devicebroker-private"))
	if err != nil || !within(state, profileDirectory) {
		return resolved{}, ErrNeedsMigration
	}
	leaseDirectory, err := ensurePrivateDirectory(leaseCandidate)
	if err != nil || !within(workspace, leaseDirectory) || samePath(leaseDirectory, workspace) || within(state, leaseDirectory) || within(leaseDirectory, state) {
		return resolved{}, ErrNeedsMigration
	}
	serverManager := options.ADBServer
	if serverManager == nil {
		serverManager = defaultADBServer{}
	}
	adbLease, err := serverManager.Acquire(ctx, adb)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	keepLease := false
	defer func() {
		if !keepLease {
			_ = adbLease.Close()
		}
	}()
	endpoint := adbLease.Endpoint()
	if endpoint.Host != "127.0.0.1" || endpoint.Port != 5037 {
		return resolved{}, ErrNeedsMigration
	}
	filtered.ADB = adb
	filtered.ADBHost = endpoint.Host
	filtered.ADBPort = endpoint.Port
	// StateDirectory is selected by the host for this account. Keep the derived
	// config account-distinct as a second guard against accidental sharing of a
	// broader state parent; the durable legacy ledger itself is never relocated.
	accountHash := sha256.Sum256([]byte(options.Account))
	filteredConfig := filepath.Join(profileDirectory, "profile-"+hex.EncodeToString(accountHash[:8])+".json")
	filteredHash, err := writeProfile(filteredConfig, filtered)
	if err != nil {
		return resolved{}, ErrNeedsMigration
	}
	filteredConfig, err = canonicalFile(filteredConfig)
	if err != nil || !within(profileDirectory, filteredConfig) {
		return resolved{}, ErrNeedsMigration
	}
	if actual, hashErr := hashFile(filteredConfig); hashErr != nil || actual != filteredHash {
		return resolved{}, ErrNeedsMigration
	}

	seals := make([]fileSeal, 0, 5)
	for _, path := range []string{python, runtimeBridge, adb} {
		seal, err := makeSeal(path)
		if err != nil {
			return resolved{}, ErrNeedsMigration
		}
		seals = append(seals, seal)
	}
	seals = append(seals, fileSeal{path: legacyConfig, hash: legacyHash}, fileSeal{path: filteredConfig, hash: filteredHash})

	leases := map[string]string{}
	for alias, device := range filtered.Devices {
		leases[alias] = physicalLeaseKey(device.Serial)
	}
	privateDirs := []string{profileDirectory, leaseDirectory}
	keepLease = true
	return resolved{profile: filtered, adb: adb, storage: legacyStorage, seals: seals, dirs: append(append([]string{}, workspace, state, profileDirectory, packageDir, legacyStorage), append(adbRoots, leaseDirectory)...), privateDirs: privateDirs, leaseDirectory: leaseDirectory, leases: leases, adbLease: adbLease}, nil
}

type defaultADBServer struct{}

func (defaultADBServer) Acquire(ctx context.Context, path string) (adbserver.Lease, error) {
	return adbserver.Acquire(ctx, path)
}

func brokerBinding(seals []fileSeal, leases map[string]string, leaseDirectory, adbBinding string) string {
	// json.Marshal orders string map keys deterministically. Paths and serials
	// remain inside this digest; callers receive no file or device identifiers.
	type bindingSeal struct {
		Path string `json:"path"`
		Hash string `json:"hash"`
	}
	boundSeals := make([]bindingSeal, 0, len(seals))
	for _, seal := range seals {
		boundSeals = append(boundSeals, bindingSeal{Path: seal.path, Hash: seal.hash})
	}
	payload, _ := json.Marshal(struct {
		Seals     []bindingSeal     `json:"seals"`
		Leases    map[string]string `json:"leases"`
		LeaseRoot string            `json:"lease_root"`
		ADBServer string            `json:"adb_server"`
	}{Seals: boundSeals, Leases: leases, LeaseRoot: leaseDirectory, ADBServer: adbBinding})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func canonicalADBRoots(roots []string, workspace string) ([]string, error) {
	if len(roots) == 0 || len(roots) > 8 {
		return nil, errors.New("invalid adb roots")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		canonical, err := canonicalDirectory(root)
		if err != nil || !within(workspace, canonical) || samePath(workspace, canonical) || seen[canonical] {
			return nil, errors.New("invalid adb root")
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out, nil
}

func canonicalADB(path string, roots []string) (string, error) {
	adb, err := canonicalFile(path)
	if err != nil {
		return "", err
	}
	name := filepath.Base(adb)
	if !(name == "adb" || strings.EqualFold(name, "adb.exe")) || !withinAny(adb, roots...) {
		return "", errors.New("untrusted adb")
	}
	return adb, nil
}

func physicalLeaseKey(serial string) string {
	// Android's default adb server is shared on the local machine. Different
	// trusted adb binaries can therefore still control the same serial; do not
	// include their path or version in this physical-device lock identity.
	sum := sha256.Sum256([]byte(strings.ToLower(serial)))
	return hex.EncodeToString(sum[:])
}

func legacyPaths(legacy mcptools.ServerConfig, runtimeBridge, compatibility string) (config, storage string, err error) {
	if legacy.URL != "" || len(legacy.Env) != 0 || len(legacy.Headers) != 0 || (legacy.Transport != "" && legacy.Transport != "stdio") {
		return "", "", ErrNeedsMigration
	}
	args := legacy.Args
	i := 0
	flags := map[string]bool{}
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		flag := args[i]
		if (flag != "-I" && flag != "-B" && flag != "-u") || flags[flag] {
			return "", "", ErrNeedsMigration
		}
		flags[flag] = true
		i++
	}
	if i >= len(args) {
		return "", "", ErrNeedsMigration
	}
	script, scriptErr := canonicalFile(args[i])
	if scriptErr != nil || script != runtimeBridge && (compatibility == "" || script != compatibility) {
		return "", "", ErrNeedsMigration
	}
	i++
	for i < len(args) {
		if i+1 >= len(args) || (args[i] != "--config" && args[i] != "--storage") {
			return "", "", ErrNeedsMigration
		}
		value := args[i+1]
		if !filepath.IsAbs(value) {
			return "", "", ErrNeedsMigration
		}
		if args[i] == "--config" {
			if config != "" {
				return "", "", ErrNeedsMigration
			}
			config = value
		} else {
			if storage != "" {
				return "", "", ErrNeedsMigration
			}
			storage = value
		}
		i += 2
	}
	if config == "" || storage == "" {
		return "", "", ErrNeedsMigration
	}
	return config, storage, nil
}

type profile struct {
	ADB     string                   `json:"adb"`
	ADBHost string                   `json:"adb_host,omitempty"`
	ADBPort uint16                   `json:"adb_port,omitempty"`
	Devices map[string]profileDevice `json:"devices"`
}
type profileDevice struct {
	Serial   string   `json:"serial"`
	Enabled  bool     `json:"enabled"`
	Packages []string `json:"packages"`
	Actions  []string `json:"actions"`
	Browsers []string `json:"browsers,omitempty"`
	WebHosts []string `json:"web_hosts,omitempty"`
}

func filteredProfile(path string, devices, actions []string) (profile, string, error) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64<<10 {
		return profile{}, "", ErrNeedsMigration
	}
	var source profile
	if json.Unmarshal(data, &source) != nil || !filepath.IsAbs(source.ADB) || len(source.Devices) == 0 || len(source.Devices) > 16 {
		return profile{}, "", ErrNeedsMigration
	}
	if len(devices) == 0 || len(devices) > 16 || len(actions) > 9 {
		return profile{}, "", ErrNeedsMigration
	}
	allowedActions := map[string]bool{}
	for _, action := range actions {
		if !knownAction(action) || action == "observe" {
			return profile{}, "", ErrNeedsMigration
		}
		allowedActions[action] = true
	}
	out := profile{Devices: map[string]profileDevice{}}
	for _, id := range devices {
		if !identifier.MatchString(id) || out.Devices[id].Serial != "" {
			return profile{}, "", ErrNeedsMigration
		}
		device, ok := source.Devices[id]
		if !ok || !device.Enabled || !validDevice(device) {
			return profile{}, "", ErrNeedsMigration
		}
		permitted := make([]string, 0, len(device.Actions))
		observed := false
		for _, action := range device.Actions {
			if action == "observe" {
				observed = true
				permitted = append(permitted, action)
				continue
			}
			if allowedActions[action] {
				permitted = append(permitted, action)
			}
		}
		if !observed {
			return profile{}, "", ErrNeedsMigration
		}
		device.Actions = permitted
		out.Devices[id] = device
	}
	if len(out.Devices) == 0 {
		return profile{}, "", ErrNeedsMigration
	}
	return out, source.ADB, nil
}

func validDevice(device profileDevice) bool {
	if !serial.MatchString(device.Serial) || len(device.Packages) == 0 || len(device.Packages) > 30 || len(device.Actions) == 0 || len(device.Actions) > 9 || len(device.Browsers) > len(device.Packages) || len(device.WebHosts) > 100 {
		return false
	}
	packages := map[string]bool{}
	for _, value := range device.Packages {
		if !packageName.MatchString(value) || packages[value] {
			return false
		}
		packages[value] = true
	}
	seenActions := map[string]bool{}
	for _, value := range device.Actions {
		if !knownAction(value) || seenActions[value] {
			return false
		}
		seenActions[value] = true
	}
	for _, value := range device.Browsers {
		if !packages[value] {
			return false
		}
	}
	for _, value := range device.WebHosts {
		if !hostName.MatchString(value) {
			return false
		}
	}
	return true
}

func knownAction(value string) bool {
	switch value {
	case "observe", "launch", "tap", "swipe", "text", "back", "home", "capture", "open_url":
		return true
	}
	return false
}

func allowedTool(name string) bool {
	switch name {
	case "devices", "observe", "act", "read_artifact":
		return true
	}
	return false
}

func writeProfile(path string, value profile) (string, error) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 64<<10 {
		return "", err
	}
	// Recheck the trusted private state directory immediately before creating a
	// temporary file. chmod(0600) below is meaningful on POSIX; on Windows the
	// protected broker leaf provides the file DACL, while the host-selected
	// StateDirectory remains the parent-deletion trust boundary. This package
	// makes no claim that chmod alone provides DACL isolation.
	if parent, parentErr := canonicalDirectory(filepath.Dir(path)); parentErr != nil || !samePath(parent, filepath.Dir(path)) {
		return "", errors.New("private directory changed")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".devicebroker-")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name())
	if err = temporary.Chmod(0600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = os.Rename(temporary.Name(), path); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func makeSeal(path string) (fileSeal, error) {
	canonical, err := canonicalFile(path)
	if err != nil {
		return fileSeal{}, err
	}
	hash, err := hashFile(canonical)
	return fileSeal{path: canonical, hash: hash}, err
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func canonicalFile(path string) (string, error) {
	canonical, err := canonical(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("not regular")
	}
	return canonical, nil
}

func canonicalDirectory(path string) (string, error) {
	canonical, err := canonical(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("not directory")
	}
	return canonical, nil
}

func canonical(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("not absolute")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil || !samePath(clean, filepath.Clean(resolved)) {
		return "", errors.New("link or unresolved")
	}
	return clean, nil
}

func absoluteLeaf(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("not absolute")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func within(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func withinAny(candidate string, roots ...string) bool {
	for _, root := range roots {
		if within(root, candidate) {
			return true
		}
	}
	return false
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
