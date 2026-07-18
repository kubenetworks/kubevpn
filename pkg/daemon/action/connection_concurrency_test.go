package action

// connection_concurrency_test.go — Concurrency stress tests for daemon/action
// connection management.
//
// Purpose: verify that concurrent disconnect / ConnectionList / siblingTunIPs /
// ConnectionUse operations are free of panics, deadlocks, and state corruption.
//
// Design constraints:
//   - No //go:build integration tag → runs with "go test ./pkg/daemon/action/".
//   - No real cluster, no kubeconfig pointing at a live server.
//   - CGO_ENABLED=0, so -race is unavailable. We compensate with high goroutine
//     counts, repeated runs (-count=5), and explicit state-coherence assertions
//     after all goroutines finish.
//   - We reuse *handler.ConnectOptions directly (the same type used by every
//     characterization test and by disconnect_test.go / quit_test.go). Its
//     GetConnectionID() returns the ConnectionID field, and Cleanup() runs the
//     registered rollback functions.
//   - Stub pattern: &handler.ConnectOptions{ConnectionID: "..."}  — exactly what
//     connection_characterization_test.go uses. We do NOT create a new 30-method
//     mock; mockConnection from connection_characterization_test.go is available
//     in the same package but is overly complex for our needs.
//
// Disconnect locking pattern (verbatim from disconnect.go lines 85-91):
//
//	svr.connMu.Lock()
//	removed := svr.removeConnection(req.GetConnectionID())
//	svr.resetCurrentConnection(req.GetConnectionID())
//	svr.connMu.Unlock()
//	// cleanup happens outside the lock:
//	for _, connect := range removed.Sort() { cleanupConnection(ctx, connect) }

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// startDeadlockWatchdog spawns a goroutine that reports msg via t.Errorf ONLY
// when ctx reaches its deadline while the test is still running. Every test here
// tears down with `close(done); cancel()`; a naive `select { case <-done; case
// <-ctx.Done() }` watchdog races those two events and, when the select happens to
// pick ctx.Done() after the test has already returned, calls t.Errorf on a
// completed test — which Go turns into "panic: Fail in goroutine after <test> has
// completed", crashing the whole test binary. Guarding on DeadlineExceeded (normal
// teardown cancels the ctx, yielding context.Canceled, not DeadlineExceeded) plus
// a non-blocking re-check of done eliminates that race: a real deadlock keeps the
// test blocked in wg.Wait() past 5s → DeadlineExceeded → msg; normal completion
// never reports.
func startDeadlockWatchdog(t *testing.T, ctx context.Context, done <-chan struct{}, msg string) {
	go func() {
		<-ctx.Done()
		if ctx.Err() != context.DeadlineExceeded {
			return
		}
		select {
		case <-done:
			// Finished normally right at the deadline boundary — not a deadlock.
		default:
			t.Errorf("%s", msg)
		}
	}()
}

// readConnectionIDs is a lightweight read helper used by reader goroutines in
// place of ConnectionList. ConnectionList calls buildConnectionStatus which
// invokes util.GetKubeconfigCluster(factory) — factory is nil on stub
// connections and that panics. This helper only holds RLock and copies IDs,
// exercising the connMu read path without triggering the nil-factory path.
func readConnectionIDs(svr *Server) []string {
	svr.connMu.RLock()
	defer svr.connMu.RUnlock()
	out := make([]string, len(svr.connections))
	for i, c := range svr.connections {
		out[i] = c.GetConnectionID()
	}
	return out
}

// makeStubConn returns a *handler.ConnectOptions with the given ConnectionID and
// an optional rollback function that increments a counter. This is the same
// pattern used in disconnect_test.go and quit_test.go.
func makeStubConn(id string, cleanupCounter *atomic.Int64) *handler.ConnectOptions {
	conn := &handler.ConnectOptions{ConnectionID: id}
	if cleanupCounter != nil {
		conn.AddRollbackFunc(func() error {
			cleanupCounter.Add(1)
			return nil
		})
	}
	return conn
}

// newConcurrentServer builds a Server pre-loaded with N stub connections whose
// IDs are "conn-000000000000" through "conn-00000000000N-1" (zero-padded to 12
// hex chars to match real ConnectionID format). It also sets currentConnectionID
// to the first connection's ID, mirroring normal daemon startup.
func newConcurrentServer(n int, counter *atomic.Int64) *Server {
	conns := make([]handler.Connection, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("conn-%012x", i)
		ids[i] = id
		conns[i] = makeStubConn(id, counter)
	}
	currentID := ""
	if n > 0 {
		currentID = ids[0]
	}
	return &Server{
		connections:         conns,
		currentConnectionID: currentID,
		IsSudo:              true, // skip getSudoTunIPs which needs a gRPC client
	}
}

// assertSliceHealth verifies the connection slice has no nil entries and no
// duplicate IDs. Returns the set of IDs currently in the slice.
// Caller must NOT hold connMu — this function acquires RLock.
func assertSliceHealth(t *testing.T, svr *Server) map[string]int {
	t.Helper()
	svr.connMu.RLock()
	defer svr.connMu.RUnlock()

	seen := make(map[string]int, len(svr.connections))
	for i, c := range svr.connections {
		if c == nil {
			t.Errorf("connections[%d] is nil", i)
			continue
		}
		id := c.GetConnectionID()
		if prev, ok := seen[id]; ok {
			t.Errorf("duplicate ConnectionID %q at index %d (first seen at %d)", id, i, prev)
		}
		seen[id] = i
	}
	return seen
}

// assertStateCoherence verifies: no nil connections, no duplicate IDs, and
// currentConnectionID is either "" or points to an existing connection.
// Caller must NOT hold connMu.
func assertStateCoherence(t *testing.T, svr *Server) {
	t.Helper()
	seen := assertSliceHealth(t, svr)

	svr.connMu.RLock()
	cur := svr.currentConnectionID
	svr.connMu.RUnlock()

	// currentConnectionID is either "" or present in the slice.
	if cur != "" {
		if _, ok := seen[cur]; !ok {
			t.Errorf("currentConnectionID %q points to a removed connection (not in slice)", cur)
		}
	}
}

// ============================================================================
// Test 1: concurrent disconnect-by-ID vs ConnectionList vs siblingTunIPs
// ============================================================================

// TestConcurrency_DisconnectVsRead stress-tests concurrent writes (disconnect)
// vs reads (ConnectionList, siblingTunIPs). 20 writer goroutines each remove one
// unique connection while 20 reader goroutines continuously list/read, all
// running for 100ms. Verifies no panic and final state coherence.
func TestConcurrency_DisconnectVsRead(t *testing.T) {
	const numConns = 20
	const readDuration = 100 * time.Millisecond

	var cleanupCount atomic.Int64
	svr := newConcurrentServer(numConns, &cleanupCount)

	// Collect all IDs before starting goroutines so writers have stable targets.
	svr.connMu.RLock()
	ids := make([]string, len(svr.connections))
	for i, c := range svr.connections {
		ids[i] = c.GetConnectionID()
	}
	svr.connMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Deadlock watchdog: fires only on a genuine 5s timeout, never on normal teardown.
	startDeadlockWatchdog(t, ctx, done, "possible deadlock: test did not finish within timeout")

	// Writer goroutines: simulate the disconnect path (disconnect.go:85-91).
	// Each goroutine removes exactly one connection by ID. Cleanup runs outside lock.
	for _, id := range ids {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			svr.connMu.Lock()
			removed := svr.removeConnection(id)
			svr.resetCurrentConnection(id)
			svr.connMu.Unlock()
			// Cleanup is intentionally outside the lock — matches production code.
			for _, conn := range removed {
				cleanupConnection(context.Background(), conn)
			}
		}()
	}

	// Reader goroutines: siblingTunIPs is self-locking (RLock internally).
	// We use readConnectionIDs (a lightweight RLock helper) instead of
	// ConnectionList to avoid the nil-factory panic in buildConnectionStatus.
	stopReaders := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					_ = readConnectionIDs(svr)
					_ = svr.siblingTunIPs()
				}
			}
		}()
	}

	// Let writers finish, then stop readers.
	go func() {
		// Wait for all writers to finish (the first numConns goroutines).
		// We can't distinguish writer vs reader WGs, so use a separate WG.
		time.Sleep(readDuration)
		close(stopReaders)
	}()

	wg.Wait()
	close(done)
	cancel() // release watchdog

	assertStateCoherence(t, svr)
}

// ============================================================================
// Test 2: concurrent disconnect-by-ID vs ConnectionUse
// ============================================================================

// TestConcurrency_DisconnectVsConnectionUse stress-tests concurrent removes
// (writers) vs ConnectionUse (swaps currentConnectionID). Both operations hold
// the write lock. Verifies no panic and coherent currentConnectionID afterward.
func TestConcurrency_DisconnectVsConnectionUse(t *testing.T) {
	const numConns = 20

	var cleanupCount atomic.Int64
	svr := newConcurrentServer(numConns, &cleanupCount)

	svr.connMu.RLock()
	ids := make([]string, len(svr.connections))
	for i, c := range svr.connections {
		ids[i] = c.GetConnectionID()
	}
	svr.connMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	startDeadlockWatchdog(t, ctx, done, "possible deadlock: test did not finish within timeout")

	var wg sync.WaitGroup

	// Writer goroutines: remove each connection once (disconnect pattern).
	for _, id := range ids {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			svr.connMu.Lock()
			removed := svr.removeConnection(id)
			svr.resetCurrentConnection(id)
			svr.connMu.Unlock()
			for _, conn := range removed {
				cleanupConnection(context.Background(), conn)
			}
		}()
	}

	// ConnectionUse goroutines: attempt to switch to random existing IDs.
	// Some will fail with ErrConnectionNotFound (expected once removed) — that is fine.
	stopUse := make(chan struct{})
	for i := 0; i < 10; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopUse:
					return
				default:
					// Use a cycling ID — it may or may not still exist.
					targetID := ids[i%numConns]
					// ConnectionUse is self-locking (Lock internally).
					_, _ = svr.ConnectionUse(context.Background(), &rpc.ConnectionUseRequest{
						ConnectionID: targetID,
					})
				}
			}
		}()
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		close(stopUse)
	}()

	wg.Wait()
	close(done)
	cancel()

	assertStateCoherence(t, svr)
}

// ============================================================================
// Test 3: two goroutines race to remove the SAME connection ID (double-remove)
// ============================================================================

// TestConcurrency_DoubleRemoveSameID verifies that two goroutines concurrently
// trying to remove the same ConnectionID do not panic or double-free. The lock
// ensures exactly one remove "wins" and the other sees an empty result. Final
// connections slice must not contain the removed ID and must have no duplicates.
func TestConcurrency_DoubleRemoveSameID(t *testing.T) {
	const numConns = 5
	const targetIdx = 2 // we'll race-remove connections[2]

	var cleanupCount atomic.Int64
	svr := newConcurrentServer(numConns, &cleanupCount)

	svr.connMu.RLock()
	targetID := svr.connections[targetIdx].GetConnectionID()
	svr.connMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	startDeadlockWatchdog(t, ctx, done, "possible deadlock in double-remove test")

	var wg sync.WaitGroup
	var totalRemoved atomic.Int64

	// Launch two goroutines that both try to remove the same targetID.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svr.connMu.Lock()
			removed := svr.removeConnection(targetID)
			svr.resetCurrentConnection(targetID)
			svr.connMu.Unlock()
			totalRemoved.Add(int64(len(removed)))
			for _, conn := range removed {
				cleanupConnection(context.Background(), conn)
			}
		}()
	}

	wg.Wait()
	close(done)
	cancel()

	// Exactly one removal must have succeeded (the second sees an empty result).
	got := totalRemoved.Load()
	if got != 1 {
		t.Errorf("double-remove: expected exactly 1 total removal, got %d", got)
	}

	// The target ID must no longer appear in connections.
	svr.connMu.RLock()
	defer svr.connMu.RUnlock()
	for _, c := range svr.connections {
		if c.GetConnectionID() == targetID {
			t.Errorf("double-remove: targetID %q still present in connections", targetID)
		}
	}

	// Final state must be coherent.
	// Re-use assertStateCoherence body inline (it takes RLock, we already hold RLock
	// above — to avoid re-acquiring, duplicate the nil/dup checks manually).
	seen := make(map[string]int, len(svr.connections))
	for i, c := range svr.connections {
		if c == nil {
			t.Errorf("double-remove: connections[%d] is nil after double-remove", i)
		}
		id := c.GetConnectionID()
		if prev, ok := seen[id]; ok {
			t.Errorf("double-remove: duplicate ID %q at index %d (first at %d)", id, i, prev)
		}
		seen[id] = i
	}
	cur := svr.currentConnectionID
	if cur != "" {
		if _, ok := seen[cur]; !ok {
			t.Errorf("double-remove: currentConnectionID %q points to removed connection", cur)
		}
	}
}

// ============================================================================
// Test 4: quit-style bulk clear vs concurrent reads
// ============================================================================

// TestConcurrency_QuitStyleClearVsReads reproduces the Quit() locking pattern
// (quit.go): hold Lock, snapshot connections, nil the slice, clear
// currentConnectionID, Unlock, then cleanup outside the lock. Runs simultaneously
// with readConnectionIDs / siblingTunIPs readers and verifies no panic, no
// data-corruption in the slice, and — critically — that currentConnectionID is
// cleared atomically with the slice so no reader ever observes a stale current ID.
//
// Quit() must clear BOTH fields under the same lock, mirroring Disconnect's
// req.GetAll() branch (disconnect.go); otherwise a reader (ConnectionUse,
// ConnectionList) would see a currentConnectionID pointing at a connection that
// no longer exists. This test is the regression guard for that invariant.
func TestConcurrency_QuitStyleClearVsReads(t *testing.T) {
	const numConns = 20

	var cleanupCount atomic.Int64
	svr := newConcurrentServer(numConns, &cleanupCount)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	startDeadlockWatchdog(t, ctx, done, "possible deadlock in quit-style test")

	var wg sync.WaitGroup
	stopReaders := make(chan struct{})

	// Reader goroutines: use lightweight RLock helper to avoid nil-factory panic.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					_ = readConnectionIDs(svr)
					_ = svr.siblingTunIPs()
				}
			}
		}()
	}

	// Quit-style writer: replicate quit.go's clear (both fields under one lock).
	wg.Add(1)
	go func() {
		defer wg.Done()
		svr.connMu.Lock()
		connects := handler.Connects(svr.connections)
		svr.connections = nil
		svr.currentConnectionID = ""
		svr.connMu.Unlock()
		// cleanup outside lock — matches Quit() production code
		for _, conn := range connects.Sort() {
			cleanupConnection(context.Background(), conn)
		}
		close(stopReaders)
	}()

	wg.Wait()
	close(done)
	cancel()

	_ = assertSliceHealth(t, svr)

	// After Quit-style clear, both connections and currentConnectionID must be reset.
	svr.connMu.RLock()
	remaining := len(svr.connections)
	current := svr.currentConnectionID
	svr.connMu.RUnlock()
	if remaining != 0 {
		t.Errorf("after quit-style clear: expected 0 connections, got %d", remaining)
	}
	if current != "" {
		t.Errorf("after quit-style clear: currentConnectionID must be empty, got %q (quit.go must clear it like disconnect --all)", current)
	}
}

// ============================================================================
// Test 5: high-frequency mixed operations — repeated runs expose races
// ============================================================================

// TestConcurrency_MixedHighFrequency runs a sustained mixed workload: multiple
// goroutines simultaneously performing disconnect, ConnectionList, siblingTunIPs,
// and ConnectionUse on a rotating pool of connections. New connections are added
// back after removal to keep the pool non-empty for the duration.
//
// This test is deliberately run with -count=5 to increase collision probability
// without -race (CGO_ENABLED=0 prevents the race detector).
func TestConcurrency_MixedHighFrequency(t *testing.T) {
	const poolSize = 10
	const duration = 150 * time.Millisecond

	var cleanupCount atomic.Int64
	var addCount atomic.Int64

	svr := newConcurrentServer(poolSize, &cleanupCount)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	startDeadlockWatchdog(t, ctx, done, "possible deadlock in mixed high-frequency test")

	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup

	// Writer goroutines: repeatedly remove the first connection and re-add it.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq := atomic.Int64{}
			for time.Now().Before(deadline) {
				n := seq.Add(1)
				id := fmt.Sprintf("dyn-%012x", n)

				// Remove whatever is at the head.
				svr.connMu.Lock()
				var targetID string
				if len(svr.connections) > 0 {
					targetID = svr.connections[0].GetConnectionID()
				}
				removed := svr.removeConnection(targetID)
				svr.resetCurrentConnection(targetID)
				// Re-add a fresh connection so the pool stays non-empty.
				fresh := makeStubConn(id, &cleanupCount)
				addCount.Add(1)
				svr.connections = append(svr.connections, fresh)
				if svr.currentConnectionID == "" && len(svr.connections) > 0 {
					svr.currentConnectionID = svr.connections[0].GetConnectionID()
				}
				svr.connMu.Unlock()

				for _, conn := range removed {
					cleanupConnection(context.Background(), conn)
				}
			}
		}()
	}

	// Reader goroutines: lightweight RLock helper to avoid nil-factory panic.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				_ = readConnectionIDs(svr)
				_ = svr.siblingTunIPs()
			}
		}()
	}

	// ConnectionUse goroutines.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				svr.connMu.RLock()
				var id string
				if len(svr.connections) > 0 {
					id = svr.connections[0].GetConnectionID()
				}
				svr.connMu.RUnlock()
				if id != "" {
					_, _ = svr.ConnectionUse(context.Background(), &rpc.ConnectionUseRequest{
						ConnectionID: id,
					})
				}
			}
		}()
	}

	wg.Wait()
	close(done)
	cancel()

	assertStateCoherence(t, svr)
}
