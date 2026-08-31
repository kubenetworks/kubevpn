package log

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestThrottle_AllowsFirstThenSuppressesWithinInterval(t *testing.T) {
	th := NewThrottle(time.Hour)
	if !th.Allow("k") {
		t.Fatal("first call must be allowed")
	}
	if th.Allow("k") {
		t.Fatal("second call within the interval must be suppressed")
	}
	if !th.Allow("other") {
		t.Fatal("a different key must not be suppressed by an unrelated one")
	}
}

func TestThrottle_AllowsAgainAfterInterval(t *testing.T) {
	th := NewThrottle(time.Millisecond)
	if !th.Allow("k") {
		t.Fatal("first call must be allowed")
	}
	time.Sleep(5 * time.Millisecond)
	if !th.Allow("k") {
		t.Fatal("call after the interval elapsed must be allowed")
	}
}

// A zero interval must never suppress: callers use it to disable throttling (notably tests).
func TestThrottle_ZeroIntervalNeverSuppresses(t *testing.T) {
	th := NewThrottle(0)
	for i := 0; i < 5; i++ {
		if !th.Allow("k") {
			t.Fatalf("call %d suppressed with a zero interval", i)
		}
	}
}

// A nil Throttle must allow everything: a diagnostic must never be lost to an uninitialised field.
func TestThrottle_NilAllowsEverything(t *testing.T) {
	var th *Throttle
	if !th.Allow("k") || !th.Allow("k") {
		t.Fatal("nil Throttle must allow every call")
	}
	th.Warnf(context.Background(), "k", "must not panic")
}

func TestThrottle_BoundsKeyTableGrowth(t *testing.T) {
	th := NewThrottle(time.Hour)
	for i := 0; i < throttleMaxKeys*2+10; i++ {
		th.Allow(fmt.Sprintf("key-%d", i))
	}
	th.mu.Lock()
	n := len(th.last)
	th.mu.Unlock()
	if n > throttleMaxKeys {
		t.Fatalf("key table grew to %d entries, want <= %d", n, throttleMaxKeys)
	}
}

func TestThrottle_WarnfWritesOnceThenSuppresses(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithLogger(context.Background(), GetLoggerForClient(int32(4) /* warn */, &buf))

	th := NewThrottle(time.Hour)
	for i := 0; i < 3; i++ {
		th.Warnf(ctx, "same-key", "recurring failure #%d", i)
	}
	if got := strings.Count(buf.String(), "recurring failure"); got != 1 {
		t.Fatalf("logged %d times, want exactly 1:\n%s", got, buf.String())
	}
	// Distinct keys are independent, so one noisy site cannot mask another.
	th.Warnf(ctx, "other-key", "a different failure")
	if !strings.Contains(buf.String(), "a different failure") {
		t.Fatalf("an unrelated key was suppressed:\n%s", buf.String())
	}
}

func TestThrottle_ConcurrentAllowLetsExactlyOneThrough(t *testing.T) {
	th := NewThrottle(time.Hour)
	const goroutines = 64
	var wg sync.WaitGroup
	var allowed int64
	var mu sync.Mutex
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if th.Allow("contended") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 1 {
		t.Fatalf("%d goroutines were allowed, want exactly 1", allowed)
	}
}
