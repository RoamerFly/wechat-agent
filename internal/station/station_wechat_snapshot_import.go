package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"wechat-bot/internal/identity"
)

// errLegacySnapshotIntegrity deliberately has no source-path or content detail:
// callers may show it to an operator while the snapshot can contain private
// WeChat material.
var errLegacySnapshotIntegrity = errors.New("旧微信快照完整性校验失败")

// legacySnapshotGuard pins a generated snapshot's manifest at the time its
// import source is opened.  A nil guard represents a pre-snapshot, manually
// managed legacy import and deliberately preserves that compatibility path.
type legacySnapshotGuard struct {
	manifest       legacySnapshotManifest
	manifestBytes  []byte
	manifestDigest string
	legacyHostRoot string
}

// loadLegacySnapshotGuard performs only cheap manifest validation.  It must be
// safe to call while listing configured imports: full content hashes are
// deferred to verify, which is called only by an explicit consumption path.
func loadLegacySnapshotGuard(root *os.Root, source legacyImportSource) (*legacySnapshotGuard, error) {
	if root == nil {
		return nil, errLegacySnapshotIntegrity
	}
	raw, err := readLegacyImportFile(root, legacyWechatSnapshotManifestFile, legacyImportArchiveLimit)
	if errors.Is(err, os.ErrNotExist) {
		if source.ManifestDigest != "" {
			return nil, errLegacySnapshotIntegrity
		}
		return nil, nil
	}
	if err != nil {
		return nil, errLegacySnapshotIntegrity
	}
	var manifest legacySnapshotManifest
	if json.Unmarshal(raw, &manifest) != nil || !validLegacySnapshotManifest(manifest, source.Primary) {
		return nil, errLegacySnapshotIntegrity
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !bytes.Equal(raw, canonical) {
		return nil, errLegacySnapshotIntegrity
	}
	digest := hexDigest(raw)
	if source.ManifestDigest != "" && (!validLegacySnapshotDigest(source.ManifestDigest) || source.ManifestDigest != digest) {
		return nil, errLegacySnapshotIntegrity
	}
	return &legacySnapshotGuard{
		manifest:       manifest,
		manifestBytes:  append([]byte(nil), raw...),
		manifestDigest: digest,
		legacyHostRoot: cutoverPath(source.LegacyHostRoot),
	}, nil
}

func validLegacySnapshotManifest(manifest legacySnapshotManifest, primary string) bool {
	if manifest.Version != 1 || !identity.ValidPathID(manifest.Primary) || manifest.Primary != primary ||
		manifest.Accounts < 1 || manifest.MemoryFiles < 0 || manifest.DeliveryRecords < 0 || manifest.ManagementDeliveryRecords < 0 {
		return false
	}
	seen := make(map[string]bool, len(manifest.Files))
	previous := ""
	for _, entry := range manifest.Files {
		if !validLegacySnapshotManifestPath(entry.Path) || entry.Path <= previous || seen[entry.Path] ||
			entry.Bytes <= 0 || !validLegacySnapshotDigest(entry.Digest) {
			return false
		}
		seen[entry.Path] = true
		previous = entry.Path
	}
	return seen[legacyStopMarkerFile]
}

func validLegacySnapshotManifestPath(name string) bool {
	if name == "" || name == legacyWechatSnapshotManifestFile || !fs.ValidPath(name) || filepath.ToSlash(filepath.FromSlash(name)) != name ||
		strings.Contains(name, "\\") || strings.Contains(name, ":") || path.IsAbs(name) || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if !validLegacySnapshotImportLeaf(part) {
			return false
		}
	}
	return true
}

// Keep manifest parsing portable.  The Windows writer performs the additional
// reserved-device-name check; this parser accepts only the portable subset.
func validLegacySnapshotImportLeaf(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") ||
		strings.ContainsAny(name, `\\/:"<>|?*`) {
		return false
	}
	upper := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" {
		return false
	}
	return !(len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '1' && upper[3] <= '9')
}

func validLegacySnapshotDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// verify is intentionally comprehensive.  It is invoked immediately before
// an import preview/apply reads source content, not when an import source is
// merely listed in the desktop UI.
func (guard *legacySnapshotGuard) verify(ctx context.Context, root *os.Root) error {
	if guard == nil {
		return nil
	}
	if root == nil || verifyLegacySnapshotPrivacy(root) != nil {
		return errLegacySnapshotIntegrity
	}
	raw, err := readLegacyImportFile(root, legacyWechatSnapshotManifestFile, legacyImportArchiveLimit)
	if err != nil || !bytes.Equal(raw, guard.manifestBytes) || hexDigest(raw) != guard.manifestDigest {
		return errLegacySnapshotIntegrity
	}
	stop, err := readLegacyImportFile(root, legacyStopMarkerFile, 16<<10)
	var marker legacyStopMarker
	if err != nil || json.Unmarshal(stop, &marker) != nil || marker.Version != 2 || marker.State != "stopped" ||
		(guard.legacyHostRoot != "" && !sameCutoverPath(marker.DataRoot, guard.legacyHostRoot)) {
		return errLegacySnapshotIntegrity
	}
	// Hash all manifest inputs again, but skip SQLite quick_check.  Snapshot
	// creation and the explicit snapshot --verify command retain quick_check;
	// importing must not turn routine source consumption into a costly DB scan.
	plan, err := planLegacyWechatSnapshot(ctx, root, marker.DataRoot, guard.manifest.Primary, false)
	if err != nil {
		return errLegacySnapshotIntegrity
	}
	canonical, err := json.Marshal(plan.manifest)
	if err != nil || !bytes.Equal(canonical, guard.manifestBytes) || !legacySnapshotTreeMatches(root, plan) {
		return errLegacySnapshotIntegrity
	}
	return nil
}

// readFile gives generated snapshots a byte-accurate file view.  Optional
// source files that were absent from the manifest remain absent; a later file
// with that name is an integrity failure instead of silently becoming input.
func (guard *legacySnapshotGuard) readFile(root *os.Root, name string, max int64) ([]byte, error) {
	if guard == nil {
		return readLegacyImportFile(root, name, max)
	}
	canonical, ok := canonicalLegacySnapshotPath(name)
	if !ok {
		return nil, errLegacySnapshotIntegrity
	}
	entry, exists := guard.entry(canonical)
	if !exists {
		_, err := readLegacyImportFile(root, canonical, max)
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, errLegacySnapshotIntegrity
	}
	if entry.Bytes > max {
		return nil, errLegacySnapshotIntegrity
	}
	raw, err := readLegacyImportFile(root, canonical, max)
	if err != nil || int64(len(raw)) != entry.Bytes || hexDigest(raw) != entry.Digest {
		return nil, errLegacySnapshotIntegrity
	}
	return raw, nil
}

// checkOpened is for the SQLite import path, which already holds a safely
// opened descriptor.  Hash that descriptor through its pinned directory rather
// than opening the database again by path.
func (guard *legacySnapshotGuard) checkOpened(ctx context.Context, dir *os.Root, relative string, file *os.File) error {
	if guard == nil {
		return nil
	}
	canonical, ok := canonicalLegacySnapshotPath(relative)
	if !ok || file == nil || dir == nil {
		return errLegacySnapshotIntegrity
	}
	expected, found := guard.entry(canonical)
	if !found {
		return errLegacySnapshotIntegrity
	}
	actual, err := hashLegacySnapshotFile(ctx, dir, filepath.Base(filepath.FromSlash(canonical)), file, io.Discard)
	if err != nil || actual.Bytes != expected.Bytes || actual.Digest != expected.Digest {
		return errLegacySnapshotIntegrity
	}
	return nil
}

func (guard *legacySnapshotGuard) digest() string {
	if guard == nil {
		return ""
	}
	return guard.manifestDigest
}

func (guard *legacySnapshotGuard) entry(name string) (legacySnapshotEntry, bool) {
	for _, entry := range guard.manifest.Files {
		if entry.Path == name {
			return entry, true
		}
	}
	return legacySnapshotEntry{}, false
}

func canonicalLegacySnapshotPath(name string) (string, bool) {
	name = filepath.ToSlash(filepath.Clean(name))
	if !validLegacySnapshotManifestPath(name) {
		return "", false
	}
	return name, true
}
