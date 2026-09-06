package util

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

// execStreamRetryBackoff bounds retries of a pod-exec stream: ~5 attempts over a
// few seconds. The remotecommand stream to a pod (over the apiserver) can drop
// mid-flight — especially on flaky/nested-VM clusters — surfacing as
// "error stream protocol error" or an EOF while copying the stream; a fresh exec
// usually succeeds. The cap keeps a genuinely broken exec from stalling callers.
var execStreamRetryBackoff = wait.Backoff{
	Steps:    5,
	Duration: time.Second,
	Factor:   1.5,
	Jitter:   0.2,
}

// isTransientExecStreamErr reports whether err is a transient pod-exec stream
// failure worth retrying. Context cancellation/deadline is never transient (the
// caller asked to stop). Matches the known mid-stream drop signatures observed
// from client-go remotecommand and the SPDY/websocket stream copy.
func isTransientExecStreamErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"error stream protocol error",
		"error copying from local connection to remote stream",
		"error copying from remote stream to local connection",
		"connection reset by peer",
		"unexpected eof",
		"broken pipe",
		"http2:",
		"use of closed network connection",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// retryTransientExec runs fn, retrying with a bounded backoff only while it returns
// a transient exec-stream error (see isTransientExecStreamErr). It returns the last
// error on exhaustion, or fn's non-transient error immediately.
func retryTransientExec(fn func() error) error {
	return retry.OnError(execStreamRetryBackoff, isTransientExecStreamErr, fn)
}
