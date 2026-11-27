package xds

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// tunAllocsEntry mirrors leases.persistedAlloc for reading the TUN_ALLOCS
// ConfigMap key back in tests.
type tunAllocsEntry struct {
	IPv4      string `json:"ipv4"`
	Version   int64  `json:"version"`
	LastRenew int64  `json:"lastRenew"`
	Hostname  string `json:"hostname,omitempty"`
	IsAgent   bool   `json:"isAgent,omitempty"`
}

// waitForTunAllocsEntry polls the TUN_ALLOCS ConfigMap key until ownerID has an
// entry (saveAllocs runs synchronously inside GetTunIP, so this converges fast;
// the poll just avoids a fixed sleep on slow CI).
func waitForTunAllocsEntry(t *testing.T, env *testEnv, ownerID string) tunAllocsEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cm, err := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err == nil && cm.Data[config.KeyTunAllocs] != "" {
			var allocs map[string]tunAllocsEntry
			if uerr := yaml.Unmarshal([]byte(cm.Data[config.KeyTunAllocs]), &allocs); uerr == nil {
				if entry, ok := allocs[ownerID]; ok {
					return entry
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("TUN_ALLOCS entry for owner %s did not appear within 2s", ownerID)
	return tunAllocsEntry{}
}

// TestIntegration_AgentLeaseIdentity wires the full path a node-agent tunnel
// takes: the agent vpn container's kubevpn server parses agent=<podUID> from
// its -l URL and issues GetTunIP with OwnerID=<podUID> IsAgent=true, so the
// manager's lease store (persisted TUN_ALLOCS) records who the tunnel is.
//
// Phase 1: agent lease lands in the agent pool.
// Phase 2: lease store records IsAgent=true + OwnerID=<podUID>.
// Phase 3: heartbeat renewal keeps the same IP and identity.
// Phase 4: a non-agent (user) lease is unaffected — user pool, IsAgent=false.
func TestIntegration_AgentLeaseIdentity(t *testing.T) {
	env := newTestEnv(t)
	const podUID = "0f2a3b4c-1d2e-3f40-agent-pod-uid"

	// Phase 1: agent lease — allocated from the dedicated agent pool.
	agentResp, err := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   podUID,
		Namespace: "test-ns",
		IsAgent:   true,
	})
	if err != nil {
		t.Fatalf("agent GetTunIP: %v", err)
	}
	agentIP := parseV4(t, agentResp)
	if !config.CIDRAgent.Contains(agentIP) {
		t.Fatalf("agent IP %s not in agent pool %s", agentIP, config.CIDRAgent)
	}
	if config.CIDRUser.Contains(agentIP) {
		t.Fatalf("agent IP %s must not come from the user pool %s", agentIP, config.CIDRUser)
	}

	// Phase 2: the lease store identifies the agent.
	entry := waitForTunAllocsEntry(t, env, podUID)
	if !entry.IsAgent {
		t.Errorf("TUN_ALLOCS entry for %s has isAgent=false, want true", podUID)
	}
	if entry.IPv4 != agentResp.IPv4 {
		t.Errorf("TUN_ALLOCS entry IPv4 = %q, want %q", entry.IPv4, agentResp.IPv4)
	}

	// Phase 3: heartbeat renewal — same request renews in place, keeping the IP.
	renewResp, err := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   podUID,
		Namespace: "test-ns",
		IsAgent:   true,
	})
	if err != nil {
		t.Fatalf("agent lease renewal: %v", err)
	}
	if renewResp.IPv4 != agentResp.IPv4 {
		t.Errorf("renewal changed the agent IP: %s vs %s", agentResp.IPv4, renewResp.IPv4)
	}

	// Phase 4: non-agent connection is unaffected — user pool, isAgent=false.
	userResp, err := env.client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   "user-client-1",
		Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("user GetTunIP: %v", err)
	}
	userIP := parseV4(t, userResp)
	if !config.CIDRUser.Contains(userIP) {
		t.Fatalf("user IP %s not in user pool %s", userIP, config.CIDRUser)
	}
	userEntry := waitForTunAllocsEntry(t, env, "user-client-1")
	if userEntry.IsAgent {
		t.Errorf("TUN_ALLOCS entry for user-client-1 has isAgent=true, want false")
	}
	if userEntry.IPv4 != userResp.IPv4 {
		t.Errorf("TUN_ALLOCS entry IPv4 = %q, want %q", userEntry.IPv4, userResp.IPv4)
	}
}
