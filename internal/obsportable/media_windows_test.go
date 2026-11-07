//go:build windows

package obsportable

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type trackedMediaSource struct {
	io.Reader
	closes   int
	closeErr error
}

func (s *trackedMediaSource) Close() error {
	s.closes++
	return s.closeErr
}

func syntheticMedia(contents string) (Media, *trackedMediaSource) {
	source := &trackedMediaSource{Reader: strings.NewReader(contents)}
	sum := sha256.Sum256([]byte(contents))
	return Media{Source: source, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(contents)), Width: 1920, Height: 1080}, source
}

func TestMediaReservationAlwaysOwnsSource(t *testing.T) {
	station, workspace, studio := syntheticInstallation(t)
	install, err := Open(t.Context(), station, workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"hash", "size", "width", "height", "source"} {
		t.Run(invalid, func(t *testing.T) {
			media, source := syntheticMedia("verified host stream")
			switch invalid {
			case "hash":
				media.SHA256 = "bad"
			case "size":
				media.Bytes = 0
			case "width":
				media.Width = 0
			case "height":
				media.Height = -1
			case "source":
				media.Source = nil
			}
			if _, err := install.EncoderWithMedia(t.Context(), &media); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid metadata accepted: %v", err)
			}
			if invalid != "source" && source.closes != 1 {
				t.Fatalf("failed reservation leaked source: %d", source.closes)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	media, source := syntheticMedia("verified host stream")
	if _, err := install.EncoderWithMedia(ctx, &media); !errors.Is(err, context.Canceled) || source.closes != 1 {
		t.Fatalf("canceled reservation: %v, closes %d", err, source.closes)
	}
	encoder, err := install.Encoder(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	media, source = syntheticMedia("verified host stream")
	if _, err := install.EncoderWithMedia(t.Context(), &media); !errors.Is(err, ErrBusy) || source.closes != 1 {
		t.Fatalf("busy reservation: %v, closes %d", err, source.closes)
	}
	if err := encoder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(studio, filepath.FromSlash(executable)), []byte("changed runtime"), 0600); err != nil {
		t.Fatal(err)
	}
	media, source = syntheticMedia("verified host stream")
	if _, err := install.EncoderWithMedia(t.Context(), &media); !errors.Is(err, ErrInvalid) || source.closes != 1 {
		t.Fatalf("changed runtime reservation: %v, closes %d", err, source.closes)
	}
}

func TestMediaCloseFailureIsNotSuccessfulStop(t *testing.T) {
	station, workspace, _ := syntheticInstallation(t)
	install, err := Open(t.Context(), station, workspace)
	if err != nil {
		t.Fatal(err)
	}
	media, source := syntheticMedia("verified host stream")
	closeErr := errors.New("host snapshot cleanup failed")
	source.closeErr = closeErr
	encoder, err := install.EncoderWithMedia(t.Context(), &media)
	if err != nil {
		t.Fatal(err)
	}
	// Take a value copy: caller metadata changes cannot replace the owned stream.
	media.Source = nil
	for range 2 {
		if err := encoder.Stop(t.Context()); !errors.Is(err, closeErr) {
			t.Fatalf("false cleanup success: %v", err)
		}
	}
	if source.closes != 1 {
		t.Fatalf("source closed %d times", source.closes)
	}
	next, err := install.Encoder(t.Context())
	if err != nil {
		t.Fatalf("source close error leaked reservation: %v", err)
	}
	if err := next.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	media, source = syntheticMedia("verified host stream")
	media.SHA256 = "invalid"
	source.closeErr = closeErr
	if _, err := install.EncoderWithMedia(t.Context(), &media); !errors.Is(err, closeErr) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("factory hid close error: %v", err)
	}
}

func TestMediaProfileCopiesPrivateSnapshotAndKeepsRuntimeValid(t *testing.T) {
	for _, loop := range []bool{false, true} {
		t.Run(map[bool]string{false: "once", true: "loop"}[loop], func(t *testing.T) {
			station, workspace, studio := syntheticInstallation(t)
			install, err := Open(t.Context(), station, workspace)
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(studio)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := root.MkdirAll(filepath.FromSlash("config/obs-studio"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := root.WriteFile(filepath.FromSlash(programMedia), []byte("stale private program"), 0600); err != nil {
				t.Fatal(err)
			}
			media, source := syntheticMedia("synthetic verified host bytes; decoder probing belongs to host")
			media.Loop = loop
			owned := &ownedMedia{Media: media}
			if err := writeProfileWithMedia(t.Context(), root, studio, 32500, strings.Repeat("a", 48), owned); err != nil {
				t.Fatal(err)
			}
			copied, err := root.ReadFile(filepath.FromSlash(programMedia))
			if err != nil || string(copied) != "synthetic verified host bytes; decoder probing belongs to host" || source.closes != 1 {
				t.Fatalf("snapshot: %q, %v, closes %d", copied, err, source.closes)
			}
			if err := install.verify(t.Context(), root); err != nil {
				t.Fatalf("private media invalidated inventory: %v", err)
			}
			if _, err := Open(t.Context(), station, workspace); err != nil {
				t.Fatalf("cannot reopen after media copy: %v", err)
			}
			raw, err := root.ReadFile(filepath.FromSlash("config/obs-studio/basic/scenes/Station.json"))
			if err != nil {
				t.Fatal(err)
			}
			var collection struct {
				Sources []struct {
					ID       string         `json:"id"`
					Muted    bool           `json:"muted"`
					Volume   float64        `json:"volume"`
					Mixers   int            `json:"mixers"`
					Settings map[string]any `json:"settings"`
				} `json:"sources"`
			}
			if err := json.Unmarshal(raw, &collection); err != nil {
				t.Fatal(err)
			}
			if len(collection.Sources) != 3 || collection.Sources[0].ID != "color_source" || collection.Sources[1].ID != "ffmpeg_source" || collection.Sources[2].ID != "scene" {
				t.Fatalf("unexpected source inventory: %s", raw)
			}
			s := collection.Sources[1]
			if s.Muted || s.Volume != 1 || s.Mixers != 1 || s.Settings["is_local_file"] != true || s.Settings["looping"] != loop || s.Settings["local_file"] != filepath.Join(studio, filepath.FromSlash(programMedia)) || s.Settings["ffmpeg_options"] != "protocol_whitelist=file,pipe format_whitelist=mov" {
				t.Fatalf("unsafe/inaudible media source: %+v", s)
			}
			if _, exists := s.Settings["input"]; exists {
				t.Fatal("network input configured")
			}
		})
	}
}

func TestMediaSceneFitsDisplayAspectWithoutCropping(t *testing.T) {
	for _, size := range [][2]int{{1920, 1080}, {1080, 1920}, {2000, 500}} {
		media, _ := syntheticMedia("host stream")
		media.Width, media.Height = size[0], size[1]
		raw, err := mediaSceneCollection(`E:\private\config\obs-studio\program.mp4`, &ownedMedia{Media: media})
		if err != nil {
			t.Fatal(err)
		}
		var collection map[string]any
		if err := json.Unmarshal(raw, &collection); err != nil {
			t.Fatal(err)
		}
		sources := collection["sources"].([]any)
		items := sources[2].(map[string]any)["settings"].(map[string]any)["items"].([]any)
		item := items[1].(map[string]any)
		pos, scale := item["pos"].(map[string]any), item["scale"].(map[string]any)
		sx, sy := scale["x"].(float64), scale["y"].(float64)
		w, h := float64(size[0])*sx, float64(size[1])*sy
		if sx != sy || w > 1280.00001 || h > 720.00001 || math.Abs(2*pos["x"].(float64)+w-1280) > 0.00001 || math.Abs(2*pos["y"].(float64)+h-720) > 0.00001 || item["align"] != float64(5) {
			t.Fatalf("aspect/centering for %v: %s", size, raw)
		}
	}
}

type mediaErrorReader struct{ err error }

func (s mediaErrorReader) Read([]byte) (int, error) { return 0, s.err }

type blockingMediaSource struct {
	io.ReadCloser
	reading chan struct{}
}

func (s *blockingMediaSource) Read(p []byte) (int, error) {
	close(s.reading)
	return s.ReadCloser.Read(p)
}

func TestMediaCopyFailuresPreventProcessAndRemovePartialFiles(t *testing.T) {
	for _, failure := range []string{"hash", "short", "long", "read", "close", "canceled", "relay"} {
		t.Run(failure, func(t *testing.T) {
			station, workspace, studio := syntheticInstallation(t)
			install, err := Open(t.Context(), station, workspace)
			if err != nil {
				t.Fatal(err)
			}
			media, source := syntheticMedia("expected host bytes")
			marker := errors.New("synthetic stream failure")
			switch failure {
			case "hash":
				media.SHA256 = strings.Repeat("0", 64)
			case "short":
				media.Bytes++
			case "long":
				media.Bytes--
				sum := sha256.Sum256([]byte("expected host byte"))
				media.SHA256 = hex.EncodeToString(sum[:])
			case "read":
				source.Reader = mediaErrorReader{marker}
			case "close":
				source.closeErr = marker
			}
			encoder, err := install.EncoderWithMedia(t.Context(), &media)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "canceled" {
				cancel()
			}
			port := 32500
			if failure == "relay" {
				port = 0
			}
			if err := encoder.Start(ctx, port, strings.Repeat("a", 48)); err == nil {
				t.Fatal("failure reported success")
			}
			if source.closes != 1 || encoder.process != nil {
				t.Fatalf("failed copy started process or leaked source: %d, %+v", source.closes, encoder.process)
			}
			if _, err := os.Stat(filepath.Join(studio, filepath.FromSlash(programMedia))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed copy left program: %v", err)
			}
			stopErr := encoder.Stop(t.Context())
			if failure == "close" && !errors.Is(stopErr, marker) {
				t.Fatalf("Stop hid source cleanup failure: %v", stopErr)
			}
			if failure != "close" && stopErr != nil {
				t.Fatal(stopErr)
			}
		})
	}
}

func TestMediaCancellationInterruptsBlockedRead(t *testing.T) {
	_, _, studio := syntheticInstallation(t)
	root, err := os.OpenRoot(studio)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeProfile(root, 32500, strings.Repeat("a", 48)); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	media, _ := syntheticMedia("host stream")
	source := &blockingMediaSource{ReadCloser: reader, reading: make(chan struct{})}
	media.Source = source
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- copyProgramMedia(ctx, root, &ownedMedia{Media: media}) }()
	select {
	case <-source.reading:
	case <-time.After(2 * time.Second):
		t.Fatal("copy did not enter host stream read")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("copy did not stop after cancellation")
	}
	if _, err := root.Stat(filepath.FromSlash(programMedia)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled copy retained media: %v", err)
	}
}
