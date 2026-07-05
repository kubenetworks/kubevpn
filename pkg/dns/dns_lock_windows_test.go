//go:build windows

package dns

import (
	"errors"
	"sync"
	"testing"
)

// TestWithHostsFileLock_RunsFn is a smoke test: the Windows named-mutex lock must execute fn and
// propagate its result. Mutual exclusion itself is an OS guarantee and is not unit-tested here.
func TestWithHostsFileLock_RunsFn(t *testing.T) {
	sentinel := errors.New("sentinel")
	if err := withHostsFileLock(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("withHostsFileLock did not propagate fn error: %v", err)
	}

	// Sequential re-acquire (same process/thread) must not deadlock.
	var n int
	for i := 0; i < 3; i++ {
		if err := withHostsFileLock(func() error { n++; return nil }); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	if n != 3 {
		t.Fatalf("fn ran %d times, want 3", n)
	}

	// Concurrent callers must all complete (serialized by the mutex) without panicking.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = withHostsFileLock(func() error { return nil }) }()
	}
	wg.Wait()
}
