package packagecheckout

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWindowsCheckoutRejectsJunctions(t *testing.T) {
	sourcePath, outside := t.TempDir(), t.TempDir()
	data := []byte("synthetic external package file")
	if err := os.WriteFile(filepath.Join(outside, "tool.py"), data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", filepath.Join(sourcePath, "junction"), outside)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("test junction unavailable: %v %s", err, output)
	}
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if exists, err := ExistingDirectory(source, "junction"); err == nil || exists {
		t.Fatal("linked destination was accepted", exists, err)
	}
	stage, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	sum := sha256.Sum256(data)
	var total int64
	if err := CopyCheckedFile(t.Context(), source, stage, "junction/tool.py", hex.EncodeToString(sum[:]), &total); err == nil || total != 0 {
		t.Fatal("copied a file through a junction outside the source", total, err)
	}
}
