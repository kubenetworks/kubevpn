package util

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestIsTransientExecStreamErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"wrapped canceled", fmt.Errorf("exec: %w", context.Canceled), false},
		{"permanent", errors.New("command terminated with exit code 1"), false},
		{"not found", errors.New("pods \"x\" not found"), false},
		{"stream protocol", errors.New("error stream protocol error: unknown error"), true},
		{"copy remote stream EOF", errors.New("error copying from local connection to remote stream: EOF"), true},
		{"conn reset", errors.New("read tcp 1.2.3.4:5->6.7.8.9:10: read: connection reset by peer"), true},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"wrapped io.EOF", fmt.Errorf("stream: %w", io.EOF), true},
		{"http2", errors.New("http2: client connection lost"), true},
		{"closed conn", errors.New("use of closed network connection"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTransientExecStreamErr(c.err); got != c.want {
				t.Fatalf("isTransientExecStreamErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestRetryTransientExec_RetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := retryTransientExec(func() error {
		calls++
		if calls < 3 {
			return errors.New("error stream protocol error: unknown error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after transient errors, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls (2 transient + 1 success), got %d", calls)
	}
}

func TestRetryTransientExec_PermanentNotRetried(t *testing.T) {
	calls := 0
	permanent := errors.New("command terminated with exit code 1")
	err := retryTransientExec(func() error {
		calls++
		return permanent
	})
	if !errors.Is(err, permanent) {
		t.Fatalf("expected the permanent error back, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected no retry on a permanent error, got %d calls", calls)
	}
}

func TestRetryTransientExec_ContextCanceledNotRetried(t *testing.T) {
	calls := 0
	err := retryTransientExec(func() error {
		calls++
		return fmt.Errorf("exec: %w", context.Canceled)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled back, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected no retry on context cancellation, got %d calls", calls)
	}
}
