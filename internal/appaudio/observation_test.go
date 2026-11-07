package appaudio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/mediaspec"
)

func syntheticMedia(t *testing.T, format string, audioSeconds float64) []byte {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " unavailable")
		}
	}
	path := filepath.Join(t.TempDir(), "synthetic."+format)
	args := []string{"-v", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "color=c=blue:s=64x48:r=10:d=3"}
	if audioSeconds > 0 {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=16000:duration=%.3f", audioSeconds))
	}
	video, audio := "libx264", "aac"
	if format == "webm" {
		video, audio = "libvpx", "libopus"
	}
	args = append(args, "-c:v", video, "-threads", "1")
	if audioSeconds > 0 {
		args = append(args, "-c:a", audio)
	}
	args = append(args, path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("encode synthetic media: %v %.200s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requestFor(t *testing.T, media []byte, kind string) Request {
	t.Helper()
	return Request{Media: media, ContentType: kind, PrivateDirectory: t.TempDir(), Coverage: "合成视频 · 本机音轨", SourceID: "synthetic-video-1", SourceURL: "https://example.invalid/video/1"}
}

func allowed() error { return nil }
func textTranscriber(_ context.Context, wav []byte, check func() error) (string, error) {
	if len(wav) < 44 || string(wav[:4]) != "RIFF" {
		return "", errors.New("not decoded WAV")
	}
	if err := check(); err != nil {
		return "", err
	}
	return "  合成音轨文字。  ", nil
}
func cleanStage(t *testing.T, directory string) {
	t.Helper()
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary media left behind: %v %v", files, err)
	}
}

func TestReadAudioPaginatesActualMP4AndWebMWithStableProvenance(t *testing.T) {
	for _, format := range []string{"mp4", "webm"} {
		t.Run(format, func(t *testing.T) {
			in := requestFor(t, syntheticMedia(t, format, 3), "video/"+format)
			in.Limit = 1
			var previous float64
			var hash string
			for pageNumber := 0; pageNumber < 8; pageNumber++ {
				page, err := Read(t.Context(), in, allowed, textTranscriber)
				if err != nil {
					t.Fatal(err)
				}
				proof := page.AudioProof
				if proof == nil || proof.RequestedSeconds != 1 || proof.StartSeconds != previous {
					t.Fatalf("page %d: %+v", pageNumber, page)
				}
				if hash != "" && hash != proof.MediaSHA256 {
					t.Fatal("source changed while paging")
				}
				hash = proof.MediaSHA256
				if proof.State == "no_audio_in_range" {
					// Opus/WebM may advertise a final codec-padding fraction with
					// no actual PCM. Do not invent speech to fill that tail.
					if !page.Complete || proof.DecodedSeconds != 0 || len(page.Chunks) != 0 || pageNumber < 2 {
						t.Fatal("unexpected empty range", page)
					}
				} else {
					if proof.State != "transcribed" || proof.DecodedSeconds <= 0 || len(page.Chunks) != 1 {
						t.Fatal("audio missing from nonempty range", page)
					}
					chunk := page.Chunks[0]
					if chunk.Text != "合成音轨文字。" || chunk.Position != pageNumber || chunk.SourceID != in.SourceID || chunk.SourceURL != in.SourceURL || page.Coverage != in.Coverage {
						t.Fatal("lost text or provenance", page)
					}
				}
				if proof.WindowEndSeconds > proof.StartSeconds+1 || proof.DecodedSeconds > proof.WindowEndSeconds-proof.StartSeconds+.001 {
					t.Fatal("requested interval confused with actual decode")
				}
				cleanStage(t, in.PrivateDirectory)
				if page.Complete {
					if page.NextCursor != "" || proof.WindowEndSeconds != proof.MediaDurationSeconds || pageNumber < 2 {
						t.Fatal("invalid completion", page)
					}
					return
				}
				if page.NextCursor == "" {
					t.Fatal("missing continuation")
				}
				previous = proof.WindowEndSeconds
				in.Cursor = page.NextCursor
			}
			t.Fatal("audio pages did not reach actual end")
		})
	}
}

func TestReadAudioSeparatesAbsentTrackEmptyRangeAndNoSpeech(t *testing.T) {
	t.Run("no audio track", func(t *testing.T) {
		in := requestFor(t, syntheticMedia(t, "mp4", 0), "video/mp4")
		in.Limit = 1
		calls := 0
		page, err := Read(t.Context(), in, allowed, func(context.Context, []byte, func() error) (string, error) { calls++; return "unexpected", nil })
		if err != nil || page.AudioProof == nil || page.AudioProof.State != "no_audio_track" || !page.Complete || page.AudioProof.DecodedSeconds != 0 || len(page.Chunks) != 0 || calls != 0 {
			t.Fatalf("no audio outcome: %+v %v calls=%d", page, err, calls)
		}
		cleanStage(t, in.PrivateDirectory)
	})
	t.Run("no audio in late range", func(t *testing.T) {
		in := requestFor(t, syntheticMedia(t, "mp4", 1), "video/mp4")
		in.Limit = 2
		page, err := Read(t.Context(), in, allowed, textTranscriber)
		if err != nil || page.Complete || page.NextCursor == "" {
			t.Fatalf("initial gapped audio: %+v %v", page, err)
		}
		in.Cursor = page.NextCursor
		calls := 0
		page, err = Read(t.Context(), in, allowed, func(context.Context, []byte, func() error) (string, error) { calls++; return "unexpected", nil })
		if err != nil || page.AudioProof == nil || page.AudioProof.State != "no_audio_in_range" || page.AudioProof.DecodedSeconds != 0 || !page.Complete || calls != 0 {
			t.Fatalf("empty late range: %+v %v calls=%d", page, err, calls)
		}
		cleanStage(t, in.PrivateDirectory)
	})
	t.Run("recognizer reports no words", func(t *testing.T) {
		in := requestFor(t, syntheticMedia(t, "mp4", 3), "video/mp4")
		page, err := Read(t.Context(), in, allowed, func(context.Context, []byte, func() error) (string, error) {
			return "", &mediaspec.Failure{Code: "ASR_RESPONSE_HAVE_NO_WORDS"}
		})
		if err != nil || page.AudioProof == nil || page.AudioProof.State != "no_recognizable_speech" || page.AudioProof.DecodedSeconds <= 0 || len(page.Chunks) != 0 || !page.Complete {
			t.Fatalf("no speech outcome: %+v %v", page, err)
		}
		cleanStage(t, in.PrivateDirectory)
	})
}

func TestReadAudioErrorsDoNotBecomeNoSpeechAndCleanPrivateFiles(t *testing.T) {
	media := syntheticMedia(t, "mp4", 3)
	for _, transcription := range []struct {
		name, text string
		err        error
	}{
		{"service failure", "", errors.New("synthetic-private-service-address")},
		{"empty success", "  ", nil},
	} {
		t.Run(transcription.name, func(t *testing.T) {
			in := requestFor(t, media, "video/mp4")
			page, err := Read(t.Context(), in, allowed, func(context.Context, []byte, func() error) (string, error) {
				return transcription.text, transcription.err
			})
			if !errors.Is(err, apphost.ErrObservationAudioTranscription) || page.AudioProof != nil || strings.Contains(err.Error(), "private-service") {
				t.Fatalf("transcription failure: %+v %v", page, err)
			}
			cleanStage(t, in.PrivateDirectory)
		})
	}
	in := requestFor(t, []byte("synthetic invalid media, no private file names"), "video/mp4")
	page, err := Read(t.Context(), in, allowed, textTranscriber)
	if !errors.Is(err, apphost.ErrObservationAudioDecode) || page.AudioProof != nil {
		t.Fatalf("decode failure: %+v %v", page, err)
	}
	cleanStage(t, in.PrivateDirectory)
}

func TestReadAudioRejectsChangedSourceAndInvalidLimits(t *testing.T) {
	media := syntheticMedia(t, "mp4", 3)
	in := requestFor(t, media, "video/mp4; codecs=avc1")
	in.Limit = 1
	first, err := Read(t.Context(), in, allowed, textTranscriber)
	if err != nil || first.NextCursor == "" {
		t.Fatal(first, err)
	}
	in.Cursor = first.NextCursor
	in.Media = append(append([]byte{}, media...), 0)
	if page, err := Read(t.Context(), in, allowed, textTranscriber); !errors.Is(err, apphost.ErrObservationAudioSourceChanged) || page.AudioProof != nil {
		t.Fatalf("source mismatch: %+v %v", page, err)
	}
	in.Media = media
	in.Cursor = ""
	for _, limit := range []int{-1, MaxSeconds + 1} {
		in.Limit = limit
		if _, err := Read(t.Context(), in, allowed, textTranscriber); !errors.Is(err, apphost.ErrObservationAudioLimit) {
			t.Fatal("silently clamped requested window", limit, err)
		}
	}
	for _, limit := range []int{0, MaxSeconds} {
		in.Limit = limit
		page, err := Read(t.Context(), in, allowed, textTranscriber)
		want := limit
		if want == 0 {
			want = DefaultSeconds
		}
		if err != nil || page.AudioProof.RequestedSeconds != want || !page.Complete || page.AudioProof.WindowEndSeconds > 3.1 {
			t.Fatalf("effective window: %+v %v", page, err)
		}
	}
	in.Media = make([]byte, MaxMediaBytes+1)
	if _, err := Read(t.Context(), in, allowed, textTranscriber); !errors.Is(err, apphost.ErrObservationPlaybackSize) {
		t.Fatal("media boundary lost", err)
	}
	in.Media = media
	in.ContentType = "application/x-mpegURL"
	if _, err := Read(t.Context(), in, allowed, textTranscriber); !errors.Is(err, apphost.ErrObservationAudioMediaUnavailable) {
		t.Fatal("playlist accepted", err)
	}
	in.ContentType = "video/mp4"
	in.PrivateDirectory = "relative-directory"
	if _, err := Read(t.Context(), in, allowed, textTranscriber); !errors.Is(err, apphost.ErrObservationWebStorageUnavailable) {
		t.Fatal("relative staging path accepted", err)
	}
}

func TestReadAudioRevocationAndCancellationDiscardCompletedStage(t *testing.T) {
	media := syntheticMedia(t, "mp4", 3)
	for phase := 1; phase <= 7; phase++ {
		t.Run(fmt.Sprintf("authorization-check-%d", phase), func(t *testing.T) {
			in := requestFor(t, media, "video/mp4")
			checks := 0
			page, err := Read(t.Context(), in, func() error {
				checks++
				if checks >= phase {
					return apphost.ErrObservationForbidden
				}
				return nil
			}, func(context.Context, []byte, func() error) (string, error) { return "must not escape", nil })
			if !errors.Is(err, apphost.ErrObservationForbidden) || page.AudioProof != nil || len(page.Chunks) != 0 {
				t.Fatalf("revoked evidence escaped: %+v %v checks=%d", page, err, checks)
			}
			cleanStage(t, in.PrivateDirectory)
		})
	}
	in := requestFor(t, media, "video/mp4")
	ctx, cancel := context.WithCancel(t.Context())
	page, err := Read(ctx, in, allowed, func(context.Context, []byte, func() error) (string, error) {
		cancel()
		return "late successful words", nil
	})
	if !errors.Is(err, context.Canceled) || page.AudioProof != nil {
		t.Fatalf("cancelled successful ASR escaped: %+v %v", page, err)
	}
	cleanStage(t, in.PrivateDirectory)
}

func TestAudioCursorStrictness(t *testing.T) {
	digest := sha256.Sum256([]byte("synthetic source"))
	valid := audioCursor{MediaSHA256: hex.EncodeToString(digest[:]), NextSeconds: 1, Position: 1}
	raw, _ := json.Marshal(valid)
	if _, err := parseCursor(string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"{}", string(raw) + " {}", strings.Replace(string(raw), `"position":1`, `"position":0`, 1),
		strings.Replace(string(raw), `"next_seconds":1`, `"next_seconds":0`, 1),
		strings.Replace(string(raw), `"position":1`, `"position":1,"unknown":true`, 1),
		strings.Replace(string(raw), valid.MediaSHA256, strings.ToUpper(valid.MediaSHA256), 1),
		strings.Repeat("x", 513),
	} {
		if _, err := parseCursor(bad); !errors.Is(err, apphost.ErrObservationInvalid) {
			t.Fatalf("invalid continuation accepted: %s %v", bad, err)
		}
	}
}

func TestReadAudioSupportsLongHostPrivateDirectory(t *testing.T) {
	in := requestFor(t, syntheticMedia(t, "mp4", 3), "video/mp4")
	in.PrivateDirectory = filepath.Join(in.PrivateDirectory, strings.Repeat("synthetic-host-", 7), strings.Repeat("grant-", 16))
	if err := os.MkdirAll(in.PrivateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if len(in.PrivateDirectory) < 260 {
		t.Fatal("fixture did not exercise a Windows long path")
	}
	page, err := Read(t.Context(), in, allowed, textTranscriber)
	if err != nil || page.AudioProof == nil || page.AudioProof.State != "transcribed" {
		t.Fatalf("long host path: %+v %v", page, err)
	}
	cleanStage(t, in.PrivateDirectory)
}
