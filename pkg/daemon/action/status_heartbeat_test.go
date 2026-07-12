package action

import (
	"testing"
	"time"
)

func TestDeriveConnectionStatus(t *testing.T) {
	fresh := time.Now()
	stale := time.Now().Add(-2 * heartbeatStaleThreshold)

	tests := []struct {
		name          string
		tunUp         bool
		lastHeartbeat time.Time
		want          string
	}{
		{"no tun -> disconnected", false, fresh, StatusFailed},
		{"no tun ignores heartbeat", false, time.Time{}, StatusFailed},
		{"tun + fresh heartbeat -> connected", true, fresh, StatusOk},
		{"tun + stale heartbeat -> unhealthy", true, stale, StatusUnhealthy},
		{"tun + never heartbeat -> unhealthy", true, time.Time{}, StatusUnhealthy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveConnectionStatus(tt.tunUp, tt.lastHeartbeat); got != tt.want {
				t.Errorf("deriveConnectionStatus(%v, %v) = %q, want %q",
					tt.tunUp, tt.lastHeartbeat, got, tt.want)
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
