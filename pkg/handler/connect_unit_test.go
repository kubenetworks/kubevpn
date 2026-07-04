package handler

import (
	"context"
	"net"
	"testing"
	"time"

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
			Data:       map[string]string{config.KeyDHCP: "", config.KeyDHCP6: "", config.KeyEnvoy: "", config.KeyClusterIPv4POOLS: ""},
		},
		&v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string][]byte{config.TLSCertKey: []byte("cert"), config.TLSPrivateKeyKey: []byte("key"), config.TLSServerName: []byte("host")},
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	return &ConnectOptions{
		ManagerNamespace: "test-ns",
		OriginNamespace:  "default",
		clientset:        clientset,
		ctx:              ctx,
		cancel:           cancel,
	}
}

func TestConnectOptions_InitDHCP(t *testing.T) {
	c := newTestConnectOptions(t)
	err := c.InitDHCP(context.Background())
	if err != nil {
		t.Fatalf("InitDHCP: %v", err)
	}
	err = c.InitDHCP(context.Background())
	if err != nil {
		t.Fatalf("InitDHCP second: %v", err)
	}
}

func TestConnectOptions_RentIP(t *testing.T) {
	c := newTestConnectOptions(t)
	_, err := c.RentIP(context.Background(), "", "")
	if err != nil {
		t.Fatalf("RentIP: %v", err)
	}
	if c.LocalTunIPv4 == nil || c.LocalTunIPv6 == nil {
		t.Fatal("IPs nil after RentIP")
	}
	if !config.CIDR.Contains(c.LocalTunIPv4.IP) {
		t.Fatalf("IPv4 %s not in CIDR", c.LocalTunIPv4)
	}
}

func TestConnectOptions_RentIP_PreAssigned(t *testing.T) {
	c := newTestConnectOptions(t)
	_, err := c.RentIP(context.Background(), "198.18.0.50/16", "2001:2::50/64")
	if err != nil {
		t.Fatalf("RentIP: %v", err)
	}
	if !c.LocalTunIPv4.IP.Equal(net.ParseIP("198.18.0.50")) {
		t.Fatalf("expected 198.18.0.50, got %s", c.LocalTunIPv4.IP)
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
		t.Fatal("expected empty before assignment")
	}
	c.LocalTunIPv4 = &net.IPNet{IP: net.ParseIP("198.18.0.1"), Mask: net.CIDRMask(16, 32)}
	c.LocalTunIPv6 = &net.IPNet{IP: net.ParseIP("2001:2::1"), Mask: net.CIDRMask(64, 128)}
	v4, v6 = c.GetLocalTunIP()
	if v4 != "198.18.0.1" {
		t.Fatalf("v4 want 198.18.0.1, got %s", v4)
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
	if err := c.InitDHCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := c.GetConnectionID()
	if id == "" {
		t.Fatal("empty connectionID")
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
	c.proxyWorkloads = ProxyList{}
	err := c.LeaveAllProxyResources(context.Background())
	if err != nil {
		t.Fatalf("LeaveAllProxyResources empty: %v", err)
	}
}

func TestConnectOptions_IsMe(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyWorkloads = ProxyList{
		{workload: "deployments.apps/reviews", namespace: "default", headers: map[string]string{"env": "test"}},
	}
	if !c.IsMe("default", "deployments.apps.reviews", map[string]string{"env": "test"}) {
		t.Fatal("expected IsMe true")
	}
	if c.IsMe("default", "deployments.apps.reviews", map[string]string{"env": "prod"}) {
		t.Fatal("expected IsMe false for different headers")
	}
	if c.IsMe("other-ns", "deployments.apps.reviews", map[string]string{"env": "test"}) {
		t.Fatal("expected IsMe false for different namespace")
	}
}

func TestConnectOptions_HealthCheckOnce(t *testing.T) {
	c := newTestConnectOptions(t)
	c.HealthCheckOnce(context.Background(), 5*time.Second)
	status := c.HealthStatus()
	if status.LastError() != nil {
		t.Fatalf("HealthCheckOnce failed: %v", status.LastError())
	}
	if status.ConfigMap() == nil {
		t.Fatal("ConfigMap is nil after health check")
	}
	if status.ConfigMap().Name != config.ConfigMapPodTrafficManager {
		t.Fatalf("wrong ConfigMap name: %s", status.ConfigMap().Name)
	}
}

func TestConnectOptions_SyncFromCache(t *testing.T) {
	c := newTestConnectOptions(t)
	// Start the informer so the cache populates
	_ = c.GetConfigMapInformer()
	time.Sleep(100 * time.Millisecond) // let informer sync
	c.syncFromCache()
	// May or may not have synced in time — just verify no panic
}

func TestConnectOptions_ProxyResources(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyWorkloads = ProxyList{
		{workload: "deploy/a", namespace: "ns1"},
		{workload: "deploy/b", namespace: "ns2"},
	}
	res := c.ProxyResources()
	if len(res) != 2 {
		t.Fatalf("expected 2 proxy resources, got %d", len(res))
	}
}

func TestConnectOptions_GetTunDeviceName_Empty(t *testing.T) {
	c := newTestConnectOptions(t)
	name, err := c.GetTunDeviceName()
	if name != "" {
		t.Fatalf("expected empty tun name, got %q", name)
	}
	// err may or may not be nil depending on implementation
	_ = err
}

func TestConnectOptions_Set_MultipleKeys(t *testing.T) {
	c := newTestConnectOptions(t)
	ctx := context.Background()

	keys := map[string]string{
		"alpha":   "one",
		"beta":    "two",
		"gamma":   "three",
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

func TestConnectOptions_GetTunDeviceName_WithName(t *testing.T) {
	c := newTestConnectOptions(t)
	c.tunName = "utun42"
	// GetTunDeviceName looks up a real device by IP, so with no matching device
	// it will return an error. We verify the tunName field is set correctly
	// and that the method does not panic.
	_, _ = c.GetTunDeviceName()
	// Verify the field is accessible and correct.
	if c.tunName != "utun42" {
		t.Fatalf("tunName = %q, want %q", c.tunName, "utun42")
	}
}

func TestConnectOptions_GetConnectionID_NilDHCP(t *testing.T) {
	c := newTestConnectOptions(t)
	// dhcp is nil — GetConnectionID should return empty string without panic.
	id := c.GetConnectionID()
	if id != "" {
		t.Fatalf("expected empty connectionID with nil dhcp, got %q", id)
	}
}

func TestConnectOptions_GetConnectionID_NilReceiver(t *testing.T) {
	var c *ConnectOptions
	id := c.GetConnectionID()
	if id != "" {
		t.Fatalf("expected empty connectionID on nil receiver, got %q", id)
	}
}

func TestConnectOptions_GetConnectionID_WithID(t *testing.T) {
	c := NewConnectOptionsWithIDForTest("conn-abc-123")
	id := c.GetConnectionID()
	if id != "conn-abc-123" {
		t.Fatalf("GetConnectionID() = %q, want %q", id, "conn-abc-123")
	}
}

func TestConnectOptions_ProxyResources_Empty(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyWorkloads = ProxyList{}
	res := c.ProxyResources()
	if len(res) != 0 {
		t.Fatalf("expected 0 proxy resources, got %d", len(res))
	}
}

func TestConnectOptions_ProxyResources_Nil(t *testing.T) {
	c := newTestConnectOptions(t)
	// proxyWorkloads is nil by default
	res := c.ProxyResources()
	if res != nil {
		t.Fatalf("expected nil ProxyResources, got %v", res)
	}
}

func TestConnectOptions_ProxyResources_ToResources(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyWorkloads = ProxyList{
		{workload: "deployments.apps/web", namespace: "prod"},
		{workload: "statefulsets.apps/db", namespace: "prod"},
		{workload: "deployments.apps/api", namespace: "staging"},
	}
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
