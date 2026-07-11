package inject

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

// user is a test helper representing a single kubevpn user session.
type user struct {
	name    string
	ownerID string
	tunIPv4 string
	headers map[string]string
}

// testCluster holds a fake K8s cluster with shared ConfigMap for envoy rules.
type testCluster struct {
	clientset *fake.Clientset
	ns        string
}

func newTestCluster(ns string) *testCluster {
	return &testCluster{
		clientset: fake.NewSimpleClientset(&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
			Data:       map[string]string{config.KeyEnvoy: ""},
		}),
		ns: ns,
	}
}

func (c *testCluster) proxy(t *testing.T, u user, nodeID string) {
	t.Helper()
	err := addEnvoyConfig(context.Background(), c.clientset.CoreV1().ConfigMaps(c.ns), envoyRuleSpec{
		Namespace:    c.ns,
		NodeID:       nodeID,
		LocalTunIPv4: u.tunIPv4,
		Headers:      u.headers,
		OwnerID:      u.ownerID,
		Ports:        []controlplane.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})
	if err != nil {
		t.Fatalf("[%s] proxy %s failed: %v", u.name, nodeID, err)
	}
}

func (c *testCluster) leave(t *testing.T, u user, nodeID string) (empty, found bool) {
	t.Helper()
	empty, found, err := removeEnvoyConfig(context.Background(), c.clientset.CoreV1().ConfigMaps(c.ns), c.ns, nodeID, u.ownerID)
	if err != nil {
		t.Fatalf("[%s] leave %s failed: %v", u.name, nodeID, err)
	}
	return
}

func (c *testCluster) ruleCount(t *testing.T, nodeID string) int {
	t.Helper()
	virtuals := getVirtuals(t, c.clientset, c.ns)
	for _, v := range virtuals {
		if v.UID == nodeID && v.Namespace == c.ns {
			return len(v.Rules)
		}
	}
	return 0
}

func (c *testCluster) ruleOwners(t *testing.T, nodeID string) []string {
	t.Helper()
	virtuals := getVirtuals(t, c.clientset, c.ns)
	var owners []string
	for _, v := range virtuals {
		if v.UID == nodeID && v.Namespace == c.ns {
			for _, r := range v.Rules {
				owners = append(owners, r.OwnerID)
			}
		}
	}
	return owners
}

// ============================================================================
// Story 1: Alice and Bob proxy the same service, then leave one by one
//
// Timeline:
//   t0: Alice connects, proxies reviews with header env=alice
//   t1: Bob connects, proxies reviews with header env=bob
//   t2: Both rules coexist, traffic splits by header
//   t3: Alice leaves → only Bob's rule remains
//   t4: Bob leaves → virtual entry removed entirely
// ============================================================================

func TestStory_AliceBob_ProxyAndLeaveSequentially(t *testing.T) {
	cluster := newTestCluster("default")
	reviews := "deployments.apps.reviews"

	alice := user{name: "Alice", ownerID: "alice-uuid-01", tunIPv4: "198.18.0.1", headers: map[string]string{"env": "alice"}}
	bob := user{name: "Bob", ownerID: "bob-uuid-0001", tunIPv4: "198.18.0.2", headers: map[string]string{"env": "bob"}}

	// t0-t1: Both proxy
	cluster.proxy(t, alice, reviews)
	cluster.proxy(t, bob, reviews)

	// t2: Both rules coexist
	if n := cluster.ruleCount(t, reviews); n != 2 {
		t.Fatalf("expected 2 rules (Alice + Bob), got %d", n)
	}

	// t3: Alice leaves
	empty, found := cluster.leave(t, alice, reviews)
	if !found {
		t.Fatal("Alice's rule should be found")
	}
	if empty {
		t.Fatal("virtual should NOT be empty (Bob still present)")
	}
	if n := cluster.ruleCount(t, reviews); n != 1 {
		t.Fatalf("expected 1 rule (Bob only), got %d", n)
	}
	owners := cluster.ruleOwners(t, reviews)
	if len(owners) != 1 || owners[0] != "bob-uuid-0001" {
		t.Fatalf("only Bob should remain, got %v", owners)
	}

	// t4: Bob leaves
	empty, found = cluster.leave(t, bob, reviews)
	if !found || !empty {
		t.Fatal("Bob's leave should remove the last rule and the virtual entry")
	}
	if n := cluster.ruleCount(t, reviews); n != 0 {
		t.Fatal("no rules should remain")
	}
}

// ============================================================================
// Story 2: Alice proxies, Bob takes over with same headers, Alice tries to leave
//
// Timeline:
//   t0: Alice proxies reviews with header env=test
//   t1: Bob proxies reviews with same header env=test → takes over
//   t2: Alice tries to leave → nothing to remove (her OwnerID is gone)
//   t3: Bob leaves → clean
// ============================================================================

func TestStory_Takeover_AliceLosesOwnership(t *testing.T) {
	cluster := newTestCluster("default")
	reviews := "deployments.apps.reviews"

	alice := user{name: "Alice", ownerID: "alice-uuid-01", tunIPv4: "198.18.0.1", headers: map[string]string{"env": "test"}}
	bob := user{name: "Bob", ownerID: "bob-uuid-0001", tunIPv4: "198.18.0.2", headers: map[string]string{"env": "test"}}

	// t0: Alice proxies
	cluster.proxy(t, alice, reviews)
	owners := cluster.ruleOwners(t, reviews)
	if len(owners) != 1 || owners[0] != "alice-uuid-01" {
		t.Fatalf("Alice should own the rule, got %v", owners)
	}

	// t1: Bob takes over (same headers)
	cluster.proxy(t, bob, reviews)
	owners = cluster.ruleOwners(t, reviews)
	if len(owners) != 1 || owners[0] != "bob-uuid-0001" {
		t.Fatalf("Bob should have taken over, got %v", owners)
	}

	// t2: Alice tries to leave — her OwnerID is gone (replaced by Bob's)
	_, found := cluster.leave(t, alice, reviews)
	if found {
		t.Fatal("Alice's OwnerID should NOT be found (Bob took over)")
	}

	// t3: Bob leaves normally
	empty, found := cluster.leave(t, bob, reviews)
	if !found || !empty {
		t.Fatal("Bob should be able to leave cleanly")
	}
}

// ============================================================================
// Story 3: Three users proxy different resources, then leave in random order
//
// Timeline:
//   t0: Alice proxies frontend, Bob proxies backend, Carol proxies database
//   t1: Bob leaves backend → frontend and database unaffected
//   t2: Alice leaves frontend → database unaffected
//   t3: Carol leaves database → all clean
// ============================================================================

func TestStory_ThreeUsers_DifferentResources_IndependentLifecycle(t *testing.T) {
	cluster := newTestCluster("default")

	alice := user{name: "Alice", ownerID: "alice-uuid-01", tunIPv4: "198.18.0.1", headers: map[string]string{"user": "alice"}}
	bob := user{name: "Bob", ownerID: "bob-uuid-0001", tunIPv4: "198.18.0.2", headers: map[string]string{"user": "bob"}}
	carol := user{name: "Carol", ownerID: "carol-uuid-01", tunIPv4: "198.18.0.3", headers: map[string]string{"user": "carol"}}

	// t0: All proxy different resources
	cluster.proxy(t, alice, "deployments.apps.frontend")
	cluster.proxy(t, bob, "deployments.apps.backend")
	cluster.proxy(t, carol, "statefulsets.apps.database")

	virtuals := getVirtuals(t, cluster.clientset, cluster.ns)
	if len(virtuals) != 3 {
		t.Fatalf("expected 3 virtuals, got %d", len(virtuals))
	}

	// t1: Bob leaves backend
	cluster.leave(t, bob, "deployments.apps.backend")
	if cluster.ruleCount(t, "deployments.apps.frontend") != 1 {
		t.Fatal("frontend should still have 1 rule")
	}
	if cluster.ruleCount(t, "statefulsets.apps.database") != 1 {
		t.Fatal("database should still have 1 rule")
	}

	// t2: Alice leaves frontend
	cluster.leave(t, alice, "deployments.apps.frontend")
	if cluster.ruleCount(t, "statefulsets.apps.database") != 1 {
		t.Fatal("database should still have 1 rule after alice leaves")
	}

	// t3: Carol leaves database
	empty, _ := cluster.leave(t, carol, "statefulsets.apps.database")
	if !empty {
		t.Fatal("last virtual should be empty")
	}

	virtuals = getVirtuals(t, cluster.clientset, cluster.ns)
	if len(virtuals) != 0 {
		t.Fatalf("all virtuals should be removed, got %d", len(virtuals))
	}
}

// ============================================================================
// Story 4: User re-proxies after crash (same OwnerID, new IP)
//
// Timeline:
//   t0: Alice and Bob both proxy reviews
//   t1: Alice's daemon crashes, gets new TUN IP on reconnect
//   t2: Alice re-proxies with same OwnerID but new IP → updates own rule
//   t3: Bob's rule is unaffected throughout
// ============================================================================

func TestStory_CrashReconnect_OwnerIDPreserved(t *testing.T) {
	cluster := newTestCluster("default")
	reviews := "deployments.apps.reviews"

	alice := user{name: "Alice", ownerID: "alice-uuid-01", tunIPv4: "198.18.0.1", headers: map[string]string{"env": "dev"}}
	bob := user{name: "Bob", ownerID: "bob-uuid-0001", tunIPv4: "198.18.0.2", headers: map[string]string{"env": "staging"}}

	// t0: Both proxy
	cluster.proxy(t, alice, reviews)
	cluster.proxy(t, bob, reviews)

	// t1-t2: Alice crashes, reconnects with new IP but same OwnerID
	aliceReconnected := user{name: "Alice-reconnected", ownerID: "alice-uuid-01", tunIPv4: "198.18.0.99", headers: map[string]string{"env": "dev"}}
	cluster.proxy(t, aliceReconnected, reviews)

	// Should still be 2 rules (update, not append)
	if n := cluster.ruleCount(t, reviews); n != 2 {
		t.Fatalf("expected 2 rules (alice updated + bob), got %d", n)
	}

	// Alice's rule should have new IP
	rules := findRulesByOwner(getVirtuals(t, cluster.clientset, cluster.ns), reviews, "default", "alice-uuid-01")
	if len(rules) != 1 || rules[0].LocalTunIPv4 != "198.18.0.99" {
		t.Fatalf("Alice's IP should be updated to 198.18.0.99, got %+v", rules)
	}

	// t3: Bob unaffected
	bobRules := findRulesByOwner(getVirtuals(t, cluster.clientset, cluster.ns), reviews, "default", "bob-uuid-0001")
	if len(bobRules) != 1 || bobRules[0].LocalTunIPv4 != "198.18.0.2" {
		t.Fatalf("Bob should be unaffected, got %+v", bobRules)
	}
}

// ============================================================================
// Story 5: User crashes with NEW OwnerID (pre-fix behavior) → orphaned rule
//
// Timeline:
//   t0: Alice proxies reviews (ownerID=old-uuid)
//   t1: Alice crashes, daemon restarts with NEW ownerID (old-uuid lost)
//   t2: Alice re-proxies with new ownerID → old rule becomes orphan
//   t3: Alice leaves with new ownerID → removes only new rule, orphan stays
// ============================================================================

func TestStory_CrashWithNewOwnerID_OrphanedRule(t *testing.T) {
	cluster := newTestCluster("default")
	reviews := "deployments.apps.reviews"

	aliceOld := user{name: "Alice-old", ownerID: "old-uuid-1234", tunIPv4: "198.18.0.1", headers: map[string]string{"env": "dev"}}
	aliceNew := user{name: "Alice-new", ownerID: "new-uuid-5678", tunIPv4: "198.18.0.99", headers: map[string]string{"env": "dev"}}

	// t0: Alice proxies with old OwnerID
	cluster.proxy(t, aliceOld, reviews)

	// t1-t2: Alice crashes, re-proxies with NEW OwnerID and SAME headers
	// Case 3 in addVirtualRule: headers match → takeover (old rule replaced)
	cluster.proxy(t, aliceNew, reviews)

	// With same headers, Case 3 triggers: ownership transferred to new-uuid
	owners := cluster.ruleOwners(t, reviews)
	if len(owners) != 1 || owners[0] != "new-uuid-5678" {
		t.Fatalf("same headers → takeover, expected new-uuid-5678, got %v", owners)
	}

	// t3: Alice leaves with new ownerID → removes the rule
	empty, found := cluster.leave(t, aliceNew, reviews)
	if !found || !empty {
		t.Fatal("new OwnerID should be removable")
	}

	// Old OwnerID rule is gone because takeover replaced it
	_, found = cluster.leave(t, aliceOld, reviews)
	if found {
		t.Fatal("old OwnerID should not exist anymore (was taken over)")
	}
}

// ============================================================================
// Story 6: Three users, interleaved proxy and leave on same resource
//
// Timeline:
//   t0: Alice proxies reviews (env=alice)
//   t1: Bob proxies reviews (env=bob)
//   t2: Carol proxies reviews (env=carol)
//   t3: Bob leaves → Alice and Carol remain
//   t4: Alice re-proxies with new IP → rule updated
//   t5: Carol leaves → only Alice remains
//   t6: Alice leaves → all clean
// ============================================================================

func TestStory_ThreeUsers_InterleavedProxyLeave(t *testing.T) {
	cluster := newTestCluster("default")
	reviews := "deployments.apps.reviews"

	alice := user{name: "Alice", ownerID: "alice-001234", tunIPv4: "198.18.0.1", headers: map[string]string{"env": "alice"}}
	bob := user{name: "Bob", ownerID: "bob-00001234", tunIPv4: "198.18.0.2", headers: map[string]string{"env": "bob"}}
	carol := user{name: "Carol", ownerID: "carol-001234", tunIPv4: "198.18.0.3", headers: map[string]string{"env": "carol"}}

	// t0-t2
	cluster.proxy(t, alice, reviews)
	cluster.proxy(t, bob, reviews)
	cluster.proxy(t, carol, reviews)
	if n := cluster.ruleCount(t, reviews); n != 3 {
		t.Fatalf("t2: expected 3 rules, got %d", n)
	}

	// t3: Bob leaves
	cluster.leave(t, bob, reviews)
	if n := cluster.ruleCount(t, reviews); n != 2 {
		t.Fatalf("t3: expected 2 rules, got %d", n)
	}

	// t4: Alice re-proxies with new IP
	aliceUpdated := user{name: "Alice", ownerID: "alice-001234", tunIPv4: "198.18.0.55", headers: map[string]string{"env": "alice"}}
	cluster.proxy(t, aliceUpdated, reviews)
	if n := cluster.ruleCount(t, reviews); n != 2 {
		t.Fatalf("t4: re-proxy should update not append, got %d rules", n)
	}

	// t5: Carol leaves
	cluster.leave(t, carol, reviews)
	if n := cluster.ruleCount(t, reviews); n != 1 {
		t.Fatalf("t5: expected 1 rule (Alice), got %d", n)
	}

	// t6: Alice leaves
	empty, _ := cluster.leave(t, alice, reviews)
	if !empty {
		t.Fatal("t6: last user leaves → empty")
	}
}

// ============================================================================
// Story 7: Two users, two clusters, cross-cluster operations
//
// Timeline:
//   t0: Alice connects cluster-A, proxies reviews in cluster-A
//   t1: Bob connects cluster-B, proxies reviews in cluster-B
//   t2: Alice leaves in cluster-A → cluster-B unaffected
//   t3: Bob leaves in cluster-B → both clean
// ============================================================================

func TestStory_TwoUsers_TwoClusters_CrossCluster(t *testing.T) {
	clusterA := newTestCluster("ns-a")
	clusterB := newTestCluster("ns-b")
	reviews := "deployments.apps.reviews"

	alice := user{name: "Alice", ownerID: "alice-uuid-01", tunIPv4: "198.18.0.1", headers: map[string]string{"user": "alice"}}
	bob := user{name: "Bob", ownerID: "bob-uuid-0001", tunIPv4: "198.18.0.2", headers: map[string]string{"user": "bob"}}

	// t0-t1
	clusterA.proxy(t, alice, reviews)
	clusterB.proxy(t, bob, reviews)

	// t2: Alice leaves cluster-A → cluster-B unaffected
	clusterA.leave(t, alice, reviews)
	if clusterA.ruleCount(t, reviews) != 0 {
		t.Fatal("cluster-A should be clean")
	}
	if clusterB.ruleCount(t, reviews) != 1 {
		t.Fatal("cluster-B should be unaffected")
	}
	bobRules := findRulesByOwner(getVirtuals(t, clusterB.clientset, clusterB.ns), reviews, "ns-b", "bob-uuid-0001")
	if len(bobRules) != 1 || bobRules[0].LocalTunIPv4 != "198.18.0.2" {
		t.Fatal("Bob's rule in cluster-B should be intact")
	}

	// t3
	clusterB.leave(t, bob, reviews)
	if clusterB.ruleCount(t, reviews) != 0 {
		t.Fatal("cluster-B should be clean")
	}
}

// ============================================================================
// Story 8: Header ordering — empty-header rule must be last
//
// Timeline:
//   t0: Alice proxies reviews with NO headers (catch-all)
//   t1: Bob proxies reviews with header env=test
//   t2: Envoy rules should be ordered: Bob's first, Alice's last
//        (otherwise Alice's catch-all would steal all traffic)
// ============================================================================

func TestStory_HeaderOrdering_EmptyHeaderLast(t *testing.T) {
	cluster := newTestCluster("default")
	reviews := "deployments.apps.reviews"

	alice := user{name: "Alice", ownerID: "alice-uuid-01", tunIPv4: "198.18.0.1", headers: map[string]string{}}
	bob := user{name: "Bob", ownerID: "bob-uuid-0001", tunIPv4: "198.18.0.2", headers: map[string]string{"env": "test"}}

	// Alice adds catch-all first
	cluster.proxy(t, alice, reviews)
	// Bob adds specific header
	cluster.proxy(t, bob, reviews)

	virtuals := getVirtuals(t, cluster.clientset, cluster.ns)
	rules := virtuals[0].Rules
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}

	// Bob's rule (with headers) should come BEFORE Alice's (empty headers)
	if len(rules[0].Headers) == 0 {
		t.Fatal("first rule should have headers (Bob), but it's empty-header (Alice). Wrong ordering.")
	}
	if len(rules[1].Headers) != 0 {
		t.Fatal("last rule should be empty-header (Alice catch-all)")
	}
}

// ============================================================================
// Story 9: Rapid proxy/leave cycles on the same resource
//
// Timeline:
//   Alice proxies → leaves → proxies → leaves → proxies (5 cycles)
//   After final proxy, exactly 1 rule should exist.
// ============================================================================

func TestStory_RapidProxyLeaveCycles(t *testing.T) {
	cluster := newTestCluster("default")
	reviews := "deployments.apps.reviews"
	alice := user{name: "Alice", ownerID: "alice-uuid-01", tunIPv4: "198.18.0.1", headers: map[string]string{"env": "dev"}}

	for i := 0; i < 5; i++ {
		alice.tunIPv4 = "198.18.0." + string(rune('1'+i))
		cluster.proxy(t, alice, reviews)
		if i < 4 { // leave all but the last
			cluster.leave(t, alice, reviews)
		}
	}

	if n := cluster.ruleCount(t, reviews); n != 1 {
		t.Fatalf("after 5 cycles, expected 1 rule, got %d", n)
	}
}
