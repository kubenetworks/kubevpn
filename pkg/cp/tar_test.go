package cp

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// readTarEntries reads all entries from a tar archive and returns them as a map
// of header name to file content (empty string for directories/symlinks).
func readTarEntries(t *testing.T, data []byte) map[string]tarEntry {
	t.Helper()
	entries := make(map[string]tarEntry)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		var content []byte
		if hdr.Typeflag == tar.TypeReg {
			content, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("reading tar entry %q: %v", hdr.Name, err)
			}
		}
		entries[hdr.Name] = tarEntry{
			Header:  hdr,
			Content: content,
		}
	}
	return entries
}

type tarEntry struct {
	Header  *tar.Header
	Content []byte
}

func TestMakeTar_SingleFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a single file.
	filePath := filepath.Join(tmpDir, "hello.txt")
	if err := os.WriteFile(filePath, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	src := newLocalPath(filePath)
	dest := newRemotePath("/remote/hello.txt")
	if err := makeTar(src, dest, &buf); err != nil {
		t.Fatalf("makeTar: %v", err)
	}

	entries := readTarEntries(t, buf.Bytes())
	if len(entries) != 1 {
		t.Fatalf("expected 1 tar entry, got %d", len(entries))
	}

	entry, ok := entries["hello.txt"]
	if !ok {
		// Print actual keys for debugging.
		for k := range entries {
			t.Logf("  entry key: %q", k)
		}
		t.Fatal("expected entry with name \"hello.txt\"")
	}
	if string(entry.Content) != "hello world" {
		t.Errorf("content = %q, want %q", string(entry.Content), "hello world")
	}
}

func TestMakeTar_EmptyDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	// Create an empty subdirectory.
	emptyDir := filepath.Join(tmpDir, "emptydir")
	if err := os.Mkdir(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	src := newLocalPath(emptyDir)
	dest := newRemotePath("/remote/emptydir")
	if err := makeTar(src, dest, &buf); err != nil {
		t.Fatalf("makeTar: %v", err)
	}

	entries := readTarEntries(t, buf.Bytes())
	if len(entries) != 1 {
		t.Fatalf("expected 1 tar entry for empty dir, got %d", len(entries))
	}

	entry, ok := entries["emptydir"]
	if !ok {
		for k := range entries {
			t.Logf("  entry key: %q", k)
		}
		t.Fatal("expected entry with name \"emptydir\"")
	}
	if entry.Header.Typeflag != tar.TypeDir {
		t.Errorf("expected TypeDir, got %d", entry.Header.Typeflag)
	}
}

func TestMakeTar_NestedDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	// Build a nested structure:
	//   srcdir/
	//     a.txt
	//     sub/
	//       b.txt
	srcDir := filepath.Join(tmpDir, "srcdir")
	subDir := filepath.Join(srcDir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "b.txt"), []byte("bbb"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	src := newLocalPath(srcDir)
	dest := newRemotePath("/remote/srcdir")
	if err := makeTar(src, dest, &buf); err != nil {
		t.Fatalf("makeTar: %v", err)
	}

	entries := readTarEntries(t, buf.Bytes())

	// Expect: srcdir/a.txt and srcdir/sub/b.txt
	if len(entries) != 2 {
		t.Fatalf("expected 2 tar entries, got %d", len(entries))
	}

	aEntry, ok := entries["srcdir/a.txt"]
	if !ok {
		for k := range entries {
			t.Logf("  entry key: %q", k)
		}
		t.Fatal("missing srcdir/a.txt")
	}
	if string(aEntry.Content) != "aaa" {
		t.Errorf("a.txt content = %q, want %q", string(aEntry.Content), "aaa")
	}

	bEntry, ok := entries["srcdir/sub/b.txt"]
	if !ok {
		for k := range entries {
			t.Logf("  entry key: %q", k)
		}
		t.Fatal("missing srcdir/sub/b.txt")
	}
	if string(bEntry.Content) != "bbb" {
		t.Errorf("b.txt content = %q, want %q", string(bEntry.Content), "bbb")
	}
}

func TestMakeTar_Symlink(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file and a symlink pointing to it.
	filePath := filepath.Join(tmpDir, "target.txt")
	if err := os.WriteFile(filePath, []byte("target content"), 0644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(tmpDir, "link.txt")
	if err := os.Symlink("target.txt", linkPath); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	src := newLocalPath(linkPath)
	dest := newRemotePath("/remote/link.txt")
	if err := makeTar(src, dest, &buf); err != nil {
		t.Fatalf("makeTar: %v", err)
	}

	entries := readTarEntries(t, buf.Bytes())
	if len(entries) != 1 {
		t.Fatalf("expected 1 tar entry, got %d", len(entries))
	}

	entry, ok := entries["link.txt"]
	if !ok {
		for k := range entries {
			t.Logf("  entry key: %q", k)
		}
		t.Fatal("expected entry with name \"link.txt\"")
	}
	if entry.Header.Typeflag != tar.TypeSymlink {
		t.Errorf("expected TypeSymlink, got %d", entry.Header.Typeflag)
	}
	if entry.Header.Linkname != "target.txt" {
		t.Errorf("Linkname = %q, want %q", entry.Header.Linkname, "target.txt")
	}
}

func TestMakeTar_DirectoryWithSymlink(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a directory with a regular file and a symlink.
	srcDir := filepath.Join(tmpDir, "mixed")
	if err := os.Mkdir(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "real.txt"), []byte("real"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(srcDir, "sym.txt")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	src := newLocalPath(srcDir)
	dest := newRemotePath("/remote/mixed")
	if err := makeTar(src, dest, &buf); err != nil {
		t.Fatalf("makeTar: %v", err)
	}

	entries := readTarEntries(t, buf.Bytes())
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	real, ok := entries["mixed/real.txt"]
	if !ok {
		for k := range entries {
			t.Logf("  entry key: %q", k)
		}
		t.Fatal("missing mixed/real.txt")
	}
	if string(real.Content) != "real" {
		t.Errorf("real.txt content = %q, want %q", string(real.Content), "real")
	}

	sym, ok := entries["mixed/sym.txt"]
	if !ok {
		for k := range entries {
			t.Logf("  entry key: %q", k)
		}
		t.Fatal("missing mixed/sym.txt")
	}
	if sym.Header.Typeflag != tar.TypeSymlink {
		t.Errorf("sym.txt Typeflag = %d, want TypeSymlink (%d)", sym.Header.Typeflag, tar.TypeSymlink)
	}
	if sym.Header.Linkname != "real.txt" {
		t.Errorf("sym.txt Linkname = %q, want %q", sym.Header.Linkname, "real.txt")
	}
}

func TestRecursiveTar_EmptyDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	emptyDir := filepath.Join(tmpDir, "empty")
	if err := os.Mkdir(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	srcDir := newLocalPath(tmpDir)
	srcFile := newLocalPath("empty")
	destDir := newRemotePath("/dest")
	destFile := newRemotePath("empty")

	if err := recursiveTar(srcDir, srcFile, destDir, destFile, tw); err != nil {
		t.Fatalf("recursiveTar: %v", err)
	}
	tw.Close()

	entries := readTarEntries(t, buf.Bytes())
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	entry, ok := entries["empty"]
	if !ok {
		for k := range entries {
			t.Logf("  entry key: %q", k)
		}
		t.Fatal("expected entry with name \"empty\"")
	}
	if entry.Header.Typeflag != tar.TypeDir {
		t.Errorf("expected TypeDir, got %d", entry.Header.Typeflag)
	}
}

func TestRecursiveTar_PreservesFileContent(t *testing.T) {
	tmpDir := t.TempDir()

	content := "the quick brown fox jumps over the lazy dog"
	if err := os.WriteFile(filepath.Join(tmpDir, "fox.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	srcDir := newLocalPath(tmpDir)
	srcFile := newLocalPath("fox.txt")
	destDir := newRemotePath("/dest")
	destFile := newRemotePath("fox.txt")

	if err := recursiveTar(srcDir, srcFile, destDir, destFile, tw); err != nil {
		t.Fatalf("recursiveTar: %v", err)
	}
	tw.Close()

	entries := readTarEntries(t, buf.Bytes())
	entry, ok := entries["fox.txt"]
	if !ok {
		t.Fatal("missing fox.txt entry")
	}
	if string(entry.Content) != content {
		t.Errorf("content = %q, want %q", string(entry.Content), content)
	}
}

func TestRecursiveTar_DeeplyNested(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a/b/c/d.txt
	deepDir := filepath.Join(tmpDir, "root", "a", "b", "c")
	if err := os.MkdirAll(deepDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deepDir, "d.txt"), []byte("deep"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	srcDir := newLocalPath(tmpDir)
	srcFile := newLocalPath("root")
	destDir := newRemotePath("/dest")
	destFile := newRemotePath("root")

	if err := recursiveTar(srcDir, srcFile, destDir, destFile, tw); err != nil {
		t.Fatalf("recursiveTar: %v", err)
	}
	tw.Close()

	entries := readTarEntries(t, buf.Bytes())
	_, ok := entries["root/a/b/c/d.txt"]
	if !ok {
		for k := range entries {
			t.Logf("  entry key: %q", k)
		}
		t.Fatal("missing root/a/b/c/d.txt")
	}
}

func TestRecursiveTar_SymlinkTarget(t *testing.T) {
	tmpDir := t.TempDir()

	// Create target file and a symlink.
	if err := os.WriteFile(filepath.Join(tmpDir, "original.txt"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original.txt", filepath.Join(tmpDir, "shortcut.txt")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	srcDir := newLocalPath(tmpDir)
	srcFile := newLocalPath("shortcut.txt")
	destDir := newRemotePath("/dest")
	destFile := newRemotePath("shortcut.txt")

	if err := recursiveTar(srcDir, srcFile, destDir, destFile, tw); err != nil {
		t.Fatalf("recursiveTar: %v", err)
	}
	tw.Close()

	entries := readTarEntries(t, buf.Bytes())
	entry, ok := entries["shortcut.txt"]
	if !ok {
		t.Fatal("missing shortcut.txt entry")
	}
	if entry.Header.Typeflag != tar.TypeSymlink {
		t.Errorf("Typeflag = %d, want TypeSymlink (%d)", entry.Header.Typeflag, tar.TypeSymlink)
	}
	if entry.Header.Linkname != "original.txt" {
		t.Errorf("Linkname = %q, want %q", entry.Header.Linkname, "original.txt")
	}
}

func TestMakeTar_LargeFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file larger than a typical buffer size.
	data := make([]byte, 128*1024) // 128 KB
	for i := range data {
		data[i] = byte(i % 251) // prime modulus for variety
	}
	filePath := filepath.Join(tmpDir, "large.bin")
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	src := newLocalPath(filePath)
	dest := newRemotePath("/remote/large.bin")
	if err := makeTar(src, dest, &buf); err != nil {
		t.Fatalf("makeTar: %v", err)
	}

	entries := readTarEntries(t, buf.Bytes())
	entry, ok := entries["large.bin"]
	if !ok {
		t.Fatal("missing large.bin entry")
	}
	if len(entry.Content) != len(data) {
		t.Fatalf("content length = %d, want %d", len(entry.Content), len(data))
	}
	if !bytes.Equal(entry.Content, data) {
		t.Error("content mismatch for large file")
	}
}

func TestMakeTar_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()

	filePath := filepath.Join(tmpDir, "empty.txt")
	if err := os.WriteFile(filePath, nil, 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	src := newLocalPath(filePath)
	dest := newRemotePath("/remote/empty.txt")
	if err := makeTar(src, dest, &buf); err != nil {
		t.Fatalf("makeTar: %v", err)
	}

	entries := readTarEntries(t, buf.Bytes())
	entry, ok := entries["empty.txt"]
	if !ok {
		t.Fatal("missing empty.txt entry")
	}
	if len(entry.Content) != 0 {
		t.Errorf("expected empty content, got %d bytes", len(entry.Content))
	}
	if entry.Header.Typeflag != tar.TypeReg {
		t.Errorf("expected TypeReg, got %d", entry.Header.Typeflag)
	}
}

func TestMakeTar_MultipleFilesInDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "multi")
	if err := os.Mkdir(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"one.txt":   "1",
		"two.txt":   "2",
		"three.txt": "3",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	src := newLocalPath(srcDir)
	dest := newRemotePath("/remote/multi")
	if err := makeTar(src, dest, &buf); err != nil {
		t.Fatalf("makeTar: %v", err)
	}

	entries := readTarEntries(t, buf.Bytes())
	if len(entries) != len(files) {
		t.Fatalf("expected %d entries, got %d", len(files), len(entries))
	}
	for name, wantContent := range files {
		entry, ok := entries["multi/"+name]
		if !ok {
			for k := range entries {
				t.Logf("  entry key: %q", k)
			}
			t.Fatalf("missing multi/%s", name)
		}
		if string(entry.Content) != wantContent {
			t.Errorf("%s content = %q, want %q", name, string(entry.Content), wantContent)
		}
	}
}
