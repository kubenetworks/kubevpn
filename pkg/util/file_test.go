package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupTempFilesIn_RemovesFilesKeepsDirs(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{
		filepath.Join(dir, "a"),
		filepath.Join(dir, "b"),
		filepath.Join(sub, "c"),
	}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := cleanupTempFilesIn(dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, f := range files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("file %s should have been removed (stat err=%v)", f, err)
		}
	}
	for _, d := range []string{dir, sub} {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			t.Errorf("dir %s should remain (err=%v)", d, err)
		}
	}
}

func TestCleanupTempFilesIn_EmptyDir(t *testing.T) {
	if err := cleanupTempFilesIn(t.TempDir()); err != nil {
		t.Fatalf("empty dir should return nil, got %v", err)
	}
}

// Regression: a missing temp dir made WalkDir invoke the callback with nil info
// and a non-nil err; the old code dereferenced info.IsDir() and panicked.
func TestCleanupTempFilesIn_NonexistentDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := cleanupTempFilesIn(missing); err != nil {
		t.Fatalf("nonexistent dir should return nil (no panic), got %v", err)
	}
}
