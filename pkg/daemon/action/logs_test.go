package action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeekToLastLine_BasicFile(t *testing.T) {
	// File with 5 lines
	content := "line1\nline2\nline3\nline4\nline5\n"
	f := createTempFile(t, content)

	// Ask for last 3 lines
	offset, size, err := seekToLastLine(f, 3)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), size)
	}
	// offset should point to the start of "line3\n"
	tail := content[offset:]
	if tail != "line3\nline4\nline5\n" {
		t.Errorf("expected last 3 lines, got %q", tail)
	}
}

func TestSeekToLastLine_RequestMoreLinesThanExist(t *testing.T) {
	content := "line1\nline2\n"
	f := createTempFile(t, content)

	// Ask for 100 lines but file has only 2
	offset, size, err := seekToLastLine(f, 100)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	if offset != 0 {
		t.Errorf("expected offset 0 (start of file), got %d", offset)
	}
	if size != 0 {
		t.Errorf("expected size 0 (fallback), got %d", size)
	}
}

func TestSeekToLastLine_EmptyFile(t *testing.T) {
	f := createTempFile(t, "")

	offset, size, err := seekToLastLine(f, 10)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	if offset != 0 {
		t.Errorf("expected offset 0, got %d", offset)
	}
	if size != 0 {
		t.Errorf("expected size 0, got %d", size)
	}
}

func TestSeekToLastLine_SingleLine(t *testing.T) {
	content := "only one line\n"
	f := createTempFile(t, content)

	// File has 1 newline, request 1 line. lineCount reaches 1 but never > 1,
	// so the function returns (0, 0, nil) meaning "start from beginning".
	offset, size, err := seekToLastLine(f, 1)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	if offset != 0 {
		t.Errorf("expected offset 0 (whole file), got %d", offset)
	}
	if size != 0 {
		t.Errorf("expected size 0 (fallback), got %d", size)
	}
}

func TestSeekToLastLine_ZeroLines(t *testing.T) {
	content := "line1\nline2\nline3\n"
	f := createTempFile(t, content)

	// Requesting 0 lines should return the position after the last newline
	offset, size, err := seekToLastLine(f, 0)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), size)
	}
	// With 0 lines requested, the first newline encountered increments lineCount
	// to 1 which is > 0, so offset should point past the last newline
	tail := content[offset:]
	if tail != "" {
		t.Errorf("expected empty tail for 0 lines, got %q", tail)
	}
}

func TestSeekToLastLine_NoTrailingNewline(t *testing.T) {
	content := "line1\nline2\nline3"
	f := createTempFile(t, content)

	// 2 newlines in file, requesting 2 lines. lineCount reaches 2 but
	// never > 2, so the function returns (0, 0, nil).
	offset, size, err := seekToLastLine(f, 2)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	if offset != 0 {
		t.Errorf("expected offset 0 (not enough newlines), got %d", offset)
	}
	if size != 0 {
		t.Errorf("expected size 0 (fallback), got %d", size)
	}
}

func TestSeekToLastLine_ExactLineCount(t *testing.T) {
	content := "a\nb\nc\n"
	f := createTempFile(t, content)

	// File has exactly 3 lines, ask for 3
	offset, _, err := seekToLastLine(f, 3)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	// 3 newlines found, lineCount reaches 3 but never > 3, so returns 0
	if offset != 0 {
		t.Errorf("expected offset 0 for exact match, got %d", offset)
	}
}

func TestSeekToLastLine_LargeFile(t *testing.T) {
	// Create a file larger than the 4096 buffer size
	var b strings.Builder
	lineCount := 1000
	for i := 0; i < lineCount; i++ {
		b.WriteString("this is a somewhat long log line to pad the file past the buffer boundary\n")
	}
	content := b.String()
	f := createTempFile(t, content)

	// Ask for last 5 lines
	offset, size, err := seekToLastLine(f, 5)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), size)
	}

	tail := content[offset:]
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
	}
}

func TestSeekToLastLine_NonexistentFile(t *testing.T) {
	_, _, err := seekToLastLine("/nonexistent/path/file.log", 10)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.ErrNotExist, got: %v", err)
	}
}

func TestSeekToLastLine_OneLine_RequestOne(t *testing.T) {
	// Edge case: single line with trailing newline, request exactly 1.
	// 1 newline, lineCount=1, never > 1, so returns (0, 0, nil).
	content := "hello\n"
	f := createTempFile(t, content)

	offset, size, err := seekToLastLine(f, 1)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	if offset != 0 {
		t.Errorf("expected offset 0, got %d", offset)
	}
	if size != 0 {
		t.Errorf("expected size 0 (fallback), got %d", size)
	}
}

func TestSeekToLastLine_ConsecutiveNewlines(t *testing.T) {
	// Empty lines count as lines
	content := "a\n\n\nb\n"
	f := createTempFile(t, content)

	// 4 newlines total. Ask for 2 lines.
	offset, _, err := seekToLastLine(f, 2)
	if err != nil {
		t.Fatalf("seekToLastLine: %v", err)
	}
	tail := content[offset:]
	// Should get the last 2 newline-terminated segments: "\n" and "b\n"
	if tail != "\nb\n" {
		t.Errorf("expected '\\nb\\n', got %q", tail)
	}
}

func createTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	return path
}
