package handler

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/containernetworking/cni/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

// ---- tunHostRoutes ----

func TestTunHostRoutes_BothFamilies(t *testing.T) {
	v4 := &net.IPNet{IP: net.ParseIP("198.18.0.10"), Mask: net.CIDRMask(16, 32)}
	v6 := &net.IPNet{IP: net.ParseIP("2001:2::10"), Mask: net.CIDRMask(64, 128)}

	routes := tunHostRoutes(v4, v6)

	if len(routes) != 2 {
		t.Fatalf("expected 2 routes, got %d: %v", len(routes), routes)
	}

	// IPv4 route must be a /32 host route.
	assertRoute(t, routes[0], v4.IP, 32, 32)
	// IPv6 route must be a /128 host route.
	assertRoute(t, routes[1], v6.IP, 128, 128)
}

func TestTunHostRoutes_OnlyIPv4(t *testing.T) {
	v4 := &net.IPNet{IP: net.ParseIP("198.18.0.5"), Mask: net.CIDRMask(16, 32)}

	routes := tunHostRoutes(v4, nil)

	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d: %v", len(routes), routes)
	}
	assertRoute(t, routes[0], v4.IP, 32, 32)
}

func TestTunHostRoutes_OnlyIPv6(t *testing.T) {
	v6 := &net.IPNet{IP: net.ParseIP("2001:2::7"), Mask: net.CIDRMask(64, 128)}

	routes := tunHostRoutes(nil, v6)

	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d: %v", len(routes), routes)
	}
	assertRoute(t, routes[0], v6.IP, 128, 128)
}

func TestTunHostRoutes_BothNil(t *testing.T) {
	routes := tunHostRoutes(nil, nil)
	if len(routes) != 0 {
		t.Fatalf("expected 0 routes for nil inputs, got %d: %v", len(routes), routes)
	}
}

// assertRoute verifies a Route's Dst IP and mask bits.
func assertRoute(t *testing.T, r types.Route, wantIP net.IP, ones, bits int) {
	t.Helper()
	if !r.Dst.IP.Equal(wantIP) {
		t.Errorf("route IP: want %s, got %s", wantIP, r.Dst.IP)
	}
	wantMask := net.CIDRMask(ones, bits)
	if r.Dst.Mask.String() != wantMask.String() {
		t.Errorf("route mask: want /%d, got %s", ones, r.Dst.Mask)
	}
}

// ---- buildTunConfig ----

func TestBuildTunConfig_IPv4Only(t *testing.T) {
	v4 := &net.IPNet{IP: net.ParseIP("198.18.0.20"), Mask: net.CIDRMask(16, 32)}
	routes := []types.Route{
		{Dst: net.IPNet{IP: net.ParseIP("10.0.0.0"), Mask: net.CIDRMask(8, 32)}},
	}

	cfg := buildTunConfig(v4, nil, routes)

	wantAddr := (&net.IPNet{IP: v4.IP, Mask: net.CIDRMask(32, 32)}).String()
	if cfg.Addr != wantAddr {
		t.Errorf("Addr: want %q, got %q", wantAddr, cfg.Addr)
	}
	if cfg.MTU != config.DefaultMTU {
		t.Errorf("MTU: want %d, got %d", config.DefaultMTU, cfg.MTU)
	}
	if len(cfg.Routes) != len(routes) {
		t.Errorf("Routes: want %d, got %d", len(routes), len(cfg.Routes))
	}
	// Addr6 must be empty when v6 is nil.
	if cfg.Addr6 != "" {
		t.Errorf("Addr6: want empty, got %q", cfg.Addr6)
	}
}

func TestBuildTunConfig_MTUPassthrough(t *testing.T) {
	v4 := &net.IPNet{IP: net.ParseIP("198.18.0.1"), Mask: net.CIDRMask(32, 32)}
	cfg := buildTunConfig(v4, nil, nil)
	if cfg.MTU != config.DefaultMTU {
		t.Errorf("MTU want %d got %d", config.DefaultMTU, cfg.MTU)
	}
}

func TestBuildTunConfig_RoutesPassedThrough(t *testing.T) {
	v4 := &net.IPNet{IP: net.ParseIP("198.18.0.3"), Mask: net.CIDRMask(32, 32)}
	routes := []types.Route{
		{Dst: net.IPNet{IP: net.ParseIP("192.168.0.0"), Mask: net.CIDRMask(16, 32)}},
		{Dst: net.IPNet{IP: net.ParseIP("10.96.0.0"), Mask: net.CIDRMask(12, 32)}},
	}
	cfg := buildTunConfig(v4, nil, routes)
	if len(cfg.Routes) != 2 {
		t.Errorf("want 2 routes in config, got %d", len(cfg.Routes))
	}
}

// buildTunConfig with a non-nil v6 when IPv6 is *disabled* on the host should leave
// Addr6 empty (the function gates on IsIPv6Enabled). We cannot force-enable IPv6,
// but we can assert the function does not panic and returns a valid config.
func TestBuildTunConfig_V6NoPanic(t *testing.T) {
	v4 := &net.IPNet{IP: net.ParseIP("198.18.0.4"), Mask: net.CIDRMask(32, 32)}
	v6 := &net.IPNet{IP: net.ParseIP("2001:2::4"), Mask: net.CIDRMask(128, 128)}
	cfg := buildTunConfig(v4, v6, nil)
	// cfg.Addr6 is either set (if IPv6 enabled) or empty (if disabled) — both are valid.
	// Validate Addr is always the v4 host address.
	wantAddr := (&net.IPNet{IP: v4.IP, Mask: net.CIDRMask(32, 32)}).String()
	if cfg.Addr != wantAddr {
		t.Errorf("Addr: want %q, got %q", wantAddr, cfg.Addr)
	}
	_ = cfg.Addr6 // no assertion on v6 — depends on host config
}

// buildTunConfig must always set MTU regardless of whether routes are empty.
func TestBuildTunConfig_EmptyRoutes(t *testing.T) {
	v4 := &net.IPNet{IP: net.ParseIP("198.18.0.2"), Mask: net.CIDRMask(32, 32)}
	cfg := buildTunConfig(v4, nil, []types.Route{})
	wantAddr := (&net.IPNet{IP: v4.IP, Mask: net.CIDRMask(32, 32)}).String()
	if cfg.Addr != wantAddr {
		t.Errorf("Addr: want %q, got %q", wantAddr, cfg.Addr)
	}
	if cfg.MTU != config.DefaultMTU {
		t.Errorf("MTU: want %d, got %d", config.DefaultMTU, cfg.MTU)
	}
}

// buildTunConfig: ensure the returned tun.Config is the right type (smoke test).
func TestBuildTunConfig_ReturnType(t *testing.T) {
	v4 := &net.IPNet{IP: net.ParseIP("198.18.0.99"), Mask: net.CIDRMask(32, 32)}
	var cfg tun.Config = buildTunConfig(v4, nil, nil)
	_ = cfg
}

// ---- listResolvableNamespaces ----

func newNMWithFakeClient(clientset *fake.Clientset, workloadNS string) *NetworkManager {
	return newNetworkManager(NetworkConfig{
		Clientset:         clientset,
		WorkloadNamespace: workloadNS,
		Lock:              &sync.Mutex{},
	})
}

func TestListResolvableNamespaces_WorkloadFirst(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "my-app"}},
	)
	nm := newNMWithFakeClient(clientset, "my-app")

	ns := nm.listResolvableNamespaces(context.Background())

	if len(ns) == 0 {
		t.Fatal("expected non-empty namespace list")
	}
	if ns[0] != "my-app" {
		t.Errorf("WorkloadNamespace must be first, got %q", ns[0])
	}
}

func TestListResolvableNamespaces_NoDuplicates(t *testing.T) {
	// "my-app" is the workload namespace and also exists in the cluster list.
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "my-app"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
	)
	nm := newNMWithFakeClient(clientset, "my-app")

	ns := nm.listResolvableNamespaces(context.Background())

	seen := make(map[string]int)
	for i, n := range ns {
		if prev, ok := seen[n]; ok {
			t.Errorf("duplicate namespace %q at positions %d and %d", n, prev, i)
		}
		seen[n] = i
	}
}

func TestListResolvableNamespaces_ContainsAllClusterNS(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-a"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-b"}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-c"}},
	)
	nm := newNMWithFakeClient(clientset, "ns-a")

	ns := nm.listResolvableNamespaces(context.Background())

	want := map[string]bool{"ns-a": true, "ns-b": true, "ns-c": true}
	for name := range want {
		found := false
		for _, n := range ns {
			if n == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("namespace %q missing from list %v", name, ns)
		}
	}
}

func TestListResolvableNamespaces_FallbackOnListError(t *testing.T) {
	// An empty fake clientset has no Namespace objects; List returns an empty list
	// (not an error), so we use a workload-NS not present in the cluster to exercise
	// the "only workload ns" path indirectly.
	clientset := fake.NewSimpleClientset() // no namespaces registered
	nm := newNMWithFakeClient(clientset, "isolated-ns")

	ns := nm.listResolvableNamespaces(context.Background())

	if len(ns) == 0 {
		t.Fatal("expected at least WorkloadNamespace in result")
	}
	if ns[0] != "isolated-ns" {
		t.Errorf("WorkloadNamespace must be first, got %q", ns[0])
	}
}
