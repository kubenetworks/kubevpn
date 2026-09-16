package log

import (
	"context"
	"sync"
	"time"
)

// throttleMaxKeys bounds a Throttle's memory. Keys are usually low-cardinality (a fixed call site,
// or a peer address from the TUN pool), but nothing structurally prevents an unbounded stream of
// them, so the table is dropped wholesale once it grows past this. Losing the timestamps only means
// the next occurrence of each key logs once more than strictly necessary.
const throttleMaxKeys = 1024

// Throttle rate-limits repeated log messages per key.
//
// It exists because the data plane's failure paths were all Debug-level: a client whose route
// announcement silently produced nothing, and a server dropping every heartbeat echo reply for want
// of a route, together took the tunnel down for 20 hours while emitting nothing at the default log
// level. These paths recur every few seconds, so they cannot be plain Warn either — the fix is to
// report them, but at most once per interval per key.
//
// A nil *Throttle allows everything (never silently drops a diagnostic).
type Throttle struct {
	interval time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

// NewThrottle returns a Throttle admitting at most one message per key per interval.
func NewThrottle(interval time.Duration) *Throttle {
	return &Throttle{interval: interval, last: make(map[string]time.Time)}
}

// Allow reports whether key may log now, recording the decision when it may.
func (t *Throttle) Allow(key string) bool {
	if t == nil {
		return true
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[key]; ok && now.Sub(last) < t.interval {
		return false
	}
	if len(t.last) >= throttleMaxKeys {
		t.last = make(map[string]time.Time, throttleMaxKeys)
	}
	t.last[key] = now
	return true
}

// Warnf logs at warning level if key is not currently throttled. Callers pass a key identifying
// what recurs (a call site, a peer address), not the fully formatted message.
func (t *Throttle) Warnf(ctx context.Context, key, format string, args ...any) {
	if t.Allow(key) {
		G(ctx).Warnf(format, args...)
	}
}

// Reset drops all recorded timestamps so the next occurrence of every key logs immediately. It
// exists for tests that share a package-level throttle across cases: without it, one case that
// trips a key inside the interval silently swallows a later case's diagnostic for the same key.
// A nil *Throttle is a no-op.
func (t *Throttle) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.last = make(map[string]time.Time)
	t.mu.Unlock()
}
