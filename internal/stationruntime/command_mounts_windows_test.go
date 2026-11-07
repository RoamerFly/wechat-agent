package stationruntime

import (
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestPackagedCommandRuntimeRootsDoNotFollowJunctions(t *testing.T) {
	pkg := t.TempDir()
	cfg := runnerFixture(t, pkg)
	runtime := filepath.Join(pkg, "runtimes")
	python, media := filepath.Join(runtime, "windows-python-cli"), filepath.Join(runtime, "windows-media")
	writeRunnerFixture(t, filepath.Join(runtime, "windows-music", "tool.exe"), []byte("not an approved runtime"))
	writeRunnerFixture(t, filepath.Join(media, "ffmpeg.exe"), []byte("approved media runtime"))
	if roots := PackagedCommandRuntimeRoots(&cfg); !slices.Equal(roots, []string{python, media}) {
		t.Fatalf("unexpected ordinary runtime roots: %v", roots)
	}

	pkg = t.TempDir()
	cfg = runnerFixture(t, pkg)
	runtime = filepath.Join(pkg, "runtimes")
	python, media = filepath.Join(runtime, "windows-python-cli"), filepath.Join(runtime, "windows-media")
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", media, t.TempDir()).CombinedOutput(); err != nil {
		t.Skipf("test junction unavailable: %v %s", err, output)
	}
	if roots := PackagedCommandRuntimeRoots(&cfg); !slices.Equal(roots, []string{python}) {
		t.Fatalf("linked runtime was included in the read-only mounts: %v", roots)
	}
}
