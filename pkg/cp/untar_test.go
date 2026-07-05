package cp

import (
	"archive/tar"
	"path/filepath"
	"testing"
)

// Regression: a circular link chain (A -> B -> A) must be rejected with an error
// rather than recursing until the goroutine stack overflows.
func TestCopyFromLink_CircularChain(t *testing.T) {
	dir := t.TempDir()
	gen := func(name string) localPath {
		return newLocalPath(filepath.Join(dir, name))
	}
	headers := []tar.Header{
		{Name: "A", Linkname: "B"},
		{Name: "B", Linkname: "A"},
	}

	err := copyFromLink(headers, headers[0], gen)
	if err == nil {
		t.Fatal("expected an error for a circular link chain, got nil")
	}
}
