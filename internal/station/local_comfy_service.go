package station

// The station ComfyUI runner is deliberately a typed, host-owned adapter.  A
// chat model can select an approved workflow through the workflow registry,
// but it cannot supply a Python command, graph endpoint, model directory or
// listener.  ComfyUI is always bound to a private IPv4 loopback port and its
// mutable files live under the host data directory, never in the Agent home.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/pluginruntime"
)

const (
	localComfyRegistrationFile = "station-comfyui.json"
	localComfyServiceOwner     = "station-comfyui"
	localComfyDefaultIdle      = 5 * time.Minute
	localComfyMaxManifest      = 128 << 10
	localComfyMaxIdleSeconds   = int64((1<<63 - 1) / int64(time.Second))
)

var (
	errLocalComfyNotRunning = errors.New("station_comfyui_not_running")
	localComfyFishPattern   = regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,160}$`)
	localComfyHexDigest     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// localComfyClient intentionally exposes the small audited client surface,
// not a generic HTTP transport.  This keeps arbitrary API nodes and custom
// endpoints out of the host integration.
type localComfyClient interface {
	Check(context.Context) error
	Submit(context.Context, comfyui.Prepared) error
	Status(context.Context, comfyui.JobRef) (comfyui.JobStatus, error)
	Cancel(context.Context, comfyui.JobRef) (bool, error)
	FetchPNG(context.Context, comfyui.JobRef, int) (comfyui.Image, error)
	// MarkSettled is called only after the host has committed every requested
	// artifact (or durably settled a terminal/cancelled job). It releases the
	// job-aware idle hold; a merely completed upstream job stays warm for
	// collection because ComfyUI's queue/history is volatile.
	MarkSettled(context.Context, comfyui.JobRef) error
}

type localComfyAudioClient interface {
	localComfyClient
	YuERequiredNodeSchemas(context.Context) (json.RawMessage, error)
	UploadWAV(context.Context, comfyui.JobRef, []byte) (string, error)
	AudioStatus(context.Context, comfyui.JobRef) (comfyui.AudioStatus, error)
	FetchFLAC(context.Context, comfyui.JobRef) (comfyui.FLAC, error)
}

type localComfyBackend interface {
	Info(context.Context, string) (localComfyDeployment, error)
	// Use holds an owned service for the complete operation.  start=false is
	// recovery-only: it never launches a new ComfyUI because the upstream queue
	// is volatile and cannot be reconstructed after a process exit.
	Use(context.Context, string, string, bool, func(localComfyClient) error) error
	Stop(context.Context) error
	Close(context.Context) error
}

// localComfyDeployment is safe for a durable host job. Runtime/model paths
// are deliberately not exposed; WorkflowRoot is consumed only by the trusted
// workflow registry in this package.
type localComfyDeployment struct {
	Binding      string `json:"binding"`
	WorkflowRoot string `json:"workflow_root"`
	Running      bool   `json:"running"`
	Busy         bool   `json:"busy"`
}

type localComfyRegistration struct {
	SchemaVersion int    `json:"schema_version"`
	Enabled       bool   `json:"enabled"`
	Runtime       string `json:"runtime"`
	Python        string `json:"python"`
	Entry         string `json:"entry"`
	Frontend      string `json:"frontend"`
	Manifest      string `json:"manifest"`
	Models        string `json:"models"`
	Workflows     string `json:"workflows"`
	Port          int    `json:"port"`
	IdleSeconds   int    `json:"idle_seconds,omitempty"`
	Device        string `json:"device"`
	CUDADevice    int    `json:"cuda_device,omitempty"`
}

type localComfyManifest struct {
	SchemaVersion int               `json:"schema_version"`
	Policy        string            `json:"policy"`
	Files         map[string]string `json:"files"`
}

type localComfyService struct {
	dataDir      string
	editableRoot string
	services     pluginruntime.ManagedServices

	mu     sync.Mutex
	closed bool
	timers map[string]*time.Timer
	gens   map[string]uint64
}

// localComfyManagedSpec is the shared, host-owned process lifecycle used by
// the image and YuE deployments.  A capability-specific wrapper is allowed to
// validate its own immutable registration, but it must not duplicate listener
// admission, owner verification, idle management or process shutdown.
type localComfyManagedSpec struct {
	Owner       string
	ID          string
	Binding     string
	Port        int
	Python      string
	Entry       string
	Frontend    string
	Arguments   []string
	Environment []string
	LogPath     string
	PendingPath string
	StopTimeout time.Duration
	Check       func(context.Context) error
	PortError   string
	StartError  string
	Timeout     time.Duration
}

func newLocalComfyService(dataDir, workspace string) localComfyBackend {
	return newLocalComfyServiceAt(dataDir, workspace)
}

// newLocalComfyServiceAt keeps tests independent of the packed executable.
// All runnable paths are owner-configured and pinned in station-comfyui.json.
func newLocalComfyServiceAt(dataDir, editableRoot string) *localComfyService {
	return &localComfyService{
		dataDir:      filepath.Clean(dataDir),
		editableRoot: filepath.Clean(editableRoot),
		timers:       make(map[string]*time.Timer),
		gens:         make(map[string]uint64),
	}
}

func (s *localComfyService) Info(ctx context.Context, fish string) (localComfyDeployment, error) {
	if err := checkLocalComfyFish(fish); err != nil {
		return localComfyDeployment{}, err
	}
	registration, manifest, err := s.loadRegistration(ctx)
	if err != nil {
		return localComfyDeployment{}, err
	}
	status := s.services.Status(localComfyServiceID(fish))
	return localComfyDeployment{
		Binding:      localComfyBinding(registration, manifest),
		WorkflowRoot: registration.Workflows,
		Running:      status.Running,
		Busy:         status.Busy,
	}, nil
}

func (s *localComfyService) Use(ctx context.Context, fish, binding string, start bool, fn func(localComfyClient) error) error {
	if err := checkLocalComfyFish(fish); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("station_comfyui_operation_required")
	}
	registration, manifest, err := s.loadRegistration(ctx)
	if err != nil {
		return err
	}
	if binding == "" || binding != localComfyBinding(registration, manifest) {
		return errors.New("station_comfyui_deployment_changed")
	}
	id := localComfyServiceID(fish)
	s.cancelIdle(id)
	if start {
		if err := s.start(ctx, fish, binding, registration, manifest); err != nil {
			return err
		}
	} else if !s.services.Status(id).Running {
		return errLocalComfyNotRunning
	}
	release, ok := s.services.Acquire(id)
	if !ok {
		return errLocalComfyNotRunning
	}
	defer func() {
		release()
		s.scheduleIdle(id, registration.idleTimeout())
	}()

	// This final registration check prevents a job pinned to an old deployment
	// from inheriting a newly edited source/model registration while startup was
	// in progress.
	current, currentManifest, err := s.loadRegistration(ctx)
	if err != nil || localComfyBinding(current, currentManifest) != binding {
		return errors.New("station_comfyui_deployment_changed")
	}
	client, err := s.ownedClient(id, registration.Port)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err := client.CheckVersion(ctx, comfyui.ReferenceVersion); err != nil {
		return errors.New("station_comfyui_owned_process_unhealthy")
	}
	return fn(&localComfyLeasedClient{client: client, journal: s, fish: fish, binding: binding})
}

func (s *localComfyService) Stop(ctx context.Context) error {
	s.cancelAllIdle()
	return s.services.StopOwner(ctx, localComfyServiceOwner)
}

func (s *localComfyService) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cancelAllIdle()
	return s.services.Close(ctx)
}

func (s *localComfyService) start(ctx context.Context, fish, expectedBinding string, registration localComfyRegistration, manifest localComfyManifest) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("station_comfyui_service_closed")
	}
	if expectedBinding != localComfyBinding(registration, manifest) {
		return errors.New("station_comfyui_deployment_changed")
	}
	private, err := s.ensurePrivateScope(fish)
	if err != nil {
		return err
	}
	if !s.services.Status(localComfyServiceID(fish)).Running {
		if err := s.resetStaleJournal(private); err != nil {
			return err
		}
	}
	logDir := filepath.Join(s.dataDir, "station-comfyui-logs")
	if err := os.MkdirAll(logDir, 0700); err != nil || !localComfyServiceSafeDirectory(logDir) {
		return errors.New("station_comfyui_log_directory_unavailable")
	}
	python, entry, frontend := registration.paths()
	arguments := localComfyArguments(registration, entry, frontend, private)
	check := func(checkCtx context.Context) error {
		current, currentManifest, err := s.loadRegistration(checkCtx)
		if err != nil || localComfyBinding(current, currentManifest) != expectedBinding {
			return errors.New("station_comfyui_registration_changed")
		}
		if !localComfySafeRegular(python) || !localComfySafeRegular(entry) || !localComfyServiceSafeDirectory(frontend) {
			return errors.New("station_comfyui_source_unavailable")
		}
		return nil
	}
	return s.startManaged(ctx, localComfyManagedSpec{
		Owner: localComfyServiceOwner, ID: localComfyServiceID(fish), Binding: expectedBinding, Port: registration.Port,
		Python: python, Entry: entry, Frontend: frontend, Arguments: arguments, Environment: localComfyEnvironment(registration, private),
		LogPath: filepath.Join(logDir, localComfyFishHash(fish)+".log"), PendingPath: filepath.Join(private, "jobs.json"),
		StopTimeout: 2 * time.Second, Check: check, PortError: "station_comfyui_port_occupied_no_adoption", StartError: "station_comfyui_start_failed_check_private_log", Timeout: 45 * time.Second,
	})
}

func (s *localComfyService) startManaged(ctx context.Context, managed localComfyManagedSpec) error {
	if managed.Owner == "" || managed.ID == "" || managed.Binding == "" || managed.Port < 1024 || managed.Port > 65535 || !localComfySafeRegular(managed.Python) || !localComfySafeRegular(managed.Entry) || !localComfyServiceSafeDirectory(managed.Frontend) || managed.Check == nil {
		return errors.New("station_comfyui_managed_spec_invalid")
	}
	if managed.PortError == "" {
		managed.PortError = "station_comfyui_port_occupied_no_adoption"
	}
	if managed.StartError == "" {
		managed.StartError = "station_comfyui_start_failed_check_private_log"
	}
	if managed.StopTimeout <= 0 {
		managed.StopTimeout = 2 * time.Second
	}
	if managed.Timeout <= 0 {
		managed.Timeout = 45 * time.Second
	}
	launched := false
	_, err := s.services.Start(ctx, managed.ID, pluginruntime.ManagedServiceSpec{
		OwnerPlugin: managed.Owner, Binding: managed.Binding, Executable: managed.Python, Arguments: managed.Arguments,
		Directory: filepath.Dir(managed.Entry), Environment: managed.Environment, LogPath: managed.LogPath, PendingJobsPath: managed.PendingPath,
		StopTimeout: managed.StopTimeout, Check: managed.Check,
	}, func(admitCtx context.Context, launch func() error) error {
		listener, listenErr := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(managed.Port))
		if listenErr != nil {
			return errors.New(managed.PortError)
		}
		_ = listener.Close()
		if err := managed.Check(admitCtx); err != nil {
			return err
		}
		if err := launch(); err != nil {
			return errors.New(managed.StartError)
		}
		launched = true
		return nil
	})
	if err != nil {
		return err
	}
	if launched {
		if err := s.waitHealthyFor(ctx, managed.ID, managed.Port, managed.Timeout); err != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
			_ = s.services.Stop(stopCtx, managed.ID)
			cancel()
			return err
		}
	}
	return nil
}

func (s *localComfyService) waitHealthy(ctx context.Context, id string, port int) error {
	return s.waitHealthyFor(ctx, id, port, 45*time.Second)
}

func (s *localComfyService) waitHealthyFor(ctx context.Context, id string, port int, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		client, err := s.ownedClient(id, port)
		if err == nil {
			err = client.CheckVersion(requestCtx, comfyui.ReferenceVersion)
			client.CloseIdleConnections()
		}
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("station_comfyui_start_timeout")
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (s *localComfyService) ownedClient(id string, port int) (*comfyui.Client, error) {
	pid, ok := s.services.ProcessID(id)
	if !ok {
		return nil, errLocalComfyNotRunning
	}
	verify := func() error {
		current, live := s.services.ProcessID(id)
		if !live || current != pid {
			return errLocalComfyNotRunning
		}
		return localComfyListenerOwner(port, pid)
	}
	if err := verify(); err != nil {
		return nil, err
	}
	return comfyui.NewOwnedClient(uint16(port), verify)
}

func (s *localComfyService) loadRegistration(ctx context.Context) (localComfyRegistration, localComfyManifest, error) {
	if err := ctx.Err(); err != nil {
		return localComfyRegistration{}, localComfyManifest{}, err
	}
	data, err := readLocalComputeRegistration(filepath.Join(s.dataDir, localComfyRegistrationFile), "station_comfyui_", localComfyMaxManifest)
	if err != nil {
		return localComfyRegistration{}, localComfyManifest{}, err
	}
	var registration localComfyRegistration
	var fields map[string]json.RawMessage
	if localComfyStrictDecode(data, &registration) != nil || json.Unmarshal(data, &fields) != nil || !localComfyRegistrationFields(fields) || registration.SchemaVersion != 1 {
		return localComfyRegistration{}, localComfyManifest{}, errors.New("station_comfyui_registration_invalid")
	}
	if !registration.Enabled {
		return localComfyRegistration{}, localComfyManifest{}, errors.New("station_comfyui_not_installed_or_authorized")
	}
	if err := registration.validate(s.dataDir, s.editableRoot); err != nil {
		return localComfyRegistration{}, localComfyManifest{}, err
	}
	manifest, err := localComfyLoadManifest(registration)
	if err != nil {
		return localComfyRegistration{}, localComfyManifest{}, err
	}
	return registration, manifest, nil
}

func localComfyRegistrationFields(fields map[string]json.RawMessage) bool {
	allowed := map[string]bool{"schema_version": true, "enabled": true, "runtime": true, "python": true, "entry": true, "frontend": true, "manifest": true, "models": true, "workflows": true, "port": true, "idle_seconds": true, "device": true, "cuda_device": true}
	for name := range fields {
		if !allowed[name] {
			return false
		}
	}
	for _, name := range []string{"schema_version", "enabled", "runtime", "python", "entry", "frontend", "manifest", "models", "workflows", "port", "device"} {
		if fields[name] == nil {
			return false
		}
	}
	return true
}

func (r localComfyRegistration) validate(dataDir, editableRoot string) error {
	if r.Port < 1024 || r.Port > 65535 || r.IdleSeconds < 0 || int64(r.IdleSeconds) > localComfyMaxIdleSeconds || !localComfyServiceSafeDirectory(r.Runtime) || !localComfyServiceSafeDirectory(r.Models) || !localComfyServiceSafeDirectory(r.Workflows) {
		return errors.New("station_comfyui_registration_invalid")
	}
	if localComfyInside(dataDir, editableRoot) || localComfyPathsOverlap(r.Runtime, dataDir) || localComfyPathsOverlap(r.Runtime, editableRoot) || localComfyPathsOverlap(r.Models, dataDir) || localComfyPathsOverlap(r.Models, editableRoot) || localComfyPathsOverlap(r.Workflows, dataDir) || localComfyPathsOverlap(r.Workflows, editableRoot) || localComfyPathsOverlap(r.manifestPath(), editableRoot) {
		return errors.New("station_comfyui_scope_overlaps_agent_home")
	}
	for _, relative := range []string{r.Python, r.Entry, r.Frontend} {
		if !localComfySafeRelative(relative) {
			return errors.New("station_comfyui_registration_invalid")
		}
	}
	python, entry, frontend := r.paths()
	manifestPath := r.manifestPath()
	manifestAllowed := localComfySafeRelative(r.Manifest) || (localComfySafeRegular(r.Manifest) && localComfyInside(r.Manifest, dataDir))
	if !localComfySafeRegular(python) || !localComfySafeRegular(entry) || !localComfyServiceSafeDirectory(frontend) || !manifestAllowed || !localComfySafeRegular(manifestPath) || filepath.Base(python) != "python.exe" || filepath.Base(entry) != "main.py" {
		return errors.New("station_comfyui_source_invalid")
	}
	if r.Device != "cpu" && r.Device != "cuda" || r.CUDADevice < 0 || r.CUDADevice > 64 {
		return errors.New("station_comfyui_device_invalid")
	}
	return nil
}

func (r localComfyRegistration) paths() (python, entry, frontend string) {
	return filepath.Join(r.Runtime, r.Python), filepath.Join(r.Runtime, r.Entry), filepath.Join(r.Runtime, r.Frontend)
}

// manifestPath accepts an owner-pinned manifest either next to an immutable
// portable runtime (legacy/offline package layout) or in private host data.
// The latter lets verification avoid writing anything into an installed tool.
func (r localComfyRegistration) manifestPath() string {
	if filepath.IsAbs(r.Manifest) {
		return r.Manifest
	}
	return filepath.Join(r.Runtime, r.Manifest)
}

func (r localComfyRegistration) idleTimeout() time.Duration {
	if r.IdleSeconds == 0 {
		return localComfyDefaultIdle
	}
	return time.Duration(r.IdleSeconds) * time.Second
}

func localComfyLoadManifest(registration localComfyRegistration) (localComfyManifest, error) {
	data, err := localComfyServiceReadRegular(registration.manifestPath(), localComfyMaxManifest)
	if err != nil {
		return localComfyManifest{}, errors.New("station_comfyui_deployment_pin_invalid")
	}
	var manifest localComfyManifest
	var fields map[string]json.RawMessage
	if localComfyStrictDecode(data, &manifest) != nil || json.Unmarshal(data, &fields) != nil || manifest.SchemaVersion != 1 || manifest.Policy != "station-comfyui-v1" || len(manifest.Files) < 2 || len(manifest.Files) > 128 || !localComfyManifestFields(fields) {
		return localComfyManifest{}, errors.New("station_comfyui_deployment_pin_invalid")
	}
	required := map[string]bool{filepath.Clean(registration.Python): false, filepath.Clean(registration.Entry): false}
	seen := make(map[string]bool, len(manifest.Files))
	for relative, want := range manifest.Files {
		if !localComfySafeRelative(relative) || !localComfyHexDigest.MatchString(want) {
			return localComfyManifest{}, errors.New("station_comfyui_deployment_pin_invalid")
		}
		key := filepath.Clean(relative)
		if seen[key] {
			return localComfyManifest{}, errors.New("station_comfyui_deployment_pin_invalid")
		}
		seen[key] = true
		path := filepath.Join(registration.Runtime, key)
		if !localComfySafeRegular(path) || !localComfyFileMatches(path, want) {
			return localComfyManifest{}, errors.New("station_comfyui_deployment_pin_invalid")
		}
		if _, requiredFile := required[key]; requiredFile {
			required[key] = true
		}
	}
	for _, present := range required {
		if !present {
			return localComfyManifest{}, errors.New("station_comfyui_deployment_pin_invalid")
		}
	}
	return manifest, nil
}

func localComfyManifestFields(fields map[string]json.RawMessage) bool {
	if len(fields) != 3 || fields["schema_version"] == nil || fields["policy"] == nil || fields["files"] == nil {
		return false
	}
	for name := range fields {
		if name != "schema_version" && name != "policy" && name != "files" {
			return false
		}
	}
	return true
}

func localComfyFileMatches(path, want string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	hash := sha256.New()
	const limit = 64 << 20
	if n, err := io.Copy(hash, io.LimitReader(file, limit+1)); err != nil || n > limit {
		return false
	}
	return hex.EncodeToString(hash.Sum(nil)) == want
}

func localComfyArguments(r localComfyRegistration, entry, frontend, private string) []string {
	args := []string{"-s", "-B", entry, "--listen", "127.0.0.1", "--port", strconv.Itoa(r.Port), "--disable-auto-launch", "--disable-all-custom-nodes", "--disable-api-nodes", "--disable-metadata", "--base-directory", private, "--user-directory", filepath.Join(private, "user"), "--models-directory", r.Models, "--input-directory", filepath.Join(private, "input"), "--output-directory", filepath.Join(private, "output"), "--temp-directory", filepath.Join(private, "temp"), "--front-end-root", frontend, "--database-url", "sqlite:///:memory:"}
	if r.Device == "cpu" {
		return append(args, "--cpu")
	}
	return append(args, "--cuda-device", strconv.Itoa(r.CUDADevice))
}

func localComfyEnvironment(r localComfyRegistration, private string) []string {
	systemRoot := os.Getenv("SystemRoot")
	path := filepath.Dir(filepath.Join(r.Runtime, r.Python)) + string(os.PathListSeparator) + filepath.Join(systemRoot, "System32")
	env := []string{"SystemRoot=" + systemRoot, "WINDIR=" + os.Getenv("WINDIR"), "SystemDrive=" + os.Getenv("SystemDrive"), "PATH=" + path, "TEMP=" + filepath.Join(private, "temp"), "TMP=" + filepath.Join(private, "temp"), "APPDATA=" + filepath.Join(private, "appdata"), "LOCALAPPDATA=" + filepath.Join(private, "appdata"), "USERPROFILE=" + filepath.Join(private, "home"), "USERNAME=station-comfyui", "HF_HOME=" + filepath.Join(private, "hf-cache"), "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "PYTHONDONTWRITEBYTECODE=1", "PYTHONUTF8=1"}
	if r.Device == "cuda" {
		env = append(env, "CUDA_VISIBLE_DEVICES="+strconv.Itoa(r.CUDADevice))
	}
	return env
}

func (s *localComfyService) ensurePrivateScope(fish string) (string, error) {
	private := filepath.Join(s.dataDir, "station-comfyui", localComfyFishHash(fish))
	for _, directory := range []string{private, filepath.Join(private, "input"), filepath.Join(private, "output"), filepath.Join(private, "temp"), filepath.Join(private, "user"), filepath.Join(private, "appdata"), filepath.Join(private, "home"), filepath.Join(private, "hf-cache")} {
		if err := os.MkdirAll(directory, 0700); err != nil || !localComfyServiceSafeDirectory(directory) {
			return "", errors.New("station_comfyui_private_scope_unavailable")
		}
	}
	return private, nil
}

// The upstream Comfy queue disappears with its process. Durable plugin jobs
// stay in the host store and are marked needs_review there; this short-lived
// supervisor journal is discarded before a new process generation so a lost
// old job cannot pin a fresh runtime forever.
func (s *localComfyService) resetStaleJournal(private string) error {
	path := filepath.Join(private, "jobs.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil || !localComfySafeRegular(path) {
		return errors.New("station_comfyui_job_journal_invalid")
	}
	if err := os.Remove(path); err != nil {
		return errors.New("station_comfyui_job_journal_unavailable")
	}
	return nil
}

func (s *localComfyService) scheduleIdle(id string, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	s.mu.Lock()
	if timer := s.timers[id]; timer != nil {
		timer.Stop()
	}
	s.gens[id]++
	generation := s.gens[id]
	s.timers[id] = time.AfterFunc(timeout, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
		defer cancel()
		_, _ = s.services.StopIfIdleGuarded(ctx, id, timeout, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.gens[id] == generation
		})
		s.mu.Lock()
		if s.gens[id] == generation {
			delete(s.timers, id)
		}
		s.mu.Unlock()
	})
	s.mu.Unlock()
}

func (s *localComfyService) cancelIdle(id string) {
	s.mu.Lock()
	s.gens[id]++
	if timer := s.timers[id]; timer != nil {
		timer.Stop()
		delete(s.timers, id)
	}
	s.mu.Unlock()
}

func (s *localComfyService) cancelAllIdle() {
	s.mu.Lock()
	for id, timer := range s.timers {
		timer.Stop()
		delete(s.timers, id)
		s.gens[id]++
	}
	s.mu.Unlock()
}

// localComfyLeasedClient maintains the pending-job journal used by
// ManagedServices. Submission and uncertain outcomes keep the owned process
// alive; only an explicit terminal status permits idle reclamation.
type localComfyLeasedClient struct {
	client  *comfyui.Client
	journal localComfyJobJournal
	fish    string
	binding string
}

// Both controlled ComfyUI deployments use the same submission/settlement
// lease. The registration verifier differs, but a submitted or uncertain job
// must keep either process warm until the host has durably settled it.
type localComfyJobJournal interface {
	recordJob(string, string, string, string) error
}

func (c *localComfyLeasedClient) Check(ctx context.Context) error { return c.client.Check(ctx) }

func (c *localComfyLeasedClient) Submit(ctx context.Context, prepared comfyui.Prepared) error {
	ref := prepared.Ref()
	if err := c.journal.recordJob(c.fish, c.binding, ref.ID, "submitted"); err != nil {
		return err
	}
	err := c.client.Submit(ctx, prepared)
	if errors.Is(err, comfyui.ErrRejected) {
		_ = c.journal.recordJob(c.fish, c.binding, ref.ID, "failed")
	} else if err != nil {
		_ = c.journal.recordJob(c.fish, c.binding, ref.ID, "unknown")
	}
	return err
}

func (c *localComfyLeasedClient) Status(ctx context.Context, ref comfyui.JobRef) (comfyui.JobStatus, error) {
	status, err := c.client.Status(ctx, ref)
	if err == nil {
		c.observe(ref, status.State)
	}
	return status, err
}

func (c *localComfyLeasedClient) observe(ref comfyui.JobRef, remote comfyui.State) {
	state := string(remote)
	if remote == comfyui.Completed {
		state = "ready"
	}
	_ = c.journal.recordJob(c.fish, c.binding, ref.ID, state)
}

func (c *localComfyLeasedClient) YuERequiredNodeSchemas(ctx context.Context) (json.RawMessage, error) {
	return c.client.YuERequiredNodeSchemas(ctx)
}

func (c *localComfyLeasedClient) UploadWAV(ctx context.Context, ref comfyui.JobRef, wav []byte) (string, error) {
	return c.client.UploadWAV(ctx, ref, wav)
}

func (c *localComfyLeasedClient) AudioStatus(ctx context.Context, ref comfyui.JobRef) (comfyui.AudioStatus, error) {
	status, err := c.client.AudioStatus(ctx, ref)
	if err == nil {
		c.observe(ref, status.State)
	}
	return status, err
}

func (c *localComfyLeasedClient) FetchFLAC(ctx context.Context, ref comfyui.JobRef) (comfyui.FLAC, error) {
	return c.client.FetchFLAC(ctx, ref)
}

func (c *localComfyLeasedClient) Cancel(ctx context.Context, ref comfyui.JobRef) (bool, error) {
	if err := c.journal.recordJob(c.fish, c.binding, ref.ID, "cancelling"); err != nil {
		return false, err
	}
	cancelled, err := c.client.Cancel(ctx, ref)
	if err != nil {
		_ = c.journal.recordJob(c.fish, c.binding, ref.ID, "unknown")
	}
	return cancelled, err
}

func (c *localComfyLeasedClient) FetchPNG(ctx context.Context, ref comfyui.JobRef, index int) (comfyui.Image, error) {
	return c.client.FetchPNG(ctx, ref, index)
}

func (c *localComfyLeasedClient) MarkSettled(_ context.Context, ref comfyui.JobRef) error {
	return c.journal.recordJob(c.fish, c.binding, ref.ID, "delivered")
}

func (s *localComfyService) recordJob(fish, binding, jobID, state string) error {
	parsed, err := uuid.Parse(jobID)
	if err != nil || parsed == uuid.Nil || parsed.String() != jobID || !localComfyJournalState(state) {
		return errors.New("station_comfyui_job_state_invalid")
	}
	registration, manifest, err := s.loadRegistration(context.Background())
	if err != nil || localComfyBinding(registration, manifest) != binding {
		return errors.New("station_comfyui_deployment_changed")
	}
	private, err := s.ensurePrivateScope(fish)
	if err != nil {
		return err
	}
	path := filepath.Join(private, "jobs.json")
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make(map[string]struct {
		State string `json:"state"`
	})
	if _, statErr := os.Lstat(path); statErr == nil {
		data, readErr := localComfyServiceReadRegular(path, 4<<20)
		if readErr != nil || (len(data) != 0 && json.Unmarshal(data, &jobs) != nil) {
			return errors.New("station_comfyui_job_journal_invalid")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("station_comfyui_job_journal_invalid")
	}
	jobs[jobID] = struct {
		State string `json:"state"`
	}{State: state}
	data, err := json.Marshal(jobs)
	if err != nil {
		return errors.New("station_comfyui_job_journal_invalid")
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0600); err != nil {
		return errors.New("station_comfyui_job_journal_unavailable")
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return errors.New("station_comfyui_job_journal_unavailable")
	}
	return nil
}

func localComfyJournalState(state string) bool {
	switch comfyui.State(state) {
	case comfyui.Unknown, comfyui.Pending, comfyui.Running, comfyui.Completed, comfyui.Failed, comfyui.Cancelled:
		return true
	}
	return state == "submitted" || state == "cancelling" || state == "ready" || state == "delivered"
}

func checkLocalComfyFish(fish string) error {
	if !localComfyFishPattern.MatchString(fish) {
		return errors.New("station_comfyui_fish_invalid")
	}
	return nil
}

func localComfyServiceID(fish string) string {
	return localComfyServiceOwner + "." + localComfyFishHash(fish)
}
func localComfyFishHash(fish string) string {
	sum := sha256.Sum256([]byte("station-comfyui:" + fish))
	return hex.EncodeToString(sum[:])
}

func localComfyBinding(registration localComfyRegistration, manifest localComfyManifest) string {
	files := make([]string, 0, len(manifest.Files))
	for name, digest := range manifest.Files {
		files = append(files, name+":"+digest)
	}
	sort.Strings(files)
	value, _ := json.Marshal(struct {
		Schema, Port, CUDADevice                                    int
		Runtime, Python, Entry, Frontend, Models, Workflows, Device string
		Files                                                       []string
	}{registration.SchemaVersion, registration.Port, registration.CUDADevice, registration.Runtime, registration.Python, registration.Entry, registration.Frontend, registration.Models, registration.Workflows, registration.Device, files})
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func localComfyPathsOverlap(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return localComfyInside(left, right) || localComfyInside(right, left)
}

func localComfyInside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func localComfySafeRelative(path string) bool {
	path = filepath.FromSlash(path)
	return path != "" && !strings.ContainsAny(path, ":\x00") && filepath.VolumeName(path) == "" && !filepath.IsAbs(path) && filepath.Clean(path) == path && path != "." && path != ".." && !strings.HasPrefix(path, ".."+string(os.PathSeparator))
}

func localComfyServiceSafeDirectory(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir() && localComfyServiceLinkFree(path)
}

func localComfySafeRegular(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && localComfyServiceLinkFree(path)
}

func localComfyServiceReadRegular(path string, limit int64) ([]byte, error) {
	if limit <= 0 || !localComfySafeRegular(path) {
		return nil, errors.New("station_comfyui_regular_file_required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("station_comfyui_file_limit")
	}
	return data, nil
}

func localComfyServiceLinkFree(path string) bool {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return true
		}
	}
}
