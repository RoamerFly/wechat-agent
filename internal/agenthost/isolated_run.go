package agenthost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"runtime"
	"strings"
)

// RunIsolated executes a trusted broker's fixed argv in the same mandatory
// system sandbox as Agent programs. It never exposes host credentials or falls
// back to a host shell. The caller controls deadline/cancellation and mounts.
func RunIsolated(ctx context.Context, config Config, cwd string, argv []string) ([]byte, error) {
	out, _, err := runIsolated(ctx, config, cwd, argv, "", nil, false)
	return out, err
}

// RunMusicIsolated is only for a trusted named-model broker after reading the
// administrator's explicit policy. Generic Agent command tools never call it.
func RunMusicIsolated(ctx context.Context, config Config, cwd string, argv []string, policy string, environment map[string]string) ([]byte, []byte, error) {
	if runtime.GOOS != "windows" || policy != "registry-read-private-network-v1" {
		return nil, nil, errors.New("windows_music_policy_not_authorized")
	}
	return runIsolated(ctx, config, cwd, argv, policy, environment, true)
}

// RunSpeechIsolated runs the trusted offline speech runtime in the Windows
// broker.  It intentionally has no caller-supplied policy: speech synthesis
// may read the Windows registry for its local runtime, but must not receive
// any network capability.  Generic Agent command tools never call it.
func RunSpeechIsolated(ctx context.Context, config Config, cwd string, argv []string, environment map[string]string) ([]byte, []byte, error) {
	if runtime.GOOS != "windows" {
		return nil, nil, errors.New("windows_speech_policy_not_authorized")
	}
	return runIsolated(ctx, config, cwd, argv, "registry-read-offline-speech-v1", environment, true)
}

func runIsolated(ctx context.Context, config Config, cwd string, argv []string, policy string, environment map[string]string, captureDiagnostic bool) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if config.Isolation == nil || len(argv) == 0 {
		return nil, nil, errors.New("sandbox_required")
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return nil, nil, errors.New("invalid_isolated_argv")
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	if runtime.GOOS == "windows" {
		if config.Windows == nil {
			return nil, nil, errors.New("sandbox_unavailable: Windows broker is not configured")
		}
		if e := config.Validate(); e != nil {
			return nil, nil, e
		}
		var nonce [16]byte
		if _, e := rand.Read(nonce[:]); e != nil {
			return nil, nil, e
		}
		out := &limitedOutput{limit: config.OutputBytes}
		var diagnostic bytes.Buffer
		var diagnosticWriter io.Writer
		if captureDiagnostic {
			diagnosticWriter = &diagnostic
		}
		e := runWindowsPolicyRequest(ctx, config, "broker-"+hex.EncodeToString(nonce[:]), cwd, "", argv, policy, environment, out, diagnosticWriter)
		if ctx.Err() != nil {
			return nil, diagnostic.Bytes(), ctx.Err()
		}
		if e != nil {
			return out.data, diagnostic.Bytes(), errors.New("isolated_program_failed")
		}
		if out.truncated {
			return nil, diagnostic.Bytes(), errors.New("isolated_output_incomplete")
		}
		return out.data, diagnostic.Bytes(), nil
	}
	cmd, e := isolatedCommand(ctx, config, cwd, strings.Join(quoted, " "))
	if e != nil {
		return nil, nil, e
	}
	out := &limitedOutput{limit: config.OutputBytes}
	cmd.Stdout = out
	// Raw runtime diagnostics can contain local file paths; do not merge them
	// into the structured result given to the model.
	if e = cmd.Run(); e != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return out.data, nil, errors.New("isolated_program_failed")
	}
	if out.truncated {
		return nil, nil, errors.New("isolated_output_incomplete")
	}
	return out.data, nil, nil
}
