package action

import (
	"context"
	"io"
	"os"
	"testing"
)

func TestStreamWriter_Write_CallsSendWithCorrectString(t *testing.T) {
	var received string
	send := func(msg string) error {
		received = msg
		return nil
	}
	w := &streamWriter{send: send}

	input := []byte("hello world")
	_, _ = w.Write(input)

	if received != "hello world" {
		t.Fatalf("expected send to receive %q, got %q", "hello world", received)
	}
}

func TestStreamWriter_Write_ReturnsLenAndNil(t *testing.T) {
	send := func(msg string) error {
		return nil
	}
	w := &streamWriter{send: send}

	input := []byte("test data")
	n, err := w.Write(input)

	if n != len(input) {
		t.Fatalf("expected n=%d, got %d", len(input), n)
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestNewStreamWriter_ReturnsNonNil(t *testing.T) {
	send := func(msg string) error {
		return nil
	}
	w := newStreamWriter(send)

	if w == nil {
		t.Fatal("expected non-nil io.Writer")
	}

	// Verify it implements io.Writer
	var _ io.Writer = w
}

func TestResolveKubeconfig_NilJump_EmptyBytes_CreatesTempFile(t *testing.T) {
	ctx := context.Background()
	kubeconfigBytes := "apiVersion: v1\nkind: Config\n"

	path, err := resolveKubeconfig(ctx, nil, kubeconfigBytes, false)
	if err != nil {
		t.Fatalf("resolveKubeconfig: %v", err)
	}
	defer os.Remove(path)

	if path == "" {
		t.Fatal("expected non-empty path")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected file to exist at %s: %v", path, err)
	}
	if info.Size() == 0 {
		t.Fatal("expected file to have content")
	}
}

func TestResolveKubeconfig_NilJump_FileReadable(t *testing.T) {
	ctx := context.Background()
	kubeconfigBytes := "apiVersion: v1\nkind: Config\nclusters: []\n"

	path, err := resolveKubeconfig(ctx, nil, kubeconfigBytes, false)
	if err != nil {
		t.Fatalf("resolveKubeconfig: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file at %s: %v", path, err)
	}

	if string(data) != kubeconfigBytes {
		t.Fatalf("file content mismatch:\n  got:  %q\n  want: %q", string(data), kubeconfigBytes)
	}
}
