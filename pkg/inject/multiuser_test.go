package inject

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds"
)

func newTestConfigMap(ns string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
		Data:       map[string]string{config.KeyEnvoy: ""},
	}
}

func getVirtuals(t *testing.T, clientset *fake.Clientset, ns string) []*xds.Virtual {
	t.Helper()
	cm, err := clientset.CoreV1().ConfigMaps(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var v []*xds.Virtual
	if cm.Data[config.KeyEnvoy] != "" {
		if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &v); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

func findRulesByOwner(virtuals []*xds.Virtual, nodeID, ns, ownerID string) []*xds.Rule {
	for _, v := range virtuals {
		if v.UID == nodeID && v.Namespace == ns {
			var result []*xds.Rule
			for _, r := range v.Rules {
				if r.OwnerID == ownerID {
					result = append(result, r)
				}
			}
			return result
		}
	}
	return nil
}

// ============================================================================
// Scenario: Two users proxy the SAME workload with DIFFERENT headers
// ============================================================================

func TestMultiUser_ProxySameWorkload_DifferentHeaders(t *testing.T) {
	ns := "default"
	nodeID := "deployments.apps.reviews"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	ctx := context.Background()

	// User A proxies with header env=dev
	err := addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
		Namespace:    ns,
		NodeID:       nodeID,
		LocalTunIPv4: "198.18.0.1",
		Headers:      map[string]string{"env": "dev"},
		OwnerID:      "user-a-owner",
		Ports:        []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// User B proxies same workload with header env=staging
	err = addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
		Namespace:    ns,
		NodeID:       nodeID,
		LocalTunIPv4: "198.18.0.2",
		Headers:      map[string]string{"env": "staging"},
		OwnerID:      "user-b-owner",
		Ports:        []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals) != 1 {
		t.Fatalf("expected 1 virtual (same workload), got %d", len(virtuals))
	}
	if len(virtuals[0].Rules) != 2 {
		t.Fatalf("expected 2 rules (2 users), got %d", len(virtuals[0].Rules))
	}

	// Each user's rule should be present
	rulesA := findRulesByOwner(virtuals, nodeID, ns, "user-a-owner")
	rulesB := findRulesByOwner(virtuals, nodeID, ns, "user-b-owner")
	if len(rulesA) != 1 || rulesA[0].LocalTunIPv4 != "198.18.0.1" {
		t.Fatalf("user A rule wrong: %+v", rulesA)
	}
	if len(rulesB) != 1 || rulesB[0].LocalTunIPv4 != "198.18.0.2" {
		t.Fatalf("user B rule wrong: %+v", rulesB)
	}
}

// ============================================================================
// Scenario: Two users proxy the SAME workload with SAME headers → takeover
// ============================================================================

func TestMultiUser_ProxySameWorkload_SameHeaders_Takeover(t *testing.T) {
	ns := "default"
	nodeID := "deployments.apps.reviews"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	ctx := context.Background()

	// User A proxies with header env=test
	err := addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
		Namespace:    ns,
		NodeID:       nodeID,
		LocalTunIPv4: "198.18.0.1",
		Headers:      map[string]string{"env": "test"},
		OwnerID:      "user-a",
	})
	if err != nil {
		t.Fatal(err)
	}

	// User B proxies same workload with same header → should takeover
	err = addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
		Namespace:    ns,
		NodeID:       nodeID,
		LocalTunIPv4: "198.18.0.2",
		Headers:      map[string]string{"env": "test"},
		OwnerID:      "user-b",
	})
	if err != nil {
		t.Fatal(err)
	}

	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals[0].Rules) != 1 {
		t.Fatalf("same headers → takeover, expected 1 rule, got %d", len(virtuals[0].Rules))
	}
	// Rule should now belong to user B
	rule := virtuals[0].Rules[0]
	if rule.OwnerID != "user-b" {
		t.Fatalf("expected user-b to take over, got owner %q", rule.OwnerID)
	}
	if rule.LocalTunIPv4 != "198.18.0.2" {
		t.Fatalf("expected user-b IP, got %q", rule.LocalTunIPv4)
	}
}

// ============================================================================
// Scenario: User A leaves, User B's rule stays
// ============================================================================

func TestMultiUser_OneLeaves_OtherStays(t *testing.T) {
	ns := "default"
	nodeID := "deployments.apps.reviews"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	ctx := context.Background()

	// Two users proxy same workload
	for _, u := range []struct{ ip, owner, env string }{
		{"198.18.0.1", "user-a", "dev"},
		{"198.18.0.2", "user-b", "staging"},
	} {
		err := addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
			Namespace:    ns,
			NodeID:       nodeID,
			LocalTunIPv4: u.ip,
			Headers:      map[string]string{"env": u.env},
			OwnerID:      u.owner,
			Ports:        []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// User A leaves
	empty, found, err := removeEnvoyConfig(ctx, mapInterface, ns, nodeID, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("user-a rule should be found for removal")
	}
	if empty {
		t.Fatal("virtual should NOT be empty (user-b still present)")
	}

	// Verify user B's rule still exists
	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals) != 1 || len(virtuals[0].Rules) != 1 {
		t.Fatalf("expected 1 virtual with 1 rule, got %d virtuals", len(virtuals))
	}
	if virtuals[0].Rules[0].OwnerID != "user-b" {
		t.Fatalf("remaining rule should be user-b, got %q", virtuals[0].Rules[0].OwnerID)
	}
}

// ============================================================================
// Scenario: Last user leaves → virtual entry removed entirely
// ============================================================================

func TestMultiUser_AllLeave_VirtualRemoved(t *testing.T) {
	ns := "default"
	nodeID := "deployments.apps.reviews"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	ctx := context.Background()

	// Single user proxy
	err := addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
		Namespace:    ns,
		NodeID:       nodeID,
		LocalTunIPv4: "198.18.0.1",
		Headers:      map[string]string{"env": "test"},
		OwnerID:      "only-user",
		Ports:        []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Leave → should remove virtual entirely
	empty, found, err := removeEnvoyConfig(ctx, mapInterface, ns, nodeID, "only-user")
	if err != nil {
		t.Fatal(err)
	}
	if !found || !empty {
		t.Fatalf("expected found=true, empty=true, got found=%v, empty=%v", found, empty)
	}

	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals) != 0 {
		t.Fatalf("all virtuals should be removed, got %d", len(virtuals))
	}
}

// ============================================================================
// Scenario: Multiple users proxy DIFFERENT workloads simultaneously
// ============================================================================

func TestMultiUser_DifferentWorkloads_Isolated(t *testing.T) {
	ns := "default"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	ctx := context.Background()

	workloads := []struct{ nodeID, ip, owner string }{
		{"deployments.apps.frontend", "198.18.0.1", "user-a"},
		{"deployments.apps.backend", "198.18.0.2", "user-b"},
		{"statefulsets.apps.database", "198.18.0.3", "user-c"},
	}

	for _, w := range workloads {
		err := addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
			Namespace:    ns,
			NodeID:       w.nodeID,
			LocalTunIPv4: w.ip,
			Headers:      map[string]string{"user": w.owner},
			OwnerID:      w.owner,
			Ports:        []xds.ContainerPort{{ContainerPort: 8080, Protocol: "TCP"}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals) != 3 {
		t.Fatalf("expected 3 virtuals (3 workloads), got %d", len(virtuals))
	}

	// Leave user-b's workload → others unaffected
	empty, _, err := removeEnvoyConfig(ctx, mapInterface, ns, "deployments.apps.backend", "user-b")
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Fatal("backend virtual should be empty after only user leaves")
	}

	virtuals = getVirtuals(t, clientset, ns)
	if len(virtuals) != 2 {
		t.Fatalf("expected 2 remaining virtuals, got %d", len(virtuals))
	}
}

// ============================================================================
// Scenario: Concurrent proxy from N users to same workload
// ============================================================================

func TestMultiUser_ConcurrentProxy_SameWorkload(t *testing.T) {
	// Note: fake.Clientset doesn't simulate ResourceVersion conflicts,
	// so concurrent updates may overwrite each other. This test verifies
	// no panics, no corrupted YAML, and valid YAML structure.
	// Full conflict-retry validation requires a real K8s API (CI).
	ns := "default"
	nodeID := "deployments.apps.reviews"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	ctx := context.Background()

	n := 10
	var wg sync.WaitGroup
	var errCount int32

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
				Namespace:    ns,
				NodeID:       nodeID,
				LocalTunIPv4: fmt.Sprintf("198.18.0.%d", i+1),
				Headers:      map[string]string{"user": fmt.Sprintf("user-%d", i)},
				OwnerID:      fmt.Sprintf("owner-%d", i),
				Ports:        []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
			})
			if err != nil {
				atomic.AddInt32(&errCount, 1)
			}
		}(i)
	}
	wg.Wait()

	if errCount > 0 {
		t.Fatalf("%d concurrent addEnvoyConfig calls returned errors", errCount)
	}

	// Verify YAML is not corrupted and has at least some rules
	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(virtuals))
	}
	if len(virtuals[0].Rules) == 0 {
		t.Fatal("expected at least some rules after concurrent writes")
	}
	// All present rules should have valid OwnerIDs
	for _, rule := range virtuals[0].Rules {
		if rule.OwnerID == "" {
			t.Fatal("rule with empty OwnerID found — YAML corruption")
		}
		if rule.LocalTunIPv4 == "" {
			t.Fatal("rule with empty IP found — YAML corruption")
		}
	}
	t.Logf("concurrent proxy: %d/%d rules survived (fake clientset, no conflict retry)", len(virtuals[0].Rules), n)
}

// ============================================================================
// Scenario: Concurrent leave from N users
// ============================================================================

func TestMultiUser_ConcurrentLeave(t *testing.T) {
	// Note: fake.Clientset doesn't simulate ResourceVersion conflicts.
	// Concurrent removes may skip some rules. This test verifies no panics
	// and no corrupted YAML. Full test requires real K8s API.
	ns := "default"
	nodeID := "deployments.apps.reviews"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	ctx := context.Background()

	n := 10
	for i := 0; i < n; i++ {
		err := addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
			Namespace:    ns,
			NodeID:       nodeID,
			LocalTunIPv4: fmt.Sprintf("198.18.0.%d", i+1),
			Headers:      map[string]string{"user": fmt.Sprintf("user-%d", i)},
			OwnerID:      fmt.Sprintf("owner-%d", i),
			Ports:        []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := removeEnvoyConfig(ctx, mapInterface, ns, nodeID, fmt.Sprintf("owner-%d", i))
			if err != nil {
				t.Errorf("removeEnvoyConfig owner-%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// Verify YAML is valid, no panics occurred
	virtuals := getVirtuals(t, clientset, ns)
	remaining := 0
	for _, v := range virtuals {
		remaining += len(v.Rules)
	}
	t.Logf("concurrent leave: %d/%d rules remain (fake clientset, no conflict retry)", remaining, n)
}

// ============================================================================
// Scenario: User re-proxies (update own rule) while others are present
// ============================================================================

func TestMultiUser_ReProxy_UpdatesOwnRule(t *testing.T) {
	ns := "default"
	nodeID := "deployments.apps.reviews"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	ctx := context.Background()

	// User A and B both proxy
	addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
		Namespace: ns, NodeID: nodeID, LocalTunIPv4: "198.18.0.1",
		Headers: map[string]string{"env": "dev"}, OwnerID: "user-a",
		Ports: []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})
	addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
		Namespace: ns, NodeID: nodeID, LocalTunIPv4: "198.18.0.2",
		Headers: map[string]string{"env": "staging"}, OwnerID: "user-b",
		Ports: []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})

	// User A re-proxies (new IP, same OwnerID) → should update, not add
	addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
		Namespace: ns, NodeID: nodeID, LocalTunIPv4: "198.18.0.99",
		Headers: map[string]string{"env": "dev"}, OwnerID: "user-a",
		Ports: []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})

	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals[0].Rules) != 2 {
		t.Fatalf("expected 2 rules (update, not append), got %d", len(virtuals[0].Rules))
	}

	rulesA := findRulesByOwner(virtuals, nodeID, ns, "user-a")
	if len(rulesA) != 1 || rulesA[0].LocalTunIPv4 != "198.18.0.99" {
		t.Fatalf("user-a IP should be updated to 198.18.0.99, got %+v", rulesA)
	}

	// User B should be unaffected
	rulesB := findRulesByOwner(virtuals, nodeID, ns, "user-b")
	if len(rulesB) != 1 || rulesB[0].LocalTunIPv4 != "198.18.0.2" {
		t.Fatalf("user-b should be unaffected, got %+v", rulesB)
	}
}

// ============================================================================
// Scenario: Users proxy same workload across DIFFERENT namespaces
// ============================================================================

func TestMultiUser_SameWorkload_DifferentNamespaces(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		newTestConfigMap("ns-a"),
		newTestConfigMap("ns-b"),
	)
	ctx := context.Background()
	nodeID := "deployments.apps.reviews"

	// User A in ns-a
	addEnvoyConfig(ctx, clientset.CoreV1().ConfigMaps("ns-a"), envoyRuleSpec{
		Namespace: "ns-a", NodeID: nodeID, LocalTunIPv4: "198.18.0.1",
		Headers: map[string]string{"env": "dev"}, OwnerID: "user-a",
		Ports: []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})

	// User B in ns-b (same workload name, different namespace)
	addEnvoyConfig(ctx, clientset.CoreV1().ConfigMaps("ns-b"), envoyRuleSpec{
		Namespace: "ns-b", NodeID: nodeID, LocalTunIPv4: "198.18.0.2",
		Headers: map[string]string{"env": "dev"}, OwnerID: "user-b",
		Ports: []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})

	// Each namespace has its own ConfigMap — fully isolated
	virtualsA := getVirtuals(t, clientset, "ns-a")
	virtualsB := getVirtuals(t, clientset, "ns-b")

	if len(virtualsA) != 1 || virtualsA[0].Rules[0].OwnerID != "user-a" {
		t.Fatal("ns-a should have user-a's rule only")
	}
	if len(virtualsB) != 1 || virtualsB[0].Rules[0].OwnerID != "user-b" {
		t.Fatal("ns-b should have user-b's rule only")
	}

	// Leave user-a in ns-a → ns-b unaffected
	removeEnvoyConfig(ctx, clientset.CoreV1().ConfigMaps("ns-a"), "ns-a", nodeID, "user-a")
	virtualsA = getVirtuals(t, clientset, "ns-a")
	virtualsB = getVirtuals(t, clientset, "ns-b")
	if len(virtualsA) != 0 {
		t.Fatal("ns-a should be empty after leave")
	}
	if len(virtualsB) != 1 {
		t.Fatal("ns-b should be unaffected")
	}
}

// ============================================================================
// Scenario: Leave with wrong OwnerID → no-op
// ============================================================================

func TestMultiUser_LeaveWrongOwner_NoOp(t *testing.T) {
	ns := "default"
	nodeID := "deployments.apps.reviews"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	ctx := context.Background()

	addEnvoyConfig(ctx, mapInterface, envoyRuleSpec{
		Namespace: ns, NodeID: nodeID, LocalTunIPv4: "198.18.0.1",
		Headers: map[string]string{"env": "dev"}, OwnerID: "real-owner",
		Ports: []xds.ContainerPort{{ContainerPort: 9080, Protocol: "TCP"}},
	})

	// Try to remove with wrong OwnerID
	_, found, err := removeEnvoyConfig(ctx, mapInterface, ns, nodeID, "wrong-owner")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("should not find rule with wrong OwnerID")
	}

	// Original rule should still exist
	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals) != 1 || len(virtuals[0].Rules) != 1 {
		t.Fatal("rule should be untouched")
	}
	if virtuals[0].Rules[0].OwnerID != "real-owner" {
		t.Fatal("rule owner should be unchanged")
	}
}
