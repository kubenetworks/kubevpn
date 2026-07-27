package util

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

// connectReadRetryBackoff bounds retries of a read-only apiserver call made while
// establishing a connection: ~6 attempts over ~20-30s. The apiserver behind a
// nested-VM / just-warming-up cluster (or an SSH port-forward to it) can transiently
// drop connections — surfacing as EOF or "net/http: TLS handshake timeout" — so the
// very first probe (e.g. DetectPodExists) would otherwise hard-fail the whole connect
// on a single blip. The cap keeps a genuinely unreachable apiserver from stalling
// connect for long. A var so tests can shrink it.
var connectReadRetryBackoff = wait.Backoff{
	Steps:    6,
	Duration: 500 * time.Millisecond,
	Factor:   2,
	Jitter:   0.1,
	Cap:      8 * time.Second,
}

// isTransientAPIReadErr reports whether err from a read-only apiserver call is worth
// retrying. Context cancellation/deadline is never transient (the caller asked to stop),
// and definitive API rejections (NotFound/Forbidden/…) will not change on retry. Server
// timeouts, throttling and 5xx, plus transport-level drops (EOF, TLS handshake timeout,
// connection reset/refused) are transient. Mirrors isTransientExecStreamErr in style.
func isTransientAPIReadErr(err error) bool {
	if err == nil {
		return false
	}
	// Caller-driven stop: do not retry (checked first so a "timeout" message on a
	// cancelled context is not mistaken for a transient transport error).
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Definitive API rejections: retrying cannot help.
	if k8serrors.IsNotFound(err) || k8serrors.IsForbidden(err) || k8serrors.IsUnauthorized(err) ||
		k8serrors.IsInvalid(err) || k8serrors.IsBadRequest(err) || k8serrors.IsMethodNotSupported(err) ||
		k8serrors.IsConflict(err) || k8serrors.IsAlreadyExists(err) {
		return false
	}
	// Transient server-side conditions.
	if k8serrors.IsServerTimeout(err) || k8serrors.IsTimeout(err) || k8serrors.IsTooManyRequests(err) ||
		k8serrors.IsInternalError(err) || k8serrors.IsServiceUnavailable(err) {
		return true
	}
	// Transport-level drops.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"eof",
		"tls handshake timeout",
		"connection refused",
		"connection reset",
		"i/o timeout",
		"use of closed network connection",
		"broken pipe",
		"http2:",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// retryAPIRead runs fn (a read-only apiserver call), retrying with a bounded backoff
// only while it returns a transient error (see isTransientAPIReadErr). It returns the
// last error on exhaustion, or fn's non-transient error immediately.
func retryAPIRead(fn func() error) error {
	return retry.OnError(connectReadRetryBackoff, isTransientAPIReadErr, fn)
}
