package station

// The station RVC bridge is a host-owned Windows service.  This file is
// deliberately separate from the generic project-tool controller: an Agent
// can request a conversion, but it never supplies a command, a listener, a
// model path, a profile path, or a token.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginruntime"
)

const (
	localRVCRegistrationFile = "station-rvc.json"
	localRVCTokenFile        = "station-rvc-token"
	localRVCServiceOwner     = "station-rvc"
	localRVCHeaderFish       = "X-Agent-Station-Fish"
	localRVCMaxJSON          = 36 << 20
	localRVCMaxAudio         = 128 << 20
	localRVCDefaultIdle      = 5 * time.Minute
)

// localRVCBackend is intentionally narrower than a general HTTP client.  It
// is the only boundary through which the host can reach the local RVC bridge.
// In particular, path is a checked station API path rather than a URL.
type localRVCBackend interface {
	Info(context.Context, string) (localRVCDeployment, error)
	Call(context.Context, string, string, string, string, string, []byte) ([]byte, int, error)
	CallExisting(context.Context, string, string, string, string, string, []byte) ([]byte, int, error)
	Stop(context.Context) error
	Close(context.Context) error
}

// localRVCDeployment is safe to attach to a durable job.  It deliberately
// excludes local paths, ports, credentials and process identifiers.
type localRVCDeployment struct {
	Binding  string          `json:"binding"`
	Profiles []localRVCVoice `json:"profiles"`
	Running  bool            `json:"running"`
	Busy     bool            `json:"busy"`
}

// localRVCVoice is a closed, owner-approved entry in the station registration.
// It is not a file path and cannot select a profile which is not registered.
type localRVCVoice struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	LegacyVoiceID string `json:"legacy_voice_id,omitempty"`
}

type localRVCRegistration struct {
	SchemaVersion int             `json:"schema_version"`
	Enabled       bool            `json:"enabled"`
	Workspace     string          `json:"workspace"`
	Media         string          `json:"media"`
	Port          int             `json:"port"`
	IdleSeconds   int             `json:"idle_seconds,omitempty"`
	Profiles      []localRVCVoice `json:"profiles"`
	FishID        string          `json:"fish_id,omitempty"`
}

type localRVCService struct {
	dataDir       string
	editableRoot  string
	executableDir string
	services      pluginruntime.ManagedServices
	client        *http.Client

	mu     sync.Mutex
	timers map[string]*time.Timer
	gens   map[string]uint64
	ready  map[string]localRVCReady
	closed bool
}

// localRVCReady exists only after this registry's own start closure observes
// the bridge's one-time readiness proof.  A lingering ready file is never
// treated as evidence for a later process.
type localRVCReady struct {
	Binding string
	Port    int
	Scope   string
}

var (
	localRVCFishPattern    = regexp.MustCompile(`^fish-[0-9a-f]{48}$`)
	localRVCVoicePattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	localRVCJobPattern     = regexp.MustCompile(`^[a-f0-9]{32}$`)
	localRVCRequestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// newLocalRVCService takes the Agent-editable home root, not the RVC
// workspace.  The latter is selected only by the private registration and is
// rejected if it overlaps this home root.  This avoids accidentally granting
// a conversion process the Agent's editable files as its service mount.
func newLocalRVCService(dataDir, workspace string) localRVCBackend {
	exe, _ := os.Executable()
	return newLocalRVCServiceAt(dataDir, workspace, filepath.Dir(exe))
}

// newLocalRVCServiceAt exists for isolated tests.  Production callers use the
// packaged executable directory obtained by newLocalRVCService.
func newLocalRVCServiceAt(dataDir, editableRoot, executableDir string) *localRVCService {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &localRVCService{
		dataDir:       filepath.Clean(dataDir),
		editableRoot:  filepath.Clean(editableRoot),
		executableDir: filepath.Clean(executableDir),
		client: &http.Client{Timeout: 45 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("station_rvc_redirect_rejected")
		}},
		timers: make(map[string]*time.Timer),
		gens:   make(map[string]uint64),
		ready:  make(map[string]localRVCReady),
	}
}

func (s *localRVCService) Info(ctx context.Context, fish string) (localRVCDeployment, error) {
	if err := checkLocalRVCFish(fish); err != nil {
		return localRVCDeployment{}, err
	}
	registration, manifest, err := s.loadRegistration(ctx)
	if err != nil {
		return localRVCDeployment{}, err
	}
	if registration.FishID != "" && registration.FishID != fish {
		return localRVCDeployment{}, errors.New("station_rvc_profile_registration_invalid")
	}
	binding := localRVCBinding(registration, manifest)
	profiles := append([]localRVCVoice(nil), registration.Profiles...)
	status := s.services.Status(localRVCServiceID(fish))
	return localRVCDeployment{Binding: binding, Profiles: profiles, Running: status.Running, Busy: status.Busy}, nil
}

var errLocalRVCNotRunning = errors.New("原 RVC 服务当前未运行")

func (s *localRVCService) Call(ctx context.Context, fish, binding, method, path, key string, body []byte) ([]byte, int, error) {
	return s.call(ctx, fish, binding, method, path, key, body, true)
}

func (s *localRVCService) CallExisting(ctx context.Context, fish, binding, method, path, key string, body []byte) ([]byte, int, error) {
	if method != http.MethodGet {
		return nil, 0, errors.New("后台 RVC 核对只允许读取原任务")
	}
	return s.call(ctx, fish, binding, method, path, key, body, false)
}

func (s *localRVCService) call(ctx context.Context, fish, binding, method, path, key string, body []byte, startIfNeeded bool) ([]byte, int, error) {
	if err := checkLocalRVCFish(fish); err != nil {
		return nil, 0, err
	}
	if err := validateLocalRVCRequest(method, path, key, body); err != nil {
		return nil, 0, err
	}
	registration, manifest, err := s.loadRegistration(ctx)
	if err != nil {
		return nil, 0, err
	}
	if registration.FishID != "" && registration.FishID != fish {
		return nil, 0, errors.New("station_rvc_profile_registration_invalid")
	}
	if binding == "" || binding != localRVCBinding(registration, manifest) {
		return nil, 0, errors.New("station_rvc_deployment_changed")
	}
	if err := localRVCRequestedProfileAllowed(registration.Profiles, method, path, body); err != nil {
		return nil, 0, err
	}
	id := localRVCServiceID(fish)
	if startIfNeeded {
		s.cancelIdle(id)
		if err := s.start(ctx, fish, binding, registration, manifest); err != nil {
			return nil, 0, err
		}
	}
	release, ok := s.services.Acquire(id)
	if !ok {
		return nil, 0, errLocalRVCNotRunning
	}
	s.cancelIdle(id)
	defer func() {
		release()
		s.scheduleIdle(id, registration.idleTimeout())
	}()

	token, err := s.readExistingToken()
	if err != nil {
		return nil, 0, err
	}
	requestURL := "http://127.0.0.1:" + strconv.Itoa(registration.Port) + path
	// A task is pinned to the deployment returned by Info.  Recheck at the
	// final boundary too: configuration can change while a local supervisor is
	// warming up, and a new profile registration must never inherit its job.
	current, currentManifest, err := s.loadRegistration(ctx)
	if err != nil || localRVCBinding(current, currentManifest) != binding {
		return nil, 0, errors.New("station_rvc_deployment_changed")
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("station_rvc_request_invalid")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(localRVCHeaderFish, localRVCFishHash(fish))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := s.client.Do(req)
	if err != nil {
		return nil, 0, errors.New("station_rvc_transport_failed")
	}
	defer response.Body.Close()
	if response.Header.Get(localRVCHeaderFish) != localRVCFishHash(fish) {
		return nil, response.StatusCode, errors.New("station_rvc_scope_response_invalid")
	}
	limit := int64(localRVCMaxJSON)
	if strings.HasSuffix(path, "/audio") {
		limit = localRVCMaxAudio
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(payload)) > limit {
		return nil, response.StatusCode, errors.New("station_rvc_response_limit")
	}
	if response.StatusCode < 100 || response.StatusCode > 599 {
		return nil, response.StatusCode, errors.New("station_rvc_status_invalid")
	}
	return payload, response.StatusCode, nil
}

func (s *localRVCService) Stop(ctx context.Context) error {
	s.mu.Lock()
	for id, timer := range s.timers {
		timer.Stop()
		delete(s.timers, id)
		s.gens[id]++
		delete(s.ready, id)
	}
	s.mu.Unlock()
	return s.services.StopOwner(ctx, localRVCServiceOwner)
}

func (s *localRVCService) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for id, timer := range s.timers {
		timer.Stop()
		delete(s.timers, id)
		s.gens[id]++
		delete(s.ready, id)
	}
	s.mu.Unlock()
	return s.services.Close(ctx)
}

func (s *localRVCService) start(ctx context.Context, fish, expectedBinding string, registration localRVCRegistration, manifest map[string]string) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("station_rvc_service_closed")
	}
	if err := s.ensurePrivateScope(registration.Workspace, fish); err != nil {
		return err
	}
	var token string
	control := filepath.Join(s.executableDir, "runtimes", "windows-station-rvc")
	python := filepath.Join(s.executableDir, "runtimes", "windows-python-cli", "python.exe")
	private := localRVCPrivateScope(registration.Workspace, fish)
	state := filepath.Join(private, "music", "jobs")
	logDir := filepath.Join(s.dataDir, "station-rvc-logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return errors.New("station_rvc_log_directory_unavailable")
	}
	scratch := filepath.Join(private, "scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		return errors.New("station_rvc_private_scope_unavailable")
	}
	binding := localRVCBinding(registration, manifest)
	if binding != expectedBinding {
		return errors.New("station_rvc_deployment_changed")
	}
	readyPath, readyNonce, err := s.newReadyFile(fish)
	if err != nil {
		return err
	}
	launched := false
	check := func(checkCtx context.Context) error {
		current, currentManifest, err := s.loadRegistration(checkCtx)
		if err != nil || localRVCBinding(current, currentManifest) != expectedBinding {
			return errors.New("station_rvc_registration_changed")
		}
		if _, err = os.Stat(python); err != nil {
			return errors.New("station_rvc_python_unavailable")
		}
		if _, err = os.Stat(control); err != nil {
			return errors.New("station_rvc_control_unavailable")
		}
		return nil
	}
	spec := pluginruntime.ManagedServiceSpec{
		OwnerPlugin: localRVCServiceOwner,
		Binding:     binding,
		Executable:  python,
		Arguments: []string{"-X", "utf8", filepath.Join(control, "yue_bridge.py"), "--rvc-only", "--station-fish", fish,
			"--project", filepath.Join(registration.Workspace, "public_workspace"), "--managed-public", filepath.Join(registration.Workspace, "public_workspace"),
			"--managed-private", private, "--state", state, "--token-file", filepath.Join(s.dataDir, localRVCTokenFile), "--ready-file", readyPath, "--ready-nonce", readyNonce,
			"--bind", "127.0.0.1", "--port", strconv.Itoa(registration.Port), "--desktop"},
		Directory:       control,
		Environment:     localRVCEnvironment(registration.Media, scratch),
		LogPath:         filepath.Join(logDir, localRVCFishHash(fish)+".log"),
		PendingJobsPath: filepath.Join(state, "jobs.json"),
		StopInput:       "stop\n", StopTimeout: 50 * time.Second, Check: check,
	}
	_, err = s.services.Start(ctx, localRVCServiceID(fish), spec, func(admitCtx context.Context, launch func() error) error {
		listener, listenErr := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(registration.Port))
		if listenErr != nil {
			return errors.New("station_rvc_port_occupied_no_adoption")
		}
		_ = listener.Close()
		if err := check(admitCtx); err != nil {
			return err
		}
		var tokenErr error
		token, tokenErr = s.readOrCreateToken()
		if tokenErr != nil || token == "" {
			return errors.New("station_rvc_authentication_unavailable")
		}
		if err := launch(); err != nil {
			return errors.New("station_rvc_start_failed_check_private_log")
		}
		launched = true
		return nil
	})
	if err != nil {
		_ = os.Remove(readyPath)
		return err
	}
	if token == "" {
		var tokenErr error
		token, tokenErr = s.readExistingToken()
		if tokenErr != nil {
			return tokenErr
		}
	}
	if launched {
		if err := s.waitReady(ctx, fish, binding, registration.Port, readyPath, readyNonce); err != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 56*time.Second)
			_ = s.services.Stop(stopCtx, localRVCServiceID(fish))
			cancel()
			return err
		}
	} else if !s.readyMatches(localRVCServiceID(fish), binding, registration.Port, localRVCFishHash(fish)) {
		_ = os.Remove(readyPath)
		return errors.New("station_rvc_ready_state_missing")
	}
	return s.waitHealthy(ctx, fish, registration.Port, token)
}

func (s *localRVCService) waitHealthy(ctx context.Context, fish string, port int, token string) error {
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, _ := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/health", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(localRVCHeaderFish, localRVCFishHash(fish))
		response, err := s.client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			if response.StatusCode == http.StatusOK && response.Header.Get(localRVCHeaderFish) == localRVCFishHash(fish) {
				cancel()
				return nil
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("station_rvc_start_timeout")
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (s *localRVCService) newReadyFile(fish string) (string, string, error) {
	directory := filepath.Join(s.dataDir, "station-rvc-ready")
	if err := os.MkdirAll(directory, 0700); err != nil || !localRVCSafeDirectory(directory) {
		return "", "", errors.New("station_rvc_ready_directory_unavailable")
	}
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", "", errors.New("station_rvc_ready_nonce_unavailable")
	}
	nonce := hex.EncodeToString(bytes)
	return filepath.Join(directory, localRVCFishHash(fish)+"-"+nonce+".json"), nonce, nil
}

func (s *localRVCService) waitReady(ctx context.Context, fish, binding string, port int, path, nonce string) error {
	defer os.Remove(path)
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	wantScope := localRVCFishHash(fish)
	for {
		data, err := localRVCReadRegular(path, 4096)
		if err == nil && len(data) > 0 {
			var proof struct {
				Nonce string `json:"nonce"`
				Port  int    `json:"port"`
				Scope string `json:"scope"`
				PID   int    `json:"pid"`
			}
			if json.Unmarshal(data, &proof) == nil && proof.Nonce == nonce && proof.Port == port && proof.Scope == wantScope && proof.PID > 0 {
				s.mu.Lock()
				s.ready[localRVCServiceID(fish)] = localRVCReady{Binding: binding, Port: port, Scope: wantScope}
				s.mu.Unlock()
				return nil
			}
			if err == nil {
				return errors.New("station_rvc_ready_proof_invalid")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("station_rvc_ready_timeout")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *localRVCService) readyMatches(id, binding string, port int, scope string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	proof, ok := s.ready[id]
	return ok && proof.Binding == binding && proof.Port == port && proof.Scope == scope
}

func (s *localRVCService) loadRegistration(ctx context.Context) (localRVCRegistration, map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return localRVCRegistration{}, nil, err
	}
	data, err := localRVCReadRegular(filepath.Join(s.dataDir, localRVCRegistrationFile), 64<<10)
	if err != nil || len(data) == 0 {
		return localRVCRegistration{}, nil, errors.New("station_rvc_not_installed_or_authorized")
	}
	var registration localRVCRegistration
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &registration) != nil || json.Unmarshal(data, &fields) != nil || !localRVCRegistrationFields(fields) || registration.SchemaVersion != 1 || !registration.Enabled {
		return localRVCRegistration{}, nil, errors.New("station_rvc_not_installed_or_authorized")
	}
	if err := registration.validate(s.dataDir, s.editableRoot); err != nil {
		return localRVCRegistration{}, nil, err
	}
	manifest, err := localRVCControlManifest(filepath.Join(s.executableDir, "runtimes", "windows-station-rvc"))
	if err != nil {
		return localRVCRegistration{}, nil, err
	}
	return registration, manifest, nil
}

func localRVCRegistrationFields(fields map[string]json.RawMessage) bool {
	if fields == nil {
		return false
	}
	allowed := map[string]bool{"schema_version": true, "enabled": true, "workspace": true, "media": true, "port": true, "idle_seconds": true, "profiles": true, "fish_id": true}
	for name := range fields {
		if !allowed[name] {
			return false
		}
	}
	for _, required := range []string{"schema_version", "enabled", "workspace", "media", "port", "profiles"} {
		if !allowed[required] || fields[required] == nil {
			return false
		}
	}
	return true
}

func (r localRVCRegistration) validate(dataDir, editableRoot string) error {
	if r.Port < 1024 || r.Port > 65535 || !localRVCSafeDirectory(r.Workspace) || !localRVCSafeDirectory(r.Media) {
		return errors.New("station_rvc_registration_invalid")
	}
	if localRVCInside(dataDir, editableRoot) {
		return errors.New("station_rvc_host_storage_overlaps_agent_home")
	}
	if localRVCPathsOverlap(r.Workspace, editableRoot) || localRVCPathsOverlap(r.Workspace, dataDir) || localRVCPathsOverlap(r.Media, editableRoot) || localRVCPathsOverlap(r.Media, dataDir) {
		return errors.New("station_rvc_workspace_overlaps_agent_home")
	}
	if !localRVCSafeDirectory(filepath.Join(r.Workspace, "public_workspace")) || !localRVCSafeDirectory(filepath.Join(r.Workspace, "pri_workspace")) {
		return errors.New("station_rvc_workspace_invalid")
	}
	for _, executable := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		if !localRVCSafeRegular(filepath.Join(r.Media, executable)) {
			return errors.New("station_rvc_media_invalid")
		}
	}
	if r.IdleSeconds < 0 || int64(r.IdleSeconds) > int64((1<<63-1)/int64(time.Second)) {
		return errors.New("station_rvc_idle_seconds_invalid")
	}
	seen := make(map[string]bool, len(r.Profiles))
	aliases := make(map[string]bool, len(r.Profiles))
	if r.FishID != "" && !localRVCFishPattern.MatchString(r.FishID) {
		return errors.New("station_rvc_profile_registration_invalid")
	}
	for _, profile := range r.Profiles {
		id := strings.ToLower(profile.ID)
		if !localRVCVoicePattern.MatchString(profile.ID) || strings.TrimSpace(profile.Label) == "" || len([]rune(profile.Label)) > 160 || seen[id] {
			return errors.New("station_rvc_profile_registration_invalid")
		}
		seen[id] = true
	}
	for _, profile := range r.Profiles {
		alias := strings.ToLower(profile.LegacyVoiceID)
		if alias == "" {
			continue
		}
		if !localTTSVoiceIDPattern.MatchString(profile.LegacyVoiceID) || aliases[alias] || (seen[alias] && profile.LegacyVoiceID != profile.ID) {
			return errors.New("station_rvc_profile_registration_invalid")
		}
		aliases[alias] = true
	}
	return nil
}

func (r localRVCRegistration) idleTimeout() time.Duration {
	if r.IdleSeconds == 0 {
		return localRVCDefaultIdle
	}
	return time.Duration(r.IdleSeconds) * time.Second
}

func (s *localRVCService) ensurePrivateScope(workspace, fish string) error {
	private := localRVCPrivateScope(workspace, fish)
	for _, directory := range []string{private, filepath.Join(private, "music"), filepath.Join(private, "music", "jobs")} {
		if err := os.MkdirAll(directory, 0700); err != nil || !localRVCSafeDirectory(directory) {
			return errors.New("station_rvc_private_scope_unavailable")
		}
	}
	return nil
}

func (s *localRVCService) readOrCreateToken() (string, error) {
	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return "", errors.New("station_rvc_authentication_unavailable")
	}
	if token, err := s.readExistingToken(); err == nil {
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	path := filepath.Join(s.dataDir, localRVCTokenFile)
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", errors.New("station_rvc_authentication_unavailable")
	}
	token := hex.EncodeToString(bytes)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return s.readOrCreateToken()
	}
	if err != nil {
		return "", errors.New("station_rvc_authentication_unavailable")
	}
	_, writeErr := file.WriteString(token)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return "", errors.New("station_rvc_authentication_unavailable")
	}
	return token, nil
}

func (s *localRVCService) readExistingToken() (string, error) {
	path := filepath.Join(s.dataDir, localRVCTokenFile)
	data, err := localRVCReadRegular(path, 4096)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", errors.New("station_rvc_authentication_unavailable")
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || len(token) > 256 || strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("station_rvc_authentication_unavailable")
	}
	return token, nil
}

func (s *localRVCService) scheduleIdle(id string, timeout time.Duration) {
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
		ctx, cancel := context.WithTimeout(context.Background(), 56*time.Second)
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

func (s *localRVCService) cancelIdle(id string) {
	s.mu.Lock()
	s.gens[id]++
	if timer := s.timers[id]; timer != nil {
		timer.Stop()
		delete(s.timers, id)
	}
	s.mu.Unlock()
}

func localRVCRequestedProfileAllowed(profiles []localRVCVoice, method, path string, body []byte) error {
	profile := ""
	if method == http.MethodGet && strings.HasPrefix(path, "/rvc-health?") {
		value, err := url.ParseRequestURI(path)
		if err != nil {
			return errors.New("station_rvc_profile_not_registered")
		}
		profile = value.Query().Get("voice_profile_id")
	}
	if method == http.MethodPost && path == "/rvc-jobs" {
		var request struct {
			VoiceProfileID string `json:"voice_profile_id"`
		}
		if json.Unmarshal(body, &request) != nil {
			return errors.New("station_rvc_request_invalid")
		}
		profile = request.VoiceProfileID
	}
	if profile == "" {
		return nil
	}
	for _, allowed := range profiles {
		if allowed.ID == profile {
			return nil
		}
	}
	return errors.New("station_rvc_profile_not_registered")
}

func localRVCEnvironment(media, scratch string) []string {
	systemRoot := os.Getenv("SystemRoot")
	return []string{"SystemRoot=" + systemRoot, "WINDIR=" + os.Getenv("WINDIR"), "SystemDrive=" + os.Getenv("SystemDrive"),
		"PATH=" + media + string(os.PathListSeparator) + filepath.Join(systemRoot, "System32"),
		"WECHAT_WINDOWS_MUSIC_POLICY=registry-read-private-network-v1", "WECHAT_WINDOWS_MUSIC_MEDIA=" + media,
		"TEMP=" + scratch, "TMP=" + scratch, "APPDATA=" + scratch, "LOCALAPPDATA=" + scratch, "USERPROFILE=" + scratch,
		"USERNAME=station-rvc", "HF_HOME=" + filepath.Join(scratch, "hf-cache"), "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1", "PYTHONDONTWRITEBYTECODE=1", "PYTHONUTF8=1"}
}

func localRVCServiceID(fish string) string {
	return localRVCServiceOwner + "." + localRVCFishHash(fish)
}
func localRVCPrivateScope(workspace, fish string) string {
	return filepath.Join(workspace, "pri_workspace", localRVCFishHash(fish))
}
func localRVCFishHash(fish string) string { return mediaspec.SpeechSHA256([]byte(fish)) }

func localRVCBinding(registration localRVCRegistration, manifest map[string]string) string {
	// Marshal a canonical value instead of raw registration bytes, so harmless
	// JSON formatting changes do not invalidate a still-identical deployment.
	profiles := append([]localRVCVoice(nil), registration.Profiles...)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	files := make([]string, 0, len(manifest))
	for name, digest := range manifest {
		files = append(files, name+":"+digest)
	}
	sort.Strings(files)
	value, _ := json.Marshal(struct {
		Schema, Port     int
		Workspace, Media string
		Profiles         []localRVCVoice
		Files            []string
		FishID           string `json:",omitempty"`
	}{registration.SchemaVersion, registration.Port, registration.Workspace, registration.Media, profiles, files, registration.FishID})
	return mediaspec.SpeechSHA256(value)
}

func localRVCControlManifest(control string) (map[string]string, error) {
	if !localRVCSafeDirectory(control) {
		return nil, errors.New("station_rvc_control_unavailable")
	}
	manifestPath := filepath.Join(control, "runtime-manifest.json")
	if !localRVCSafeRegular(manifestPath) {
		return nil, errors.New("station_rvc_control_unavailable")
	}
	data, err := localRVCReadRegular(manifestPath, 64<<10)
	if err != nil || len(data) == 0 {
		return nil, errors.New("station_rvc_control_unavailable")
	}
	var manifest struct {
		SchemaVersion int               `json:"schema_version"`
		Platform      string            `json:"platform"`
		Policy        string            `json:"policy"`
		Files         map[string]string `json:"files"`
	}
	if json.Unmarshal(data, &manifest) != nil || manifest.SchemaVersion != 1 || manifest.Platform != "windows-x64" || manifest.Policy != "station-rvc-v1" || len(manifest.Files) != len(localRVCControlFiles) {
		return nil, errors.New("station_rvc_control_invalid")
	}
	entries, err := os.ReadDir(control)
	if err != nil || len(entries) != len(localRVCControlFiles)+1 {
		return nil, errors.New("station_rvc_control_invalid")
	}
	for _, name := range localRVCControlFiles {
		expected, exists := manifest.Files[name]
		if !exists || !localRVCRequestPattern.MatchString(expected) {
			return nil, errors.New("station_rvc_control_invalid")
		}
		path := filepath.Join(control, name)
		if !localRVCSafeRegular(path) {
			return nil, errors.New("station_rvc_control_invalid")
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, errors.New("station_rvc_control_invalid")
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, io.LimitReader(file, 8<<20))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || hex.EncodeToString(hash.Sum(nil)) != expected {
			return nil, errors.New("station_rvc_control_invalid")
		}
	}
	return manifest.Files, nil
}

var localRVCControlFiles = []string{
	"managed_music_runner.py", "music_account_scope.py", "process_window.py", "rvc_backend.py", "rvc_cover.py", "rvc_pitch.py", "rvc_profiles.py", "rvc_recovery.py", "rvc_voice.py", "rvc_windows.py", "singing_config.py", "singing_voice.py", "station_rvc.py", "windows_lpac.py", "yue_bridge.py", "yue_recovery.py", "yue_sampling.py",
}

func validateLocalRVCRequest(method, path, key string, body []byte) error {
	if method != http.MethodGet && method != http.MethodPost || len(body) > localRVCMaxJSON {
		return errors.New("station_rvc_request_invalid")
	}
	u, err := url.ParseRequestURI(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/") {
		return errors.New("station_rvc_request_invalid")
	}
	valid := false
	if method == http.MethodGet {
		valid = (u.Path == "/health" && u.RawQuery == "") || localRVCRequestPattern.MatchString(strings.TrimPrefix(u.Path, "/rvc-requests/")) && strings.HasPrefix(u.Path, "/rvc-requests/") && u.RawQuery == "" || localRVCJobPath(u.Path) && u.RawQuery == ""
		if u.Path == "/rvc-health" {
			query := u.Query()
			valid = len(query) == 1 && len(query["voice_profile_id"]) == 1 && localRVCVoicePattern.MatchString(query.Get("voice_profile_id"))
		}
		if key != "" || len(body) != 0 {
			valid = false
		}
	} else {
		valid = u.RawQuery == "" && ((u.Path == "/rvc-jobs" && localRVCRequestPattern.MatchString(key)) || (localRVCJobActionPath(u.Path) && key == ""))
	}
	if !valid {
		return errors.New("station_rvc_request_invalid")
	}
	return nil
}

func localRVCJobPath(path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return len(parts) == 2 && parts[0] == "jobs" && localRVCJobPattern.MatchString(parts[1]) || len(parts) == 3 && parts[0] == "jobs" && localRVCJobPattern.MatchString(parts[1]) && parts[2] == "audio"
}
func localRVCJobActionPath(path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return len(parts) == 3 && parts[0] == "jobs" && localRVCJobPattern.MatchString(parts[1]) && (parts[2] == "cancel" || parts[2] == "resume")
}
func checkLocalRVCFish(fish string) error {
	if !localRVCFishPattern.MatchString(fish) {
		return errors.New("station_rvc_fish_invalid")
	}
	return nil
}
func localRVCPathsOverlap(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return localRVCInside(left, right) || localRVCInside(right, left)
}
func localRVCInside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}
func localRVCSafeDirectory(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir() && localRVCLinkFree(path)
}
func localRVCSafeRegular(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && localRVCLinkFree(path)
}

func localRVCReadRegular(path string, limit int64) ([]byte, error) {
	if limit <= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("station_rvc_regular_file_required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !localRVCLinkFree(path) {
		return nil, errors.New("station_rvc_regular_file_required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("station_rvc_file_limit")
	}
	return data, nil
}

func localRVCLinkFree(path string) bool {
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
