package inject

import (
	"context"
	"fmt"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds"
)

// TestVirtualStore_ConcurrentMutate_NoDataRace verifies that concurrent calls to
// Mutate on the same VirtualStore do not panic and always leave a valid YAML in the
// ConfigMap, even when the fake clientset silently overwrites concurrent writes.
//
// Assertion: after N goroutines each call Mutate (add a unique rule), the ConfigMap
// YAML is valid and deserializable. We do NOT assert an exact rule count because
// fake.Clientset has no conflict detection (see CLAUDE.md: "Does NOT simulate
// ResourceVersion conflicts — concurrent Update() calls overwrite each other silently").
func TestVirtualStore_ConcurrentMutate_NoDataRace(t *testing.T) {
	const ns = "default"
	const N = 10
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	store := NewVirtualStore(clientset.CoreV1().ConfigMaps(ns))

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_ = store.AddRule(context.Background(), envoyRuleSpec{
				Namespace:    ns,
				NodeID:       "workload-a",
				LocalTunIPv4: fmt.Sprintf("198.18.0.%d", i+1),
				OwnerID:      fmt.Sprintf("owner-%02d", i),
				Headers:      map[string]string{"x-user": fmt.Sprintf("user%d", i)},
			})
		}()
	}
	wg.Wait()

	// Validate YAML is parseable after concurrent writes.
	cm, err := clientset.CoreV1().ConfigMaps(ns).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	var virtuals []*xds.Virtual
	if str := cm.Data[config.KeyEnvoy]; str != "" {
		if err := yaml.Unmarshal([]byte(str), &virtuals); err != nil {
			t.Fatalf("invalid YAML after concurrent Mutate: %v\nYAML: %s", err, str)
		}
	}
	// Must have at least 1 rule (at least one write succeeded).
	totalRules := 0
	for _, v := range virtuals {
		totalRules += len(v.Rules)
	}
	if totalRules == 0 {
		t.Errorf("concurrent Mutate: expected at least 1 rule, got 0")
	}
}

// TestVirtualStore_AddRule_Basic verifies that AddRule inserts a rule that is
// retrievable from the ConfigMap with the expected fields.
func TestVirtualStore_AddRule_Basic(t *testing.T) {
	const ns = "test-ns"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	store := NewVirtualStore(clientset.CoreV1().ConfigMaps(ns))

	spec := envoyRuleSpec{
		Namespace:    ns,
		NodeID:       "deploy-foo",
		LocalTunIPv4: "198.18.0.1",
		OwnerID:      "owner-abc",
		Headers:      map[string]string{"x-env": "dev"},
	}
	if err := store.AddRule(context.Background(), spec); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(virtuals))
	}
	if len(virtuals[0].Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(virtuals[0].Rules))
	}
	r := virtuals[0].Rules[0]
	if r.OwnerID != "owner-abc" {
		t.Errorf("OwnerID: got %q, want %q", r.OwnerID, "owner-abc")
	}
	if r.LocalTunIPv4 != "198.18.0.1" {
		t.Errorf("LocalTunIPv4: got %q, want %q", r.LocalTunIPv4, "198.18.0.1")
	}
}

// TestVirtualStore_RemoveRule_Found verifies that RemoveRule correctly removes a
// matching rule and sets found=true.
func TestVirtualStore_RemoveRule_Found(t *testing.T) {
	const ns = "test-ns"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	store := NewVirtualStore(clientset.CoreV1().ConfigMaps(ns))

	spec := envoyRuleSpec{
		Namespace:    ns,
		NodeID:       "deploy-bar",
		LocalTunIPv4: "198.18.0.2",
		OwnerID:      "owner-xyz",
		Headers:      map[string]string{"x-env": "staging"},
	}
	if err := store.AddRule(context.Background(), spec); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	empty, found, err := store.RemoveRule(context.Background(), ns, "deploy-bar", "owner-xyz")
	if err != nil {
		t.Fatalf("RemoveRule: %v", err)
	}
	if !found {
		t.Error("expected found=true")
	}
	if !empty {
		t.Error("expected empty=true (last rule removed)")
	}

	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals) != 0 {
		t.Errorf("expected 0 virtuals after remove, got %d", len(virtuals))
	}
}

// TestVirtualStore_RemoveRule_NotFound verifies that RemoveRule returns found=false
// when the ownerID does not match any rule.
func TestVirtualStore_RemoveRule_NotFound(t *testing.T) {
	const ns = "test-ns"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	store := NewVirtualStore(clientset.CoreV1().ConfigMaps(ns))

	spec := envoyRuleSpec{
		Namespace:    ns,
		NodeID:       "deploy-baz",
		LocalTunIPv4: "198.18.0.3",
		OwnerID:      "owner-a",
		Headers:      map[string]string{"x-env": "prod"},
	}
	if err := store.AddRule(context.Background(), spec); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	empty, found, err := store.RemoveRule(context.Background(), ns, "deploy-baz", "owner-b")
	if err != nil {
		t.Fatalf("RemoveRule: %v", err)
	}
	if found {
		t.Error("expected found=false for unknown ownerID")
	}
	if empty {
		t.Error("expected empty=false when rule was not removed")
	}

	// The rule for owner-a must still be present.
	virtuals := getVirtuals(t, clientset, ns)
	if len(virtuals) != 1 || len(virtuals[0].Rules) != 1 {
		t.Errorf("expected 1 virtual with 1 rule, got virtuals=%d", len(virtuals))
	}
}
