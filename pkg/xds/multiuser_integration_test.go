package xds

import (
	"context"
	"net"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// userSession represents a user going through the full kubevpn lifecycle.
type userSession struct {
	name    string
	ownerID string
	tunIPv4 string
	tunIPv6 string
	headers map[string]string
}

// ipOnly returns the bare IP portion of s, stripping any "/prefix" CIDR suffix.
// envoy rule LocalTunIPv4/v6 must be a bare IP — it flows into envoy's
// SocketAddress.Address via tunIPs→toEndPoint (cache.go), which rejects CIDR
// notation. GetTunIP returns IPNet.String() (e.g. "198.18.0.1/32"); this helper
// converts that to the bare-IP form the rest of the codebase (cache_test.go,
// inject_service_test.go, tun_config_sync_test.go) and production envoy config use.
func ipOnly(s string) string {
	if ip, _, err := net.ParseCIDR(s); err == nil {
		return ip.String()
	}
	return s
}

func addVirtualRuleInternal(v []*Virtual, u *userSession, nodeID, ns string) []*Virtual {
	newRule := &Rule{
		Headers:      u.headers,
		LocalTunIPv4: ipOnly(u.tunIPv4),
		LocalTunIPv6: ipOnly(u.tunIPv6),
		OwnerID:      u.ownerID,
	}
	for i, virtual := range v {
		if virtual.UID == nodeID && virtual.Namespace == ns {
			for j, rule := range virtual.Rules {
				if rule.OwnerID == u.ownerID {
					v[i].Rules[j] = newRule
					return v
				}
			}
			v[i].Rules = append(v[i].Rules, newRule)
			return v
		}
	}
	return append(v, &Virtual{
		SchemaVersion: CurrentSchemaVersion,
		UID:           nodeID,
		Namespace:     ns,
		Ports:         []ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
		Rules:         []*Rule{newRule},
	})
}

// ============================================================================
// Integration Test: Full multi-user lifecycle across DHCP + envoy rules
//
// Three users (Alice, Bob, Carol) share a cluster:
//   Phase 1: allocate IPs via gRPC DHCP → all get unique IPs
//   Phase 2: all proxy same workload → 3 envoy rules in ConfigMap
//   Phase 3: Bob leaves → his rule removed, Alice+Carol IPs unchanged
//   Phase 4: Alice crashes, reconnects → same OwnerID, new IP, rule updated
//   Phase 5: all disconnect → rules cleaned
// ============================================================================

func TestIntegration_MultiUser_IPConflict(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Alice excludes 198.18.0.1 (local interface conflict)
	aliceResp, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{
		OwnerID: "alice-conflict", Namespace: "test-ns", ExcludeIPs: []string{"198.18.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	aliceIP := parseV4(t, aliceResp)
	if aliceIP.Equal(net.ParseIP("198.18.0.1")) {
		t.Fatal("Alice should NOT get excluded IP")
	}

	// Bob has no exclusions
	bobResp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "bob-conflict1", Namespace: "test-ns"})
	bobIP := parseV4(t, bobResp)

	if aliceIP.Equal(bobIP) {
		t.Fatalf("Alice and Bob got same IP: %s", aliceIP)
	}

	// Both renew independently, get same IPs back
	aliceRenew, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice-conflict", Namespace: "test-ns"})
	bobRenew, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "bob-conflict1", Namespace: "test-ns"})
	if aliceRenew.IPv4 != aliceResp.IPv4 || bobRenew.IPv4 != bobResp.IPv4 {
		t.Fatal("renew should return same IPs")
	}
}

// ============================================================================
// Integration Test: One user's lease expires, other's IP unaffected
// ============================================================================
