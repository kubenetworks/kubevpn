package util

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err = w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// UnzipKubeVPNIntoFile must write to the caller-provided filename and never derive
// the output path from the (attacker-controlled) zip entry name — i.e. no zip-slip.
func TestUnzipKubeVPNIntoFile_WritesToTargetIgnoringEntryPath(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	writeTestZip(t, zipPath, map[string]string{"evil/../kubevpn": "BINARY"})

	dst := filepath.Join(dir, "out", "kubevpn")
	if err := UnzipKubeVPNIntoFile(zipPath, dst); err != nil {
		t.Fatalf("unzip: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "BINARY" {
		t.Fatalf("content = %q, want BINARY", got)
	}
	// The entry's own path components must not have been used to write anywhere.
	if _, err := os.Stat(filepath.Join(dir, "evil")); !os.IsNotExist(err) {
		t.Fatalf("zip entry path leaked to disk: %v", err)
	}
}

func TestUnzipKubeVPNIntoFile_ChecksumMatches(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	content := "kubevpn-binary-bytes"
	sum := sha256.Sum256([]byte(content))
	writeTestZip(t, zipPath, map[string]string{
		"bin/kubevpn":   content,
		"checksums.txt": hex.EncodeToString(sum[:]) + "\n",
	})
	dst := filepath.Join(dir, "kubevpn")
	if err := UnzipKubeVPNIntoFile(zipPath, dst); err != nil {
		t.Fatalf("unzip with valid checksum: %v", err)
	}
}

func TestUnzipKubeVPNIntoFile_ChecksumMismatchFails(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	writeTestZip(t, zipPath, map[string]string{
		"bin/kubevpn":   "real-bytes",
		"checksums.txt": strings.Repeat("0", 64), // wrong digest
	})
	err := UnzipKubeVPNIntoFile(zipPath, filepath.Join(dir, "kubevpn"))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got %v", err)
	}
}

func TestUnzipKubeVPNIntoFile_NoChecksumSkips(t *testing.T) {
	// Older release zips without checksums.txt must still extract (graceful).
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	writeTestZip(t, zipPath, map[string]string{"bin/kubevpn": "bytes"})
	if err := UnzipKubeVPNIntoFile(zipPath, filepath.Join(dir, "kubevpn")); err != nil {
		t.Fatalf("unzip without checksums.txt should succeed: %v", err)
	}
}

func TestUnzipKubeVPNIntoFile_NoKubevpnEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	writeTestZip(t, zipPath, map[string]string{"README.md": "x"})
	err := UnzipKubeVPNIntoFile(zipPath, filepath.Join(dir, "out"))
	if err == nil || !strings.Contains(err.Error(), "cannot find kubevpn") {
		t.Fatalf("expected 'cannot find kubevpn', got %v", err)
	}
}

func TestUnzipKubeVPNIntoFile_CorruptZip(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.zip")
	if err := os.WriteFile(bad, []byte("this is not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UnzipKubeVPNIntoFile(bad, filepath.Join(dir, "out")); err == nil {
		t.Fatal("expected error for corrupt zip, got nil")
	}
}

// Download must reject a non-200 response instead of writing the error page out as
// if it were the binary (previously it did not check the status code).
func TestDownload_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>not found</html>"))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "out")
	err := Download(srv.Client(), srv.URL, dst, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error on non-200 download, got nil")
	}
}

func TestGetManifest_Non200FallsToNextMirror(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","assets":[{"name":"kubevpn_v2.0.0_linux_amd64.zip","browser_download_url":"http://example.com/a.zip"}]}`))
	}))
	defer good.Close()

	orig := address
	address = []string{bad.URL, good.URL}
	defer func() { address = orig }()

	ver, url, err := GetManifest(&http.Client{}, "linux", "amd64")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if ver != "v2.0.0" || url == "" {
		t.Fatalf("got version=%q url=%q", ver, url)
	}
}

func TestGetManifest_UnsupportedPlatform(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","assets":[{"name":"kubevpn_v2.0.0_linux_amd64.zip","browser_download_url":"http://example.com/a.zip"}]}`))
	}))
	defer good.Close()

	orig := address
	address = []string{good.URL}
	defer func() { address = orig }()

	if _, _, err := GetManifest(&http.Client{}, "plan9", "sparc64"); err == nil {
		t.Fatal("expected unsupported-platform error, got nil")
	}
}

// TestGetManifest_ArchMustBeWholeToken pins the fix for the arch-substring bug: a
// release asset "kubevpn_v2.0.0_linux_arm64.zip" must NOT be selected when arch="arm",
// because "arm" is a substring of "arm64" — handing a 32-bit ARM user the arm64 binary
// (which would fail to run). With the old strings.Contains, the arm64 asset matched
// arch="arm" and was returned; nameHasToken now requires "arm" as a whole segment.
func TestGetManifest_ArchMustBeWholeToken(t *testing.T) {
	// Release ships only the arm64 asset (no 32-bit arm build). arch="arm" must NOT
	// match it and must surface the unsupported-platform error instead.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","assets":[{"name":"kubevpn_v2.0.0_linux_arm64.zip","browser_download_url":"http://example.com/arm64.zip"}]}`))
	}))
	defer srv.Close()

	orig := address
	address = []string{srv.URL}
	defer func() { address = orig }()

	if _, _, err := GetManifest(&http.Client{}, "linux", "arm"); err == nil {
		t.Fatal("arch=\"arm\" must NOT match the arm64 asset; expected unsupported-platform error, got nil")
	}
}

// TestGetManifest_ArchWholeTokenMatches is the positive counterpart: when the release
// ships an exact arm asset, arch="arm" must select it (not be rejected by the tighter
// match introduced to fix the arm64 false-positive).
func TestGetManifest_ArchWholeTokenMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","assets":[{"name":"kubevpn_v2.0.0_linux_arm64.zip","browser_download_url":"http://example.com/arm64.zip"},{"name":"kubevpn_v2.0.0_linux_arm.zip","browser_download_url":"http://example.com/arm.zip"}]}`))
	}))
	defer srv.Close()

	orig := address
	address = []string{srv.URL}
	defer func() { address = orig }()

	_, url, err := GetManifest(&http.Client{}, "linux", "arm")
	if err != nil {
		t.Fatalf("expected the arm asset to match, got error: %v", err)
	}
	if url != "http://example.com/arm.zip" {
		t.Fatalf("expected arm.zip, got %q (arch=\"arm\" matched the wrong asset)", url)
	}
}

