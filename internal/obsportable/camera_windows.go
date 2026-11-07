//go:build windows

package obsportable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"wechat-bot/internal/obspreview"
	"wechat-bot/internal/workspacefs"
)

const cameraPlugin = "core/win-dshow/win-dshow.dll"
const cameraMapping = "OBSVirtualCamVideo"

var ErrCameraConsumerBusy = errors.New("OBS virtual camera shared memory remains held by another consumer")
var errCameraNotReady = errors.New("virtual camera queue is not ready")
var cameraOpenFileMapping = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenFileMappingW")

type cameraProcess interface {
	Running() bool
	StopGracefully(context.Context) error
}

// Camera owns a separately verified, locked portable OBS session. It does not
// register the DirectShow driver, start streaming, or control an existing OBS.
// Readiness proves local video publication, not recipient device selection.
type Camera struct {
	mu                    sync.Mutex
	lease                 *Encoder
	process               cameraProcess
	started, profileOwned bool
	consumerPending       bool
	mappingName           string
	launch                func(context.Context, obspreview.EncoderLaunch) (cameraProcess, error)
}

// CameraWithMedia takes ownership of Source on every path, like EncoderWithMedia.
// A nil media selects the dedicated solid-color program.
func (i *Installation) CameraWithMedia(ctx context.Context, media *Media) (*Camera, error) {
	lease, err := i.EncoderWithMedia(ctx, media)
	if err != nil {
		return nil, err
	}
	// OBS 33.0.1's Windows package places its producer module here. Keep this
	// camera-only requirement out of the existing encoder installation contract.
	if digest, ok := i.files[cameraPlugin]; !ok || !digestValid(digest) {
		return nil, errors.Join(ErrInvalid, lease.Stop(ctx))
	}
	return &Camera{lease: lease, mappingName: cameraMapping,
		launch: func(ctx context.Context, launch obspreview.EncoderLaunch) (cameraProcess, error) {
			return obspreview.LaunchPortableEncoderProcess(ctx, launch)
		}}, nil
}

func cameraArguments() []string {
	return []string{"--portable", "--multi", "--only-bundled-plugins", "--disable-updater",
		"--disable-missing-files-check", "--profile", profileName, "--collection", profileName, "--startvirtualcam"}
}

func (c *Camera) Start(ctx context.Context) (result error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lease == nil || c.lease.lock == 0 || c.started {
		return ErrInvalid
	}
	c.started = true
	defer func() {
		if result != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			result = errors.Join(result, c.stopLocked(cleanup))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := requireCameraMappingAbsent(c.mappingName); err != nil {
		return err
	}
	root, err := workspacefs.OpenPath(c.lease.installation.studio)
	if err != nil {
		return ErrInvalid
	}
	defer root.Close()
	if err := c.lease.installation.verify(ctx, root); err != nil {
		return err
	}
	// Own config before writing so partial profile/media failures are cleaned.
	// First validate the config entry, avoiding ownership of an unrelated link.
	if info, err := root.Lstat("config"); err == nil {
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return ErrInvalid
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ErrInvalid
	}
	c.profileOwned = true
	if err := writeProfileWithMedia(ctx, root, c.lease.installation.studio, 1, strings.Repeat("0", 48), c.lease.media); err != nil {
		return err
	}
	// This session has no streaming service. The profile writer is reused only
	// for fixed video dimensions, disabled WebSocket, and verified local media.
	if err := root.Remove(filepath.FromSlash("config/obs-studio/basic/profiles/" + profileName + "/service.json")); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Recheck immediately before launch. OBS itself also refuses an existing
	// mapping; successful fresh-log evidence below is required for ownership.
	if err := requireCameraMappingAbsent(c.mappingName); err != nil {
		return err
	}
	exe := filepath.Join(c.lease.installation.studio, filepath.FromSlash(executable))
	c.process, err = c.launch(ctx, obspreview.EncoderLaunch{Executable: exe, Directory: filepath.Dir(exe),
		SHA256: c.lease.installation.files[executable], Arguments: cameraArguments()})
	if err != nil {
		return err
	}
	return c.checkLocked(ctx)
}

// Check probes frame freshness within one second, so a frozen producer cannot
// hold up Stop for a long host request timeout. This is the health probe window;
// Start's initial readiness wait uses the caller's entire context instead.
func (c *Camera) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkLocked(ctx)
}

func (c *Camera) checkLocked(ctx context.Context) error {
	if c.lease == nil || !c.profileOwned || c.process == nil {
		return ErrInvalid
	}
	root, err := workspacefs.OpenPath(c.lease.installation.studio)
	if err != nil {
		return ErrInvalid
	}
	defer root.Close()
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	var prior uint32
	progress := 0
	havePrior := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !c.process.Running() {
			return errors.New("owned OBS exited before virtual camera readiness")
		}
		logged, err := cameraStartedInLogs(root)
		if err != nil {
			return err
		}
		if logged {
			sample, err := sampleCameraQueue(c.mappingName)
			if err == nil {
				advance := sample - prior // unsigned arithmetic permits counter wrap.
				if havePrior && advance > 0 && advance < 1<<31 {
					progress++
				} else {
					progress = 0
				}
				prior, havePrior = sample, true
				if progress >= 2 && c.process.Running() {
					return nil
				}
			} else if errors.Is(err, errCameraNotReady) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
				havePrior, progress = false, 0
			} else {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Stop closes only this process. A lingering mapping is reported after owned
// profile cleanup and lock release: another consumer may retain its handle.
func (c *Camera) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopLocked(ctx)
}

func (c *Camera) stopLocked(ctx context.Context) error {
	if c.lease == nil {
		return c.checkConsumerReleased()
	}
	var result error
	if c.lease.media != nil {
		result = c.lease.media.close()
	}
	hadProcess := c.process != nil
	if hadProcess {
		// Running(false) may mean stopping, not a confirmed process exit.
		// StopGracefully's successful wait is the only cleanup authority.
		if err := c.process.StopGracefully(ctx); err != nil {
			return errors.Join(result, err)
		}
		c.process = nil
		c.consumerPending = true
	}
	if c.profileOwned {
		root, err := workspacefs.OpenPath(c.lease.installation.studio)
		if err != nil {
			return errors.Join(result, err)
		}
		err = root.RemoveAll("config")
		_ = root.Close()
		if err != nil {
			return errors.Join(result, err)
		}
		c.profileOwned = false
	}
	if err := c.lease.Stop(ctx); err != nil {
		return errors.Join(result, err)
	}
	c.lease = nil
	return errors.Join(result, c.checkConsumerReleased())
}

func (c *Camera) checkConsumerReleased() error {
	if !c.consumerPending {
		return nil
	}
	if err := requireCameraMappingAbsent(c.mappingName); err != nil {
		if errors.Is(err, ErrBusy) {
			return ErrCameraConsumerBusy
		}
		return err
	}
	c.consumerPending = false
	return nil
}

func openCameraMapping(name string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	handle, _, callErr := cameraOpenFileMapping.Call(windows.FILE_MAP_READ, 0, uintptr(unsafe.Pointer(p)))
	if handle == 0 {
		return 0, callErr
	}
	return windows.Handle(handle), nil
}

func requireCameraMappingAbsent(name string) error {
	handle, err := openCameraMapping(name)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	} // Access denied is not absence.
	_ = windows.CloseHandle(handle)
	return ErrBusy
}

// The ABI below is pinned to the upstream 33.0.1 queue_header, including its
// uint64 alignment padding. Mapping only 80 bytes avoids frames and timestamps.
// All loads are read-only atomics, never Interlocked operations that can write.
// https://github.com/obsproject/obs-studio/blob/33.0.1/shared/obs-shared-memory-queue/shared-memory-queue.c
func sampleCameraQueue(name string) (uint32, error) {
	handle, err := openCameraMapping(name)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	view, err := windows.MapViewOfFile(handle, windows.FILE_MAP_READ, 0, 0, 80)
	if err != nil {
		return 0, err
	}
	defer windows.UnmapViewOfFile(view)
	base := cameraViewPointer(view)
	load := func(offset uintptr) uint32 { return atomic.LoadUint32((*uint32)(unsafe.Add(base, offset))) }
	if load(8) != 2 {
		return 0, errCameraNotReady
	}
	if load(24) != 0 || load(28) != 1280 || load(32) != 720 {
		return 0, ErrInvalid
	}
	// NV12 frame byte count + its 32-byte timestamp/header, aligned to 32.
	const stride = 1280*720*3/2 + 32
	for j := uintptr(0); j < 3; j++ {
		if load(12+j*4) != uint32(96+j*stride) {
			return 0, ErrInvalid
		}
	}
	interval := atomic.LoadUint64((*uint64)(unsafe.Add(base, 40)))
	if interval != 10000000/30 {
		return 0, ErrInvalid
	}
	return load(4), nil
}

// MapViewOfFile returns a native pointer encoded as uintptr. Reinterpret that
// native result once, then use bounded pointer arithmetic inside the live view.
// This is non-Go memory and is never retained after UnmapViewOfFile.
func cameraViewPointer(view uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&view))
}

// Config was removed before launch, so these logs belong to this session.
// Bounds prevent hostile/truncated logs from exhausting memory or scan time.
func cameraStartedInLogs(root *os.Root) (bool, error) {
	const directory = "config/obs-studio/logs"
	logs, err := workspacefs.OpenExistingDirectory(root, filepath.FromSlash(directory))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer logs.Close()
	dir, err := logs.Open(".")
	if err != nil {
		return false, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(33)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if len(entries) > 32 {
		return false, errors.New("too many OBS camera log entries")
	}
	found, logCount := false, 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}
		logCount++
		if logCount > 1 {
			return false, errors.New("multiple OBS camera process logs in one session")
		}
		file, err := workspacefs.OpenRegular(logs, entry.Name())
		if err != nil {
			return false, err
		}
		const maxLog = 2 << 20
		raw, readErr := io.ReadAll(io.LimitReader(file, maxLog+1))
		_ = file.Close()
		if readErr != nil {
			return false, readErr
		}
		if len(raw) > maxLog {
			return false, fmt.Errorf("OBS camera log exceeds %d bytes", maxLog)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			message := strings.TrimSpace(line)
			if strings.HasSuffix(message, ": Virtual output stopped") || strings.HasSuffix(message, ": Virtual output stopping") ||
				strings.HasSuffix(message, ": starting virtual-output failed") {
				return false, errors.New("owned OBS virtual camera output stopped or failed")
			}
			found = found || strings.HasSuffix(message, ": Virtual output started")
		}
	}
	return found, nil
}
