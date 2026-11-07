package mediaobserve

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"wechat-bot/internal/deepseek"
)

func TestExplicitFramePageAndBatchExceedOldDefaults(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pattern.mp4")
	if b, e := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=96x64:r=12:d=8", "-c:v", "libx264", "-threads", "1", path).CombinedOutput(); e != nil {
		t.Fatalf("fixture %v %.100s", e, b)
	}
	in := FrameOptions{Step: 1, BatchSize: 20, MaxDimension: 96, Tokens: 1200}
	calls, seen := 0, 0
	rows, next, e := InspectFrames(context.Background(), path, dir, in, 80, 96, func(_ context.Context, _ string, images []deepseek.Image, tokens int) (string, error) {
		calls++
		seen += len(images)
		if len(images) != 20 || tokens != 1200 {
			t.Fatal("explicit options overridden")
		}
		return "synthetic analysis", nil
	})
	if e != nil || len(rows) != 4 || calls != 4 || seen != 80 || next != 80 {
		t.Fatal("page or batch clipped", len(rows), calls, seen, next, e)
	}
	remaining, _ := os.ReadDir(dir)
	if len(remaining) != 1 {
		t.Fatal("decoded batches were not cleaned incrementally", len(remaining))
	}
}
