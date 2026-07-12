package handler

import (
	"context"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
)

// waitCancelled reports whether ctx is cancelled within timeout.
func waitCancelled(ctx context.Context, timeout time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(timeout):
		return false
	}
}

// TestWatchLiveness_NeverPrimedTriggersReconnect: a session that never sees a fresh heartbeat echo
// reply (LastReply stays zero) is treated as black-holed and force-reconnected after startupDeadline.
func TestWatchLiveness_NeverPrimedTriggersReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	neverReplies := func() time.Time { return time.Time{} }
	// startupDeadline small (50ms), steadyThreshold large (irrelevant here).
	go watchLiveness(ctx, cancel, time.Now(), 10*time.Millisecond, 50*time.Millisecond, time.Second, neverReplies)

	if !waitCancelled(ctx, time.Second) {
		t.Fatal("expected a never-primed (black-holed) session to be force-reconnected, but it was not")
	}
}

// TestWatchLiveness_PrimedThenSilentTriggersReconnect: a session that produced a fresh reply (primed)
// then went silent is torn down once the reply ages past steadyThreshold.
func TestWatchLiveness_PrimedThenSilentTriggersReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stats := &core.HeartbeatStats{}
	sessionStart := time.Now().Add(-time.Millisecond) // slightly in the pass so MarkReply is strictly After
	stats.MarkReply()                                 // one fresh reply after sessionStart → primes, then never again

	// startupDeadline large (so it does not fire), steadyThreshold small (50ms).
	go watchLiveness(ctx, cancel, sessionStart, 10*time.Millisecond, time.Second, 50*time.Millisecond, stats.LastReply)

	if !waitCancelled(ctx, time.Second) {
		t.Fatal("expected a primed-then-silent session to be reconnected, but it was not")
	}
}

// TestWatchLiveness_FreshHeartbeatKeepsSession: while heartbeat echo replies keep arriving, the
// watchdog must not tear the session down (neither startup nor steady deadline fires).
func TestWatchLiveness_FreshHeartbeatKeepsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stats := &core.HeartbeatStats{}
	sessionStart := time.Now()
	stats.MarkReply()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				stats.MarkReply()
			}
		}
	}()

	go watchLiveness(ctx, cancel, sessionStart, 10*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond, stats.LastReply)

	if waitCancelled(ctx, 300*time.Millisecond) {
		t.Fatal("watchdog tore down a healthy session that kept receiving heartbeat replies")
	}
}

// TestWatchLiveness_StaleReplyDoesNotPrime: a reply older than sessionStart (left over from a
// previous session, LastReply is shared) must NOT count as priming this session — an otherwise
// black-holed reconnect is still detected via startupDeadline.
func TestWatchLiveness_StaleReplyDoesNotPrime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stats := &core.HeartbeatStats{}
	stats.MarkReply()          // reply happens BEFORE sessionStart → stale for this session
	sessionStart := time.Now() // this session starts after the stale reply

	go watchLiveness(ctx, cancel, sessionStart, 10*time.Millisecond, 50*time.Millisecond, time.Second, stats.LastReply)

	if !waitCancelled(ctx, time.Second) {
		t.Fatal("a stale pre-session reply must not prime; the black-holed session should reconnect")
	}
}

// TestNewNetworkManager_AllocatesHeartbeatStats: heartbeatStats is allocated at construction (before
// any goroutine) so the watchdog on the very first port-forward session — which spawns before
// startTUN — has a non-nil, race-free instance to observe rather than short-circuiting.
func TestNewNetworkManager_AllocatesHeartbeatStats(t *testing.T) {
	nm := newNetworkManager(NetworkConfig{})
	if nm.heartbeatStats == nil {
		t.Fatal("newNetworkManager must allocate heartbeatStats so the first-session watchdog is active")
	}
	if got := nm.heartbeatStats.LastReply(); !got.IsZero() {
		t.Fatalf("fresh heartbeatStats should report a zero LastReply, got %v", got)
	}
}

// TestWatchDataPlaneLiveness_NotReadyReturns: if the session never becomes ready and the context is
// cancelled (teardown), the watchdog must exit cleanly without spinning.
func TestWatchDataPlaneLiveness_NotReadyReturns(t *testing.T) {
	nm := newNetworkManager(NetworkConfig{}) // allocates heartbeatStats

	ctx, cancel := context.WithCancel(context.Background())
	neverReady := make(chan struct{})

	done := make(chan struct{})
	go func() { defer close(done); nm.watchDataPlaneLiveness(ctx, cancel, neverReady) }()

	cancel() // teardown before ready

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchDataPlaneLiveness should return when ctx is cancelled before ready")
	}
}
