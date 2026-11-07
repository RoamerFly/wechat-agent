//go:build windows

package obsportable

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
	"wechat-bot/internal/workspacefs"
)

// Version and ArchiveSHA256 describe the only production archive accepted.
func Version() string       { return releaseVersion }
func ArchiveSHA256() string { return releaseDigest }

var (
	ErrArchiveMissing   = errors.New("portable OBS archive is missing")
	ErrArchiveDigest    = errors.New("portable OBS archive digest does not match")
	ErrArchiveFormat    = errors.New("portable OBS archive or installation path is invalid")
	ErrAlreadyInstalled = errors.New("portable OBS is already installed")
	ErrInstallCanceled  = errors.New("portable OBS installation was canceled")
	ErrInstallStorage   = errors.New("portable OBS installation storage operation failed")
)

// Keep local paths and OS diagnostics out of UI messages while preserving
// errors.Is and the underlying cause for private host diagnostics.
type installError struct{ kind, cause error }

func (e *installError) Error() string        { return e.kind.Error() }
func (e *installError) Unwrap() []error      { return []error{e.kind, e.cause} }
func installFailure(kind, cause error) error { return &installError{kind, cause} }
func installContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return installFailure(ErrInstallCanceled, err)
	}
	return nil
}

type installPolicy struct{ digest string }

// Install imports a locally selected, pinned official ZIP without running OBS,
// scripts or a downloader. All writes remain below the private station root.
func Install(ctx context.Context, stationData, workspace, archivePath string) error {
	return installArchive(ctx, canonicalInstallInput(stationData), canonicalInstallInput(workspace), canonicalInstallInput(archivePath), installPolicy{releaseDigest})
}

// Normalize only already absolute local drive paths. Relative paths and UNC
// paths retain their original form and are rejected by localInstallPath.
func canonicalInstallInput(name string) string {
	volume := filepath.VolumeName(name)
	if filepath.IsAbs(name) && len(volume) == 2 && volume[1] == ':' {
		return filepath.Clean(name)
	}
	return name
}

func installArchive(ctx context.Context, stationData, workspace, archivePath string, policy installPolicy) (result error) {
	if err := installContext(ctx); err != nil {
		return err
	}
	if !localInstallPath(stationData) || !localInstallPath(workspace) || !localInstallPath(archivePath) ||
		pathContains(workspace, stationData) || pathContains(stationData, workspace) {
		return installFailure(ErrArchiveFormat, ErrInvalid)
	}
	archive, err := openInstallArchive(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	h := sha256.New()
	if _, err := copyInstall(ctx, h, archive); err != nil {
		return installCopyFailure(ctx, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != policy.digest {
		return ErrArchiveDigest
	}
	info, err := archive.Stat()
	if err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	zr, err := zip.NewReader(archive, info.Size())
	if err != nil {
		return installFailure(ErrArchiveFormat, err)
	}
	entries, err := installEntries(ctx, zr)
	if err != nil {
		return err
	}
	station, err := workspacefs.EnsurePath(stationData)
	if err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	defer station.Close()
	stationHandle, err := station.Open(".")
	if err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	defer stationHandle.Close()
	lock, err := installRelativeHandle(stationHandle, ".managed-obs-install.lock", windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE, 0, windows.FILE_OPEN_IF, windows.FILE_NON_DIRECTORY_FILE)
	if errors.Is(err, windows.STATUS_SHARING_VIOLATION) {
		return ErrBusy
	}
	if err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	defer windows.CloseHandle(lock)
	if err := rejectInstallReparse(lock); err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	if _, err := station.Lstat("managed-obs"); err == nil {
		return ErrAlreadyInstalled
	} else if !errors.Is(err, os.ErrNotExist) {
		return installFailure(ErrInstallStorage, err)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	stageName := ".managed-obs-stage-" + hex.EncodeToString(nonce)
	if err := station.Mkdir(stageName, 0700); err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	// Root.RemoveAll does not follow symlinks, and only this invocation's random
	// direct child is removed. Never roll back an existing or promoted runtime.
	defer func() {
		if err := station.RemoveAll(stageName); err != nil {
			result = installFailure(ErrInstallStorage, errors.Join(result, err))
		}
	}()
	stage, err := workspacefs.EnsureDirectory(station, stageName)
	if err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	defer stage.Close()
	studio, err := workspacefs.EnsureDirectory(stage, "studio")
	if err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	defer studio.Close()
	m := manifest{Version: releaseVersion, ArchiveDigest: policy.digest, Files: map[string]string{}}
	for _, entry := range entries {
		if err := installContext(ctx); err != nil {
			return err
		}
		name := filepath.FromSlash(entry.name)
		if entry.file.FileInfo().IsDir() {
			dir, err := workspacefs.EnsureDirectory(studio, name)
			if err != nil {
				return installFailure(ErrInstallStorage, err)
			}
			dir.Close()
			continue
		}
		parent, err := workspacefs.EnsureDirectory(studio, filepath.Dir(name))
		if err != nil {
			return installFailure(ErrInstallStorage, err)
		}
		output, err := parent.OpenFile(filepath.Base(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		parent.Close()
		if err != nil {
			return installFailure(ErrInstallStorage, err)
		}
		input, err := entry.file.Open()
		if err != nil {
			output.Close()
			return installFailure(ErrArchiveFormat, err)
		}
		hash := sha256.New()
		_, copyErr := copyInstall(ctx, io.MultiWriter(output, hash), input)
		closeErr := errors.Join(input.Close(), output.Close())
		if copyErr != nil {
			return installCopyFailure(ctx, copyErr)
		}
		if closeErr != nil {
			return installFailure(ErrInstallStorage, closeErr)
		}
		m.Files[entry.name] = hex.EncodeToString(hash.Sum(nil))
	}
	for _, name := range requiredRuntimeFiles {
		if _, ok := m.Files[name]; !ok {
			return ErrArchiveFormat
		}
	}
	for _, name := range []string{"bin", "data", "core"} {
		dir, err := workspacefs.OpenExistingDirectory(studio, name)
		if err != nil {
			return installFailure(ErrArchiveFormat, err)
		}
		dir.Close()
	}
	in := Installation{files: m.Files}
	if err := in.verify(ctx, studio); err != nil {
		if ctx.Err() != nil {
			return installFailure(ErrInstallCanceled, ctx.Err())
		}
		return installFailure(ErrArchiveFormat, err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	f, err := stage.OpenFile("install.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	_, writeErr := f.Write(raw)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	if err := installContext(ctx); err != nil {
		return err
	}
	if err := studio.Close(); err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	if err := stage.Close(); err != nil {
		return installFailure(ErrInstallStorage, err)
	}
	// Native rename with ReplaceIfExists=false is an atomic no-clobber commit.
	if err := promoteInstall(stationHandle, stageName); err != nil {
		if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			return ErrAlreadyInstalled
		}
		return installFailure(ErrInstallStorage, err)
	}
	return nil
}

func localInstallPath(name string) bool {
	volume := filepath.VolumeName(name)
	return filepath.IsAbs(name) && filepath.Clean(name) == name && len(volume) == 2 && volume[1] == ':' &&
		!strings.ContainsAny(name[2:], ":\x00")
}

func openInstallArchive(name string) (*os.File, error) {
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, installFailure(ErrArchiveFormat, err)
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, installFailure(ErrArchiveMissing, err)
	}
	if err != nil {
		return nil, installFailure(ErrInstallStorage, err)
	}
	if err := rejectInstallReparse(handle); err != nil {
		windows.CloseHandle(handle)
		return nil, installFailure(ErrArchiveFormat, err)
	}
	return os.NewFile(uintptr(handle), name), nil
}
func rejectInstallReparse(handle windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return ErrInvalid
	}
	return nil
}

func installRelativeHandle(parent *os.File, name string, access, share, disposition, options uint32) (windows.Handle, error) {
	value, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: windows.Handle(parent.Fd()), ObjectName: value, Attributes: windows.OBJ_CASE_INSENSITIVE}
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access, &oa, &windows.IO_STATUS_BLOCK{}, nil, windows.FILE_ATTRIBUTE_NORMAL, share, disposition, options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	return handle, err
}
func promoteInstall(parent *os.File, stage string) error {
	handle, err := installRelativeHandle(parent, stage, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var attributes windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &attributes); err != nil {
		return err
	}
	if attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrInvalid
	}
	// FILE_RENAME_INFORMATION uses a BOOLEAN followed by naturally aligned HANDLE.
	var info struct {
		Replace uint8
		Root    windows.Handle
		Length  uint32
		Name    [32]uint16
	}
	info.Root = windows.Handle(parent.Fd())
	value, _ := windows.UTF16FromString("managed-obs")
	copy(info.Name[:], value)
	info.Length = uint32((len(value) - 1) * 2)
	return windows.NtSetInformationFile(handle, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Offsetof(info.Name))+info.Length, windows.FileRenameInformation)
}

type installEntry struct {
	name string
	file *zip.File
}

func installEntries(ctx context.Context, zr *zip.Reader) ([]installEntry, error) {
	seen := map[string]bool{}
	prefixes := map[string]bool{}
	names := make([]string, len(zr.File))
	for i, entry := range zr.File {
		if err := installContext(ctx); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if !safeInstallEntry(name) || entry.Mode()&fs.ModeSymlink != 0 || (!entry.Mode().IsRegular() && !entry.FileInfo().IsDir()) || entry.ExternalAttrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return nil, ErrArchiveFormat
		}
		fold := strings.ToLower(name)
		if _, ok := seen[fold]; ok {
			return nil, ErrArchiveFormat
		}
		seen[fold] = entry.FileInfo().IsDir()
		names[i] = name
		parts := strings.Split(name, "/")
		if runtimeRoot(parts[0]) {
			prefixes[""] = true
		} else if len(parts) > 1 && runtimeRoot(parts[1]) {
			prefixes[parts[0]] = true
		}
	}
	// Reject file/directory aliases including implicit parent directories.
	for name := range seen {
		for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(name))); parent != "."; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
			if dir, ok := seen[parent]; ok && !dir {
				return nil, ErrArchiveFormat
			}
		}
	}
	if len(prefixes) != 1 {
		return nil, ErrArchiveFormat
	}
	prefix := ""
	for name := range prefixes {
		prefix = name
	}
	var result []installEntry
	for i, name := range names {
		if prefix != "" {
			if !strings.HasPrefix(name, prefix+"/") {
				continue
			}
			name = strings.TrimPrefix(name, prefix+"/")
		}
		first, _, _ := strings.Cut(name, "/")
		if runtimeRoot(first) {
			result = append(result, installEntry{name, zr.File[i]})
		}
	}
	return result, nil
}
func runtimeRoot(name string) bool { return name == "bin" || name == "data" || name == "core" }
func safeInstallEntry(name string) bool {
	if !utf8.ValidString(name) || !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:<>\"|?*\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
		for _, r := range part {
			if r < 32 {
				return false
			}
		}
		base, _, _ := strings.Cut(strings.ToUpper(part), ".")
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return false
		}
		if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
			return false
		}
		if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
			if strings.ContainsAny(strings.TrimPrefix(strings.TrimPrefix(base, "COM"), "LPT"), "¹²³") {
				return false
			}
		}
	}
	return true
}
func copyInstall(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			written, err := dst.Write(buffer[:n])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}
func installCopyFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return installFailure(ErrInstallCanceled, ctx.Err())
	}
	if errors.Is(err, zip.ErrChecksum) || errors.Is(err, zip.ErrFormat) || errors.Is(err, zip.ErrAlgorithm) || errors.Is(err, io.ErrUnexpectedEOF) {
		return installFailure(ErrArchiveFormat, err)
	}
	return installFailure(ErrInstallStorage, err)
}
