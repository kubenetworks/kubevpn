package handler

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestCleanup_RetriesWhenCleanupFnFails locks in the core reason SessionBase.cleanup
// uses a mutex+flag instead of sync.Once: a failed cleanupFn must NOT mark the
// session cleaned, so a later call can retry and succeed.
func TestCleanup_RetriesWhenCleanupFnFails(t *testing.T) {
	sb := &SessionBase{}
	calls := 0
	fn := func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		return nil
	}

	sb.cleanup(context.Background(), fn)
	if sb.cleanedUp {
		t.Fatal("cleanedUp must be false after a failed cleanupFn (retry required)")
	}

	sb.cleanup(context.Background(), fn)
	if !sb.cleanedUp {
		t.Fatal("cleanedUp must be true after a successful retry")
	}
	if calls != 2 {
		t.Fatalf("cleanupFn should have run twice (fail then succeed), got %d", calls)
	}

	// Once cleaned, further calls are gated out.
	sb.cleanup(context.Background(), fn)
	if calls != 2 {
		t.Fatalf("cleanupFn must not run again after success, got %d calls", calls)
	}
}

// TestCleanup_ConcurrentAddRollbackAndCleanup exercises the rollbackList mutex and
// the cleanup gate under concurrency. CGO is disabled (no -race), so we assert on
// invariants: no panic and a consistent registry snapshot.
func TestCleanup_ConcurrentAddRollbackAndCleanup(t *testing.T) {
	sb := &SessionBase{}
	const n = 100

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sb.AddRollbackFunc(func() error { return nil })
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		sb.cleanup(context.Background(), func(context.Context) error {
			_ = sb.getRollbackFuncs()
			return nil
		})
	}()
	wg.Wait()

	if got := len(sb.getRollbackFuncs()); got != n {
		t.Fatalf("expected %d rollback funcs registered, got %d", n, got)
	}
}
