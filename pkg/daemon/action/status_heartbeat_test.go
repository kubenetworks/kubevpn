package action

import (
	"testing"
	"time"
)

func TestDeriveConnectionStatus(t *testing.T) {
	fresh := time.Now()
	stale := time.Now().Add(-2 * heartbeatStaleThreshold)

	tests := []struct {
		name           string
		tunUp          bool
		lastHeartbeat  time.Time
		controlPlaneOK bool
		want           string
	}{
		{"no tun -> disconnected", false, fresh, true, StatusFailed},
		{"no tun ignores heartbeat", false, time.Time{}, true, StatusFailed},
		{"tun + fresh heartbeat + cp ok -> connected", true, fresh, true, StatusOk},
		{"tun + stale heartbeat -> unhealthy", true, stale, true, StatusUnhealthy},
		{"tun + never heartbeat -> unhealthy", true, time.Time{}, true, StatusUnhealthy},
		{"tun + fresh heartbeat + cp down -> unhealthy", true, fresh, false, StatusUnhealthy},
		{"no tun + cp down still disconnected", false, fresh, false, StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveConnectionStatus(tt.tunUp, tt.lastHeartbeat, tt.controlPlaneOK); got != tt.want {
				t.Errorf("deriveConnectionStatus(%v, %v, cp=%v) = %q, want %q",
					tt.tunUp, tt.lastHeartbeat, tt.controlPlaneOK, got, tt.want)
			}
		})
	}
}

// TestResolveStatus_UserDaemonReusesSudoVerdict verifies that when the sudo map carries a
// status, resolveStatus returns it verbatim (user daemon path), and falls back to disconnected
// when the connection is unknown to the sudo daemon.
func TestResolveStatus_UserDaemonReusesSudoVerdict(t *testing.T) {
	// connect is nil -> GetConnectionID() returns "" (nil-safe); key the map by "".
	ips := map[string]tunIP{"": {status: StatusOk}}
	if got := resolveStatus(nil, ips, true); got != StatusOk {
		t.Fatalf("expected sudo verdict %q, got %q", StatusOk, got)
	}

	// Non-empty map but connection absent -> disconnected.
	ips = map[string]tunIP{"other": {status: StatusOk}}
	if got := resolveStatus(nil, ips, true); got != StatusFailed {
		t.Fatalf("expected %q for unknown connection, got %q", StatusFailed, got)
	}
}
