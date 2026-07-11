package cp

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestExtractFileSpec(t *testing.T) {
	tests := []struct {
		name         string
		arg          string
		wantPodName  string
		wantPodNs    string
		wantFile     string
		wantLocal    bool
		wantErr      bool
		skipOnNonWin bool
	}{
		{
			name:      "simple local file path",
			arg:       "/tmp/foo",
			wantFile:  "/tmp/foo",
			wantLocal: true,
		},
		{
			name:      "relative local path without colon",
			arg:       "relative/path/to/file",
			wantFile:  "relative/path/to/file",
			wantLocal: true,
		},
		{
			name:        "pod with file path",
			arg:         "my-pod:/tmp/bar",
			wantPodName: "my-pod",
			wantFile:    "/tmp/bar",
		},
		{
			name:        "namespace and pod with file path",
			arg:         "my-ns/my-pod:/tmp/baz",
			wantPodNs:   "my-ns",
			wantPodName: "my-pod",
			wantFile:    "/tmp/baz",
		},
		{
			name:    "leading colon is invalid",
			arg:     ":file",
			wantErr: true,
		},
		{
			name:    "too many slashes in pod spec",
			arg:     "a/b/c:/path",
			wantErr: true,
		},
		{
			name:      "empty string",
			arg:       "",
			wantFile:  "",
			wantLocal: true,
		},
		{
			name:        "pod with empty file path",
			arg:         "pod-name:",
			wantPodName: "pod-name",
			wantFile:    "",
		},
		{
			name:         "windows drive letter treated as local",
			arg:          `C:\Users\test\file.txt`,
			wantFile:     `C:\Users\test\file.txt`,
			wantLocal:    true,
			skipOnNonWin: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skipOnNonWin && runtime.GOOS != "windows" {
				t.Skip("windows-only test")
			}
			got, err := extractFileSpec(tt.arg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("extractFileSpec(%q) error = %v, wantErr %v", tt.arg, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.PodName != tt.wantPodName {
				t.Errorf("PodName = %q, want %q", got.PodName, tt.wantPodName)
			}
			if got.PodNamespace != tt.wantPodNs {
				t.Errorf("PodNamespace = %q, want %q", got.PodNamespace, tt.wantPodNs)
			}
			if got.File.String() != tt.wantFile {
				t.Errorf("File = %q, want %q", got.File.String(), tt.wantFile)
			}
			if tt.wantLocal {
				if _, ok := got.File.(localPath); !ok {
					t.Errorf("expected localPath, got %T", got.File)
				}
			} else {
				if _, ok := got.File.(remotePath); !ok {
					t.Errorf("expected remotePath, got %T", got.File)
				}
			}
		})
	}
}

func TestExtractFileSpecError(t *testing.T) {
	_, err := extractFileSpec(":file")
	if err != errFileSpecDoesntMatchFormat {
		t.Errorf("expected errFileSpecDoesntMatchFormat, got %v", err)
	}

	_, err = extractFileSpec("a/b/c:/path")
	if err != errFileSpecDoesntMatchFormat {
		t.Errorf("expected errFileSpecDoesntMatchFormat, got %v", err)
	}
}

func TestStripPathShortcuts(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no shortcuts",
			in:   "foo/bar/baz",
			want: "foo/bar/baz",
		},
		{
			name: "leading dot-dot",
			in:   "../foo/bar",
			want: "foo/bar",
		},
		{
			name: "multiple leading dot-dot",
			in:   "../../../foo",
			want: "foo",
		},
		{
			name: "only dot-dot",
			in:   "..",
			want: "",
		},
		{
			name: "only dot",
			in:   ".",
			want: "",
		},
		{
			name: "dot-dot in middle preserved",
			in:   "foo/../bar",
			want: "foo/../bar",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "leading slash removed",
			in:   "/foo/bar",
			want: "foo/bar",
		},
		{
			name: "dot-dot then slash path",
			in:   "..//foo",
			want: "foo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripPathShortcuts(tt.in)
			if got != tt.want {
				t.Errorf("stripPathShortcuts(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsRelative(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		target string
		want   bool
	}{
		{
			name:   "same directory",
			base:   "/tmp",
			target: "/tmp",
			want:   true,
		},
		{
			name:   "child path",
			base:   "/tmp",
			target: filepath.Join("/tmp", "foo", "bar"),
			want:   true,
		},
		{
			name:   "escapes base via dot-dot",
			base:   "/tmp/safe",
			target: "/tmp/safe/../../../etc/passwd",
			want:   false,
		},
		{
			name:   "sibling directory",
			base:   "/tmp/a",
			target: "/tmp/b",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRelative(newLocalPath(tt.base), newLocalPath(tt.target))
			if got != tt.want {
				t.Errorf("isRelative(%q, %q) = %v, want %v", tt.base, tt.target, got, tt.want)
			}
		})
	}
}
