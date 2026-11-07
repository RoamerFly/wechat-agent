package stationauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordPrecedenceAndCompatibility(t *testing.T) {
	dir := t.TempDir()
	check := func(explicit, want string) {
		t.Helper()
		if got, err := Password(dir, explicit); err != nil || got != want {
			t.Fatalf("credential selection failed (match=%t): %v", got == want, err)
		}
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	check("synthetic-explicit", "synthetic-explicit")
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("environment override wrote a credential: %v", err)
	}
	check("", "admin")
	check("", "admin")
	const oldToken = "synthetic-legacy-token\r\n"
	write("admin-token", oldToken)
	check("", "synthetic-legacy-token")
	check("synthetic-explicit", "synthetic-explicit")
	const managed = "synthetic-managed-owner\r\n"
	write("admin-password", managed)
	check("", "synthetic-managed-owner")
	check("synthetic-explicit", "synthetic-managed-owner")
	check(" invalid-explicit ", "synthetic-managed-owner")
	for name, want := range map[string]string{"admin-token": oldToken, "admin-password": managed} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("existing %s was modified: %v", name, err)
		}
	}
}

func TestPasswordRejectsInvalidManagedCredentialInsteadOfFallingBack(t *testing.T) {
	for name, value := range map[string]string{
		"short":   "bad\n",
		"too big": strings.Repeat(" ", maxPasswordFile) + "synthetic-oversized",
		"invalid": "synthetic-\xff",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "admin-password"), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
			if got, err := Password(dir, "synthetic-fallback"); got != "" || err == nil {
				t.Fatal("invalid managed credential fell back to the environment")
			}
		})
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "admin-password"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := Password(dir, "synthetic-fallback"); got != "" || err == nil {
		t.Fatal("directory was accepted as a credential")
	}
}

func TestPasswordRejectsLinkedCredential(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "other-account")
	if err := os.WriteFile(target, []byte("synthetic-other-account"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "admin-password")); err != nil {
		t.Skipf("creating a symlink is unavailable: %v", err)
	}
	if got, err := Password(dir, "synthetic-fallback"); got != "" || err == nil {
		t.Fatal("linked credential was accepted")
	}
}
