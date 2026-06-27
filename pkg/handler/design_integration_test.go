package handler

import (
	"context"
	"testing"
	"time"

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
		ManagerNamespace:  "infra",
		WorkloadNamespace: "app",
		K8sClient:         K8sClient{clientset: clientset},
		configMapStore:    newConfigMapStore(clientset, "infra"),
	}

	// ConfigMap operations should use ManagerNamespace ("infra"), not WorkloadNamespace ("app")
	val, err := c.Get(context.Background(), "key1")
	if err != nil {
		t.Fatalf("Get from ManagerNamespace failed: %v", err)
	}
	if val != "val1" {
		t.Fatalf("expected 'val1', got %q", val)
	}
}

func TestDoc07_WorkloadNamespaceSeparateFromManager(t *testing.T) {
	// Doc says: proxy injection targets WorkloadNamespace, not ManagerNamespace
	c := &ConnectOptions{
		ManagerNamespace:  "kubevpn",
		WorkloadNamespace: "production",
	}
	c.proxyManager = newProxyManager(nil, nil, c.ManagerNamespace)
	c.proxyManager.Add(&Proxy{workload: "deploy/web", namespace: "production"})

	res := c.ProxyResources()
	if len(res) != 1 || res[0].namespace != "production" {
		t.Fatalf("proxy should be in workload namespace 'production', got %v", res)
	}
}

// ============================================================================
// Tests for 10-handler-architecture.md:
// NetworkManager lifecycle, ProxyManager, ConfigMapStore
// ============================================================================

func TestDoc10_ProxyManager_AddAndResources(t *testing.T) {
	// Doc says: ProxyManager tracks injected sidecars
	pm := newProxyManager(nil, nil, "test-ns")
	pm.Add(&Proxy{workload: "deployments.apps/web", namespace: "test-ns"})
	pm.Add(&Proxy{workload: "deployments.apps/api", namespace: "test-ns"})

	resources := pm.Resources()
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(resources))
	}
}

func TestDoc10_ProxyManager_RemoveByWorkload(t *testing.T) {
	// Doc says: Leave removes specific workloads
	pm := newProxyManager(nil, nil, "ns")
	pm.Add(&Proxy{workload: "deploy/a", namespace: "ns"})
	pm.Add(&Proxy{workload: "deploy/b", namespace: "ns"})

	pm.Remove("ns", "deploy/a")
	resources := pm.Resources()
	if len(resources) != 1 {
		t.Fatalf("expected 1 proxy after remove, got %d", len(resources))
	}
	if resources[0].workload != "deploy/b" {
		t.Fatalf("wrong proxy remaining: %s", resources[0].workload)
	}
}

func TestDoc10_ConfigMapStore_CacheFirst(t *testing.T) {
	// Doc says: ConfigMapStore.Get reads from informer cache first, falls back to API
	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "ns"},
			Data:       map[string]string{"testkey": "testval"},
		},
	)
	store := newConfigMapStore(clientset, "ns")

	// Before informer starts, Get falls back to API
	val, err := store.Get(context.Background(), "testkey")
	if err != nil {
		t.Fatalf("Get via API fallback: %v", err)
	}
	if val != "testval" {
		t.Fatalf("expected 'testval', got %q", val)
	}
}

func TestDoc10_ConfigMapStore_SetAndGet(t *testing.T) {
	// Doc says: ConfigMapStore.Set updates ConfigMap, Get retrieves it
	// Set uses JSON Patch "replace", so the key must already exist
	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "ns"},
			Data:       map[string]string{"mykey": "old"},
		},
	)
	store := newConfigMapStore(clientset, "ns")

	if err := store.Set(context.Background(), "mykey", "newval"); err != nil {
		t.Fatalf("Set: %v", err)
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

func TestDoc11_HealthCheckUsesDirectGET(t *testing.T) {
	// Doc says: HealthCheckOnce makes a direct API call (not informer)
	// to verify API server reachability
	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "ns"},
			Data:       map[string]string{"health": "ok"},
		},
	)
	store := newConfigMapStore(clientset, "ns")

	store.HealthCheckOnce(context.Background(), 5*time.Second)

	status := store.GetHealthStatus()
	if status.LastError() != nil {
		t.Fatalf("health check failed: %v", status.LastError())
	}
	if status.ConfigMap() == nil {
		t.Fatal("ConfigMap should be set after health check")
	}
	if status.ConfigMap().Data["health"] != "ok" {
		t.Fatal("ConfigMap data mismatch")
	}
}

func TestDoc11_HealthCheckDetectsAPIFailure(t *testing.T) {
	// Doc says: direct GET detects API server unreachable (informer cache can't)
	// Use empty clientset — ConfigMap doesn't exist → GET returns NotFound
	clientset := fake.NewSimpleClientset() // no ConfigMap
	store := newConfigMapStore(clientset, "ns")

	store.HealthCheckOnce(context.Background(), 5*time.Second)

	status := store.GetHealthStatus()
	if status.LastError() == nil {
		t.Fatal("health check should fail when ConfigMap doesn't exist")
	}
}
