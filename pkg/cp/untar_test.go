package cp

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"
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

// TestUntarAll_MultipleFilesExtractsAll is the behavior-preserving guard for the
// defer-in-loop fix: untarAll previously deferred each outFile.Close() to function
// exit, accumulating one file descriptor per tar entry (enough to exhaust the fd
// limit on a large archive). The fix closes each file per iteration. This test
// extracts a multi-file archive and asserts every file lands on disk with the right
// content, proving the per-iteration close did not break extraction or leave entries
// unflushed/unwritten.
func TestUntarAll_MultipleFilesExtractsAll(t *testing.T) {
	dest := t.TempDir()
	const prefix = "pod-root/"

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := map[string]string{
		"pod-root/a.txt": "contents-a",
		"pod-root/b.txt": "contents-b",
		"pod-root/c.txt": "contents-c",
	}
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	o := &CopyOptions{IOStreams: genericiooptions.NewTestIOStreamsDiscard()}
	if err := o.untarAll(prefix, newLocalPath(dest), bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("untarAll: %v", err)
	}

	for name, wantContent := range files {
		rel := name[len(prefix):]
		got, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil {
			t.Errorf("expected extracted file %s, read error: %v", rel, err)
			continue
		}
		if string(got) != wantContent {
			t.Errorf("file %s: got %q, want %q", rel, got, wantContent)
		}
	}
}



