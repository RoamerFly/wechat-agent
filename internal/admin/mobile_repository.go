package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/workspacefs"
)

const mobileStateNamespace = "device-state-v1"

func (m *Manager) openMobileStateStore(create bool) (*pluginhost.Store, error) {
	if !create {
		_, dbErr := os.Stat(filepath.Join(m.dir, "plugin-host", "plugins.sqlite"))
		_, settingsErr := os.Stat(filepath.Join(m.dir, "mobile", "settings.json"))
		_, tasksErr := os.Stat(filepath.Join(m.dir, "mobile", "tasks.json"))
		if errors.Is(dbErr, os.ErrNotExist) && errors.Is(settingsErr, os.ErrNotExist) && errors.Is(tasksErr, os.ErrNotExist) {
			return nil, nil
		}
	}
	return m.packageStore()
}

// Import only when the authoritative namespace has not been initialized.
func importMobileState(ctx context.Context, store *pluginhost.Store, dir string) (pluginhost.HostState, error) {
	host, err := store.ReadHostState(ctx, mobileDevicePluginID, mobileStateNamespace)
	if err != nil || host.Revision != 0 {
		return host, err
	}
	legacy, found, err := mobile.ReadLegacyImport(filepath.Join(dir, "mobile"))
	if err != nil || !found {
		return host, err
	}
	records, err := mobileStateRecords(legacy.Settings, legacy.Tasks, store.Account())
	if err != nil {
		return host, err
	}
	records["manifest"], _ = json.Marshal(mobileImportManifest{Version: 1, SourceHash: legacy.SourceHash, TaskCount: len(legacy.Tasks)})
	return store.InitializeHostState(ctx, mobileDevicePluginID, mobileStateNamespace, legacy.SourceHash, records)
}

func mobileTasksForSource(records map[string]mobile.Task, source *mobile.Source) []mobile.Task {
	tasks := make([]mobile.Task, 0, len(records))
	for _, task := range records {
		if source != nil && !source.Admin && (source.Account != task.Source.Account || source.Thread != task.Source.Thread) {
			continue
		}
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Created.After(tasks[j].Created) })
	return tasks
}

func mobileWorkflowActive(task mobile.Task) bool {
	return task.State == "queued" || task.State == "running"
}

func readMobileScreenshot(dir string, task mobile.Task, observationID string) ([]byte, error) {
	if task.ID == "" || task.ObservationID != observationID || !regexpMobileObservation(observationID) {
		return nil, errors.New("device_observation_not_found")
	}
	return readMobileBlob(dir, "observations", observationID+".png", 128<<20)
}

func readMobileArtifact(dir string, task mobile.Task, artifactID string) (mobile.Artifact, []byte, error) {
	for _, artifact := range task.Artifacts {
		if artifact.ID == artifactID && validID(artifactID) {
			data, err := readMobileBlob(dir, "recordings", artifactID+".mp4", 128<<20)
			if err == nil {
				hash := sha256.Sum256(data)
				if len(data) != artifact.Bytes || hex.EncodeToString(hash[:]) != artifact.SHA256 {
					err = errors.New("device_recording_integrity_invalid")
				}
			}
			return artifact, data, err
		}
	}
	return mobile.Artifact{}, nil, errors.New("device_recording_not_found")
}

func regexpMobileObservation(id string) bool {
	return len(id) == 36 && strings.HasPrefix(id, "obs-") && strings.Trim(id[4:], "0123456789abcdef") == ""
}

func readMobileBlob(dir, directory, name string, limit int64) ([]byte, error) {
	root, err := workspacefs.OpenPath(filepath.Join(dir, "mobile", directory))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("device_artifact_exceeds_host_memory_budget")
	}
	return data, nil
}

// mobileRepository keeps device workflows separate from individual plugin
// invocation Jobs. Only the host can read this namespace; it is not a plugin
// configuration value or a directory mounted into the isolated runtime.
type mobileRepository struct {
	mu      sync.Mutex
	store   *pluginhost.Store
	current pluginhost.HostState
}

type mobileImportManifest struct {
	Version    int    `json:"version"`
	SourceHash string `json:"source_hash"`
	TaskCount  int    `json:"task_count"`
}

func (r *mobileRepository) Load(ctx context.Context) (mobile.RepositorySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := r.store.ReadHostState(ctx, mobileDevicePluginID, mobileStateNamespace)
	if err != nil {
		return mobile.RepositorySnapshot{}, err
	}
	snapshot, err := decodeMobileHostState(state, r.store.Account())
	if err == nil {
		r.current = state
	}
	return snapshot, err
}

func decodeMobileHostState(state pluginhost.HostState, account string) (mobile.RepositorySnapshot, error) {
	out := mobile.RepositorySnapshot{Settings: mobile.Defaults(), Tasks: map[string]mobile.Task{}, Revision: state.Revision, Initialized: state.Revision > 0}
	if state.Revision == 0 {
		return out, nil
	}
	invalid := errors.New("device_repository_invalid")
	var manifest mobileImportManifest
	if state.Revision < 1 || json.Unmarshal(state.Records["manifest"], &manifest) != nil || manifest.Version != 1 || manifest.SourceHash != state.SourceHash || json.Unmarshal(state.Records["settings"], &out.Settings) != nil {
		return mobile.RepositorySnapshot{}, invalid
	}
	for key, record := range state.Records {
		if key == "settings" || key == "manifest" || key == mobileObservationControlKey || key == mobileObservationAppGrantsKey || strings.HasPrefix(key, "workflow:") {
			continue
		}
		if !strings.HasPrefix(key, "task:") {
			return mobile.RepositorySnapshot{}, invalid
		}
		var task mobile.Task
		if json.Unmarshal(record, &task) != nil || key != mobileTaskRecordKey(task.ID) || task.ID == "" || task.RequestHash == "" || task.Source.Account != "" && task.Source.Account != account {
			return mobile.RepositorySnapshot{}, invalid
		}
		out.Tasks[task.ID] = task
	}
	return out, nil
}

func mobileTaskRecordKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "task:" + hex.EncodeToString(sum[:])
}

func mobileStateRecords(settings mobile.Settings, tasks map[string]mobile.Task, account string) (map[string]json.RawMessage, error) {
	records := make(map[string]json.RawMessage, len(tasks)+1)
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	records["settings"] = raw
	for id, task := range tasks {
		if id == "" || id != task.ID || task.RequestHash == "" || task.Source.Account != "" && task.Source.Account != account {
			return nil, errors.New("device_repository_invalid")
		}
		raw, err = json.Marshal(task)
		if err != nil {
			return nil, err
		}
		records[mobileTaskRecordKey(id)] = raw
	}
	return records, nil
}

func (r *mobileRepository) Initialize(ctx context.Context, legacy mobile.LegacyImport) (mobile.RepositorySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if legacy.ManifestVersion != 1 {
		return mobile.RepositorySnapshot{}, errors.New("device_migration_required")
	}
	records, err := mobileStateRecords(legacy.Settings, legacy.Tasks, r.store.Account())
	if err != nil {
		return mobile.RepositorySnapshot{}, err
	}
	records["manifest"], _ = json.Marshal(mobileImportManifest{Version: 1, SourceHash: legacy.SourceHash, TaskCount: len(legacy.Tasks)})
	state, err := r.store.InitializeHostState(ctx, mobileDevicePluginID, mobileStateNamespace, legacy.SourceHash, records)
	if err != nil {
		return mobile.RepositorySnapshot{}, mobileRepositoryError(err)
	}
	r.current = state
	return decodeMobileHostState(state, r.store.Account())
}

func mobileRepositoryError(err error) error {
	if errors.Is(err, pluginhost.ErrRevision) {
		return mobile.ErrRepositoryConflict
	}
	return err
}

func (r *mobileRepository) Commit(ctx context.Context, revision int64, next mobile.RepositorySnapshot) (mobile.RepositorySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if revision < 0 || r.current.Revision != revision {
		return mobile.RepositorySnapshot{}, mobile.ErrRepositoryConflict
	}
	records, err := mobileStateRecords(next.Settings, next.Tasks, r.store.Account())
	if err != nil {
		return mobile.RepositorySnapshot{}, err
	}
	if revision == 0 {
		// A fresh install has no JSON source. Its first explicit save creates
		// the authoritative namespace in one transaction, without legacy files.
		sum := sha256.Sum256([]byte("mobile-empty-origin-v1"))
		sourceHash := hex.EncodeToString(sum[:])
		records["manifest"], _ = json.Marshal(mobileImportManifest{Version: 1, SourceHash: sourceHash})
		state, err := r.store.InitializeHostState(ctx, mobileDevicePluginID, mobileStateNamespace, sourceHash, records)
		if err != nil {
			return mobile.RepositorySnapshot{}, mobileRepositoryError(err)
		}
		r.current = state
		return decodeMobileHostState(state, r.store.Account())
	}
	// The import manifest stays immutable. Each update touches only changed
	// settings/tasks, even though Control provides its bounded in-memory view.
	records["manifest"] = r.current.Records["manifest"]
	puts := map[string]json.RawMessage{}
	deletes := []string{}
	for key, raw := range records {
		if !bytes.Equal(raw, r.current.Records[key]) {
			puts[key] = raw
		}
	}
	for key := range r.current.Records {
		if key == mobileObservationControlKey || key == mobileObservationAppGrantsKey || strings.HasPrefix(key, "workflow:") {
			records[key] = r.current.Records[key]
			continue
		}
		if _, exists := records[key]; !exists {
			deletes = append(deletes, key)
		}
	}
	updated, err := r.store.UpdateHostState(ctx, mobileDevicePluginID, mobileStateNamespace, revision, puts, deletes)
	if err != nil {
		return mobile.RepositorySnapshot{}, mobileRepositoryError(err)
	}
	r.current = pluginhost.HostState{Revision: updated, SourceHash: r.current.SourceHash, Records: records}
	return decodeMobileHostState(r.current, r.store.Account())
}

var _ mobile.Repository = (*mobileRepository)(nil)
