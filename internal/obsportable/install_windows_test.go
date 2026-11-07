//go:build windows

package obsportable

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

type syntheticZipEntry struct {
	name, data string
	mode       os.FileMode
}

func installFixture(t *testing.T, prefix string, extras ...syntheticZipEntry) (string, string, string, installPolicy) {
	t.Helper()
	dir := t.TempDir()
	archive := filepath.Join(dir, "offline.zip")
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entries := []syntheticZipEntry{{prefix + "data/theme.json", "synthetic data", 0600}}
	for _, name := range requiredRuntimeFiles {
		entries = append(entries, syntheticZipEntry{prefix + name, "synthetic " + name, 0600})
	}
	entries = append(entries, extras...)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(entry.mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buffer.Bytes()
	sum := sha256.Sum256(raw)
	if err := os.WriteFile(archive, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "station"), filepath.Join(dir, "workspace"), archive, installPolicy{hex.EncodeToString(sum[:])}
}
func assertNoInstallStage(t *testing.T, station string) {
	t.Helper()
	stages, err := filepath.Glob(filepath.Join(station, ".managed-obs-stage-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("stages remain: %v, %v", stages, err)
	}
}

func TestOfflineInstallSyntheticInventoryAndNoOverwrite(t *testing.T) {
	for _, prefix := range []string{"", "OBS-Studio/"} {
		t.Run(prefix, func(t *testing.T) {
			station, workspace, archive, policy := installFixture(t, prefix, syntheticZipEntry{"ignored/readme.txt", "ignored", 0600}, syntheticZipEntry{prefix + "data/nested/extra.dat", "preserved full tree", 0600})
			if err := installArchive(t.Context(), station, workspace, archive, policy); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(station, "managed-obs", "install.json"))
			if err != nil {
				t.Fatal(err)
			}
			var m manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			if m.Version != Version() || m.ArchiveDigest != policy.digest || len(m.Files) != len(requiredRuntimeFiles)+2 {
				t.Fatalf("inventory: %+v", m)
			}
			root, err := os.OpenRoot(filepath.Join(station, "managed-obs", "studio"))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			in := Installation{files: m.Files}
			if err := in.verify(t.Context(), root); err != nil {
				t.Fatal(err)
			}
			if _, err := root.Stat("ignored"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ignored tree extracted: %v", err)
			}
			if err := installArchive(t.Context(), station, workspace, archive, policy); !errors.Is(err, ErrAlreadyInstalled) {
				t.Fatalf("overwrite: %v", err)
			}
			after, _ := os.ReadFile(filepath.Join(station, "managed-obs", "install.json"))
			if !bytes.Equal(after, raw) {
				t.Fatal("existing manifest changed")
			}
			assertNoInstallStage(t, station)
		})
	}
}
func TestOfflineInstallProductionAlwaysChecksPinnedHash(t *testing.T) {
	station, workspace, archive, _ := installFixture(t, "")
	if Version() != "33.0.1" || ArchiveSHA256() != "0d9436451c2d772b33428e3fdae9776f071895c335de17283176306fb72651d3" {
		t.Fatal("release policy changed")
	}
	if err := Install(t.Context(), station, workspace, archive); !errors.Is(err, ErrArchiveDigest) {
		t.Fatalf("unpinned ZIP accepted: %v", err)
	}
	if _, err := os.Stat(station); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("digest failure wrote station: %v", err)
	}
	if err := Install(t.Context(), station, workspace, filepath.Join(t.TempDir(), "missing.zip")); !errors.Is(err, ErrArchiveMissing) || strings.Contains(err.Error(), "missing.zip") {
		t.Fatalf("missing or private path: %v", err)
	}
}
func TestOfflineInstallRejectsEveryUnsafeEntryEvenIgnoredRoots(t *testing.T) {
	for _, name := range []string{"../escape", "/abs", "C:/abs", "data/file:stream", "ignored/../escape", "ignored/CON.txt", "ignored/COM1", "ignored/LPT².dat", "ignored/trailing.", "ignored/trailing ", "ignored\\backslash", "ignored/a\x01", "ignored//empty", "ignored/./dot", "data/theme.json", "DATA/THEME.JSON", "bin/64bit/OBS64.EXE"} {
		t.Run(name, func(t *testing.T) {
			station, workspace, archive, policy := installFixture(t, "", syntheticZipEntry{name, "bad", 0600})
			err := installArchive(t.Context(), station, workspace, archive, policy)
			if !errors.Is(err, ErrArchiveFormat) {
				t.Fatalf("unsafe %q: %v", name, err)
			}
			if _, err := os.Stat(filepath.Join(station, "managed-obs")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsafe archive promoted")
			}
			assertNoInstallStage(t, station)
		})
	}
	station, workspace, archive, policy := installFixture(t, "", syntheticZipEntry{"ignored/link", "target", os.ModeSymlink | 0777})
	if err := installArchive(t.Context(), station, workspace, archive, policy); !errors.Is(err, ErrArchiveFormat) {
		t.Fatalf("ignored symlink: %v", err)
	}
}
func TestOfflineInstallRejectsMixedRootsAndMissingCore(t *testing.T) {
	station, workspace, archive, policy := installFixture(t, "", syntheticZipEntry{"other/bin/file", "mixed", 0600})
	if err := installArchive(t.Context(), station, workspace, archive, policy); !errors.Is(err, ErrArchiveFormat) {
		t.Fatalf("mixed roots: %v", err)
	}
	station, workspace, archive, policy = installFixture(t, "")
	// Change one mandatory module name without changing archive integrity policy.
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("core/aja/aja.dll"), []byte("core/aja/bja.dll"))
	if err := os.WriteFile(archive, raw, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	policy.digest = hex.EncodeToString(sum[:])
	if err := installArchive(t.Context(), station, workspace, archive, policy); !errors.Is(err, ErrArchiveFormat) {
		t.Fatalf("missing module: %v", err)
	}
	assertNoInstallStage(t, station)
}
func TestOfflineInstallSourceHandleDeniesWritesAndReparse(t *testing.T) {
	_, _, archive, _ := installFixture(t, "")
	file, err := openInstallArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if other, err := os.OpenFile(archive, os.O_WRONLY, 0600); err == nil {
		other.Close()
		t.Fatal("archive writer acquired during import")
	}
	if err := os.Rename(archive, archive+".moved"); err == nil {
		t.Fatal("archive replaced during import")
	}
	link := filepath.Join(t.TempDir(), "linked.zip")
	if err := os.Symlink(archive, link); err != nil {
		t.Logf("symlink creation unavailable: %v", err)
	} else if f, err := openInstallArchive(link); err == nil {
		f.Close()
		t.Fatal("archive symlink accepted")
	}
}
func TestOfflineInstallLockHeldAcrossProcesses(t *testing.T) {
	station, workspace, archive, policy := installFixture(t, "")
	if err := os.MkdirAll(station, 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := os.Open(station)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	lock, err := installRelativeHandle(parent, ".managed-obs-install.lock", windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE, 0, windows.FILE_OPEN_IF, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(lock)
	if err := installArchive(t.Context(), station, workspace, archive, policy); !errors.Is(err, ErrBusy) {
		t.Fatalf("installation contention: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOfflineInstallLockChildProbe$")
	cmd.Env = append(os.Environ(), "AHS_OBS_LOCK_PROBE=1", "AHS_OBS_INSTALL_STATION="+station)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child lock probe: %s %v", output, err)
	}
}
func TestOfflineInstallLockChildProbe(t *testing.T) {
	if os.Getenv("AHS_OBS_LOCK_PROBE") != "1" {
		return
	}
	parent, err := os.Open(os.Getenv("AHS_OBS_INSTALL_STATION"))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	lock, err := installRelativeHandle(parent, ".managed-obs-install.lock", windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE, 0, windows.FILE_OPEN_IF, windows.FILE_NON_DIRECTORY_FILE)
	if err == nil {
		windows.CloseHandle(lock)
		t.Fatal("cross-process installation lock acquired")
	}
	if !errors.Is(err, windows.STATUS_SHARING_VIOLATION) {
		t.Fatalf("wrong lock boundary: %v", err)
	}
}

type cancelInstallContext struct {
	context.Context
	calls, at int
	cancel    context.CancelFunc
}

func (c *cancelInstallContext) Err() error {
	c.calls++
	if c.calls >= c.at {
		c.cancel()
	}
	return c.Context.Err()
}
func TestOfflineInstallCancellationRollsBackOnlyOwnStage(t *testing.T) {
	station, workspace, archive, policy := installFixture(t, "")
	if err := os.MkdirAll(station, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(station, "private-sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &cancelInstallContext{Context: base, at: 25, cancel: cancel}
	err := installArchive(ctx, station, workspace, archive, policy)
	if !errors.Is(err, ErrInstallCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v; calls=%d", err, ctx.calls)
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "keep" {
		t.Fatal("unrelated station data touched")
	}
	if _, err := os.Stat(filepath.Join(station, "managed-obs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled runtime promoted")
	}
	assertNoInstallStage(t, station)
}
func TestOfflineInstallStorageAndNoClobberPromotion(t *testing.T) {
	station, workspace, archive, policy := installFixture(t, "")
	if err := os.WriteFile(station, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := installArchive(t.Context(), station, workspace, archive, policy); !errors.Is(err, ErrInstallStorage) || strings.Contains(err.Error(), station) {
		t.Fatalf("storage: %v", err)
	}
	raw, _ := os.ReadFile(station)
	if string(raw) != "keep" {
		t.Fatal("station file changed")
	}
	parentDir := t.TempDir()
	parent, err := os.Open(parentDir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	for _, name := range []string{"managed-obs", "stage"} {
		if err := os.Mkdir(filepath.Join(parentDir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := promoteInstall(parent, "stage"); !errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		t.Fatalf("empty existing directory overwrite: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parentDir, "stage")); err != nil {
		t.Fatal("stage lost on collision")
	}
}
func TestInstallCopyCancellationAndWriteFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if n, err := copyInstall(ctx, io.Discard, strings.NewReader("data")); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("copy cancellation: %d %v", n, err)
	}
	if n, err := copyInstall(t.Context(), shortInstallWriter{}, strings.NewReader("data")); n != 1 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %d %v", n, err)
	}
}

type shortInstallWriter struct{}

func (shortInstallWriter) Write([]byte) (int, error) { return 1, nil }

func TestOfflineInstallCancellationDuringArchiveHashAndInvalidPaths(t *testing.T) {
	station, workspace, archive, policy := installFixture(t, "")
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &cancelInstallContext{Context: base, at: 3, cancel: cancel}
	if err := installArchive(ctx, station, workspace, archive, policy); !errors.Is(err, ErrInstallCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("hash cancellation: %v", err)
	}
	if _, err := os.Stat(station); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hash cancellation created station: %v", err)
	}
	for _, paths := range [][3]string{{station, station, archive}, {station, filepath.Join(station, "workspace"), archive}, {station, workspace, "relative.zip"}} {
		if err := installArchive(t.Context(), paths[0], paths[1], paths[2], policy); !errors.Is(err, ErrArchiveFormat) {
			t.Fatalf("path boundary: %v", err)
		}
	}
}

func TestOfflineInstallNormalizesOnlyAbsoluteLocalDriveInputs(t *testing.T) {
	station, workspace, archive, _ := installFixture(t, "")
	forward := func(name string) string { return strings.ReplaceAll(name, `\`, "/") }
	redundantStation := forward(filepath.Dir(station)) + "//station/."
	// Reaching the production digest boundary proves all input paths were
	// canonicalized while preserving the pinned archive policy.
	if err := Install(t.Context(), redundantStation, forward(workspace), forward(archive)); !errors.Is(err, ErrArchiveDigest) {
		t.Fatalf("forward slash inputs: %v", err)
	}
	if got := canonicalInstallInput(redundantStation); got != station {
		t.Fatalf("canonical station=%q, want %q", got, station)
	}
	for _, name := range []string{"relative.zip", `C:relative.zip`, `\\server\share\archive.zip`} {
		if got := canonicalInstallInput(name); got != name {
			t.Fatalf("nonlocal or relative input normalized: %q => %q", name, got)
		}
		if err := Install(t.Context(), station, workspace, name); !errors.Is(err, ErrArchiveFormat) {
			t.Fatalf("nonlocal or relative input accepted: %q: %v", name, err)
		}
	}
}
