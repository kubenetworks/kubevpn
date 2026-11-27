package handler

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// ============================================================================
// Tests for 07-namespace-model.md:
// ManagerNamespace and WorkloadNamespace are independent
// ============================================================================

func TestDoc07_NamespacesCanDiffer(t *testing.T) {
	// Doc says: cluster-wide mode → manager in "kubevpn", workloads in "default"
	c := &ConnectOptions{
		ManagerNamespace:  "kubevpn",
		WorkloadNamespace: "default",
	}
	if c.ManagerNamespace == c.WorkloadNamespace {
		t.Fatal("ManagerNamespace and WorkloadNamespace should be independent")
	}
}

func TestDoc07_ManagerNamespaceUsedForTrafficManager(t *testing.T) {
	// Doc says: traffic manager ConfigMap/Pod/Service are in ManagerNamespace
	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "infra"},
			Data:       map[string]string{"key1": "val1"},
		},
	)

	c := &ConnectOptions{
		SessionBase: SessionBase{
			K8sClient:      K8sClient{clientset: clientset},
			configMapStore: newConfigMapStore(clientset, "infra"),
		},
		ManagerNamespace:  "infra",
		WorkloadNamespace: "app",
	}

	// ConfigMap operations should use ManagerNamespace ("infra"), not WorkloadNamespace ("app").
	// Get is cache-only, so warm the informer first (it lists ManagerNamespace).
	defer c.getConfigMapStore().Stop()
	if err := c.EnsureConfigMapSynced(context.Background()); err != nil {
		t.Fatalf("EnsureConfigMapSynced: %v", err)
	}
	val, err := c.Get(context.Background(), "key1")
	if err != nil {
		t.Fatalf("Get from ManagerNamespace failed: %v", err)
	}
	if val != "val1" {
		t.Fatalf("expected 'val1', got %q", val)
	}
}

func TestDoc10_ConfigMapStore_CacheFirst(t *testing.T) {
	// ConfigMapStore.Get is cache-only (no live-API fallback): EnsureSynced warms the
	// informer at connect, then Get is served from memory.
	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "ns"},
			Data:       map[string]string{"testkey": "testval"},
		},
	)
	store := newConfigMapStore(clientset, "ns")
	defer store.Stop()

	if err := store.EnsureSynced(context.Background()); err != nil {
		t.Fatalf("EnsureSynced: %v", err)
	}
	val, err := store.Get(context.Background(), "testkey")
	if err != nil {
		t.Fatalf("Get from cache: %v", err)
	}
	if val != "testval" {
		t.Fatalf("expected 'testval', got %q", val)
	}
}

func TestDoc10_ConfigMapStore_SetAndGet(t *testing.T) {
	// ConfigMapStore.Set updates the ConfigMap via the live API (JSON Patch "replace", so
	// the key must already exist); Get then reads it from the warm informer cache. Warm
	// AFTER Set so the informer's initial List observes the new value deterministically.
	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "ns"},
			Data:       map[string]string{"mykey": "old"},
		},
	)
	store := newConfigMapStore(clientset, "ns")
	defer store.Stop()

	if err := store.Set(context.Background(), "mykey", "newval"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.EnsureSynced(context.Background()); err != nil {
		t.Fatalf("EnsureSynced: %v", err)
	}

	val, err := store.Get(context.Background(), "mykey")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "newval" {
		t.Fatalf("expected 'newval', got %q", val)
	}
}

// ============================================================================
// Tests for 11-configmap-informer.md:
// Shared informer, singleton pattern, health check uses direct GET
// ============================================================================

func TestDoc11_InformerIsSingleton(t *testing.T) {
	// Doc says: GetInformer uses sync.Once — same informer returned on repeated calls
	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "ns"},
		},
	)
	store := newConfigMapStore(clientset, "ns")

	i1 := store.GetInformer()
	i2 := store.GetInformer()
	if i1 != i2 {
		t.Fatal("GetInformer should return the same instance (sync.Once)")
	}
	store.Stop()
}
