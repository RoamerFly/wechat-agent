// Package localasr validates and executes host-registered local speech recognition.
package localasr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/workspacefs"
)

const (
	PluginID                = "media-understanding"
	localASRManifestLimit   = 64 << 10
	localASRRuntimeManifest = "runtime-manifest.json"
)

// Manifest is the installed public-workspace contract. It
// deliberately includes only fields the plugin runner needs and provenance
// fields already emitted by scripts/install_local_asr.py.
type Manifest struct {
	ID             string  `json:"id"`
	Root           string  `json:"root"`
	Python         string  `json:"python"`
	Entry          string  `json:"entry"`
	Packages       string  `json:"packages"`
	Model          string  `json:"model"`
	Tokens         string  `json:"tokens"`
	VADModel       string  `json:"vad_model"`
	VADSHA256      string  `json:"vad_sha256"`
	VADThreshold   float64 `json:"vad_threshold"`
	Threads        int     `json:"threads"`
	Language       string  `json:"language"`
	ChunkSeconds   float64 `json:"chunk_seconds"`
	TimeoutSeconds float64 `json:"timeout_seconds"`
}

// Initializer only names the host-registered profile. The bounded
// wechat/local_transcription service resolves interpreter/model paths itself;
// paths and command fragments are not exposed in plugin configuration or tool
// arguments.
type Initializer struct {
	ProfileID       string `json:"profile_id"`
	ResourceVersion string `json:"resource_version"`
}

// Profile is a host-validated model and runtime registration.
type Profile struct {
	initializer Initializer
	resources   []pluginruntime.ReadOnlyResource
	runner      localASRRunner
}

// Metadata returns the resource identity used to bind calls and plugin initialization.
func (p Profile) Metadata() Initializer { return p.initializer }

// Resources returns the read-only mounts authorized by the profile.
func (p Profile) Resources() []pluginruntime.ReadOnlyResource {
	return append([]pluginruntime.ReadOnlyResource(nil), p.resources...)
}

// TimeoutSeconds returns the registered execution ceiling.
func (p Profile) TimeoutSeconds() int { return int(p.runner.TimeoutSeconds) }

type localASRRunner struct {
	Python, Packages, Model, Tokens, VADModel string
	VADThreshold                              float64
	Threads                                   int
	Language                                  string
	ChunkSeconds, TimeoutSeconds              float64
}

// Run is the host-side local ASR execution boundary
// shared by account applications and the application-independent station. It
// accepts an already verified package/profile pair; callers still need to
// revalidate their own origin and permission version before and after calling
// it. No endpoint, credential, account, or caller-provided command enters the
// local runner.
func Run(ctx context.Context, store *pluginhost.Store, scratch string, windows *agenthost.WindowsConfig, p pluginhost.Package, g pluginhost.Grant, profile Profile, wav []byte, timeoutSeconds int) (string, string, error) {
	if len(wav) == 0 || len(wav) > 10<<20 || !bytesHasWAVHeader(wav) || timeoutSeconds < 1 || timeoutSeconds > int(profile.runner.TimeoutSeconds) || int64(timeoutSeconds) > int64((1<<63-1)/int64(time.Second)) {
		return "", "", errors.New("local_asr_request_invalid")
	}
	if store == nil || g.Account == "" || store.Account() != g.Account || g.Plugin != p.ID || g.Version != p.Version || scratch == "" {
		return "", "", errors.New("local_asr_authorization_invalid")
	}
	for _, resource := range profile.resources {
		if resource.Account != g.Account || resource.Plugin != g.Plugin || resource.PackageVersion != g.Version {
			return "", "", errors.New("local_asr_authorization_invalid")
		}
	}
	installed, err := store.Installed(ctx, p.ID)
	if err != nil || !installed.Enabled || installed.Removed || installed.Package.Version != p.Version || installed.Package.ID != PluginID {
		return "", "", errors.New("local_asr_plugin_changed")
	}
	entryName := "runtime/local_asr.py"
	if _, ok := installed.Package.Files[entryName]; !ok {
		return "", "", errors.New("local_asr_runner_unavailable")
	}
	packageDir, err := store.PackageDirectory(installed.Digest)
	if err != nil {
		return "", "", err
	}
	entry, err := cleanLocalASRPath(filepath.Join(packageDir, filepath.FromSlash(entryName)), false)
	if err != nil || hashLocalASRFile(entry) != installed.Package.Files[entryName] {
		return "", "", errors.New("local_asr_runner_unavailable")
	}
	root, err := workspacefs.EnsurePath(scratch)
	if err != nil {
		return "", "", err
	}
	if err = root.Close(); err != nil {
		return "", "", err
	}
	folder, err := os.MkdirTemp(scratch, "plugin-asr-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(folder)
	input := filepath.Join(folder, "input.wav")
	if err = os.WriteFile(input, wav, 0600); err != nil {
		return "", "", err
	}
	readonly := make([]string, 0, len(profile.resources)+1)
	for _, resource := range profile.resources {
		// agenthost mounts paths rather than pluginruntime descriptors; each was
		// canonicalized and manifest-bound while resolving this profile.
		readonly = append(readonly, resource.Root)
	}
	// Installed files can have a protected DACL on Windows. The directory
	// permits traversal, while the exact script grant admits the existing file.
	readonly = append(readonly, filepath.Dir(entry), entry)
	cfg := agenthost.Defaults()
	if runtime.GOOS == "windows" {
		cfg.Windows = windows
	}
	cfg.Mode, cfg.OutputBytes, cfg.TimeoutSeconds = "full", 64000, timeoutSeconds
	cfg.Roots = []string{folder}
	cfg.Isolation = &agenthost.Isolation{ReadOnly: readonly}
	bounded, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	argv := []string{profile.runner.Python, "-X", "utf8", entry, "--packages", profile.runner.Packages, "--model", profile.runner.Model, "--tokens", profile.runner.Tokens, "--input", input, "--threads", strconv.Itoa(profile.runner.Threads), "--language", profile.runner.Language, "--chunk-seconds", strconv.FormatFloat(profile.runner.ChunkSeconds, 'f', -1, 64)}
	if profile.runner.VADModel != "" {
		argv = append(argv, "--vad-model", profile.runner.VADModel, "--vad-threshold", strconv.FormatFloat(profile.runner.VADThreshold, 'f', -1, 64))
	}
	if runtime.GOOS == "windows" {
		// The installed package digest and fish identity can make paths exceed
		// Win32 MAX_PATH. Use extended spelling only after resource validation.
		for i, path := range readonly {
			readonly[i] = pluginruntime.WindowsProcessPath(path)
		}
		for i, arg := range argv {
			if filepath.IsAbs(arg) {
				argv[i] = pluginruntime.WindowsProcessPath(arg)
			}
		}
	}
	out, err := agenthost.RunIsolated(bounded, cfg, folder, argv)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return "", "", errors.New("local_asr_execution_failed")
	}
	var result struct {
		State    string `json:"state"`
		Text     string `json:"text"`
		Complete bool   `json:"range_complete"`
	}
	if json.Unmarshal(out, &result) != nil || !result.Complete {
		return "", "", errors.New("local_asr_response_invalid")
	}
	if result.State == "no_recognizable_speech" && strings.TrimSpace(result.Text) == "" {
		return result.State, "", nil
	}
	if result.State != "transcribed" || strings.TrimSpace(result.Text) == "" {
		return "", "", errors.New("local_asr_response_invalid")
	}
	return result.State, strings.TrimSpace(result.Text), nil
}

func bytesHasWAVHeader(wav []byte) bool {
	return len(wav) >= 12 && bytes.Equal(wav[:4], []byte("RIFF")) && bytes.Equal(wav[8:12], []byte("WAVE"))
}

// Resolve validates only the media-understanding
// package selected by the supplied owner-scoped store.  The resource root is
// chosen by a trusted host registration, never by plugin configuration or an
// Agent request.
func Resolve(ctx context.Context, store *pluginhost.Store, public string, p pluginhost.Package, g pluginhost.Grant) (Profile, bool) {
	if err := ctx.Err(); err != nil || store == nil || public == "" || p.ID != PluginID || g.Account == "" || g.Account != store.Account() || g.Plugin != p.ID || g.Version != p.Version {
		return Profile{}, false
	}
	installed, err := store.Installed(ctx, PluginID)
	if err != nil || !installed.Enabled || installed.Removed || installed.Package.ID != p.ID || installed.Package.Version != p.Version {
		return Profile{}, false
	}
	configuration, err := store.ConfigurationForExecution(ctx, PluginID)
	if err != nil {
		return Profile{}, false
	}
	var backend, id string
	if json.Unmarshal(configuration.Values["asr_backend"], &backend) != nil || backend != "local" || json.Unmarshal(configuration.Values["local_asr_profile_id"], &id) != nil || !ValidID(id) {
		return Profile{}, false
	}
	profile, err := ReadProfile(public, p, g, id)
	if err != nil {
		// An absent, stale or tampered registration does not grant a broad public
		// workspace mount. The media plugin can still use its separately configured
		// remote ASR route, while its local backend remains unavailable.
		return Profile{}, false
	}
	return profile, true
}

// ReadProfile resolves a profile only beneath a
// host-selected public resource root. It deliberately shares the exact
// registration schema and content verification with legacy account ASR while
// remaining independent of a WeChat account.
func ReadProfile(public string, p pluginhost.Package, g pluginhost.Grant, id string) (Profile, error) {
	if !ValidID(id) {
		return Profile{}, errors.New("local_asr_model_id_invalid")
	}
	public, err := cleanLocalASRPath(public, true)
	if err != nil {
		return Profile{}, err
	}
	manifestPath, err := cleanLocalASRPath(filepath.Join(public, "manifests", "asr-"+id+".json"), false)
	if err != nil || !insideLocalASR(public, manifestPath) {
		return Profile{}, errors.New("local_asr_manifest_unavailable")
	}
	manifestBytes, err := readBoundedLocalASRFile(manifestPath, localASRManifestLimit)
	if err != nil {
		return Profile{}, errors.New("local_asr_manifest_invalid")
	}
	var manifest Manifest
	if json.Unmarshal(manifestBytes, &manifest) != nil || manifest.ID != "asr-"+id {
		return Profile{}, errors.New("local_asr_manifest_invalid")
	}
	root, err := localASRRegisteredPath(public, manifest.Root, true)
	if err != nil {
		return Profile{}, err
	}
	python, err := localASRRegisteredPath(public, manifest.Python, false)
	if err != nil {
		return Profile{}, err
	}
	entry, err := localASRRegisteredPath(public, manifest.Entry, false)
	if err != nil {
		return Profile{}, err
	}
	packages, err := localASRRegisteredPath(public, manifest.Packages, true)
	if err != nil {
		return Profile{}, err
	}
	model, err := localASRRegisteredPath(public, manifest.Model, false)
	if err != nil || !insideLocalASR(root, model) {
		return Profile{}, errors.New("local_asr_model_invalid")
	}
	tokens, err := localASRRegisteredPath(public, manifest.Tokens, false)
	if err != nil || !insideLocalASR(root, tokens) {
		return Profile{}, errors.New("local_asr_tokens_invalid")
	}
	// Entry remains part of the legacy registration contract, but the plugin
	// executes its verified packaged local_asr.py instead of this public path.
	if entry == "" {
		return Profile{}, errors.New("local_asr_entry_invalid")
	}
	if manifest.Threads == 0 {
		manifest.Threads = 2
	}
	if manifest.Language == "" {
		manifest.Language = "auto"
	}
	if manifest.ChunkSeconds == 0 {
		manifest.ChunkSeconds = 30
	}
	if manifest.TimeoutSeconds == 0 {
		manifest.TimeoutSeconds = 120
	}
	if manifest.Threads < 1 || manifest.ChunkSeconds <= 0 || manifest.TimeoutSeconds <= 0 || manifest.TimeoutSeconds > 3600 || !validLocalASRLanguage(manifest.Language) {
		return Profile{}, errors.New("local_asr_parameters_invalid")
	}
	resources := []pluginruntime.ReadOnlyResource{}
	manifestSum := sha256.Sum256(manifestBytes)
	version := hex.EncodeToString(manifestSum[:])
	appendResource := func(id, path, kind string) error {
		for _, resource := range resources {
			if sameLocalASRPath(resource.Root, path) {
				return nil
			}
		}
		resources = append(resources, pluginruntime.ReadOnlyResource{ID: id, Root: path, Kind: kind, Account: g.Account, Plugin: PluginID, PackageVersion: p.Version, ManifestPath: manifestPath, ManifestSHA256: version})
		return nil
	}
	if err := appendResource("asr-interpreter", filepath.Dir(python), ""); err != nil {
		return Profile{}, err
	}
	if err := appendResource("asr-sdk", packages, ""); err != nil {
		return Profile{}, err
	}
	if err := appendResource("asr-model", root, ""); err != nil {
		return Profile{}, err
	}
	if err := verifyLocalASRRuntimeManifest(packages); err != nil {
		return Profile{}, err
	}
	// The profile identifier is the explicit media-plugin configuration value.
	// The public manifest is still named `asr-<profile-id>.json`; exposing that
	// filename prefix in host metadata would make the runtime compare two
	// different identifiers and tempt callers to reconstruct a path-like ID.
	initializer := Initializer{ProfileID: id, ResourceVersion: version}
	var vadPath string
	var vadThreshold float64
	if manifest.VADModel != "" {
		vad, err := localASRRegisteredPath(public, manifest.VADModel, false)
		if err != nil || len(manifest.VADSHA256) != 64 || !validLowerSHA256(manifest.VADSHA256) || hashLocalASRFile(vad) != manifest.VADSHA256 {
			return Profile{}, errors.New("local_asr_vad_invalid")
		}
		if err := appendResource("asr-vad", vad, "file"); err != nil {
			return Profile{}, err
		}
		vadPath, vadThreshold = vad, manifest.VADThreshold
		if vadThreshold == 0 {
			vadThreshold = .5
		}
	}
	runner := localASRRunner{Python: python, Packages: packages, Model: model, Tokens: tokens, Threads: manifest.Threads, Language: manifest.Language, ChunkSeconds: manifest.ChunkSeconds, TimeoutSeconds: manifest.TimeoutSeconds}
	runner.VADModel, runner.VADThreshold = vadPath, vadThreshold
	return Profile{initializer: initializer, resources: resources, runner: runner}, nil
}

func ValidID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0) {
			return false
		}
	}
	return true
}

func validLocalASRLanguage(language string) bool {
	for _, allowed := range []string{"auto", "zh", "en", "ja", "ko", "yue"} {
		if language == allowed {
			return true
		}
	}
	return false
}

func localASRRegisteredPath(public, candidate string, directory bool) (string, error) {
	path, err := cleanLocalASRPath(candidate, directory)
	if err != nil || !insideLocalASR(public, path) {
		return "", errors.New("local_asr_resource_outside_public_workspace")
	}
	return path, nil
}

func cleanLocalASRPath(path string, directory bool) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
		return "", errors.New("local_asr_resource_path_invalid")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("local_asr_resource_linked_or_missing")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !sameLocalASRPath(path, resolved) {
		return "", errors.New("local_asr_resource_alias")
	}
	info, err := os.Stat(path)
	if err != nil || directory != info.IsDir() || !directory && !info.Mode().IsRegular() {
		return "", errors.New("local_asr_resource_type_invalid")
	}
	return path, nil
}

func insideLocalASR(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func sameLocalASRPath(left, right string) bool {
	return left == right || runtime.GOOS == "windows" && strings.EqualFold(left, right)
}

func readBoundedLocalASRFile(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("local_asr_file_invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("local_asr_file_invalid")
	}
	return data, nil
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func hashLocalASRFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func verifyLocalASRRuntimeManifest(packages string) error {
	path := filepath.Join(packages, localASRRuntimeManifest)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // Linux registrations predate this optional Windows SDK record.
	}
	path, err := cleanLocalASRPath(path, false)
	if err != nil {
		return errors.New("local_asr_sdk_manifest_invalid")
	}
	raw, err := readBoundedLocalASRFile(path, 1<<20)
	if err != nil {
		return errors.New("local_asr_sdk_manifest_invalid")
	}
	var record struct {
		Files map[string]string `json:"files"`
	}
	if json.Unmarshal(raw, &record) != nil || len(record.Files) == 0 || len(record.Files) > 4096 {
		return errors.New("local_asr_sdk_manifest_invalid")
	}
	seen := make(map[string]bool, len(record.Files))
	for relative, digest := range record.Files {
		normalized, err := localASRRuntimeRelative(relative)
		if err != nil || !validLowerSHA256(digest) || seen[normalized] {
			return errors.New("local_asr_sdk_manifest_invalid")
		}
		seen[normalized] = true
		file, err := cleanLocalASRPath(filepath.Join(packages, normalized), false)
		if err != nil || !insideLocalASR(packages, file) || hashLocalASRFile(file) != digest {
			return errors.New("local_asr_sdk_content_changed")
		}
	}
	return nil
}

// localASRRuntimeRelative accepts the native separator convention of a
// Windows-produced SDK manifest, but only after making it one canonical,
// strictly relative path. It deliberately does not turn Windows drive paths
// into relative paths on non-Windows hosts.
func localASRRuntimeRelative(relative string) (string, error) {
	if relative == "" || strings.IndexByte(relative, 0) >= 0 {
		return "", errors.New("local_asr_sdk_manifest_invalid")
	}
	normalized := relative
	if runtime.GOOS == "windows" {
		normalized = strings.ReplaceAll(normalized, `\`, "/")
	} else if strings.Contains(normalized, `\`) {
		return "", errors.New("local_asr_sdk_manifest_invalid")
	}
	// Colon excludes drive-qualified and URI-like input on every platform.
	// path.Clean is used intentionally: manifest entries use slash separators,
	// even while the SDK itself is hosted on Windows.
	if strings.Contains(normalized, ":") || strings.HasPrefix(normalized, "/") {
		return "", errors.New("local_asr_sdk_manifest_invalid")
	}
	clean := path.Clean(normalized)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != normalized {
		return "", errors.New("local_asr_sdk_manifest_invalid")
	}
	return filepath.FromSlash(clean), nil
}
