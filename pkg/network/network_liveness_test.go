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
			if mode == "silent" {
				threshold := livenessSteadyThreshold
				livenessSteadyThreshold = 3 * time.Second
				// Registered before the fixtures so their workers stop before restoration.
				t.Cleanup(func() { livenessSteadyThreshold = threshold })
			}
			srv, count := transportForwardServer(t, nil)
			nm := transportManager(t, srv.URL)
			var silent atomic.Bool
			var consulted atomic.Int32
			nm.transport.lastReply = func() time.Time {
				if silent.Load() {
					return time.Now().Add(-time.Minute)
				}
				// Stay strictly after arming even when the platform clock has coarse resolution.
				last := time.Now().Add(time.Second)
				consulted.Add(1)
				return last
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
				// A second callback proves the monitor processed the first fresh reply.
				monitorTickWait(t, func() bool {
					nm.transport.requestNetworkCheck()
					return consulted.Load() >= 2
				})
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
