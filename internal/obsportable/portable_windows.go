//go:build windows

// Package obsportable prepares the station's separate Windows OBS program.
package obsportable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/windows"

	"wechat-bot/internal/obspreview"
	"wechat-bot/internal/strictjson"
	"wechat-bot/internal/workspacefs"
)

const (
	releaseVersion = "33.0.1"
	releaseDigest  = "0d9436451c2d772b33428e3fdae9776f071895c335de17283176306fb72651d3"
	executable     = "bin/64bit/obs64.exe"
)

var requiredRuntimeFiles = []string{executable, "bin/64bit/obs.dll",
	"core/obs-outputs/obs-outputs.dll", "core/rtmp-services/rtmp-services.dll",
	"core/obs-x264/obs-x264.dll", "core/obs-ffmpeg/obs-ffmpeg.dll", "core/image-source/image-source.dll",
	"core/obs-transitions/obs-transitions.dll", "core/aja/aja.dll", "core/obs-websocket/obs-websocket.dll"}

var (
	ErrMissing = errors.New("portable OBS installation is missing")
	ErrInvalid = errors.New("portable OBS installation is incomplete or changed")
	ErrBusy    = errors.New("portable OBS installation is in use")
)

type manifest struct {
	Version       string            `json:"version"`
	ArchiveDigest string            `json:"archive_sha256"`
	Files         map[string]string `json:"files"`
}

type Installation struct {
	base, studio string
	files        map[string]string
}

// Open accepts only the host-private deployment path written by the station
// installer. It verifies the immutable runtime inventory before registration.
func Open(ctx context.Context, stationData, workspace string) (*Installation, error) {
	base := filepath.Join(stationData, "managed-obs")
	volume := filepath.VolumeName(base)
	if !filepath.IsAbs(base) || len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(workspace) ||
		pathContains(workspace, base) || pathContains(base, workspace) {
		return nil, ErrInvalid
	}
	if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
		return nil, ErrMissing
	}
	root, err := workspacefs.OpenPath(base)
	if err != nil {
		return nil, ErrInvalid
	}
	defer root.Close()
	studio, err := workspacefs.OpenExistingDirectory(root, "studio")
	if err != nil {
		return nil, ErrInvalid
	}
	defer studio.Close()
	f, err := workspacefs.OpenRegular(root, "install.json")
	if err != nil {
		return nil, ErrInvalid
	}
	const maxManifest = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(f, maxManifest+1))
	_ = f.Close()
	if err != nil || len(raw) > maxManifest {
		return nil, ErrInvalid
	}
	if _, err := strictjson.Decode(raw); err != nil {
		return nil, ErrInvalid
	}
	var m manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil || m.Version != releaseVersion || m.ArchiveDigest != releaseDigest {
		return nil, ErrInvalid
	}
	for _, required := range requiredRuntimeFiles {
		if _, ok := m.Files[required]; !ok {
			return nil, ErrInvalid
		}
	}
	seen := make(map[string]struct{}, len(m.Files))
	for name, digest := range m.Files {
		fold := strings.ToLower(name)
		if _, exists := seen[fold]; exists || !runtimePath(name) || !digestValid(digest) {
			return nil, ErrInvalid
		}
		seen[fold] = struct{}{}
	}
	in := &Installation{base: base, studio: filepath.Join(base, "studio"), files: m.Files}
	if err := in.verify(ctx, studio); err != nil {
		return nil, err
	}
	return in, nil
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func runtimePath(name string) bool {
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\:") {
		return false
	}
	top, tail, ok := strings.Cut(name, "/")
	if !ok || tail == "" || (top != "bin" && top != "data" && top != "core") {
		return false
	}

	return true
}

func digestValid(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size && value == strings.ToLower(value)
}

func (i *Installation) verify(ctx context.Context, studio *os.Root) error {
	count := 0
	err := fs.WalkDir(studio.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return ErrInvalid
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if name == "config" {
			if !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
				return ErrInvalid
			}
			return fs.SkipDir
		}
		if entry.IsDir() {
			if name == "bin" || name == "data" || name == "core" || runtimePath(name) {
				return nil
			}
			return ErrInvalid
		}
		expected, ok := i.files[name]
		if !ok {
			return ErrInvalid
		}
		file, err := workspacefs.OpenRegular(studio, filepath.FromSlash(name))
		if err != nil {
			return ErrInvalid
		}
		h := sha256.New()
		_, err = copyInstall(ctx, h, file)
		_ = file.Close()
		if err != nil || hex.EncodeToString(h.Sum(nil)) != expected {
			return ErrInvalid
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	if count != len(i.files) {
		return ErrInvalid
	}
	return nil
}

type Encoder struct {
	installation *Installation
	mu           sync.Mutex
	lock         windows.Handle
	process      *obspreview.EncoderProcess
	started      bool
	media        *ownedMedia
}

// Encoder reserves the private installation before a new profile is created.
// The non-inherited lock is released by Windows if the station exits.
func (i *Installation) Encoder(ctx context.Context) (*Encoder, error) {
	return i.EncoderWithMedia(ctx, nil)
}

// Media describes an MP4 stream already probed and authorized by the host. No
// model-provided path, URL, source type or FFmpeg options are accepted here.
// Width and Height are the probed display dimensions used for aspect fitting.
type Media struct {
	Source io.ReadCloser
	SHA256 string
	Bytes  int64
	Width  int
	Height int
	Loop   bool
}

// EncoderWithMedia reserves the installation and always takes ownership of a
// non-nil Source, including when validation or reservation fails. Start closes
// it after copying; every Start failure and Stop also close it. Close failures
// remain observable from Stop. Callers must not read or close Source afterward.
// A nil Media retains the standalone solid-color program.
func (i *Installation) EncoderWithMedia(ctx context.Context, media *Media) (_ *Encoder, result error) {
	var owned *ownedMedia
	if media != nil {
		owned = &ownedMedia{Media: *media}
	}
	defer func() {
		if result != nil && owned != nil {
			result = errors.Join(result, owned.close())
		}
	}()
	if media != nil && (media.Source == nil || !digestValid(media.SHA256) || media.Bytes <= 0 || media.Width <= 0 || media.Height <= 0) {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(filepath.Join(i.base, "session.lock"))
	if err != nil {
		return nil, ErrInvalid
	}
	lock, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return nil, ErrBusy
	}
	if err != nil {
		return nil, ErrInvalid
	}
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(lock, &info) != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(lock)
		return nil, ErrInvalid
	}
	studio, err := workspacefs.OpenPath(i.studio)
	if err != nil {
		_ = windows.CloseHandle(lock)
		return nil, ErrInvalid
	}
	defer studio.Close()
	if err = i.verify(ctx, studio); err != nil {
		_ = windows.CloseHandle(lock)
		return nil, err
	}
	return &Encoder{installation: i, lock: lock, media: owned}, nil
}

func (e *Encoder) Start(ctx context.Context, port int, ticket string) (result error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer func() {
		if result != nil && e.media != nil {
			result = errors.Join(result, e.media.close())
		}
	}()
	if e.lock == 0 || e.started || !validRelay(port, ticket) {
		return ErrInvalid
	}
	e.started = true
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := workspacefs.OpenPath(e.installation.studio)
	if err != nil {
		return ErrInvalid
	}
	defer root.Close()
	if err := writeProfileWithMedia(ctx, root, e.installation.studio, port, ticket, e.media); err != nil {
		return err
	}
	exe := filepath.Join(e.installation.studio, filepath.FromSlash(executable))
	e.process, err = obspreview.LaunchPortableEncoderProcess(ctx, obspreview.EncoderLaunch{
		Executable: exe, Directory: filepath.Dir(exe), SHA256: e.installation.files[executable],
		Arguments: []string{"--portable", "--multi", "--only-bundled-plugins", "--disable-updater",
			"--disable-missing-files-check", "--profile", profileName, "--collection", profileName, "--startstreaming"},
	})
	return err
}

func validRelay(port int, ticket string) bool {
	if port < 1 || port > 65535 || len(ticket) != 48 {
		return false
	}
	_, err := hex.DecodeString(ticket)
	return err == nil
}

func (e *Encoder) VerifyPublisher(ctx context.Context, socket *net.TCPConn) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.process == nil {
		return obspreview.ErrStreamEndpoint
	}
	return e.process.VerifyPublisher(ctx, socket)
}

func (e *Encoder) Stop(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var result error
	if e.media != nil {
		result = e.media.close()
	}
	if e.process != nil {
		if err := e.process.Stop(ctx); err != nil {
			return errors.Join(result, err)
		}
		e.process = nil
	}
	if e.lock != 0 {
		if err := windows.CloseHandle(e.lock); err != nil {
			return errors.Join(result, err)
		}
		e.lock = 0
	}
	return result
}

func loopbackService(port int, ticket string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type": "rtmp_custom",
		"settings": map[string]any{
			"server":   "rtmp://127.0.0.1:" + strconv.Itoa(port) + "/live",
			"key":      ticket,
			"use_auth": false,
		},
	})
}
