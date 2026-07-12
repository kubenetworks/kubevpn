package handler

import (
	"context"
	"testing"
	"time"
)

// TestNetworkManager_ControlPlaneHealthToggle locks the A2 state machine: a NetworkManager
// starts optimistically healthy (so a not-yet-checked session is not demoted), and reflects the
// verdicts the xds health check feeds via setControlPlaneHealthy.
func TestNetworkManager_ControlPlaneHealthToggle(t *testing.T) {
	nm := newNetworkManager(NetworkConfig{})
	if !nm.ControlPlaneHealthy() {
		t.Fatal("newNetworkManager must start optimistically healthy")
	}
	nm.setControlPlaneHealthy(false)
	if nm.ControlPlaneHealthy() {
		t.Fatal("expected unhealthy after a failed control-plane check")
	}
	nm.setControlPlaneHealthy(true)
	if !nm.ControlPlaneHealthy() {
		t.Fatal("expected healthy again after control plane recovers")
	}

	// A DataSession with this nm mirrors the state; nil nm is treated as healthy (unknown).
	ds := &DataSession{nm: nm}
	if !ds.GetControlPlaneHealthy() {
		t.Fatal("DataSession should mirror nm's healthy state")
	}
	nm.setControlPlaneHealthy(false)
	if ds.GetControlPlaneHealthy() {
		t.Fatal("DataSession should mirror nm's unhealthy state")
	}
	if got := (&DataSession{}).GetControlPlaneHealthy(); !got {
		t.Fatal("DataSession with nil nm should report healthy (unknown → don't demote)")
	}
}

// TestHealthCheckLoop_ReportsHealthyOnSuccess verifies the loop feeds onHealth(true) after a
// successful check, so the control-plane verdict propagates to status.
func TestHealthCheckLoop_ReportsHealthyOnSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	close(ready) // port-forward already "ready" so the loop proceeds to checking immediately

	got := make(chan bool, 1)
	go healthCheckLoop(ctx, cancel, ready, func(ok bool) {
		if ok {
			select {
			case got <- true:
			default:
			}
		}
	}, func() error { return nil })

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("expected onHealth(true) after a successful control-plane check")
	}
}
