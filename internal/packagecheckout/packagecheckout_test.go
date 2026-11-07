package packagecheckout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyCheckedFileStagesVerifiedSourceAndHonorsCancellation(t *testing.T) {
	base := t.TempDir()
	sourcePath := filepath.Join(base, "source")
	destinationPath := filepath.Join(base, "destination")
	if err := os.MkdirAll(filepath.Join(sourcePath, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destinationPath, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("verified source")
	if err := os.WriteFile(filepath.Join(sourcePath, "runtime", "main.py"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "cancelled.py"), data, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := os.OpenRoot(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	stageName, stage, err := NewStage(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stage.Close()
		_ = destination.RemoveAll(stageName)
	}()
	sum := sha256.Sum256(data)
	var total int64
	if err = CopyCheckedFile(t.Context(), source, stage, "runtime/main.py", hex.EncodeToString(sum[:]), &total); err != nil {
		t.Fatal(err)
	}
	if total != int64(len(data)) {
		t.Fatalf("copied bytes = %d, want %d", total, len(data))
	}
	if err = CopyCheckedFile(t.Context(), source, stage, "runtime/main.py", hex.EncodeToString(sum[:]), &total); err == nil {
		t.Fatal("second copy overwrote staged file")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err = CopyCheckedFile(cancelled, source, stage, "cancelled.py", hex.EncodeToString(sum[:]), &total); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled copy error = %v, want context cancellation", err)
	}
}

func TestSafePathAndExistingDirectoryRejectUnsafeEntries(t *testing.T) {
	for _, name := range []string{"../escape", "runtime\\main.py", "runtime/../main.py", "C:main.py", ""} {
		if SafePath(name) {
			t.Fatalf("unsafe path accepted: %q", name)
		}
	}
	if !SafePath("runtime/main.py") {
		t.Fatal("clean package path rejected")
	}
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	exists, err := ExistingDirectory(root, "directory")
	if err != nil || !exists {
		t.Fatalf("existing directory = %t, %v", exists, err)
	}
	exists, err = ExistingDirectory(root, "absent")
	if err != nil || exists {
		t.Fatalf("absent directory = %t, %v", exists, err)
	}
}

func TestCopyCheckedFileHonorsExplicitBudgetWithoutChangingPackageDefault(t *testing.T) {
	sourceDir, destinationDir := t.TempDir(), t.TempDir()
	data := []byte("verified synthetic project")
	if err := os.WriteFile(filepath.Join(sourceDir, "project.txt"), data, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := os.OpenRoot(destinationDir)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	var total int64
	for _, budget := range []int64{-1, 0, int64(len(data)) - 1, math.MaxInt64} {
		if err := CopyCheckedFileWithBudget(t.Context(), source, destination, "project.txt", hash, &total, budget); err == nil {
			t.Fatal("copy exceeded explicit budget", budget)
		}
		if total != 0 {
			t.Fatal("rejected copy changed progress", total)
		}
	}
	if _, err := os.Stat(filepath.Join(destinationDir, "project.txt")); !os.IsNotExist(err) {
		t.Fatal("rejected copy left a file", err)
	}
	if err := CopyCheckedFileWithBudget(t.Context(), source, destination, "project.txt", hash, &total, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if total != int64(len(data)) {
		t.Fatal("incorrect verified progress", total)
	}
}
