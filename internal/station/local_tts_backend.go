package station

// This file is the narrow, host-owned boundary for local GPT-SoVITS speech in
// the application-independent station.  It deliberately starts no web server
// and accepts neither a model path nor a command from the chat model.  Every
// run is a one-shot hidden LPAC child bound to a single administrator-registered
// voice.  In particular, a missing or changed named voice is an error: it may
// never fall back to the character/default voice.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/strictjson"
)

const (
	localTTSRegistrationFile = "station-speech.json"
	localTTSRegistrationMax  = 192 << 10
	// agenthost.Config caps diagnostic/stdout receipts at 64,000 bytes. WAV
	// payload has its own larger limit and is read from the private output file.
	localTTSOutputMaxBytes = 64000
	localTTSDefaultTimeout = 300 * time.Second
)

var (
	localTTSVoiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,128}$`)
	localTTSHexPattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// localTTSBackend exposes no raw path, command, endpoint, process ID, model
// digest or reference recording to the caller.  The returned binding pins a
// durable job to the owner-approved deployment seen at scheduling time.
type localTTSBackend interface {
	Info(context.Context, string) (localTTSDeployment, error)
	Synthesize(context.Context, string, string, localTTSRequest) (localTTSResult, error)
}

type localTTSDeployment struct {
	Binding string          `json:"binding"`
	Voices  []localTTSVoice `json:"voices"`
}

// localTTSVoice is intentionally display-only.  Version is a stable opaque
// resource binding, not a model filename or a user-controlled label.
type localTTSVoice struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Version  string  `json:"version"`
	SpeedMin float64 `json:"speed_min"`
	SpeedMax float64 `json:"speed_max"`
}

type localTTSRequest struct {
	VoiceID  string   `json:"voice_id"`
	Text     string   `json:"text"`
	Speed    *float64 `json:"speed,omitempty"`
	Language string   `json:"text_language,omitempty"`
}

type localTTSResult struct {
	VoiceID string  `json:"voice_id"`
	Version string  `json:"voice_version"`
	Speed   float64 `json:"speed"`
	WAV     []byte  `json:"-"`
}

// localTTSRegistration is written by a host/UI registration flow only.  It is
// deliberately a closed document: silently accepting a new field here is an
// easy way to accidentally turn a local configuration into a command channel.
// All paths stay host-private and are never returned by Info.
type localTTSRegistration struct {
	SchemaVersion int                         `json:"schema_version"`
	Enabled       bool                        `json:"enabled"`
	Project       string                      `json:"project"`
	Python        string                      `json:"python"`
	BaseModels    string                      `json:"base_models"`
	Packages      string                      `json:"packages,omitempty"`
	Runner        string                      `json:"runner"`
	RunnerSHA256  string                      `json:"runner_sha256"`
	Media         string                      `json:"media"`
	NLTKData      string                      `json:"nltk_data"`
	Device        string                      `json:"device"`
	Voices        []localTTSVoiceRegistration `json:"voices"`
}

type localTTSVoiceRegistration struct {
	ID              string  `json:"id"`
	Label           string  `json:"label"`
	GPT             string  `json:"gpt"`
	GPTSHA256       string  `json:"gpt_sha256"`
	SoVITS          string  `json:"sovits"`
	SoVITSSHA256    string  `json:"sovits_sha256"`
	Reference       string  `json:"reference"`
	ReferenceSHA256 string  `json:"reference_sha256"`
	Prompt          string  `json:"prompt"`
	PromptSHA256    string  `json:"prompt_sha256"`
	PromptLanguage  string  `json:"prompt_language"`
	TextLanguage    string  `json:"text_language"`
	SpeedMin        float64 `json:"speed_min,omitempty"`
	SpeedMax        float64 `json:"speed_max,omitempty"`
}

type localTTSService struct {
	dataDir       string
	editableRoot  string
	executableDir string
	// A stable writable mount is important on Windows: LPAC derives its token
	// from the mount set. One in-flight execution per backend also prevents a
	// second request from observing the first request's transient output.
	runGate chan struct{}
}

// newLocalTTSBackend is intentionally a one-shot worker rather than a warmed
// resident process: GPT-SoVITS is selected by one private voice binding, and
// leaving a server alive would retain a voice/model outside the durable task.
func newLocalTTSBackend(dataDir, editableRoot string) localTTSBackend {
	exe, _ := os.Executable()
	return newLocalTTSBackendAt(dataDir, editableRoot, filepath.Dir(exe))
}

func newLocalTTSBackendAt(dataDir, editableRoot, executableDir string) *localTTSService {
	return &localTTSService{
		dataDir:       filepath.Clean(dataDir),
		editableRoot:  filepath.Clean(editableRoot),
		executableDir: filepath.Clean(executableDir),
		runGate:       make(chan struct{}, 1),
	}
}

func (s *localTTSService) Info(ctx context.Context, fish string) (localTTSDeployment, error) {
	if err := localTTSCheckFish(fish); err != nil {
		return localTTSDeployment{}, err
	}
	registration, err := s.loadRegistration(ctx)
	if err != nil {
		return localTTSDeployment{}, err
	}
	// Do not advertise a deployment whose trusted Windows broker is missing.
	// The UI may display a configured voice, but that must never be mistaken for
	// an executable local capability.
	if runtime.GOOS != "windows" {
		return localTTSDeployment{}, errors.New("station_speech_windows_required")
	}
	if _, err := s.windowsConfig(); err != nil {
		return localTTSDeployment{}, err
	}
	voices := make([]localTTSVoice, 0, len(registration.Voices))
	for _, voice := range registration.Voices {
		voices = append(voices, voice.view())
	}
	sort.Slice(voices, func(i, j int) bool { return voices[i].ID < voices[j].ID })
	return localTTSDeployment{Binding: localTTSBinding(registration), Voices: voices}, nil
}

func (s *localTTSService) Synthesize(ctx context.Context, fish, binding string, request localTTSRequest) (localTTSResult, error) {
	if err := localTTSCheckFish(fish); err != nil {
		return localTTSResult{}, err
	}
	if runtime.GOOS != "windows" {
		return localTTSResult{}, errors.New("station_speech_windows_required")
	}
	if err := localTTSValidateRequest(request); err != nil {
		return localTTSResult{}, err
	}
	registration, err := s.loadRegistration(ctx)
	if err != nil {
		return localTTSResult{}, err
	}
	if binding == "" || binding != localTTSBinding(registration) {
		return localTTSResult{}, errors.New("station_speech_deployment_changed")
	}
	voice, ok := registration.voice(request.VoiceID)
	if !ok {
		return localTTSResult{}, errors.New("station_speech_voice_not_registered")
	}
	speed := 1.0
	if request.Speed != nil {
		speed = *request.Speed
	}
	if !localTTSSpeedAllowed(voice, speed) {
		return localTTSResult{}, errors.New("station_speech_speed_not_supported")
	}
	language := request.Language
	if language == "" {
		language = voice.TextLanguage
	}
	if !localTTSLanguageAllowed(language) {
		return localTTSResult{}, errors.New("station_speech_text_language_not_supported")
	}
	// Revalidate all owner-selected files immediately before mounting them. A
	// catalogue item with a stale path/hash never causes a fallback voice run.
	if err := registration.verifyResources(voice); err != nil {
		return localTTSResult{}, err
	}
	// A binding checked above can change while slow hash validation is running.
	// Read the registration once more at the execution boundary.
	current, err := s.loadRegistration(ctx)
	if err != nil || localTTSBinding(current) != binding {
		return localTTSResult{}, errors.New("station_speech_deployment_changed")
	}
	if currentVoice, exists := current.voice(voice.ID); !exists || currentVoice.version() != voice.version() {
		return localTTSResult{}, errors.New("station_speech_voice_changed")
	}
	return s.run(ctx, fish, binding, current, voice, request.Text, language, speed)
}

func (s *localTTSService) run(ctx context.Context, fish, binding string, registration localTTSRegistration, voice localTTSVoiceRegistration, text, language string, speed float64) (localTTSResult, error) {
	select {
	case s.runGate <- struct{}{}:
		defer func() { <-s.runGate }()
	case <-ctx.Done():
		return localTTSResult{}, ctx.Err()
	}
	// Keep this writable mount stable for a given fish and voice version. A
	// random MkdirTemp root would continuously mint distinct LPAC identities and
	// leave ACL entries on the interpreter/model files. Each invocation still
	// uses unique output paths, even if an old broker survived a host crash.
	directory := filepath.Join(s.dataDir, "station-speech-jobs", mediaspec.SpeechSHA256([]byte(fish+"\n"+voice.version())))
	if err := localTTSEnsureDir(directory); err != nil {
		return localTTSResult{}, errors.New("station_speech_private_directory_unavailable")
	}
	work, err := os.MkdirTemp(directory, "run-")
	if err != nil {
		return localTTSResult{}, errors.New("station_speech_private_directory_unavailable")
	}
	defer os.RemoveAll(work)
	parameters := filepath.Join(work, "parameters.json")
	output := filepath.Join(work, "speech.wav")
	parametersJSON, _ := json.Marshal(map[string]any{"speed_factor": speed})
	if err := os.WriteFile(parameters, parametersJSON, 0600); err != nil {
		return localTTSResult{}, errors.New("station_speech_request_write_failed")
	}
	argv := []string{registration.Python, registration.Runner,
		"--project", registration.Project, "--models", registration.BaseModels,
		"--gpt", voice.GPT, "--sovits", voice.SoVITS,
		"--reference", voice.Reference, "--prompt-text-file", voice.Prompt,
		"--text", text, "--text-lang", language, "--prompt-lang", voice.PromptLanguage,
		"--device", registration.Device, "--version", "v2", "--parameters", parameters,
		"--output", output,
	}
	if registration.Packages != "" {
		argv = append(argv, "--packages", registration.Packages)
	}
	if registration.Device == "cpu" {
		argv = append(argv, "--no-half")
	}
	// Runner writes stay in the selected voice's private mount. Sources are
	// explicit read-only mounts, so it cannot enumerate an entire voice library.
	config, err := s.windowsConfig()
	if err != nil {
		return localTTSResult{}, err
	}
	config.Roots = []string{directory}
	config.Mode = "full"
	config.TimeoutSeconds = int(localTTSDefaultTimeout / time.Second)
	config.OutputBytes = localTTSOutputMaxBytes
	config.Isolation = &agenthost.Isolation{GPU: registration.Device != "cpu", ReadOnly: localTTSReadOnly(registration, voice)}
	spool, err := os.MkdirTemp(config.Windows.Spool, "run-")
	if err != nil {
		return localTTSResult{}, errors.New("station_speech_private_directory_unavailable")
	}
	config.Windows.Spool = spool
	bounded, cancel := context.WithTimeout(ctx, localTTSDefaultTimeout)
	defer cancel()
	log, _, runErr := agenthost.RunSpeechIsolated(bounded, config, work, argv, localTTSEnvironment(registration, work))
	if runErr != nil {
		if ctx.Err() != nil {
			return localTTSResult{}, ctx.Err()
		}
		if bounded.Err() != nil {
			return localTTSResult{}, bounded.Err()
		}
		return localTTSResult{}, errors.New("station_speech_inference_failed")
	}
	// The broker returned a terminal receipt. Drop its plaintext argv and logs;
	// only the verified result and input digest belong in the durable job record.
	defer os.RemoveAll(spool)
	var reported mediaspec.GPTSoVITSResult
	lines := bytes.Split(bytes.TrimSpace(log), []byte("\n"))
	if len(lines) == 0 || strictjson.DecodeKnownFields(lines[len(lines)-1], &reported) != nil {
		return localTTSResult{}, errors.New("station_speech_runner_result_invalid")
	}
	canonicalPrompt, err := localTTSCanonicalPrompt(voice.Prompt)
	if err != nil || reported.Output != output || reported.GPT != voice.GPT || reported.SoVITS != voice.SoVITS || reported.GPTSHA256 != voice.GPTSHA256 || reported.SoVITSSHA256 != voice.SoVITSSHA256 || reported.ReferenceSHA256 != voice.ReferenceSHA256 || reported.PromptSHA256 != mediaspec.SpeechSHA256([]byte(canonicalPrompt)) || reported.TextSHA256 != mediaspec.SpeechSHA256([]byte(text)) || reported.Device != registration.Device || reported.Version != "v2" || reported.WeChatSends != 0 {
		return localTTSResult{}, errors.New("station_speech_runner_binding_mismatch")
	}
	wav, err := localTTSReadWAV(output)
	if err != nil {
		return localTTSResult{}, err
	}
	rate, seconds, err := mediaspec.PCM16WAVInfo(wav)
	if err != nil || rate < 8000 || rate > 192000 || reported.SHA256 != mediaspec.SpeechSHA256(wav) || reported.SampleRate != rate || math.IsNaN(reported.Seconds) || math.IsInf(reported.Seconds, 0) || reported.Seconds <= 0 || math.Abs(reported.Seconds-seconds) > 1e-8 {
		return localTTSResult{}, errors.New("station_speech_output_invalid")
	}
	// Registration edits and revoked/replaced voice files during an expensive
	// run invalidate the result.  Do not hand an old voice to publication after
	// an owner has made such a change.
	latest, err := s.loadRegistration(ctx)
	if err != nil || localTTSBinding(latest) != binding {
		return localTTSResult{}, errors.New("station_speech_deployment_changed")
	}
	latestVoice, found := latest.voice(voice.ID)
	if !found || latestVoice.version() != voice.version() || latest.verifyResources(latestVoice) != nil {
		return localTTSResult{}, errors.New("station_speech_voice_changed")
	}
	return localTTSResult{VoiceID: voice.ID, Version: voice.version(), Speed: speed, WAV: wav}, nil
}

func (s *localTTSService) windowsConfig() (agenthost.Config, error) {
	if !localTTSSafeDirectory(s.executableDir) {
		return agenthost.Config{}, errors.New("station_speech_runtime_unavailable")
	}
	pythonw := filepath.Join(s.executableDir, "runtimes", "windows-python-cli", "pythonw.exe")
	helper := filepath.Join(s.executableDir, "runtimes", "windows-agent-host", "windows_agent_host.pyw")
	if !localTTSSafeRegular(pythonw) || !localTTSSafeRegular(helper) {
		return agenthost.Config{}, errors.New("station_speech_runtime_unavailable")
	}
	spool := filepath.Join(s.dataDir, "station-speech-spool")
	if err := localTTSEnsureDir(spool); err != nil {
		return agenthost.Config{}, errors.New("station_speech_runtime_unavailable")
	}
	// Speech execution has a fixed offline policy and receives all selected
	// paths as explicit LPAC mounts. It must not receive the registration
	// document used by the unrelated music broker.
	windows := &agenthost.WindowsConfig{Pythonw: pythonw, Helper: helper, Spool: spool}
	if err := windows.Validate(); err != nil {
		return agenthost.Config{}, errors.New("station_speech_runtime_unavailable")
	}
	return agenthost.Config{Windows: windows}, nil
}

func (s *localTTSService) loadRegistration(ctx context.Context) (localTTSRegistration, error) {
	if err := ctx.Err(); err != nil {
		return localTTSRegistration{}, err
	}
	data, err := localTTSReadRegular(filepath.Join(s.dataDir, localTTSRegistrationFile), localTTSRegistrationMax)
	if err != nil || len(data) == 0 {
		return localTTSRegistration{}, errors.New("station_speech_not_installed_or_authorized")
	}
	var registration localTTSRegistration
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&registration) != nil || decoder.Decode(&struct{}{}) != io.EOF || json.Unmarshal(data, &fields) != nil || !localTTSRegistrationFields(fields) || registration.SchemaVersion != 1 || !registration.Enabled {
		return localTTSRegistration{}, errors.New("station_speech_not_installed_or_authorized")
	}
	if err := registration.validate(s.dataDir, s.editableRoot); err != nil {
		return localTTSRegistration{}, err
	}
	return registration, nil
}

func localTTSRegistrationFields(fields map[string]json.RawMessage) bool {
	allowed := map[string]bool{"schema_version": true, "enabled": true, "project": true, "python": true, "base_models": true, "packages": true, "runner": true, "runner_sha256": true, "media": true, "nltk_data": true, "device": true, "voices": true}
	for name := range fields {
		if !allowed[name] {
			return false
		}
	}
	for _, name := range []string{"schema_version", "enabled", "project", "python", "base_models", "runner", "runner_sha256", "media", "nltk_data", "device", "voices"} {
		if fields[name] == nil {
			return false
		}
	}
	return true
}

func (r localTTSRegistration) validate(dataDir, editableRoot string) error {
	if r.Device != "cuda" && r.Device != "cpu" {
		return errors.New("station_speech_registration_invalid")
	}
	// Desktop deployments normally put the managed workspace under dataDir.
	// Reject the reverse arrangement: it would place the private registry
	// inside the tree editable by the Agent.
	if localTTSWithin(dataDir, editableRoot) {
		return errors.New("station_speech_host_storage_overlaps_agent_home")
	}
	for _, path := range []struct {
		path      string
		directory bool
	}{{r.Project, true}, {r.Python, false}, {r.BaseModels, true}, {r.Runner, false}, {r.Media, true}, {r.NLTKData, true}} {
		if err := localTTSRegularPath(path.path, path.directory); err != nil {
			return errors.New("station_speech_registration_invalid")
		}
	}
	if r.Packages != "" && localTTSRegularPath(r.Packages, true) != nil {
		return errors.New("station_speech_registration_invalid")
	}
	if !localTTSHexPattern.MatchString(r.RunnerSHA256) || localTTSFileSHA(r.Runner) != r.RunnerSHA256 {
		return errors.New("station_speech_runner_changed")
	}
	if localTTSPathsOverlap(r.Project, editableRoot) || localTTSPathsOverlap(r.BaseModels, editableRoot) || localTTSPathsOverlap(r.Python, editableRoot) || localTTSPathsOverlap(r.Media, editableRoot) || localTTSPathsOverlap(r.NLTKData, editableRoot) || localTTSPathsOverlap(r.Runner, editableRoot) || r.Packages != "" && localTTSPathsOverlap(r.Packages, editableRoot) {
		return errors.New("station_speech_registration_invalid")
	}
	// The registration is station control-plane data. A broad runtime mount
	// must not include it, the fish home, or a named voice profile. Selected
	// GPT/SoVITS/reference/prompt files are added individually at execution;
	// this prevents a source tree or interpreter parent from silently granting
	// discovery of the other owner-registered voices.
	runtimeRoots := []string{r.Project, r.BaseModels, filepath.Dir(r.Python), r.Media, r.NLTKData, r.Packages}
	for _, root := range runtimeRoots {
		if root != "" && (localTTSPathsOverlap(root, dataDir) || localTTSPathsOverlap(root, editableRoot)) {
			return errors.New("station_speech_runtime_overlaps_private_data")
		}
	}
	for _, name := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		if localTTSRegularPath(filepath.Join(r.Media, name), false) != nil {
			return errors.New("station_speech_media_invalid")
		}
	}
	seen := make(map[string]bool, len(r.Voices))
	for _, voice := range r.Voices {
		if seen[voice.ID] || !localTTSVoiceIDPattern.MatchString(voice.ID) || !localTTSSafeLabel(voice.Label) || !localTTSLanguageAllowed(voice.PromptLanguage) || !localTTSLanguageAllowed(voice.TextLanguage) || !localTTSSpeedBounds(voice.SpeedMin, voice.SpeedMax) {
			return errors.New("station_speech_voice_registration_invalid")
		}
		seen[voice.ID] = true
		for _, file := range []struct {
			path, hash string
			prompt     bool
		}{{voice.GPT, voice.GPTSHA256, false}, {voice.SoVITS, voice.SoVITSSHA256, false}, {voice.Reference, voice.ReferenceSHA256, false}, {voice.Prompt, voice.PromptSHA256, true}} {
			if !localTTSHexPattern.MatchString(file.hash) || localTTSRegularPath(file.path, false) != nil || localTTSPathsOverlap(file.path, editableRoot) || localTTSPathsOverlap(file.path, dataDir) {
				return errors.New("station_speech_voice_registration_invalid")
			}
			if file.prompt {
				data, err := localTTSReadRegular(file.path, 1<<20)
				if err != nil || !utf8.Valid(data) || strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff")) == "" || mediaspec.SpeechSHA256(data) != file.hash {
					return errors.New("station_speech_prompt_changed")
				}
			}
		}
	}
	for _, root := range runtimeRoots {
		for _, voice := range r.Voices {
			for _, privateFile := range []string{voice.GPT, voice.SoVITS, voice.Reference, voice.Prompt} {
				if root != "" && localTTSPathsOverlap(root, privateFile) {
					return errors.New("station_speech_runtime_overlaps_private_voice")
				}
			}
		}
	}
	if len(r.Voices) == 0 {
		return errors.New("station_speech_no_registered_voices")
	}
	return nil
}

func (r localTTSRegistration) voice(id string) (localTTSVoiceRegistration, bool) {
	for _, voice := range r.Voices {
		if voice.ID == id {
			return voice, true
		}
	}
	return localTTSVoiceRegistration{}, false
}

func (r localTTSRegistration) verifyResources(voice localTTSVoiceRegistration) error {
	for _, file := range []struct{ path, hash string }{{r.Runner, r.RunnerSHA256}, {voice.GPT, voice.GPTSHA256}, {voice.SoVITS, voice.SoVITSSHA256}, {voice.Reference, voice.ReferenceSHA256}, {voice.Prompt, voice.PromptSHA256}} {
		if localTTSFileSHA(file.path) != file.hash {
			return errors.New("station_speech_registered_resource_changed")
		}
	}
	return nil
}

func localTTSCanonicalPrompt(path string) (string, error) {
	data, err := localTTSReadRegular(path, 1<<20)
	if err != nil || !utf8.Valid(data) {
		return "", errors.New("station_speech_prompt_changed")
	}
	// pathlib.Path.read_text performs universal-newline translation before
	// GPT-SoVITS calls strip(). Mirror that exact preparation before checking
	// the runner's prompt hash; a raw registered-file hash is checked separately.
	prompt := strings.TrimPrefix(string(data), "\ufeff")
	prompt = strings.ReplaceAll(prompt, "\r\n", "\n")
	prompt = strings.ReplaceAll(prompt, "\r", "\n")
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", errors.New("station_speech_prompt_changed")
	}
	return prompt, nil
}

func (v localTTSVoiceRegistration) view() localTTSVoice {
	minimum, maximum := v.SpeedMin, v.SpeedMax
	if minimum == 0 && maximum == 0 {
		minimum, maximum = 0.5, 2
	}
	return localTTSVoice{ID: v.ID, Label: v.Label, Version: v.version(), SpeedMin: minimum, SpeedMax: maximum}
}

func (v localTTSVoiceRegistration) version() string {
	data, _ := json.Marshal(struct {
		ID, GPT, GPTSHA, SoVITS, SoVITSSHA, Reference, ReferenceSHA, Prompt, PromptSHA, PromptLanguage, TextLanguage string
		SpeedMin, SpeedMax                                                                                           float64
	}{v.ID, v.GPT, v.GPTSHA256, v.SoVITS, v.SoVITSSHA256, v.Reference, v.ReferenceSHA256, v.Prompt, v.PromptSHA256, v.PromptLanguage, v.TextLanguage, v.SpeedMin, v.SpeedMax})
	return mediaspec.SpeechSHA256(data)
}

func localTTSBinding(registration localTTSRegistration) string {
	voices := append([]localTTSVoiceRegistration(nil), registration.Voices...)
	sort.Slice(voices, func(i, j int) bool { return voices[i].ID < voices[j].ID })
	data, _ := json.Marshal(struct {
		Schema                                                                        int
		Project, Python, BaseModels, Packages, Runner, RunnerSHA, Media, NLTK, Device string
		Voices                                                                        []localTTSVoiceRegistration
	}{registration.SchemaVersion, registration.Project, registration.Python, registration.BaseModels, registration.Packages, registration.Runner, registration.RunnerSHA256, registration.Media, registration.NLTKData, registration.Device, voices})
	return mediaspec.SpeechSHA256(data)
}

func localTTSValidateRequest(request localTTSRequest) error {
	if !localTTSVoiceIDPattern.MatchString(request.VoiceID) || strings.TrimSpace(request.Text) == "" || len(request.Text) > localTTSTextLimit || !utf8.ValidString(request.Text) || strings.ContainsRune(request.Text, 0) {
		return errors.New("station_speech_request_invalid")
	}
	if request.Speed != nil && (math.IsNaN(*request.Speed) || math.IsInf(*request.Speed, 0) || *request.Speed <= 0) {
		return errors.New("station_speech_speed_invalid")
	}
	if request.Language != "" && !localTTSLanguageAllowed(request.Language) {
		return errors.New("station_speech_text_language_not_supported")
	}
	return nil
}

func localTTSSpeedBounds(minimum, maximum float64) bool {
	if minimum == 0 && maximum == 0 {
		return true
	}
	return !math.IsNaN(minimum) && !math.IsNaN(maximum) && !math.IsInf(minimum, 0) && !math.IsInf(maximum, 0) && minimum > 0 && maximum >= minimum && maximum <= 4
}

func localTTSSpeedAllowed(voice localTTSVoiceRegistration, speed float64) bool {
	minimum, maximum := voice.SpeedMin, voice.SpeedMax
	if minimum == 0 && maximum == 0 {
		minimum, maximum = 0.5, 2
	}
	return speed >= minimum && speed <= maximum
}

func localTTSLanguageAllowed(language string) bool {
	return map[string]bool{"auto": true, "all_zh": true, "all_en": true, "all_ja": true, "all_ko": true, "all_yue": true, "zh": true, "en": true, "ja": true, "ko": true, "yue": true, "zh_en": true, "ja_en": true, "ko_en": true, "yue_en": true, "auto_zh": true, "auto_en": true, "auto_ja": true, "auto_ko": true, "auto_yue": true}[language]
}

func localTTSReadOnly(registration localTTSRegistration, voice localTTSVoiceRegistration) []string {
	// CPython resolves its DLLs beside python.exe, so mount the registered
	// interpreter directory rather than relying on an inherited host PATH.
	paths := []string{registration.Project, registration.BaseModels, filepath.Dir(registration.Python), registration.Runner, registration.Media, registration.NLTKData, voice.GPT, voice.SoVITS, voice.Reference, voice.Prompt}
	if registration.Packages != "" {
		paths = append(paths, registration.Packages)
	}
	return paths
}

func localTTSEnvironment(registration localTTSRegistration, scratch string) map[string]string {
	systemRoot := os.Getenv("SystemRoot")
	return map[string]string{
		"PATH":                          registration.Media + string(os.PathListSeparator) + filepath.Join(systemRoot, "System32"),
		"NLTK_DATA":                     registration.NLTKData,
		"HF_HOME":                       filepath.Join(scratch, "hf-cache"),
		"HF_HUB_OFFLINE":                "1",
		"TRANSFORMERS_OFFLINE":          "1",
		"PYTHONDONTWRITEBYTECODE":       "1",
		"PYTHONUTF8":                    "1",
		"MPLCONFIGDIR":                  filepath.Join(scratch, "matplotlib"),
		"NUMBA_CACHE_DIR":               filepath.Join(scratch, "numba-cache"),
		"WECHAT_WINDOWS_SPEECH_RUNTIME": "1",
		"USERNAME":                      "station-speech",
	}
}

func localTTSReadWAV(path string) ([]byte, error) {
	data, err := localTTSReadRegular(path, localTTSWAVLimit)
	if err != nil {
		return nil, errors.New("station_speech_audio_missing")
	}
	return data, nil
}

func localTTSCheckFish(fish string) error {
	if !fishidentity.ValidFishID(fish) {
		return errors.New("station_speech_fish_invalid")
	}
	return nil
}

func localTTSSafeLabel(label string) bool {
	return strings.TrimSpace(label) != "" && utf8.ValidString(label) && utf8.RuneCountInString(label) <= 160 && strings.IndexFunc(label, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func localTTSPathsOverlap(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	return localTTSWithin(left, right) || localTTSWithin(right, left)
}

func localTTSWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func localTTSEnsureDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil || !localTTSSafeDirectory(path) {
		return fmt.Errorf("directory unavailable")
	}
	return nil
}

func localTTSSafeDirectory(path string) bool { return localTTSRegularPath(path, true) == nil }
func localTTSSafeRegular(path string) bool   { return localTTSRegularPath(path, false) == nil }

func localTTSRegularPath(path string, directory bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("path invalid")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("path unavailable")
		}
		if current == path && (directory && !info.IsDir() || !directory && !info.Mode().IsRegular()) {
			return errors.New("path type invalid")
		}
		if parent := filepath.Dir(current); parent == current {
			return nil
		}
	}
}

func localTTSReadRegular(path string, limit int64) ([]byte, error) {
	if limit <= 0 || localTTSRegularPath(path, false) != nil {
		return nil, errors.New("station_speech_regular_file_required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("station_speech_file_limit")
	}
	return data, nil
}

func localTTSFileSHA(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}
