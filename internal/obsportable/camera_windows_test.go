//go:build windows

package obsportable

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"wechat-bot/internal/obspreview"
)

func syntheticCameraInstallation(t *testing.T) *Installation {
	t.Helper()
	station, workspace, studio := syntheticInstallation(t)
	manifestPath := filepath.Join(station, "managed-obs", "install.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic camera module, never executable")
	module := filepath.Join(studio, filepath.FromSlash(cameraPlugin))
	if err := os.MkdirAll(filepath.Dir(module), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	m.Files[cameraPlugin] = hex.EncodeToString(sum[:])
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	install, err := Open(t.Context(), station, workspace)
	if err != nil {
		t.Fatal(err)
	}
	return install
}

func randomCameraMapping(t *testing.T) string {
	t.Helper()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("StationCameraTest-%x", id)
}

type syntheticQueue struct {
	handle windows.Handle
	view   uintptr
}

func newSyntheticQueue(t *testing.T, name string) *syntheticQueue {
	t.Helper()
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, 80, ptr)
	if err != nil {
		t.Fatal(err)
	}
	view, err := windows.MapViewOfFile(handle, windows.FILE_MAP_WRITE, 0, 0, 80)
	if err != nil {
		windows.CloseHandle(handle)
		t.Fatal(err)
	}
	q := &syntheticQueue{handle: handle, view: view}
	q.store(28, 1280)
	q.store(32, 720)
	for j := uintptr(0); j < 3; j++ {
		q.store(12+j*4, uint32(96+j*(1280*720*3/2+32)))
	}
	atomic.StoreUint64((*uint64)(unsafe.Add(cameraViewPointer(view), 40)), 10000000/30)
	q.store(8, 2)
	t.Cleanup(q.close)
	return q
}
func (q *syntheticQueue) store(offset uintptr, value uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Add(cameraViewPointer(q.view), offset)), value)
}
func (q *syntheticQueue) close() {
	if q.view != 0 {
		windows.UnmapViewOfFile(q.view)
		q.view = 0
	}
	if q.handle != 0 {
		windows.CloseHandle(q.handle)
		q.handle = 0
	}
}

type fakeCameraProcess struct {
	running atomic.Bool
	stop    func(context.Context) error
}

func (p *fakeCameraProcess) Running() bool { return p.running.Load() }
func (p *fakeCameraProcess) StopGracefully(ctx context.Context) error {
	if p.stop != nil {
		if err := p.stop(ctx); err != nil {
			return err
		}
	}
	p.running.Store(false)
	return nil
}

func writeCameraLog(t *testing.T, studio, message string) {
	t.Helper()
	dir := filepath.Join(studio, "config", "obs-studio", "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current.txt"), []byte(message), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCameraRequiresSeparatePluginWithoutChangingEncoderContract(t *testing.T) {
	station, workspace, _ := syntheticInstallation(t)
	install, err := Open(t.Context(), station, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.CameraWithMedia(t.Context(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing camera plugin: %v", err)
	}
	encoder, err := install.Encoder(t.Context())
	if err != nil {
		t.Fatalf("camera failure retained encoder lock: %v", err)
	}
	if err := encoder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	install = syntheticCameraInstallation(t)
	camera, err := install.CameraWithMedia(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = camera.Stop(context.Background()) })
	if _, err := install.Encoder(t.Context()); !errors.Is(err, ErrBusy) {
		t.Fatalf("encoder acquired camera lease: %v", err)
	}
	if err := os.WriteFile(filepath.Join(install.studio, filepath.FromSlash(cameraPlugin)), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	camera.mappingName = randomCameraMapping(t)
	if err := camera.Start(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("changed plugin accepted: %v", err)
	}
}

func TestCameraReadOnlyQueueABIAndBusyBoundary(t *testing.T) {
	name := randomCameraMapping(t)
	if err := requireCameraMappingAbsent(name); err != nil {
		t.Fatal(err)
	}
	queue := newSyntheticQueue(t, name)
	queue.store(4, 17)
	if err := requireCameraMappingAbsent(name); !errors.Is(err, ErrBusy) {
		t.Fatalf("existing mapping accepted: %v", err)
	}
	if sample, err := sampleCameraQueue(name); err != nil || sample != 17 {
		t.Fatalf("queue sample=%d err=%v", sample, err)
	}
	queue.store(28, 640)
	if _, err := sampleCameraQueue(name); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong geometry accepted: %v", err)
	}
	queue.store(28, 1280)
	queue.store(12, 80)
	if _, err := sampleCameraQueue(name); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong frame offset accepted: %v", err)
	}
	queue.store(12, 96)
	queue.store(8, 1)
	if _, err := sampleCameraQueue(name); !errors.Is(err, errCameraNotReady) {
		t.Fatalf("starting accepted: %v", err)
	}
	queue.close()
	if err := requireCameraMappingAbsent(name); err != nil {
		t.Fatal(err)
	}
}

func TestCameraStartRequiresFreshLogAndAdvancingQueueAndCleansProfile(t *testing.T) {
	install := syntheticCameraInstallation(t)
	data := []byte("synthetic authorized mp4 stream")
	sum := sha256.Sum256(data)
	media := &Media{Source: ioReadCloser{Reader: bytes.NewReader(data)}, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)), Width: 1920, Height: 1080, Loop: true}
	camera, err := install.CameraWithMedia(t.Context(), media)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = camera.Stop(context.Background()) })
	camera.mappingName = randomCameraMapping(t)
	writeCameraLog(t, install.studio, "12:00:00.000: Virtual output started\n")
	process := &fakeCameraProcess{}
	process.running.Store(true)
	camera.launch = func(ctx context.Context, config obspreview.EncoderLaunch) (cameraProcess, error) {
		if !reflect.DeepEqual(config.Arguments, cameraArguments()) {
			t.Fatalf("unexpected camera args: %v", config.Arguments)
		}
		if strings.Contains(strings.Join(config.Arguments, " "), "startstreaming") {
			t.Fatal("streaming requested")
		}
		if _, err := os.Stat(filepath.Join(install.studio, "config", "obs-studio", "logs", "current.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old log retained: %v", err)
		}
		if _, err := os.Stat(filepath.Join(install.studio, "config", "obs-studio", "basic", "profiles", profileName, "service.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("streaming service retained: %v", err)
		}
		copied, err := os.ReadFile(filepath.Join(install.studio, filepath.FromSlash(programMedia)))
		if err != nil || !bytes.Equal(copied, data) {
			t.Fatalf("media copy: %v", err)
		}
		queue := newSyntheticQueue(t, camera.mappingName)
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			var inc uint32
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					inc++
					queue.store(4, inc)
				}
			}
		}()
		process.stop = func(context.Context) error { close(stop); <-done; queue.close(); return nil }
		writeCameraLog(t, install.studio, "12:00:01.000: Virtual output started\n")
		return process, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := camera.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := camera.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := camera.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(install.studio, "config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile not removed: %v", err)
	}
	if err := camera.Stop(ctx); err != nil {
		t.Fatalf("idempotent stop: %v", err)
	}
	encoder, err := install.Encoder(ctx)
	if err != nil {
		t.Fatalf("lock retained: %v", err)
	}
	_ = encoder.Stop(ctx)
}

// Lightweight adapter keeps test data synthetic and never executes OBS.
type ioReadCloser struct{ *bytes.Reader }

func (ioReadCloser) Close() error { return nil }

func TestCameraFailureRetainsLeaseUntilConfirmedExitAndReportsConsumer(t *testing.T) {
	install := syntheticCameraInstallation(t)
	camera, err := install.CameraWithMedia(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = camera.Stop(context.Background()) })
	camera.mappingName = randomCameraMapping(t)
	process := &fakeCameraProcess{stop: func(context.Context) error { return context.DeadlineExceeded }}
	// Running=false is insufficient authority to remove an active profile.
	camera.process = process
	camera.profileOwned = true
	if err := os.MkdirAll(filepath.Join(install.studio, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := camera.Stop(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop failure: %v", err)
	}
	if _, err := install.Encoder(t.Context()); !errors.Is(err, ErrBusy) {
		t.Fatalf("failed stop released lock: %v", err)
	}
	queue := newSyntheticQueue(t, camera.mappingName)
	process.stop = nil
	if err := camera.Stop(t.Context()); !errors.Is(err, ErrCameraConsumerBusy) {
		t.Fatalf("consumer retention: %v", err)
	}
	if err := camera.Stop(t.Context()); !errors.Is(err, ErrCameraConsumerBusy) {
		t.Fatalf("consumer retry: %v", err)
	}
	queue.close()
	if err := camera.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCameraReadinessRejectsFrozenQueueAndMissingFreshLog(t *testing.T) {
	for _, logged := range []bool{false, true} {
		t.Run(fmt.Sprint(logged), func(t *testing.T) {
			install := syntheticCameraInstallation(t)
			camera, err := install.CameraWithMedia(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = camera.Stop(context.Background()) })
			camera.mappingName = randomCameraMapping(t)
			process := &fakeCameraProcess{}
			process.running.Store(true)
			camera.launch = func(context.Context, obspreview.EncoderLaunch) (cameraProcess, error) {
				queue := newSyntheticQueue(t, camera.mappingName)
				process.stop = func(context.Context) error { queue.close(); return nil }
				if logged {
					writeCameraLog(t, install.studio, "12:00:01.000: Virtual output started\n")
				}
				return process, nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 180*time.Millisecond)
			defer cancel()
			if err := camera.Start(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("missing evidence accepted: %v", err)
			}
			if camera.lease != nil {
				t.Fatal("failed start did not clean lease")
			}
		})
	}
}

func TestCameraHealthProbeBoundsConcurrentStop(t *testing.T) {
	install := syntheticCameraInstallation(t)
	camera, err := install.CameraWithMedia(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = camera.Stop(context.Background()) })
	camera.mappingName = randomCameraMapping(t)
	queue := newSyntheticQueue(t, camera.mappingName)
	process := &fakeCameraProcess{stop: func(context.Context) error { queue.close(); return nil }}
	process.running.Store(true)
	camera.process, camera.profileOwned = process, true
	writeCameraLog(t, install.studio, "12:00:01.000: Virtual output started\n")
	probeDone := make(chan error, 1)
	go func() { probeDone <- camera.Check(context.Background()) }()
	// Wait until Check actually holds the lock before trying concurrent Stop.
	deadline := time.Now().Add(time.Second)
	for camera.mu.TryLock() {
		camera.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("health probe did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	before := time.Now()
	if err := camera.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(before); elapsed > 2*time.Second {
		t.Fatalf("Stop blocked behind health probe for %v", elapsed)
	}
	if err := <-probeDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("frozen health probe: %v", err)
	}
}

func TestCameraRefusesExistingMappingBeforeMutatingProfile(t *testing.T) {
	install := syntheticCameraInstallation(t)
	camera, err := install.CameraWithMedia(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = camera.Stop(context.Background()) })
	camera.mappingName = randomCameraMapping(t)
	queue := newSyntheticQueue(t, camera.mappingName)
	writeCameraLog(t, install.studio, "prior session")
	camera.launch = func(context.Context, obspreview.EncoderLaunch) (cameraProcess, error) {
		t.Fatal("launched OBS while mapping already exists")
		return nil, nil
	}
	if err := camera.Start(t.Context()); !errors.Is(err, ErrBusy) {
		t.Fatalf("existing mapping: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(install.studio, "config", "obs-studio", "logs", "current.txt"))
	if err != nil || string(raw) != "prior session" {
		t.Fatalf("unowned profile modified: %q, %v", raw, err)
	}
	if sample, err := sampleCameraQueue(camera.mappingName); err != nil || sample != 0 {
		t.Fatalf("external queue touched: %d %v", sample, err)
	}
	queue.close()
}

func TestCameraLogParserBoundsAndRejectsPartialMarker(t *testing.T) {
	install := syntheticCameraInstallation(t)
	root, err := os.OpenRoot(install.studio)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	writeCameraLog(t, install.studio, "12:00:01.000: Virtual output started later\n")
	if found, err := cameraStartedInLogs(root); err != nil || found {
		t.Fatalf("partial marker: %v %v", found, err)
	}
	writeCameraLog(t, install.studio, "12:00:01.000: Virtual output started\n12:00:02.000: Virtual output stopped\n")
	if found, err := cameraStartedInLogs(root); err == nil || found {
		t.Fatalf("stale start marker survived owned output stop: %v %v", found, err)
	}
	writeCameraLog(t, install.studio, strings.Repeat("x", (2<<20)+1))
	if _, err := cameraStartedInLogs(root); err == nil {
		t.Fatal("unbounded log accepted")
	}
}
