package action

import (
	"context"
	"io"
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

func TestResolveKubeconfigBytes_NilJump_ReturnsBytes(t *testing.T) {
	ctx := context.Background()
	kubeconfigBytes := "apiVersion: v1\nkind: Config\n"

	data, err := resolveKubeconfigBytes(ctx, nil, kubeconfigBytes, false)
	if err != nil {
		t.Fatalf("resolveKubeconfigBytes: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty bytes")
	}
	if string(data) != kubeconfigBytes {
		t.Fatalf("bytes mismatch:\n  got:  %q\n  want: %q", string(data), kubeconfigBytes)
	}
}

func TestResolveKubeconfigBytes_NilJump_Unchanged(t *testing.T) {
	ctx := context.Background()
	kubeconfigBytes := "apiVersion: v1\nkind: Config\nclusters: []\n"

	data, err := resolveKubeconfigBytes(ctx, nil, kubeconfigBytes, false)
	if err != nil {
		t.Fatalf("resolveKubeconfigBytes: %v", err)
	}
	if string(data) != kubeconfigBytes {
		t.Fatalf("bytes mismatch:\n  got:  %q\n  want: %q", string(data), kubeconfigBytes)
	}
}
