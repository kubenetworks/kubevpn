package handler

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
			nm.transportLastReply = func() time.Time {
				if silent.Load() {
					return time.Now().Add(-time.Minute)
				}
				return time.Now()
			}
			nm.launchTransportSupervisor()
			ctx, cancel := context.WithTimeout(nm.ctx, 8*time.Second)
			defer cancel()
			g, e := nm.waitTransport(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if mode != "unarmed" {
				nm.armTransportHealth()
			}
			if mode == "silent" {
				// Synchronize a probe event before moving the clock source into the past.
				nm.requestNetworkCheck()
				time.Sleep(30 * time.Millisecond)
				silent.Store(true)
				nm.requestNetworkCheck()
				if !waitCancelled(g.ctx, 20*time.Second) {
					t.Fatal("silent path not recovered after probe grace")
				}
			} else {
				if mode == "stale-event" {
					nm.networkChecks <- g.id + 1
				}
				for range 5 {
					nm.requestNetworkCheck()
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

func TestNewNetworkManager_AllocatesHeartbeatStats(t *testing.T) {
	nm := newNetworkManager(NetworkConfig{})
	if nm.heartbeatStats == nil || !nm.heartbeatStats.LastReply().IsZero() {
		t.Fatal("missing fresh heartbeat statistics")
	}
}
