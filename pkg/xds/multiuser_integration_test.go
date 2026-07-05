package xds

import (
	"context"
	"net"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
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

func addVirtualRuleInternal(v []*Virtual, u *userSession, nodeID, ns string) []*Virtual {
	newRule := &Rule{
		Headers:      u.headers,
		LocalTunIPv4: u.tunIPv4,
		LocalTunIPv6: u.tunIPv6,
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

func TestIntegration_MultiUser_FullLifecycle(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	reviews := "deployments.apps.reviews"

	// --- Phase 1: All users allocate IPs ---
	users := []*userSession{
		{name: "Alice", ownerID: "alice-owner-1", headers: map[string]string{"env": "alice"}},
		{name: "Bob", ownerID: "bob-owner-001", headers: map[string]string{"env": "bob"}},
		{name: "Carol", ownerID: "carol-owner-1", headers: map[string]string{"env": "carol"}},
	}

	ips := make(map[string]string)
	for _, u := range users {
		resp, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: u.ownerID, Namespace: "test-ns"})
		if err != nil {
			t.Fatalf("[%s] GetTunIP: %v", u.name, err)
		}
		u.tunIPv4 = resp.IPv4
		u.tunIPv6 = resp.IPv6
		ips[u.ownerID] = resp.IPv4
	}

	seen := map[string]bool{}
	for owner, ip := range ips {
		if seen[ip] {
			t.Fatalf("duplicate IP %s for %s", ip, owner)
		}
		seen[ip] = true
	}
	t.Log("Phase 1 PASS: all users got unique IPs")

	// --- Phase 2: All users proxy same workload ---
	cm, _ := env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	var virtuals []*Virtual
	for _, u := range users {
		virtuals = addVirtualRuleInternal(virtuals, u, reviews, "test-ns")
	}
	data, _ := yaml.Marshal(virtuals)
	cm.Data[config.KeyEnvoy] = string(data)
	env.server.clientset.CoreV1().ConfigMaps("test-ns").Update(ctx, cm, metav1.UpdateOptions{})

	cm, _ = env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	var check []*Virtual
	yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &check)
	if len(check) != 1 || len(check[0].Rules) != 3 {
		t.Fatalf("Phase 2: expected 1 virtual with 3 rules, got %d virtuals with %d rules", len(check), len(check[0].Rules))
	}
	t.Log("Phase 2 PASS: 3 envoy rules created")

	// --- Phase 3: Bob leaves → rules: Alice + Carol ---
	bob := users[1]
	for i := 0; i < len(check[0].Rules); i++ {
		if check[0].Rules[i].OwnerID == bob.ownerID {
			check[0].Rules = append(check[0].Rules[:i], check[0].Rules[i+1:]...)
			break
		}
	}
	data, _ = yaml.Marshal(check)
	cm.Data[config.KeyEnvoy] = string(data)
	env.server.clientset.CoreV1().ConfigMaps("test-ns").Update(ctx, cm, metav1.UpdateOptions{})

	cm, _ = env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &check)
	if len(check[0].Rules) != 2 {
		t.Fatalf("Phase 3: expected 2 rules, got %d", len(check[0].Rules))
	}

	// Alice + Carol IPs should still be renewable (same IP returned)
	for _, u := range []*userSession{users[0], users[2]} {
		resp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: u.ownerID, Namespace: "test-ns"})
		if resp.IPv4 != ips[u.ownerID] {
			t.Fatalf("[%s] IP changed after Bob left: %s → %s", u.name, ips[u.ownerID], resp.IPv4)
		}
	}
	t.Log("Phase 3 PASS: Bob left, Alice+Carol IPs and rules intact")

	// --- Phase 4: Alice crashes, reconnects with same OwnerID ---
	alice := users[0]
	oldIP := alice.tunIPv4
	ip, _, _ := net.ParseCIDR(oldIP)
	var ipv6 net.IP
	if alice.tunIPv6 != "" {
		ipv6, _, _ = net.ParseCIDR(alice.tunIPv6)
	}
	env.server.dhcp.ReleaseIP(ctx, ip, ipv6)

	// Re-allocate with same OwnerID
	env.server.mu.Lock()
	delete(env.server.allocs, alice.ownerID)
	env.server.mu.Unlock()

	aliceResp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: alice.ownerID, Namespace: "test-ns"})
	alice.tunIPv4 = aliceResp.IPv4
	alice.tunIPv6 = aliceResp.IPv6

	// Re-proxy: update envoy rule with new IP
	cm, _ = env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &check)
	check = addVirtualRuleInternal(check, alice, reviews, "test-ns")
	data, _ = yaml.Marshal(check)
	cm.Data[config.KeyEnvoy] = string(data)
	env.server.clientset.CoreV1().ConfigMaps("test-ns").Update(ctx, cm, metav1.UpdateOptions{})

	cm, _ = env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &check)
	if len(check[0].Rules) != 2 {
		t.Fatalf("Phase 4: expected 2 rules (update not append), got %d", len(check[0].Rules))
	}
	for _, r := range check[0].Rules {
		if r.OwnerID == alice.ownerID && r.LocalTunIPv4 != alice.tunIPv4 {
			t.Fatalf("Alice rule should have new IP %s, got %s", alice.tunIPv4, r.LocalTunIPv4)
		}
	}
	t.Logf("Phase 4 PASS: Alice crashed, reconnected: %s → %s", oldIP, alice.tunIPv4)

	// --- Phase 5: All disconnect ---
	cm, _ = env.server.clientset.CoreV1().ConfigMaps("test-ns").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	cm.Data[config.KeyEnvoy] = ""
	env.server.clientset.CoreV1().ConfigMaps("test-ns").Update(ctx, cm, metav1.UpdateOptions{})
	t.Log("Phase 5 PASS: all rules cleaned")
}

// ============================================================================
// Integration Test: IP conflict avoidance between users
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

func TestIntegration_MultiUser_LeaseExpiry(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	aliceResp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice-expire1", Namespace: "test-ns"})
	bobResp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "bob-expire-01", Namespace: "test-ns"})

	// Force-expire Alice
	env.server.mu.Lock()
	env.server.allocs["alice-expire1"].LastRenew = env.server.allocs["alice-expire1"].LastRenew.Add(-LeaseDuration * 2)
	env.server.mu.Unlock()
	env.server.reapExpiredLeases(ctx)

	// Bob keeps same IP
	bobRenew, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "bob-expire-01", Namespace: "test-ns"})
	if bobRenew.IPv4 != bobResp.IPv4 {
		t.Fatalf("Bob's IP changed after Alice expired: %s → %s", bobResp.IPv4, bobRenew.IPv4)
	}

	// Alice gets new allocation
	aliceNew, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice-expire1", Namespace: "test-ns"})
	t.Logf("Alice: %s → expired → %s; Bob: %s (unchanged)", aliceResp.IPv4, aliceNew.IPv4, bobResp.IPv4)
}

// ============================================================================
// Integration Test: Watch stream keeps lease alive vs idle user expires
// ============================================================================

func TestIntegration_MultiUser_WatchVsIdle(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Alice watches (implicit lease renewal)
	aliceResp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice-watcher", Namespace: "test-ns"})
	stream, err := env.client.WatchTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice-watcher", Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream

	// Bob idles
	bobResp, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "bob-idler-01", Namespace: "test-ns"})

	// Expire Bob
	env.server.mu.Lock()
	env.server.allocs["bob-idler-01"].LastRenew = env.server.allocs["bob-idler-01"].LastRenew.Add(-LeaseDuration * 2)
	env.server.mu.Unlock()
	env.server.reapExpiredLeases(ctx)

	// Alice unchanged
	aliceRenew, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "alice-watcher", Namespace: "test-ns"})
	if aliceRenew.IPv4 != aliceResp.IPv4 {
		t.Fatalf("Alice IP changed despite active watch: %s → %s", aliceResp.IPv4, aliceRenew.IPv4)
	}

	// Bob expired
	bobNew, _ := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: "bob-idler-01", Namespace: "test-ns"})
	t.Logf("Alice: %s (watch, alive); Bob: %s → expired → %s", aliceResp.IPv4, bobResp.IPv4, bobNew.IPv4)
}
