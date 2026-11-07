//go:build windows

package stationbrowser

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// The fixture is a deterministic four-second VP8/WebM solid-colour clip,
// generated locally from lavfi. It contains no recording or network material
// and exercises private Edge/CDP, Blob media loading, play/pause and
// HTMLMediaElement.played with a real video decoder.
func TestSamplePlaybackMeasuresActualProgressWithPrivateEdge(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed")
	}
	reader := New(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	media, err := os.ReadFile(filepath.Join("testdata", "playback.webm"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		window time.Duration
		ended  bool
	}{{"partial", time.Second, false}, {"ended", 6 * time.Second, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var authorizations atomic.Int32
			sample, err := reader.SamplePlayback(ctx, media, "video/webm", tc.window, func() error { authorizations.Add(1); return nil })
			if err != nil {
				t.Fatalf("SamplePlayback: %v", err)
			}
			if !sample.Ready || !sample.Paused || sample.Ended != tc.ended || sample.DurationSeconds < 3.5 ||
				len(sample.Intervals) != 1 || sample.Intervals[0].StartSeconds != 0 || sample.Intervals[0].EndSeconds < 0.45 {
				t.Fatalf("unexpected measured state: %+v", sample)
			}
			if tc.ended && sample.Intervals[0].EndSeconds < sample.DurationSeconds {
				t.Fatalf("natural end lost the final played range: %+v", sample)
			}
			if authorizations.Load() < 3 {
				t.Fatalf("expected authorization before media, start, and return, got %d", authorizations.Load())
			}
		})
	}
}

func TestSamplePlaybackCancellationReturnsNoResult(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed")
	}
	reader := New(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	authorize := func() error {
		if calls.Add(1) == 2 { // just before source installation/playback
			go func() { time.Sleep(120 * time.Millisecond); cancel() }()
		}
		return nil
	}
	media, err := os.ReadFile(filepath.Join("testdata", "playback.webm"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.SamplePlayback(ctx, media, "video/webm", 3*time.Second, authorize)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected cancellation, got sample=%+v err=%v", got, err)
	}
	if got.Ready || got.Ended || got.Paused || got.DurationSeconds != 0 || len(got.Intervals) != 0 {
		t.Fatalf("cancellation returned evidence: %+v", got)
	}
}
