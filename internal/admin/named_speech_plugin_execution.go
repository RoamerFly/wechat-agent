package admin

// The inference broker runs separately from the stdio plugin. Only this host
// code receives model paths; the plugin sees an opaque, verified audio ref.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/workspacefs"
)

type namedSpeechBrokerResult struct {
	Status string `json:"status"`
	Audio  struct {
		Ref        string  `json:"ref"`
		SHA256     string  `json:"sha256"`
		Bytes      int64   `json:"bytes"`
		SampleRate int     `json:"sample_rate"`
		Seconds    float64 `json:"seconds"`
	} `json:"audio"`
	Voice struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	} `json:"voice"`
	Device  string `json:"device"`
	Version string `json:"version"`
}

type namedSpeechCheckpoint struct {
	JobID     string                  `json:"job_id"`
	Binding   string                  `json:"binding"`
	Directory string                  `json:"directory"`
	Result    namedSpeechBrokerResult `json:"result"`
}

type namedSpeechRunnerResult = mediaspec.GPTSoVITSResult

func namedSpeechSHA(data []byte) string { return mediaspec.SpeechSHA256(data) }

func (p *namedSpeechPluginPolicy) execute(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) (namedSpeechCheckpoint, json.RawMessage, error) {
	var empty namedSpeechCheckpoint
	if j.ID == "" {
		return empty, nil, errors.New("named_speech_durable_job_required")
	}
	x := p.p.executionContext()
	manifests, _, err := readProjectManifests(ctx, x.Public, "gpt-sovits", true)
	var project struct {
		Root, Python string
		Models       string `json:"base_models"`
		Packages     string `json:"packages"`
	}
	if err != nil || len(manifests) != 1 || json.Unmarshal(manifests[0], &project) != nil {
		return empty, nil, errors.New("named_speech_registration_invalid")
	}
	packageDir, err := p.store.PackageDirectory(p.packageDigest)
	if err != nil {
		return empty, nil, err
	}
	// Entry from public registration is deliberately ignored. The installed,
	// hash-verified package owns the fixed inference implementation.
	runner := filepath.Join(packageDir, "runtime", "gpt_sovits.py")
	if err = regularMusicPath(runner, false); err != nil {
		return empty, nil, errors.New("named_speech_runner_missing")
	}
	// Account-private output uses a short stable root: conversation scope IDs
	// plus 64-character job IDs exceed Win32 icacls limits before Python starts.
	outputRoot := filepath.Join(x.Private, "speech-jobs")
	folder := filepath.Join(outputRoot, j.ID)
	if !pathWithin(outputRoot, folder) {
		return empty, nil, errors.New("named_speech_job_path_invalid")
	}
	if err = ensureManaged(filepath.Dir(folder)); err != nil {
		return empty, nil, err
	}
	// Refuse pre-existing outputs; only Controller's original completed private
	// checkpoint can resume one. No uncertain job gets a fresh execution folder.
	if err = os.Mkdir(folder, 0700); err != nil {
		return empty, nil, errors.New("named_speech_job_output_already_exists")
	}
	paths := map[string]string{}
	selectedPaths := []string{}
	var prompt []byte
	for _, item := range []struct {
		name string
		file voiceFile
	}{{"gpt", p.resource.GPT}, {"sovits", p.resource.SoVITS}, {"reference", p.resource.Reference}, {"prompt", p.resource.Prompt}} {
		path, err := p.p.namedSpeechFilePath(p.resource.Voice, item.file)
		if err != nil {
			return empty, nil, err
		}
		if err = regularMusicPath(path, false); err != nil {
			return empty, nil, errors.New("named_speech_resource_path_invalid")
		}
		paths[item.name] = path
		selectedPaths = append(selectedPaths, path)
		if item.name == "prompt" {
			prompt, err = p.readPrompt(item.file)
			if err != nil {
				return empty, nil, err
			}
		}
	}
	if !utf8ValidPrompt(prompt) {
		return empty, nil, errors.New("named_speech_prompt_invalid")
	}
	dest := filepath.Join(folder, "speech.wav")
	argv := []string{project.Python, runner, "--project", project.Root, "--models", project.Models, "--gpt", paths["gpt"], "--sovits", paths["sovits"], "--reference", paths["reference"], "--prompt-text-file", paths["prompt"], "--text", p.request.Text, "--device", p.request.Device, "--version", p.request.Version, "--output", dest}
	if project.Packages != "" {
		argv = append(argv, "--packages", project.Packages)
	}
	if p.request.Device == "cpu" {
		argv = append(argv, "--no-half")
	}
	if len(p.request.Parameters) > 0 {
		raw, err := json.Marshal(p.request.Parameters)
		if err != nil {
			return empty, nil, err
		}
		path := filepath.Join(folder, "parameters.json")
		if err = os.WriteFile(path, raw, 0600); err != nil {
			return empty, nil, err
		}
		argv = append(argv, "--parameters", path)
	}
	cfg, err := p.p.hostConfig()
	if err != nil {
		return empty, nil, err
	}
	cfg.Roots = []string{folder}
	cfg.Isolation.GPU = strings.HasPrefix(p.request.Device, "cuda")
	// Individual selected files are read-only mounts. The inference process
	// receives no grant to browse the account's complete voice library, and
	// completed jobs retain no duplicate copies of large model checkpoints.
	cfg.Isolation.ReadOnly = append([]string{x.Public, packageDir}, selectedPaths...)
	if runtime.GOOS == "windows" {
		base := filepath.Join(x.Private, "scratch")
		if err = ensureManaged(base); err != nil {
			return empty, nil, err
		}
		scratch, err := os.MkdirTemp(base, "voice-")
		if err != nil {
			return empty, nil, err
		}
		defer os.RemoveAll(scratch)
		cfg.Roots = []string{scratch, folder}
	}
	// The caller's floating-point timeout is the actual inference deadline.
	// Host Config.TimeoutSeconds is only generic command-tool configuration.
	bounded, cancel := context.WithTimeout(ctx, time.Duration(p.request.Timeout*float64(time.Second)))
	defer cancel()
	wrapper := "import contextlib,runpy,sys\nlog=sys.argv.pop(1)\nsys.argv=sys.argv[1:]\nwith open(log,'a',encoding='utf-8') as f, contextlib.redirect_stderr(f):\n runpy.run_path(sys.argv[0],run_name='__main__')\n"
	argv = append([]string{project.Python, "-u", "-c", wrapper, filepath.Join(folder, "stderr.log")}, argv[1:]...)
	if err = p.Check(ctx, j, cap); err != nil {
		return empty, nil, err
	}
	run := p.run
	if run == nil {
		run = func(ctx context.Context, cfg agenthost.Config, cwd string, argv []string) ([]byte, []byte, error) {
			if runtime.GOOS == "windows" {
				return p.p.manager.runWindowsNamedModel(ctx, cfg, cwd, argv)
			}
			raw, err := agenthost.RunIsolated(ctx, cfg, cwd, argv)
			return raw, nil, err
		}
	}
	log, diagnostic, runErr := run(bounded, cfg, folder, argv)
	if err = os.WriteFile(filepath.Join(folder, "execution.log"), log, 0600); err != nil {
		return empty, nil, err
	}
	if len(diagnostic) > 0 {
		if err = os.WriteFile(filepath.Join(folder, "broker-stderr.log"), diagnostic, 0600); err != nil {
			return empty, nil, err
		}
	}
	if runErr != nil {
		if ctx.Err() != nil {
			return empty, nil, ctx.Err()
		}
		return empty, nil, errors.New("named_speech_inference_needs_review")
	}
	if err = bounded.Err(); err != nil {
		return empty, nil, err
	}
	if err = p.Check(ctx, j, cap); err != nil {
		return empty, nil, err
	}
	var reported namedSpeechRunnerResult
	lines := bytes.Split(bytes.TrimSpace(log), []byte("\n"))
	if len(lines) == 0 || strictServicePluginJSON(lines[len(lines)-1], &reported) != nil {
		return empty, nil, errors.New("named_speech_runner_result_invalid")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(lines[len(lines)-1], &fields) != nil || len(fields) != 14 || string(fields["wechat_sends"]) != "0" {
		return empty, nil, errors.New("named_speech_runner_result_invalid")
	}
	canonicalPrompt := strings.TrimSpace(strings.TrimPrefix(string(prompt), "\ufeff"))
	if reported.Output != dest || reported.GPT != paths["gpt"] || reported.SoVITS != paths["sovits"] || reported.Device != p.request.Device || reported.Version != p.request.Version || reported.GPTSHA256 != p.resource.GPT.Hash || reported.SoVITSSHA256 != p.resource.SoVITS.Hash || reported.ReferenceSHA256 != p.resource.Reference.Hash || reported.PromptSHA256 != namedSpeechSHA([]byte(canonicalPrompt)) || reported.TextSHA256 != namedSpeechSHA([]byte(p.request.Text)) || reported.WeChatSends != 0 {
		return empty, nil, errors.New("named_speech_runner_binding_mismatch")
	}
	result := namedSpeechBrokerResult{Status: "synthesized", Device: p.request.Device, Version: p.request.Version}
	result.Voice.ID = p.resource.Voice.ID
	result.Voice.Version = p.resource.Version
	result.Audio.Ref = "audio_0"
	result.Audio.SHA256 = reported.SHA256
	result.Audio.SampleRate = reported.SampleRate
	result.Audio.Seconds = reported.Seconds
	data, err := namedSpeechReadWAV(folder)
	if err != nil {
		return empty, nil, err
	}
	result.Audio.Bytes = int64(len(data))
	checkpoint := namedSpeechCheckpoint{JobID: j.ID, Binding: p.binding(), Directory: folder, Result: result}
	if _, err = p.readCheckpoint(ctx, checkpoint); err != nil {
		return empty, nil, err
	}
	reply, _ := json.Marshal(result)
	return checkpoint, reply, nil
}

func utf8ValidPrompt(data []byte) bool {
	return utf8.Valid(data) && strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff")) != "" && !bytes.ContainsRune(data, 0)
}

func (p *namedSpeechPluginPolicy) readPrompt(item voiceFile) ([]byte, error) {
	base := p.p.manager.privateVoiceLibrary()
	if p.resource.Voice.Public {
		base = filepath.Join(p.p.executionContext().Public, "models", "voice-library")
	}
	path, err := p.p.namedSpeechFilePath(p.resource.Voice, item)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return nil, err
	}
	root, err := workspacefs.EnsurePath(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	source, err := workspacefs.OpenRegular(root, rel)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	if item.Bytes > toolsLimit("file") {
		return nil, errors.New("named_speech_prompt_capacity_exceeded")
	}
	data, err := io.ReadAll(io.LimitReader(source, item.Bytes+1))
	if err != nil || int64(len(data)) != item.Bytes || namedSpeechSHA(data) != item.Hash {
		return nil, errors.New("named_speech_input_changed")
	}
	return data, nil
}

func namedSpeechReadWAV(folder string) ([]byte, error) {
	if regularMusicPath(folder, true) != nil {
		return nil, errors.New("named_speech_audio_missing")
	}
	root, err := workspacefs.EnsurePath(folder)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := workspacefs.OpenRegular(root, "speech.wav")
	if err != nil {
		return nil, errors.New("named_speech_audio_missing")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, toolsLimit("file")+1))
	if err != nil || int64(len(data)) > toolsLimit("file") {
		return nil, errors.New("named_speech_audio_capacity_exceeded")
	}
	return data, nil
}

func (p *namedSpeechPluginPolicy) readCheckpoint(ctx context.Context, c namedSpeechCheckpoint) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outputRoot := filepath.Join(p.p.executionContext().Private, "speech-jobs")
	expected := filepath.Join(outputRoot, c.JobID)
	r := c.Result
	if c.JobID == "" || c.Binding != p.binding() || c.Directory != expected || !pathWithin(outputRoot, expected) || r.Status != "synthesized" || r.Voice.ID != p.resource.Voice.ID || r.Voice.Version != p.resource.Version || r.Device != p.request.Device || r.Version != p.request.Version || r.Audio.Ref != "audio_0" {
		return nil, errors.New("named_speech_checkpoint_invalid")
	}
	data, err := namedSpeechReadWAV(c.Directory)
	if err != nil {
		return nil, err
	}
	rate, seconds, err := namedSpeechWAVInfo(data)
	if err != nil || r.Audio.SHA256 != namedSpeechSHA(data) || r.Audio.Bytes != int64(len(data)) || r.Audio.SampleRate != rate || math.IsNaN(r.Audio.Seconds) || math.IsInf(r.Audio.Seconds, 0) || r.Audio.Seconds <= 0 || math.Abs(r.Audio.Seconds-seconds) > 1e-8 {
		return nil, errors.New("named_speech_audio_unverified")
	}
	return data, nil
}

func namedSpeechWAVInfo(data []byte) (int, float64, error) {
	return mediaspec.PCM16WAVInfo(data)
}
