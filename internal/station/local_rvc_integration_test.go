package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/fishidentity"
)

// fakeLocalRVCBackend is deliberately a bridge-level fixture.  It validates
// the exact protocol the host sends to the independently managed RVC service;
// it does not stand in for a model, GPU, or WeChat transport.
type fakeLocalRVCBackend struct {
	mu sync.Mutex

	fish    string
	binding string
	voice   localRVCVoice
	pin     localRVCVoicePin
	source  []byte
	output  []byte

	remoteID         string
	title            string
	lostPostReceipt  bool
	lostPostReturned bool
	submits          int
	health           int
	lookups          int
	stops            int
	closes           int
}

func (f *fakeLocalRVCBackend) Info(_ context.Context, fish string) (localRVCDeployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fish != "" && fish != f.fish {
		return localRVCDeployment{}, errSyntheticRVC("wrong fish")
	}
	return localRVCDeployment{Binding: f.binding, Profiles: []localRVCVoice{f.voice}}, nil
}

func (f *fakeLocalRVCBackend) CallExisting(ctx context.Context, fish, binding, method, path, key string, body []byte) ([]byte, int, error) {
	if method != http.MethodGet {
		return nil, 0, errSyntheticRVC("background query attempted a write")
	}
	return f.Call(ctx, fish, binding, method, path, key, body)
}

func (f *fakeLocalRVCBackend) Call(_ context.Context, fish, binding, method, path, key string, body []byte) ([]byte, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fish != f.fish || binding != f.binding {
		return nil, 0, errSyntheticRVC("unexpected deployment scope")
	}
	if method == http.MethodGet && path == "/rvc-health?voice_profile_id="+f.voice.ID {
		f.health++
		return f.json(map[string]any{
			"ready": true, "engine": "rvc", "profile": f.pin.Label,
			"voice_profile_id": f.voice.ID, "config_sha256": f.pin.ConfigHash,
			"model_sha256": f.pin.ModelHash, "index_sha256": f.pin.IndexHash,
			"requested_device": f.pin.Device,
		}), http.StatusOK, nil
	}
	if method == http.MethodPost && path == "/rvc-jobs" {
		if key == "" {
			return nil, http.StatusBadRequest, errSyntheticRVC("missing idempotency key")
		}
		var request struct {
			Title          string  `json:"title"`
			VoiceID        string  `json:"voice_profile_id"`
			Reference      string  `json:"reference_base64"`
			SourceHash     string  `json:"reference_source_sha256"`
			ConfigHash     string  `json:"profile_config_sha256"`
			StartSeconds   float64 `json:"reference_start_seconds"`
			Seconds        float64 `json:"reference_seconds"`
			ReferenceReady bool    `json:"reference_pretrimmed"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			return nil, http.StatusBadRequest, err
		}
		source, err := base64.StdEncoding.DecodeString(request.Reference)
		if err != nil || !bytes.Equal(source, f.source) || request.VoiceID != f.voice.ID ||
			request.ConfigHash != f.pin.ConfigHash || request.StartSeconds != 0 || !request.ReferenceReady || request.Seconds != 1 {
			return nil, http.StatusBadRequest, errSyntheticRVC("invalid conversion request")
		}
		sum := sha256.Sum256(f.source)
		if request.SourceHash != hex.EncodeToString(sum[:]) {
			return nil, http.StatusBadRequest, errSyntheticRVC("source hash not pinned")
		}
		f.submits++
		if f.lostPostReceipt && !f.lostPostReturned {
			f.lostPostReturned = true
			return nil, 0, errSyntheticRVC("synthetic lost RVC submit receipt")
		}
		return f.json(map[string]any{"id": f.remoteID, "engine": "rvc", "state": "queued", "voice_profile_id": f.voice.ID}), http.StatusAccepted, nil
	}
	if method == http.MethodGet && path == "/jobs/"+f.remoteID {
		f.lookups++
		return f.json(f.completed()), http.StatusOK, nil
	}
	if method == http.MethodGet && strings.HasPrefix(path, "/rvc-requests/") && len(strings.TrimPrefix(path, "/rvc-requests/")) == 64 {
		f.lookups++
		return f.json(f.completed()), http.StatusOK, nil
	}
	if method == http.MethodGet && path == "/jobs/"+f.remoteID+"/audio" {
		return append([]byte(nil), f.output...), http.StatusOK, nil
	}
	return nil, http.StatusNotFound, errSyntheticRVC("unexpected route " + method + " " + path)
}

func (f *fakeLocalRVCBackend) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

func (f *fakeLocalRVCBackend) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeLocalRVCBackend) completed() map[string]any {
	sum := sha256.Sum256(f.output)
	return map[string]any{
		"id": f.remoteID, "engine": "rvc", "state": "completed", "voice_profile_id": f.voice.ID,
		"recipe": map[string]any{
			"voice_profile_id": f.voice.ID, "reference_sha256": sha256Hex(f.source),
			"reference_source_sha256": sha256Hex(f.source), "reference_start_seconds": 0, "reference_seconds": 1, "title": f.title,
		},
		"singing_voice": map[string]any{"profile": f.pin.Label, "config_sha256": f.pin.ConfigHash, "model_sha256": f.pin.ModelHash, "index_sha256": f.pin.IndexHash, "requested_device": f.pin.Device},
		"sha256":        hex.EncodeToString(sum[:]), "bytes": len(f.output), "duration_seconds": 1,
		"frames": 8000, "sample_rate": 8000, "channels": 1, "bits_per_sample": 16,
		"profile": f.pin.Label, "model_sha256": f.pin.ModelHash, "index_sha256": f.pin.IndexHash,
		"requested_device": f.pin.Device, "rvc_device": "cpu",
	}
}

func (f *fakeLocalRVCBackend) json(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

type errSyntheticRVC string

func (e errSyntheticRVC) Error() string { return string(e) }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func syntheticMonoWAV(t *testing.T, rate, frames int) []byte {
	t.Helper()
	if rate < 1 || frames < 1 {
		t.Fatal("invalid synthetic mono WAV dimensions")
	}
	body := make([]byte, 44+frames*2)
	copy(body[:4], "RIFF")
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(body)-8))
	copy(body[8:12], "WAVE")
	copy(body[12:16], "fmt ")
	binary.LittleEndian.PutUint32(body[16:20], 16)
	binary.LittleEndian.PutUint16(body[20:22], 1)
	binary.LittleEndian.PutUint16(body[22:24], 1)
	binary.LittleEndian.PutUint32(body[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(body[28:32], uint32(rate*2))
	binary.LittleEndian.PutUint16(body[32:34], 2)
	binary.LittleEndian.PutUint16(body[34:36], 16)
	copy(body[36:40], "data")
	binary.LittleEndian.PutUint32(body[40:44], uint32(frames*2))
	for index := 0; index < frames; index++ {
		binary.LittleEndian.PutUint16(body[44+index*2:46+index*2], uint16(int16(index%1024-512)))
	}
	return body
}

func TestLocalRVCConversionThroughChatPinsVoiceAndPublishesArtifact(t *testing.T) {
	f := localChatFixture(t)
	catalog, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	source, output := syntheticMonoWAV(t, 8000, 8000), syntheticMonoWAV(t, 8000, 8000)
	fake := &fakeLocalRVCBackend{
		binding: "synthetic-rvc-binding", voice: localRVCVoice{ID: "private-test", Label: "私人测试声线", LegacyVoiceID: "legacy-test"},
		pin:    localRVCVoicePin{Label: "私人测试声线", ConfigHash: strings.Repeat("a", 64), ModelHash: strings.Repeat("b", 64), IndexHash: strings.Repeat("c", 64), Device: "cpu"},
		source: source, output: output, remoteID: strings.Repeat("d", 32), title: "合成换声验收",
	}
	f.handler.chat.projectTools.options.CatalogDirectory = catalog
	f.handler.chat.projectTools.rvc = fake

	// A registered project-tool store is a prerequisite for discovery.  The
	// test sets the expected owner only after the fixture has created it.
	if code, out := f.request("GET", "/api/local/project-tools", nil, true); code != http.StatusOK {
		t.Fatalf("initial RVC list: %d %#v", code, out)
	}
	fish, _, err := fishidentity.New(f.handler.home.dataDir).Bindings()
	if err != nil || fish == "" {
		t.Fatalf("read fixture fish identity: %q %v", fish, err)
	}
	fake.fish = fish

	const route = "/api/local/project-tools/rvc"
	if code, _ := f.request("PUT", route, map[string]any{"action": "install"}, true); code != http.StatusBadRequest {
		t.Fatalf("RVC installed without artifact approval: %d", code)
	}
	if code, out := f.request("PUT", route, map[string]any{"action": "install", "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatalf("install RVC: %d %#v", code, out)
	}
	installed := localProjectToolByID(t, f, localRVCPackage)
	if installed["installed"] != true || installed["enabled"] != false || installed["state"] != "disabled" {
		t.Fatalf("RVC install state: %#v", installed)
	}
	if code, out := f.request("PUT", route, map[string]any{"action": "enable", "revision": localProjectToolRevision(t, installed)}, true); code != http.StatusOK {
		t.Fatalf("enable RVC: %d %#v", code, out)
	}
	if enabled := localProjectToolByID(t, f, localRVCPackage); enabled["enabled"] != true || enabled["state"] != "idle" {
		t.Fatalf("RVC enable state: %#v", enabled)
	}

	_, home := f.request("GET", "/api/local/home", nil, true)
	inputPath := filepath.Join(home["directory"].(string), "Project Tools", "synthetic-rvc-source.wav")
	if err := os.WriteFile(inputPath, source, 0600); err != nil {
		t.Fatal(err)
	}
	_, registered := f.request("POST", "/api/local/artifacts", map[string]string{
		"path": "Project Tools/synthetic-rvc-source.wav", "expected_sha256": sha256Hex(source), "request_key": "rvc-source-fixture",
	}, true)
	inputID, _ := registered["artifact_id"].(string)
	if !artifactID(inputID, "work-") {
		t.Fatalf("source artifact missing: %#v", registered)
	}

	var localJob, outputID string
	var rounds int
	jobPattern := regexp.MustCompile(`job_id[^a-f0-9]+([a-f0-9]{64})`)
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(_ context.Context, system string, turns []map[string]string) (string, error) {
			rounds++
			history := localModelHistory(turns)
			switch rounds {
			case 1:
				for _, tool := range []string{localRVCVoicesTool, localRVCStartTool, localRVCJobTool} {
					if !strings.Contains(system, tool) {
						t.Errorf("enabled RVC tool %s missing from model schema", tool)
					}
				}
				return localToolCall(localRVCVoicesTool, map[string]any{}), nil
			case 2:
				if !strings.Contains(history, "private-test") || !strings.Contains(history, "legacy-test") {
					t.Error("voice list result was not supplied to the model")
				}
				return localToolCall(localRVCStartTool, map[string]any{"artifact_id": inputID, "voice_profile_id": "legacy-test", "title": "合成换声验收"}), nil
			case 3:
				matches := jobPattern.FindStringSubmatch(history)
				if len(matches) != 2 {
					t.Fatalf("missing local RVC job id in model history: %s", history)
				}
				localJob = matches[1]
				return localToolCall(localRVCJobTool, map[string]any{"job_id": localJob, "action": "status"}), nil
			default:
				for _, id := range projectArtifactIDs(turns) {
					if id != inputID {
						outputID = id
					}
				}
				return `{"action":"reply","messages":["换声成品已登记：` + outputID + `。"]}`, nil
			}
		}), nil
	}
	if code, out := localChatSend(f, "rvc-conversion", "用私人测试声线换这段音频并保存成品"); code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("RVC conversion chat: %d %#v", code, out)
	}
	if rounds != 4 || !validLocalRVCHex(localJob, 64) || !artifactID(outputID, "work-") || outputID == inputID {
		t.Fatalf("RVC model workflow: rounds=%d job=%q output=%q", rounds, localJob, outputID)
	}
	fake.mu.Lock()
	submits, health := fake.submits, fake.health
	fake.mu.Unlock()
	if submits != 1 || health != 1 {
		t.Fatalf("RVC bridge calls: submits=%d health=%d", submits, health)
	}
	if code, view := f.request("GET", "/api/local/artifacts/"+outputID, nil, true); code != http.StatusOK || view["input_artifact_id"] != inputID || view["input_sha256"] != sha256Hex(source) {
		t.Fatalf("RVC artifact lineage: %d %#v", code, view)
	}
	if got := localArtifactDownload(t, f, outputID); !bytes.Equal(got, output) {
		t.Fatal("RVC artifact download did not preserve validated bridge audio")
	}
	for range 2 {
		if code, out := localChatSend(f, "rvc-conversion", "用私人测试声线换这段音频并保存成品"); code != http.StatusOK || out["reused"] != true {
			t.Fatalf("RVC request replay: %d %#v", code, out)
		}
	}
	fake.mu.Lock()
	if fake.submits != 1 {
		t.Fatalf("replayed local chat submitted duplicate RVC job: %d", fake.submits)
	}
	fake.mu.Unlock()

	// An exact voice profile is a hard request constraint.  A typo must not
	// probe bridge health or silently fall back to another profile.
	before := fake.submits
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls := 0
		return localModelFunc(func(_ context.Context, _ string, _ []map[string]string) (string, error) {
			calls++
			if calls == 1 {
				return localToolCall(localRVCStartTool, map[string]any{"artifact_id": inputID, "voice_profile_id": "not-registered"}), nil
			}
			return `{"action":"reply","messages":["指定声线没有登记。"]}`, nil
		}), nil
	}
	if code, out := localChatSend(f, "rvc-wrong-voice", "换成不存在的声线"); code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("wrong voice recovery chat: %d %#v", code, out)
	}
	fake.mu.Lock()
	if fake.submits != before {
		t.Fatalf("wrong voice reached RVC submit: %d -> %d", before, fake.submits)
	}
	fake.mu.Unlock()
}

// TestLocalRVCRecoversLostSubmitReceipt proves that a network failure after
// the bridge accepted a conversion is not treated as permission to submit the
// source again.  The only recovery path is the original durable request key.
func TestLocalRVCRecoversLostSubmitReceiptWithoutDuplicate(t *testing.T) {
	f := localChatFixture(t)
	catalog, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	source, output := syntheticMonoWAV(t, 8000, 8000), syntheticMonoWAV(t, 8000, 8000)
	fake := &fakeLocalRVCBackend{
		binding: "synthetic-rvc-lost-receipt", voice: localRVCVoice{ID: "private-test", Label: "私人测试声线"},
		pin:    localRVCVoicePin{Label: "私人测试声线", ConfigHash: strings.Repeat("a", 64), ModelHash: strings.Repeat("b", 64), IndexHash: strings.Repeat("c", 64), Device: "cpu"},
		source: source, output: output, remoteID: strings.Repeat("e", 32), title: "迟到回执验收", lostPostReceipt: true,
	}
	f.handler.chat.projectTools.options.CatalogDirectory = catalog
	f.handler.chat.projectTools.rvc = fake
	if code, out := f.request("GET", "/api/local/project-tools", nil, true); code != http.StatusOK {
		t.Fatalf("initial RVC list: %d %#v", code, out)
	}
	fish, _, err := fishidentity.New(f.handler.home.dataDir).Bindings()
	if err != nil || fish == "" {
		t.Fatalf("read fixture fish identity: %q %v", fish, err)
	}
	fake.fish = fish
	if code, out := f.request("PUT", "/api/local/project-tools/rvc", map[string]any{"action": "install", "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatalf("install RVC: %d %#v", code, out)
	}
	installed := localProjectToolByID(t, f, localRVCPackage)
	if code, out := f.request("PUT", "/api/local/project-tools/rvc", map[string]any{"action": "enable", "revision": localProjectToolRevision(t, installed)}, true); code != http.StatusOK {
		t.Fatalf("enable RVC: %d %#v", code, out)
	}
	_, home := f.request("GET", "/api/local/home", nil, true)
	if err := os.WriteFile(filepath.Join(home["directory"].(string), "Project Tools", "lost-receipt.wav"), source, 0600); err != nil {
		t.Fatal(err)
	}
	_, registered := f.request("POST", "/api/local/artifacts", map[string]string{
		"path": "Project Tools/lost-receipt.wav", "expected_sha256": sha256Hex(source), "request_key": "rvc-lost-receipt-source",
	}, true)
	inputID, _ := registered["artifact_id"].(string)
	if !artifactID(inputID, "work-") {
		t.Fatalf("source artifact missing: %#v", registered)
	}

	var jobID, outputID string
	rounds := 0
	jobPattern := regexp.MustCompile(`job_id[^a-f0-9]+([a-f0-9]{64})`)
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			rounds++
			switch rounds {
			case 1:
				return localToolCall(localRVCStartTool, map[string]any{"artifact_id": inputID, "voice_profile_id": "private-test", "title": "迟到回执验收"}), nil
			case 2:
				match := jobPattern.FindStringSubmatch(localModelHistory(turns))
				if len(match) != 2 {
					t.Fatalf("lost submit did not leave a durable local task: %s", localModelHistory(turns))
				}
				jobID = match[1]
				return localToolCall(localRVCJobTool, map[string]any{"job_id": jobID, "action": "status"}), nil
			default:
				for _, id := range projectArtifactIDs(turns) {
					if id != inputID {
						outputID = id
					}
				}
				return `{"action":"reply","messages":["已按原请求恢复并登记成品。"]}`, nil
			}
		}), nil
	}
	if code, out := localChatSend(f, "rvc-lost-receipt", "换声，桥接回执丢失后继续查询原任务"); code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("lost receipt recovery chat: %d %#v", code, out)
	}
	if rounds != 3 || !validLocalRVCHex(jobID, 64) || !artifactID(outputID, "work-") {
		t.Fatalf("lost receipt workflow: rounds=%d job=%q artifact=%q", rounds, jobID, outputID)
	}
	fake.mu.Lock()
	submits, lookups, lost := fake.submits, fake.lookups, fake.lostPostReturned
	fake.mu.Unlock()
	if submits != 1 || lookups < 1 || !lost {
		t.Fatalf("lost receipt recovery did not use original request lookup: submits=%d lookups=%d lost=%v", submits, lookups, lost)
	}
	if downloaded := localArtifactDownload(t, f, outputID); !bytes.Equal(downloaded, output) {
		t.Fatal("lost receipt recovery published a different audio payload")
	}
	tools := projectFollowupFixtureTools(t, f, "rvc-lost-receipt")
	view, waiting, err := tools.projectFollowupStatus(t.Context(), localRVCJobTool, jobID)
	if err != nil || waiting || view["state"] != "completed" || view["artifact_id"] != outputID {
		t.Fatal("RVC background read did not reuse the verified output", view, waiting, err)
	}
}
