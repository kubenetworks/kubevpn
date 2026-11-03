package network

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func waitCancelled(ctx context.Context, timeout time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(timeout):
		return false
	}
}

// Drive the real supervisor and real SPDY transport; only heartbeat timestamps
// are controlled here (correlated packet generation is covered in core).
func TestTransportIntegration_HealthPolicy(t *testing.T) {
	for _, mode := range []string{"unarmed", "fresh", "silent", "stale-event"} {
		t.Run(mode, func(t *testing.T) {
			srv, count := transportForwardServer(t, nil)
			nm := transportManager(t, srv.URL)
			var silent atomic.Bool
			var consulted atomic.Int32
			nm.transport.lastReply = func() time.Time {
				if silent.Load() {
					return time.Now().Add(-time.Minute)
				}
				// Counted only when provably fresh (after the silent check), so a
				// non-zero count proves the monitor loop top already evaluated a
				// fresh lastReply — the precondition for arming the probe budget.
				consulted.Add(1)
				return time.Now()
			}
			nm.transport.launchTransportSupervisor()
			ctx, cancel := context.WithTimeout(nm.transport.ctx, 8*time.Second)
			defer cancel()
			g, e := nm.transport.waitTransport(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if mode != "unarmed" {
				nm.transport.armTransportHealth()
			}
			if mode == "silent" {
				// Shrink the retire threshold: 3×HeartbeatInterval (15s) plus slow-runner
				// drift eats most of the 20s budget below; 3s retires within one monitor
				// tick and leaves ample margin. Same override pattern as
				// TestNetworkMonitorIntegration_UnhealthyChangeRecoversOnlyAffectedManager.
				threshold := livenessSteadyThreshold
				livenessSteadyThreshold = 3 * time.Second
				defer func() { livenessSteadyThreshold = threshold }()
				// Synchronize before moving the clock source into the past: wait until
				// the monitor loop has consulted a fresh lastReply, so the next checks
				// event observes primed=true and arms the probe budget (a fixed sleep
				// races the goroutine's first evaluation on slow runners).
				nm.transport.requestNetworkCheck()
				monitorTickWait(t, func() bool { return consulted.Load() > 0 })
				silent.Store(true)
				nm.transport.requestNetworkCheck()
				if !waitCancelled(g.ctx, 20*time.Second) {
					t.Fatal("silent path not recovered after probe grace")
				}
			} else {
				if mode == "stale-event" {
					nm.transport.checks <- g.id + 1
				}
				for range 5 {
					nm.transport.requestNetworkCheck()
				}
				select {
				case <-g.ctx.Done():
					t.Fatal("healthy or unarmed session retired")
				case <-time.After(100 * time.Millisecond):
				}
				if count.Load() != 1 {
					t.Fatal("healthy current transport not sticky")
				}
			}
			nm.Stop()
		})
	}
}

func TestNewRuntime_AllocatesHeartbeatStats(t *testing.T) {
	nm := New(Config{})
	if nm.transport.heartbeatStats == nil || !nm.transport.heartbeatStats.LastReply().IsZero() {
		t.Fatal("missing fresh heartbeat statistics")
	}
}
