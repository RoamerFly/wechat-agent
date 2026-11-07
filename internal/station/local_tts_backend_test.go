package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/mediaspec"
)

func TestLocalTTSInfoOnlyAdvertisesRunnableRegisteredVoices(t *testing.T) {
	dataDir, editableRoot, executableDir, registration := localTTSRegistrationFixture(t)
	backend := newLocalTTSBackendAt(dataDir, editableRoot, executableDir)
	info, err := backend.Info(context.Background(), localTTSFixtureFishID())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Binding == "" || len(info.Voices) != 1 {
		t.Fatalf("unexpected deployment: %#v", info)
	}
	voice := info.Voices[0]
	if voice.ID != registration.Voices[0].ID || voice.Label != registration.Voices[0].Label || voice.Version != registration.Voices[0].version() || voice.SpeedMin != 0.5 || voice.SpeedMax != 2 {
		t.Fatalf("unexpected public voice: %#v", voice)
	}
	public, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for _, privatePath := range []string{registration.Python, registration.Runner, registration.Voices[0].GPT, registration.Voices[0].Reference} {
		if strings.Contains(string(public), privatePath) {
			t.Fatalf("Info leaked a private runtime path: %s", privatePath)
		}
	}
}

func TestLocalTTSRegistrationRejectsPrivateRuntimeMounts(t *testing.T) {
	dataDir, editableRoot, executableDir, registration := localTTSRegistrationFixture(t)
	registration.BaseModels = filepath.Join(dataDir, "models")
	if err := os.MkdirAll(registration.BaseModels, 0700); err != nil {
		t.Fatal(err)
	}
	localTTSWriteRegistration(t, dataDir, registration)
	if _, err := newLocalTTSBackendAt(dataDir, editableRoot, executableDir).Info(context.Background(), localTTSFixtureFishID()); err == nil || !strings.Contains(err.Error(), "runtime_overlaps_private_data") {
		t.Fatalf("private runtime mount was accepted: %v", err)
	}
}

func localTTSFixtureFishID() string { return "fish-" + strings.Repeat("a", 48) }

func TestLocalTTSRequestLimitsAndCanonicalPrompt(t *testing.T) {
	valid := localTTSRequest{VoiceID: "voice-1", Text: strings.Repeat("a", localTTSTextLimit)}
	if err := localTTSValidateRequest(valid); err != nil {
		t.Fatalf("exact text limit: %v", err)
	}
	valid.Text += "a"
	if err := localTTSValidateRequest(valid); err == nil {
		t.Fatal("over-limit request was accepted")
	}
	path := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf  测试\r\n提示词\r内容 \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prompt, err := localTTSCanonicalPrompt(path)
	if err != nil || prompt != "测试\n提示词\n内容" {
		t.Fatalf("canonical prompt = %q, %v", prompt, err)
	}
}

// TestLocalTTSRealWindowsLPACSyntheticRunner verifies the actual offline-speech
// broker route with only a local Python one-liner. It deliberately never loads
// GPT-SoVITS weights, opens a network connection, or uses a production voice.
// Opt in with WECHAT_WINDOWS_SPEECH_TEST_RUNTIME=<desktop runtime directory>.
func TestLocalTTSRealWindowsLPACSyntheticRunner(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows LPAC")
	}
	runtimeRoot := os.Getenv("WECHAT_WINDOWS_SPEECH_TEST_RUNTIME")
	if runtimeRoot == "" {
		t.Skip("set WECHAT_WINDOWS_SPEECH_TEST_RUNTIME to a packaged Windows runtime")
	}
	pythonRoot := filepath.Join(runtimeRoot, "windows-python-cli")
	python, pythonw := filepath.Join(pythonRoot, "python.exe"), filepath.Join(pythonRoot, "pythonw.exe")
	for _, path := range []string{python, pythonw} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Skipf("packaged speech LPAC runtime unavailable at %s", path)
		}
	}
	work := t.TempDir()
	// Use the updated source broker only in this throwaway fixture. The shipped
	// desktop runtime is not changed by an isolation test.
	brokerDir := filepath.Join(work, "runtimes", "windows-agent-host")
	if err := os.MkdirAll(brokerDir, 0700); err != nil {
		t.Fatal(err)
	}
	scripts, err := filepath.Abs(filepath.Join("..", "..", "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"windows_agent_host.pyw", "windows_lpac.py"} {
		data, readErr := os.ReadFile(filepath.Join(scripts, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(brokerDir, name), data, 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	helper := filepath.Join(brokerDir, "windows_agent_host.pyw")
	config := agenthost.Config{Mode: "full", Roots: []string{work}, TimeoutSeconds: 20, OutputBytes: 4096,
		Isolation: &agenthost.Isolation{ReadOnly: []string{pythonRoot}}, Windows: &agenthost.WindowsConfig{Pythonw: pythonw, Helper: helper, Spool: filepath.Join(work, "spool")}}
	output, diagnostic, err := agenthost.RunSpeechIsolated(context.Background(), config, work,
		[]string{python, "-X", "utf8", "-c", "import pathlib; pathlib.Path('speech-marker.txt').write_text('ok', encoding='utf8'); print('speech-ok')"},
		map[string]string{"WECHAT_WINDOWS_SPEECH_RUNTIME": "1"})
	if err != nil {
		t.Fatalf("offline speech LPAC synthetic worker: %v (diagnostic=%s, broker=%s)", err, diagnostic, localTTSLPACDetails(filepath.Join(work, "spool")))
	}
	if !strings.Contains(string(output), "speech-ok") {
		t.Fatalf("speech LPAC output missing marker: %q", output)
	}
	if data, err := os.ReadFile(filepath.Join(work, "speech-marker.txt")); err != nil || string(data) != "ok" {
		t.Fatalf("speech LPAC did not write inside its only root: %q, %v", data, err)
	}
}

// TestLocalTTSSynthesizeThroughRealWindowsLPAC uses an owner-registered,
// standard-library runner which produces a >64KiB WAV. It verifies the real
// backend's argv, binding receipt and WAV file limit without model weights.
func TestLocalTTSSynthesizeThroughRealWindowsLPAC(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows LPAC")
	}
	runtimeRoot := os.Getenv("WECHAT_WINDOWS_SPEECH_TEST_RUNTIME")
	if runtimeRoot == "" {
		t.Skip("set WECHAT_WINDOWS_SPEECH_TEST_RUNTIME to a packaged Windows runtime")
	}
	dataDir, editableRoot, executableDir, registration := localTTSRegistrationFixture(t)
	pythonSource := filepath.Join(runtimeRoot, "windows-python-cli")
	if _, err := os.Stat(filepath.Join(pythonSource, "python.exe")); err != nil {
		t.Skipf("packaged Python runtime unavailable: %v", err)
	}
	pythonTarget := filepath.Join(executableDir, "runtimes", "windows-python-cli")
	if err := os.RemoveAll(pythonTarget); err != nil {
		t.Fatal(err)
	}
	if err := localTTSCopyTree(pythonSource, pythonTarget); err != nil {
		t.Fatal(err)
	}
	scripts, err := filepath.Abs(filepath.Join("..", "..", "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	broker := filepath.Join(executableDir, "runtimes", "windows-agent-host")
	if err := os.RemoveAll(broker); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(broker, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"windows_agent_host.pyw", "windows_lpac.py"} {
		contents, readErr := os.ReadFile(filepath.Join(scripts, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(broker, name), contents, 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	runner := filepath.Join(filepath.Dir(registration.Runner), "synthetic_runner.py")
	if err := os.WriteFile(runner, []byte(localTTSSyntheticRunner), 0600); err != nil {
		t.Fatal(err)
	}
	registration.Python, registration.Runner = filepath.Join(pythonTarget, "python.exe"), runner
	registration.RunnerSHA256 = localTTSFileSHA(runner)
	localTTSWriteRegistration(t, dataDir, registration)
	backend := newLocalTTSBackendAt(dataDir, editableRoot, executableDir)
	fish := localTTSFixtureFishID()
	info, err := backend.Info(context.Background(), fish)
	if err != nil {
		t.Fatalf("speech deployment: %v", err)
	}
	request := localTTSRequest{VoiceID: registration.Voices[0].ID, Text: "离线合成夹具。"}
	result, err := backend.Synthesize(context.Background(), fish, info.Binding, request)
	if err != nil {
		t.Fatalf("real LPAC synthetic synthesis: %v (broker=%s)", err, localTTSLPACDetails(filepath.Join(dataDir, "station-speech-spool")))
	}
	if result.VoiceID != request.VoiceID || result.Version != info.Voices[0].Version || result.Speed != 1 || len(result.WAV) <= 64<<10 {
		t.Fatalf("unexpected synthesis result: %#v bytes=%d", result, len(result.WAV))
	}
	rate, seconds, err := mediaspec.PCM16WAVInfo(result.WAV)
	if err != nil || rate != 16000 || seconds != 3 {
		t.Fatalf("synthetic WAV: rate=%d seconds=%v err=%v", rate, seconds, err)
	}
	if files, err := os.ReadDir(filepath.Join(dataDir, "station-speech-spool")); err != nil || len(files) != 0 {
		t.Fatalf("completed synthesis retained plaintext broker spool: %d files, %v", len(files), err)
	}
}

func localTTSCopyTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(target, 0700)
		}
		dest := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("synthetic_lpac_runtime_member_invalid")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0600)
	})
}

const localTTSSyntheticRunner = `import argparse, hashlib, json, pathlib, struct, wave
p = argparse.ArgumentParser()
for name in ('project','models','gpt','sovits','reference','prompt-text-file','text','text-lang','prompt-lang','device','version','parameters','output'):
    p.add_argument('--' + name, required=True)
p.add_argument('--packages')
p.add_argument('--no-half', action='store_true')
a = p.parse_args()
def digest(path): return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()
raw = pathlib.Path(a.prompt_text_file).read_text(encoding='utf-8-sig')
prompt = raw.replace('\r\n', '\n').replace('\r', '\n').strip()
out = pathlib.Path(a.output)
with wave.open(str(out), 'wb') as w:
    w.setnchannels(1); w.setsampwidth(2); w.setframerate(16000)
    w.writeframes(struct.pack('<' + 'h' * 48000, *([0] * 48000)))
print(json.dumps({'output': str(out), 'sample_rate': 16000, 'seconds': 3.0, 'sha256': digest(out), 'device': a.device, 'version': a.version, 'gpt': a.gpt, 'sovits': a.sovits, 'gpt_sha256': digest(a.gpt), 'sovits_sha256': digest(a.sovits), 'reference_sha256': digest(a.reference), 'prompt_sha256': hashlib.sha256(prompt.encode('utf8')).hexdigest(), 'text_sha256': hashlib.sha256(a.text.encode('utf8')).hexdigest(), 'wechat_sends': 0}, ensure_ascii=False))
`

func localTTSLPACDetails(spool string) string {
	var details []string
	_ = filepath.WalkDir(spool, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && (entry.Name() == "result.json" || entry.Name() == "stderr.log" || entry.Name() == "output.log") {
			if data, readErr := os.ReadFile(path); readErr == nil {
				details = append(details, entry.Name()+"="+string(data))
			}
		}
		return nil
	})
	return strings.Join(details, "; ")
}

func localTTSRegistrationFixture(t *testing.T) (dataDir, editableRoot, executableDir string, registration localTTSRegistration) {
	t.Helper()
	root := t.TempDir()
	dataDir = filepath.Join(root, "station-data")
	editableRoot = filepath.Join(root, "fish-home")
	executableDir = filepath.Join(root, "desktop")
	deployment := filepath.Join(root, "registered-deployment")
	for _, directory := range []string{dataDir, editableRoot, filepath.Join(deployment, "project"), filepath.Join(deployment, "models"), filepath.Join(deployment, "media"), filepath.Join(deployment, "nltk"), filepath.Join(deployment, "profiles"), filepath.Join(deployment, "python"), filepath.Join(executableDir, "runtimes", "windows-python-cli"), filepath.Join(executableDir, "runtimes", "windows-agent-host")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, contents string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	python := write(filepath.Join(deployment, "python", "python.exe"), "python")
	runner := write(filepath.Join(deployment, "runner.py"), "runner")
	gpt := write(filepath.Join(deployment, "profiles", "voice.ckpt"), "gpt")
	sovits := write(filepath.Join(deployment, "profiles", "voice.pth"), "sovits")
	reference := write(filepath.Join(deployment, "profiles", "voice.wav"), "reference")
	prompt := write(filepath.Join(deployment, "profiles", "prompt.txt"), "\xef\xbb\xbf  已登记的提示词 \r\n")
	write(filepath.Join(deployment, "media", "ffmpeg.exe"), "ffmpeg")
	write(filepath.Join(deployment, "media", "ffprobe.exe"), "ffprobe")
	write(filepath.Join(executableDir, "runtimes", "windows-python-cli", "pythonw.exe"), "pythonw")
	write(filepath.Join(executableDir, "runtimes", "windows-agent-host", "windows_agent_host.pyw"), "helper")
	registration = localTTSRegistration{
		SchemaVersion: 1, Enabled: true,
		Project: filepath.Join(deployment, "project"), Python: python, BaseModels: filepath.Join(deployment, "models"), Runner: runner, RunnerSHA256: localTTSFileSHA(runner), Media: filepath.Join(deployment, "media"), NLTKData: filepath.Join(deployment, "nltk"), Device: "cpu",
		Voices: []localTTSVoiceRegistration{{ID: "owner-voice", Label: "主人声线", GPT: gpt, GPTSHA256: localTTSFileSHA(gpt), SoVITS: sovits, SoVITSSHA256: localTTSFileSHA(sovits), Reference: reference, ReferenceSHA256: localTTSFileSHA(reference), Prompt: prompt, PromptSHA256: localTTSFileSHA(prompt), PromptLanguage: "zh", TextLanguage: "zh"}},
	}
	localTTSWriteRegistration(t, dataDir, registration)
	return dataDir, editableRoot, executableDir, registration
}

func localTTSWriteRegistration(t *testing.T, dataDir string, registration localTTSRegistration) {
	t.Helper()
	raw, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dataDir, localTTSRegistrationFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
