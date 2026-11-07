package adapter

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaspec"
)

// These checks exercise the same media container path used by the application
// adapter.  The transcript function is deliberately local and synthetic: this
// test must not call a model service or need a provider credential.
func TestAudioObservationsDecodeBoundedPagesAndResume(t *testing.T) {
	media := makeAudioFixture(t, true)
	app := newFixture(t)
	saved := enableAudio(t, app)
	var requests []int64
	var transcriptions []int
	configure := func(app *PublicApp) {
		app.resolveMedia = func(ctx context.Context, bvid string, limit int64) (mcptools.PublicVideo, error) {
			if err := ctx.Err(); err != nil {
				return mcptools.PublicVideo{}, err
			}
			if bvid != fixtureBV || limit != maxMediaBytes {
				t.Fatalf("media request = %q / %d", bvid, limit)
			}
			requests = append(requests, limit)
			return mcptools.PublicVideo{Data: media, ContentType: "video/mp4", DurationMS: 6_000}, nil
		}
		app.SetAudioTranscriber(
			func(context.Context) (string, error) { return "synthetic-asr-v1", nil },
			func(ctx context.Context, wav []byte, check func() error) (string, error) {
				if err := check(); err != nil {
					return "", err
				}
				if len(wav) < 44 || string(wav[:4]) != "RIFF" {
					t.Fatalf("ASR did not receive a WAV page: %d bytes", len(wav))
				}
				transcriptions = append(transcriptions, len(wav))
				return "synthetic spoken fixture", nil
			},
		)
	}
	configure(app)

	dir := t.TempDir()
	registry, err := apphost.New(app.Registration())
	if err != nil {
		t.Fatal(err)
	}
	observer := apphost.NewObserver(registry, filepath.Join(dir, "checkpoints"))
	in := apphost.ObservationInput{AppID: ID, ConnectionID: connectionID, ContentID: fixtureBV, Field: apphost.ObservationAudio, Limit: 3}
	first, err := observer.Read(t.Context(), in)
	if err != nil || first.Complete || len(first.Chunks) != 1 || first.Chunks[0].Text != "synthetic spoken fixture" || first.AudioProof == nil ||
		first.AudioProof.State != "transcribed" || first.AudioProof.RequestedSeconds != 3 || first.AudioProof.StartSeconds != 0 ||
		first.AudioProof.WindowEndSeconds != 3 || first.AudioProof.DecodedSeconds <= 0 || first.AudioProof.DecodedSeconds > 3.01 || first.AudioProof.MediaDurationSeconds != 6 {
		t.Fatalf("first bounded audio page = %#v, %v", first, err)
	}
	if len(requests) != 1 || len(transcriptions) != 1 {
		t.Fatalf("first page calls media=%d ASR=%d", len(requests), len(transcriptions))
	}

	// The checkpoint is based on the actual media position. A later caller may
	// choose a different explicit window without skipping part of the source.
	in.Limit = 4
	// Both application policy and the host cursor survive a restart. Reattach
	// the non-persistent local ASR handle before continuing the same source.
	app = New(app.privateDirectory)
	configure(app)
	registry, err = apphost.New(app.Registration())
	if err != nil {
		t.Fatal(err)
	}
	observer = apphost.NewObserver(registry, filepath.Join(dir, "checkpoints"))
	second, err := observer.Read(t.Context(), in)
	if err != nil || !second.Resumed || !second.Complete || len(second.Chunks) != 1 || second.AudioProof == nil ||
		second.AudioProof.RequestedSeconds != 4 || second.AudioProof.StartSeconds != 3 || second.AudioProof.WindowEndSeconds != 6 || second.AudioProof.State != "transcribed" {
		t.Fatalf("second audio page = %#v, %v", second, err)
	}
	if len(requests) != 2 || len(transcriptions) != 2 {
		t.Fatalf("resume calls media=%d ASR=%d", len(requests), len(transcriptions))
	}
	if cached, err := observer.Read(t.Context(), in); err != nil || !cached.Cached || len(requests) != 2 {
		t.Fatalf("completed audio checkpoint = %#v, %v, calls %d", cached, err, len(requests))
	}
	in.Limit = 241
	if _, err := app.ReadObservation(t.Context(), apphost.ObservationRequest{ObservationAccess: observation(apphost.ObservationAudio, fixtureBV).ObservationAccess, Limit: in.Limit}); !errors.Is(err, apphost.ErrObservationAudioLimit) {
		t.Fatalf("audio limit = %v", err)
	}

	if _, err := app.Save(saved.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}}); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Read(t.Context(), in); !errors.Is(err, apphost.ErrObservationForbidden) {
		t.Fatalf("revoked audio cache remained available: %v", err)
	}
}

func TestAudioObservationReportsNoAudioTrackWithoutCallingASR(t *testing.T) {
	media := makeAudioFixture(t, false)
	app := newFixture(t)
	enableAudio(t, app)
	app.resolveMedia = func(context.Context, string, int64) (mcptools.PublicVideo, error) {
		return mcptools.PublicVideo{Data: media, ContentType: "video/mp4", DurationMS: 4_000}, nil
	}
	called := false
	app.SetAudioTranscriber(
		func(context.Context) (string, error) { return "synthetic-asr-v1", nil },
		func(context.Context, []byte, func() error) (string, error) {
			called = true
			return "must not run", nil
		},
	)
	page, err := app.ReadObservation(t.Context(), observation(apphost.ObservationAudio, fixtureBV))
	if err != nil || !page.Complete || len(page.Chunks) != 0 || page.AudioProof == nil || page.AudioProof.State != "no_audio_track" ||
		page.AudioProof.MediaDurationSeconds != 6 || called {
		t.Fatalf("no-audio observation = %#v, %v, ASR called=%v", page, err, called)
	}
}

func TestAudioObservationKeepsNoRecognizableSpeechAsEvidence(t *testing.T) {
	media := makeAudioFixture(t, true)
	app := newFixture(t)
	enableAudio(t, app)
	app.resolveMedia = func(context.Context, string, int64) (mcptools.PublicVideo, error) {
		return mcptools.PublicVideo{Data: media, ContentType: "video/mp4", DurationMS: 6_000}, nil
	}
	app.SetAudioTranscriber(
		func(context.Context) (string, error) { return "synthetic-asr-v1", nil },
		func(context.Context, []byte, func() error) (string, error) {
			return "", &mediaspec.Failure{Code: "ASR_RESPONSE_HAVE_NO_WORDS"}
		},
	)
	page, err := app.ReadObservation(t.Context(), observation(apphost.ObservationAudio, fixtureBV))
	if err != nil || !page.Complete || len(page.Chunks) != 0 || page.AudioProof == nil ||
		page.AudioProof.State != "no_recognizable_speech" || page.AudioProof.DecodedSeconds <= 0 {
		t.Fatalf("no-recognizable-speech observation = %#v, %v", page, err)
	}
}

func TestAudioObservationCancelsLateTranscriptionOnRevocation(t *testing.T) {
	media := makeAudioFixture(t, true)
	app := newFixture(t)
	saved := enableAudio(t, app)
	app.resolveMedia = func(context.Context, string, int64) (mcptools.PublicVideo, error) {
		return mcptools.PublicVideo{Data: media, ContentType: "video/mp4", DurationMS: 4_000}, nil
	}
	started := make(chan struct{})
	app.SetAudioTranscriber(
		func(context.Context) (string, error) { return "synthetic-asr-v1", nil },
		func(ctx context.Context, _ []byte, check func() error) (string, error) {
			if err := check(); err != nil {
				return "", err
			}
			close(started)
			<-ctx.Done()
			return "late transcript", nil
		},
	)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		_, err := app.ReadObservation(ctx, observation(apphost.ObservationAudio, fixtureBV))
		completed <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("audio ASR did not start")
	}
	if _, err := app.Save(saved.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if !errors.Is(err, apphost.ErrObservationForbidden) {
			t.Fatalf("late audio error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("audio revocation did not cancel")
	}
}

func enableAudio(t *testing.T, app *PublicApp) Settings {
	t.Helper()
	settings, err := app.View()
	if err != nil {
		t.Fatal(err)
	}
	settings, err = app.Save(settings.Revision, Settings{Enabled: true, Videos: []string{fixtureBV}, AudioEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func makeAudioFixture(t *testing.T, withAudio bool) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg is not installed: real media decode regression skipped")
	}
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	args := []string{"-v", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "color=c=blue:s=32x32:r=8:d=6"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=550:sample_rate=16000:duration=6", "-shortest", "-c:a", "aac")
	}
	args = append(args, "-c:v", "mpeg4", "-q:v", "20", "-movflags", "+faststart", path)
	cmd := exec.Command("ffmpeg", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create FFmpeg fixture: %v\n%s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("read FFmpeg fixture: %v / %d", err, len(data))
	}
	return data
}
