package handler

import (
	"context"
	"net"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func newTestConnectOptions(t *testing.T) *ConnectOptions {
	t.Helper()
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-123456789012"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: "", config.KeyClusterCIDRs: ""},
		},
		&v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string][]byte{config.TLSCertKey: []byte("cert"), config.TLSPrivateKeyKey: []byte("key"), config.TLSServerName: []byte("host")},
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	return &ConnectOptions{
		ManagerNamespace:  "test-ns",
		WorkloadNamespace: "default",
		K8sClient:         K8sClient{clientset: clientset},
		configMapStore:    newConfigMapStore(clientset, "test-ns"),
		ctx:               ctx,
		cancel:            cancel,
	}
}

func TestConnectOptions_SetGet(t *testing.T) {
	c := newTestConnectOptions(t)
	if err := c.Set(context.Background(), "mykey", "myval"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, err := c.Get(context.Background(), "mykey")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "myval" {
		t.Fatalf("want 'myval', got %q", val)
	}
}

func TestConnectOptions_GetLocalTunIP(t *testing.T) {
	c := newTestConnectOptions(t)
	v4, v6 := c.GetLocalTunIP()
	if v4 != "" || v6 != "" {
		t.Fatal("expected empty when no network manager")
	}
}

func TestConnectOptions_RollbackFuncs(t *testing.T) {
	c := newTestConnectOptions(t)
	var order []int
	c.AddRollbackFunc(func() error { order = append(order, 1); return nil })
	c.AddRollbackFunc(func() error { order = append(order, 2); return nil })

	funcs := c.getRollbackFuncs()
	if len(funcs) != 2 {
		t.Fatalf("expected 2, got %d", len(funcs))
	}
	for _, f := range funcs {
		_ = f()
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("order: %v", order)
	}
}

func TestConnectOptions_GetConnectionID(t *testing.T) {
	c := newTestConnectOptions(t)
	if c.GetConnectionID() != "" {
		t.Fatal("expected empty connectionID before assignment")
	}
	c.ConnectionID = "abc123def456"
	if c.GetConnectionID() != "abc123def456" {
		t.Fatalf("expected abc123def456, got %s", c.GetConnectionID())
	}
}

func TestConnectOptions_GetConfigMapInformer(t *testing.T) {
	c := newTestConnectOptions(t)
	i1 := c.GetConfigMapInformer()
	i2 := c.GetConfigMapInformer()
	if i1 == nil {
		t.Fatal("nil informer")
	}
	if i1 != i2 {
		t.Fatal("not cached")
	}
}

func TestConnectOptions_Context(t *testing.T) {
	c := newTestConnectOptions(t)
	if c.Context() == nil {
		t.Fatal("nil context")
	}
}

func TestConnectOptions_GetFactory(t *testing.T) {
	c := newTestConnectOptions(t)
	// factory is nil in test — just verify no panic
	f := c.GetFactory()
	if f != nil {
		t.Log("factory is set (unexpected in unit test)")
	}
}

func TestConnectOptions_LeaveAllProxyResources_Empty(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
	err := c.LeaveAllProxyResources(context.Background())
	if err != nil {
		t.Fatalf("LeaveAllProxyResources empty: %v", err)
	}
}

func TestConnectOptions_ProxyResources(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
	c.proxyManager.Add(&Proxy{workload: "deploy/a", namespace: "ns1"})
	c.proxyManager.Add(&Proxy{workload: "deploy/b", namespace: "ns2"})
	res := c.ProxyResources()
	if len(res) != 2 {
		t.Fatalf("expected 2 proxy resources, got %d", len(res))
	}
}

func TestConnectOptions_Set_MultipleKeys(t *testing.T) {
	c := newTestConnectOptions(t)
	ctx := context.Background()

	keys := map[string]string{
		"alpha": "one",
		"beta":  "two",
		"gamma": "three",
	}
	for k, v := range keys {
		if err := c.Set(ctx, k, v); err != nil {
			t.Fatalf("Set(%q, %q): %v", k, v, err)
		}
	}
	for k, want := range keys {
		got, err := c.Get(ctx, k)
		if err != nil {
			t.Fatalf("Get(%q): %v", k, err)
		}
		if got != want {
			t.Fatalf("Get(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestConnectOptions_Get_NonExistent(t *testing.T) {
	c := newTestConnectOptions(t)
	val, err := c.Get(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Get non-existent key: %v", err)
	}
	if val != "" {
		t.Fatalf("expected empty string for non-existent key, got %q", val)
	}
}

func TestConnectOptions_Set_Overwrite(t *testing.T) {
	c := newTestConnectOptions(t)
	ctx := context.Background()

	if err := c.Set(ctx, "dup", "first"); err != nil {
		t.Fatalf("Set first: %v", err)
	}
	if err := c.Set(ctx, "dup", "second"); err != nil {
		t.Fatalf("Set second: %v", err)
	}
	got, err := c.Get(ctx, "dup")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "second" {
		t.Fatalf("overwrite failed: got %q, want %q", got, "second")
	}
}

func TestConnectOptions_GetConnectionID_NilReceiver(t *testing.T) {
	var c *ConnectOptions
	id := c.GetConnectionID()
	if id != "" {
		t.Fatalf("expected empty connectionID on nil receiver, got %q", id)
	}
}

func TestConnectOptions_ProxyResources_Empty(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
	res := c.ProxyResources()
	if len(res) != 0 {
		t.Fatalf("expected 0 proxy resources, got %d", len(res))
	}
}

func TestConnectOptions_ProxyResources_Nil(t *testing.T) {
	c := newTestConnectOptions(t)
	// proxyManager is nil by default
	res := c.ProxyResources()
	if res != nil {
		t.Fatalf("expected nil ProxyResources, got %v", res)
	}
}

func TestConnectOptions_ProxyResources_ToResources(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
	c.proxyManager.Add(&Proxy{workload: "deployments.apps/web", namespace: "prod"})
	c.proxyManager.Add(&Proxy{workload: "statefulsets.apps/db", namespace: "prod"})
	c.proxyManager.Add(&Proxy{workload: "deployments.apps/api", namespace: "staging"})
	res := c.ProxyResources()
	if len(res) != 3 {
		t.Fatalf("expected 3, got %d", len(res))
	}
	resources := res.ToResources()
	if len(resources) != 3 {
		t.Fatalf("ToResources: expected 3, got %d", len(resources))
	}
	// Verify ordering is preserved.
	if resources[0].Workload != "deployments.apps/web" || resources[0].Namespace != "prod" {
		t.Fatalf("resources[0] = %+v", resources[0])
	}
	if resources[2].Workload != "deployments.apps/api" || resources[2].Namespace != "staging" {
		t.Fatalf("resources[2] = %+v", resources[2])
	}
}

func TestDedupAndFilterCIDRs(t *testing.T) {
	mustParseCIDR := func(s string) *net.IPNet {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", s, err)
		}
		return cidr
	}

	t.Run("empty input", func(t *testing.T) {
		result := dedupAndFilterCIDRs(nil, nil)
		if len(result) != 0 {
			t.Fatalf("expected empty, got %v", result)
		}
	})

	t.Run("overlapping CIDRs deduplicated", func(t *testing.T) {
		cidrs := []*net.IPNet{
			mustParseCIDR("10.0.0.0/16"),
			mustParseCIDR("10.0.1.0/24"),
		}
		result := dedupAndFilterCIDRs(cidrs, nil)
		if len(result) != 1 {
			t.Fatalf("expected 1 CIDR after dedup, got %d: %v", len(result), result)
		}
		if result[0].String() != "10.0.0.0/16" {
			t.Fatalf("expected 10.0.0.0/16, got %s", result[0])
		}
	})

	t.Run("API server IPs filtered", func(t *testing.T) {
		apiServerIPs := []net.IP{net.ParseIP("10.0.0.1")}
		cidrs := []*net.IPNet{
			mustParseCIDR("10.0.0.0/24"),
			mustParseCIDR("192.168.0.0/16"),
		}
		result := dedupAndFilterCIDRs(cidrs, apiServerIPs)
		if len(result) != 1 {
			t.Fatalf("expected 1 CIDR after filtering, got %d: %v", len(result), result)
		}
		if result[0].String() != "192.168.0.0/16" {
			t.Fatalf("expected 192.168.0.0/16, got %s", result[0])
		}
	})

	t.Run("non-overlapping preserved", func(t *testing.T) {
		cidrs := []*net.IPNet{
			mustParseCIDR("10.0.0.0/8"),
			mustParseCIDR("172.16.0.0/12"),
			mustParseCIDR("192.168.0.0/16"),
		}
		result := dedupAndFilterCIDRs(cidrs, nil)
		if len(result) != 3 {
			t.Fatalf("expected 3 CIDRs, got %d: %v", len(result), result)
		}
	})
}
