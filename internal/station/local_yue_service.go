package station

// The YuE runner intentionally does not share the image ComfyUI process. YuE
// loads large audio models, so it gets its own owner, loopback port, scratch
// tree and idle policy. Its required YuE2 nodes are built in to the pinned
// ComfyUI release; third-party custom nodes stay disabled. Disabling or
// restarting it can therefore never interrupt image work.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/stationyue"

	"github.com/google/uuid"
)

const (
	localYuERegistrationFile = "station-yue.json"
	localYuEServiceOwner     = "station-yue"
	localYuEDefaultIdle      = 20 * time.Minute
	localYuEMaxManifest      = 256 << 10
)

var errLocalYuENotRunning = errors.New("station_yue_not_running")

// localYuEBackend is deliberately audio-only.  The common client type gives
// the trusted music host the exact upload/queue/audio collection operations it
// needs, without turning this service into a generic Comfy HTTP proxy.
type localYuEBackend interface {
	Info(context.Context, string) (localYuEDeployment, error)
	Use(context.Context, string, string, bool, func(localComfyAudioClient) error) error
	Stop(context.Context) error
	Close(context.Context) error
}

type localYuEDeployment struct {
	Binding      string                     `json:"binding"`
	WorkflowRoot string                     `json:"workflow_root"`
	FFmpegPath   string                     `json:"ffmpeg_path"`
	Pins         map[stationyue.Mode]string `json:"pins"`
	Running      bool                       `json:"running"`
	Busy         bool                       `json:"busy"`
}

// localYuERegistration is written only by the host installer/UI.  It points
// at an already-installed, isolated portable runtime; this code never fetches
// Python, ComfyUI, models, custom nodes or FFmpeg.
type localYuERegistration struct {
	SchemaVersion int               `json:"schema_version"`
	Enabled       bool              `json:"enabled"`
	Runtime       string            `json:"runtime"`
	Python        string            `json:"python"`
	Entry         string            `json:"entry"`
	Frontend      string            `json:"frontend"`
	Manifest      string            `json:"manifest"`
	Models        string            `json:"models"`
	Workflows     string            `json:"workflows"`
	WorkflowPins  map[string]string `json:"workflow_pins"`
	FFmpeg        string            `json:"ffmpeg"`
	FFmpegSHA256  string            `json:"ffmpeg_sha256"`
	Port          int               `json:"port"`
	IdleSeconds   int               `json:"idle_seconds,omitempty"`
	Device        string            `json:"device"`
	CUDADevice    int               `json:"cuda_device,omitempty"`
}

type localYuEManifest = localComfyManifest

type localYuEService struct {
	dataDir      string
	editableRoot string
	managed      *localComfyService
}

func newLocalYuEService(dataDir, workspace string) localYuEBackend {
	return newLocalYuEServiceAt(dataDir, workspace)
}

func newLocalYuEServiceAt(dataDir, editableRoot string) *localYuEService {
	dataDir, editableRoot = filepath.Clean(dataDir), filepath.Clean(editableRoot)
	return &localYuEService{dataDir: dataDir, editableRoot: editableRoot, managed: newLocalComfyServiceAt(dataDir, editableRoot)}
}

func (s *localYuEService) Info(ctx context.Context, fish string) (localYuEDeployment, error) {
	if err := checkLocalComfyFish(fish); err != nil {
		return localYuEDeployment{}, err
	}
	r, m, err := s.loadRegistration(ctx)
	if err != nil {
		return localYuEDeployment{}, err
	}
	status := s.managed.services.Status(localYuEServiceID(fish))
	return localYuEDeployment{Binding: localYuEBinding(r, m), WorkflowRoot: r.Workflows, FFmpegPath: r.FFmpeg, Pins: r.pins(), Running: status.Running, Busy: status.Busy}, nil
}

func (s *localYuEService) Use(ctx context.Context, fish, binding string, start bool, fn func(localComfyAudioClient) error) error {
	if err := checkLocalComfyFish(fish); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("station_yue_operation_required")
	}
	r, m, err := s.loadRegistration(ctx)
	if err != nil {
		return err
	}
	if binding == "" || binding != localYuEBinding(r, m) {
		return errors.New("station_yue_deployment_changed")
	}
	id := localYuEServiceID(fish)
	s.managed.cancelIdle(id)
	if start {
		if err := s.start(ctx, fish, binding, r, m); err != nil {
			return err
		}
	} else if !s.managed.services.Status(id).Running {
		return errLocalYuENotRunning
	}
	release, ok := s.managed.services.Acquire(id)
	if !ok {
		return errLocalYuENotRunning
	}
	defer func() { release(); s.managed.scheduleIdle(id, r.idleTimeout()) }()
	current, currentManifest, err := s.loadRegistration(ctx)
	if err != nil || localYuEBinding(current, currentManifest) != binding {
		return errors.New("station_yue_deployment_changed")
	}
	client, err := s.ownedClient(id, r.Port)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err := client.CheckVersion(ctx, comfyui.ReferenceVersion); err != nil {
		return errors.New("station_yue_owned_process_unhealthy")
	}
	return fn(&localComfyLeasedClient{client: client, journal: s, fish: fish, binding: binding})
}

func (s *localYuEService) Stop(ctx context.Context) error {
	s.managed.cancelAllIdle()
	return s.managed.services.StopOwner(ctx, localYuEServiceOwner)
}
func (s *localYuEService) Close(ctx context.Context) error {
	return s.managed.Close(ctx)
}

func (s *localYuEService) start(ctx context.Context, fish, binding string, r localYuERegistration, m localYuEManifest) error {
	if binding != localYuEBinding(r, m) {
		return errors.New("station_yue_deployment_changed")
	}
	private, err := s.ensurePrivateScope(fish)
	if err != nil {
		return err
	}
	if !s.managed.services.Status(localYuEServiceID(fish)).Running {
		if err := s.managed.resetStaleJournal(private); err != nil {
			return err
		}
	}
	logDir := filepath.Join(s.dataDir, "station-yue-logs")
	if err := os.MkdirAll(logDir, 0700); err != nil || !localComfyServiceSafeDirectory(logDir) {
		return errors.New("station_yue_log_directory_unavailable")
	}
	python, entry, frontend := r.paths()
	check := func(checkCtx context.Context) error {
		current, manifest, err := s.loadRegistration(checkCtx)
		if err != nil || localYuEBinding(current, manifest) != binding {
			return errors.New("station_yue_registration_changed")
		}
		if !localComfySafeRegular(python) || !localComfySafeRegular(entry) || !localComfyServiceSafeDirectory(frontend) {
			return errors.New("station_yue_source_unavailable")
		}
		return nil
	}
	return s.managed.startManaged(ctx, localComfyManagedSpec{
		Owner: localYuEServiceOwner, ID: localYuEServiceID(fish), Binding: binding, Port: r.Port,
		Python: python, Entry: entry, Frontend: frontend, Arguments: localYuEArguments(r, entry, frontend, private),
		Environment: localComfyEnvironment(localComfyRegistration{Runtime: r.Runtime, Python: r.Python, Device: r.Device, CUDADevice: r.CUDADevice}, private),
		LogPath:     filepath.Join(logDir, localComfyFishHash(fish)+".log"), PendingPath: filepath.Join(private, "jobs.json"),
		StopTimeout: 3 * time.Second, Check: check, PortError: "station_yue_port_occupied_no_adoption", StartError: "station_yue_start_failed_check_private_log", Timeout: 90 * time.Second,
	})
}

func (s *localYuEService) ownedClient(id string, port int) (*comfyui.Client, error) {
	return s.managed.ownedClient(id, port)
}

func (s *localYuEService) loadRegistration(ctx context.Context) (localYuERegistration, localYuEManifest, error) {
	if err := ctx.Err(); err != nil {
		return localYuERegistration{}, localYuEManifest{}, err
	}
	data, err := readLocalComputeRegistration(filepath.Join(s.dataDir, localYuERegistrationFile), "station_yue_", localYuEMaxManifest)
	if err != nil {
		return localYuERegistration{}, localYuEManifest{}, err
	}
	var r localYuERegistration
	var fields map[string]json.RawMessage
	if localComfyStrictDecode(data, &r) != nil || json.Unmarshal(data, &fields) != nil || !localYuERegistrationFields(fields) || r.SchemaVersion != 1 {
		return localYuERegistration{}, localYuEManifest{}, errors.New("station_yue_registration_invalid")
	}
	if !r.Enabled {
		return localYuERegistration{}, localYuEManifest{}, errors.New("station_yue_not_installed_or_authorized")
	}
	if err := r.validate(s.dataDir, s.editableRoot); err != nil {
		return localYuERegistration{}, localYuEManifest{}, err
	}
	m, err := localYuELoadManifest(r)
	if err != nil {
		return localYuERegistration{}, localYuEManifest{}, err
	}
	return r, m, nil
}

func localYuERegistrationFields(f map[string]json.RawMessage) bool {
	allowed := map[string]bool{"schema_version": true, "enabled": true, "runtime": true, "python": true, "entry": true, "frontend": true, "manifest": true, "models": true, "workflows": true, "workflow_pins": true, "ffmpeg": true, "ffmpeg_sha256": true, "port": true, "idle_seconds": true, "device": true, "cuda_device": true}
	for k := range f {
		if !allowed[k] {
			return false
		}
	}
	for _, k := range []string{"schema_version", "enabled", "runtime", "python", "entry", "frontend", "manifest", "models", "workflows", "workflow_pins", "ffmpeg", "ffmpeg_sha256", "port", "device"} {
		if f[k] == nil {
			return false
		}
	}
	return true
}

func (r localYuERegistration) paths() (string, string, string) {
	return filepath.Join(r.Runtime, r.Python), filepath.Join(r.Runtime, r.Entry), filepath.Join(r.Runtime, r.Frontend)
}
func (r localYuERegistration) manifestPath() string {
	if filepath.IsAbs(r.Manifest) {
		return r.Manifest
	}
	return filepath.Join(r.Runtime, r.Manifest)
}
func (r localYuERegistration) idleTimeout() time.Duration {
	if r.IdleSeconds == 0 {
		return localYuEDefaultIdle
	}
	return time.Duration(r.IdleSeconds) * time.Second
}
func (r localYuERegistration) pins() map[stationyue.Mode]string {
	return map[stationyue.Mode]string{stationyue.ModeOriginal: r.WorkflowPins[string(stationyue.ModeOriginal)], stationyue.ModeCover: r.WorkflowPins[string(stationyue.ModeCover)]}
}

func (r localYuERegistration) validate(dataDir, editableRoot string) error {
	if r.Port < 1024 || r.Port > 65535 || r.IdleSeconds < 0 || int64(r.IdleSeconds) > localComfyMaxIdleSeconds || !localComfyServiceSafeDirectory(r.Runtime) || !localComfyServiceSafeDirectory(r.Models) || !localComfyServiceSafeDirectory(r.Workflows) || !localComfySafeRegular(r.FFmpeg) || !localComfyHexDigest.MatchString(r.FFmpegSHA256) {
		return errors.New("station_yue_registration_invalid")
	}
	if localComfyInside(dataDir, editableRoot) || localComfyPathsOverlap(r.Runtime, dataDir) || localComfyPathsOverlap(r.Runtime, editableRoot) || localComfyPathsOverlap(r.Models, dataDir) || localComfyPathsOverlap(r.Models, editableRoot) || localComfyPathsOverlap(r.Workflows, dataDir) || localComfyPathsOverlap(r.Workflows, editableRoot) || localComfyPathsOverlap(r.FFmpeg, dataDir) || localComfyPathsOverlap(r.FFmpeg, editableRoot) || localComfyPathsOverlap(r.manifestPath(), editableRoot) {
		return errors.New("station_yue_scope_overlaps_agent_home")
	}
	for _, relative := range []string{r.Python, r.Entry, r.Frontend} {
		if !localComfySafeRelative(relative) {
			return errors.New("station_yue_registration_invalid")
		}
	}
	python, entry, frontend := r.paths()
	manifest := r.manifestPath()
	manifestAllowed := localComfySafeRelative(r.Manifest) || (localComfySafeRegular(r.Manifest) && localComfyInside(r.Manifest, dataDir))
	if !localComfySafeRegular(python) || !localComfySafeRegular(entry) || !localComfyServiceSafeDirectory(frontend) || !manifestAllowed || !localComfySafeRegular(manifest) || filepath.Base(python) != "python.exe" || filepath.Base(entry) != "main.py" || !localYuEFileMatches(r.FFmpeg, r.FFmpegSHA256) {
		return errors.New("station_yue_source_invalid")
	}
	if r.Device != "cpu" && r.Device != "cuda" || r.CUDADevice < 0 || r.CUDADevice > 64 {
		return errors.New("station_yue_device_invalid")
	}
	if len(r.WorkflowPins) != 2 {
		return errors.New("station_yue_workflow_pins_invalid")
	}
	for mode, name := range map[string]string{string(stationyue.ModeOriginal): "audio_yue2_text_to_music.json", string(stationyue.ModeCover): "audio_yue2_music_cover.json"} {
		digest := r.WorkflowPins[mode]
		path := filepath.Join(r.Workflows, name)
		if !localComfyHexDigest.MatchString(digest) || !localComfySafeRegular(path) || !localYuEFileMatches(path, digest) {
			return errors.New("station_yue_workflow_pins_invalid")
		}
	}
	return nil
}

func localYuELoadManifest(r localYuERegistration) (localYuEManifest, error) {
	data, err := localComfyServiceReadRegular(r.manifestPath(), localYuEMaxManifest)
	if err != nil {
		return localYuEManifest{}, errors.New("station_yue_deployment_pin_invalid")
	}
	var m localYuEManifest
	var fields map[string]json.RawMessage
	if localComfyStrictDecode(data, &m) != nil || json.Unmarshal(data, &fields) != nil || m.SchemaVersion != 1 || m.Policy != "station-yue-v1" || len(m.Files) < 2 || len(m.Files) > 256 || !localComfyManifestFields(fields) {
		return localYuEManifest{}, errors.New("station_yue_deployment_pin_invalid")
	}
	required := map[string]bool{filepath.Clean(r.Python): false, filepath.Clean(r.Entry): false}
	seen := map[string]bool{}
	for relative, want := range m.Files {
		if !localComfySafeRelative(relative) || !localComfyHexDigest.MatchString(want) || seen[filepath.Clean(relative)] {
			return localYuEManifest{}, errors.New("station_yue_deployment_pin_invalid")
		}
		seen[filepath.Clean(relative)] = true
		path := filepath.Join(r.Runtime, relative)
		if !localComfySafeRegular(path) || !localYuEFileMatches(path, want) {
			return localYuEManifest{}, errors.New("station_yue_deployment_pin_invalid")
		}
		if _, ok := required[filepath.Clean(relative)]; ok {
			required[filepath.Clean(relative)] = true
		}
	}
	for _, present := range required {
		if !present {
			return localYuEManifest{}, errors.New("station_yue_deployment_pin_invalid")
		}
	}
	return m, nil
}

func localYuEFileMatches(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, 512<<20)); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}

// YuE2 is built-in to the approved 0.37.2 runtime, so external custom nodes
// are never enabled for this service either.
func localYuEArguments(r localYuERegistration, entry, frontend, private string) []string {
	args := []string{"-s", "-B", entry, "--listen", "127.0.0.1", "--port", strconv.Itoa(r.Port), "--disable-auto-launch", "--disable-all-custom-nodes", "--disable-api-nodes", "--disable-metadata", "--base-directory", private, "--user-directory", filepath.Join(private, "user"), "--models-directory", r.Models, "--input-directory", filepath.Join(private, "input"), "--output-directory", filepath.Join(private, "output"), "--temp-directory", filepath.Join(private, "temp"), "--front-end-root", frontend, "--database-url", "sqlite:///:memory:"}
	if r.Device == "cpu" {
		return append(args, "--cpu")
	}
	return append(args, "--cuda-device", strconv.Itoa(r.CUDADevice))
}

func (s *localYuEService) ensurePrivateScope(fish string) (string, error) {
	private := filepath.Join(s.dataDir, "station-yue", localYuEFishHash(fish))
	for _, d := range []string{private, filepath.Join(private, "input"), filepath.Join(private, "output"), filepath.Join(private, "temp"), filepath.Join(private, "user"), filepath.Join(private, "appdata"), filepath.Join(private, "home"), filepath.Join(private, "hf-cache")} {
		if err := os.MkdirAll(d, 0700); err != nil || !localComfyServiceSafeDirectory(d) {
			return "", errors.New("station_yue_private_scope_unavailable")
		}
	}
	return private, nil
}

// recordJob is the YuE registration counterpart to localComfyService.recordJob.
// The serialized format and leased-client transitions are shared; the binding
// check deliberately remains YuE-specific so an audio job can never survive a
// changed workflow, FFmpeg, runtime or model registration.
func (s *localYuEService) recordJob(fish, binding, jobID, state string) error {
	parsed, err := uuid.Parse(jobID)
	if err != nil || parsed == uuid.Nil || parsed.String() != jobID || !localComfyJournalState(state) {
		return errors.New("station_yue_job_state_invalid")
	}
	r, m, err := s.loadRegistration(context.Background())
	if err != nil || localYuEBinding(r, m) != binding {
		return errors.New("station_yue_deployment_changed")
	}
	private, err := s.ensurePrivateScope(fish)
	if err != nil {
		return err
	}
	path := filepath.Join(private, "jobs.json")
	// The shared ManagedServices instance serializes the actual process lease;
	// use its mutex through the embedded lifecycle owner to serialize journal
	// replacement with its own idle callbacks as well.
	s.managed.mu.Lock()
	defer s.managed.mu.Unlock()
	jobs := make(map[string]struct {
		State string `json:"state"`
	})
	if _, statErr := os.Lstat(path); statErr == nil {
		data, readErr := localComfyServiceReadRegular(path, 4<<20)
		if readErr != nil || (len(data) != 0 && json.Unmarshal(data, &jobs) != nil) {
			return errors.New("station_yue_job_journal_invalid")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("station_yue_job_journal_invalid")
	}
	jobs[jobID] = struct {
		State string `json:"state"`
	}{State: state}
	data, err := json.Marshal(jobs)
	if err != nil {
		return errors.New("station_yue_job_journal_invalid")
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0600); err != nil {
		return errors.New("station_yue_job_journal_unavailable")
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return errors.New("station_yue_job_journal_unavailable")
	}
	return nil
}

func localYuEServiceID(fish string) string {
	return localYuEServiceOwner + "." + localYuEFishHash(fish)
}
func localYuEFishHash(fish string) string {
	sum := sha256.Sum256([]byte("station-yue:" + fish))
	return hex.EncodeToString(sum[:])
}
func localYuEBinding(r localYuERegistration, m localYuEManifest) string {
	files := make([]string, 0, len(m.Files))
	for name, digest := range m.Files {
		files = append(files, name+":"+digest)
	}
	sort.Strings(files)
	pins := []string{string(stationyue.ModeOriginal) + ":" + r.WorkflowPins[string(stationyue.ModeOriginal)], string(stationyue.ModeCover) + ":" + r.WorkflowPins[string(stationyue.ModeCover)]}
	value, _ := json.Marshal(struct {
		Schema, Port, CUDA, Idle                                                       int
		Runtime, Python, Entry, Frontend, Models, Workflows, FFmpeg, FFmpegSHA, Device string
		Files, Pins                                                                    []string
	}{r.SchemaVersion, r.Port, r.CUDADevice, r.IdleSeconds, r.Runtime, r.Python, r.Entry, r.Frontend, r.Models, r.Workflows, r.FFmpeg, r.FFmpegSHA256, r.Device, files, pins})
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
