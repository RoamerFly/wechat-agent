package mediadecode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func trustedFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func syntheticFLAC(t *testing.T, ffmpeg string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.flac")
	command := exec.Command(ffmpeg, "-v", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=0.25", "-ac", "2", "-c:a", "flac", path)
	hideDecoderWindow(command)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fixture encode: %v %.200s", err, output)
	}
	return path
}

func TestVerifyFLACUsesFullDecodeEvidence(t *testing.T) {
	ffmpeg := trustedFFmpeg(t)
	path := syntheticFLAC(t, ffmpeg)
	evidence, err := VerifyFLAC(context.Background(), path, FLACVerificationOptions{
		FFmpegPath: ffmpeg, MaxInputBytes: 8 << 20, MaxDecodedPCMBytes: 8 << 20, MaxSampleRate: 192000, MaxChannels: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.SampleRate != 48000 || evidence.Channels != 2 || evidence.Samples != 12000 || evidence.DecodedFrames != evidence.Samples || evidence.Duration != 250*time.Millisecond || evidence.SHA256 == "" || evidence.Bytes <= 42 {
		t.Fatalf("unexpected evidence: %+v", evidence)
	}
}

func TestVerifyFLACRejectsTruncatedEvidence(t *testing.T) {
	ffmpeg := trustedFFmpeg(t)
	path := syntheticFLAC(t, ffmpeg)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(t.TempDir(), "truncated.flac")
	if err := os.WriteFile(truncated, data[:len(data)/2], 0600); err != nil {
		t.Fatal(err)
	}
	_, err = VerifyFLAC(context.Background(), truncated, FLACVerificationOptions{
		FFmpegPath: ffmpeg, MaxInputBytes: 8 << 20, MaxDecodedPCMBytes: 8 << 20, MaxSampleRate: 192000, MaxChannels: 8,
	})
	if err == nil || !(errors.Is(err, ErrFLACDecode) || errors.Is(err, ErrFLACSampleMismatch)) {
		t.Fatalf("truncated file error=%v", err)
	}
}
