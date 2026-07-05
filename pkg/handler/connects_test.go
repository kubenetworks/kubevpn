package handler

import (
	"net"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
)

// --- Connects.Sort edge cases (basic dependency cases are in sort_test.go) ---

func TestConnects_Sort_Empty(t *testing.T) {
	var connects Connects
	sorted := connects.Sort()
	if len(sorted) != 0 {
		t.Errorf("expected empty result, got %d", len(sorted))
	}
}

func TestConnects_Sort_SingleElement(t *testing.T) {
	connects := Connects{
		{OriginNamespace: "only"},
	}
	sorted := connects.Sort()
	if len(sorted) != 1 || sorted[0].OriginNamespace != "only" {
		t.Errorf("single-element sort failed")
	}
}

func TestConnects_Less_NilA(t *testing.T) {
	// When a (left side) is nil, Less returns true.
	connects := Connects{nil, {OriginNamespace: "clusterA"}}
	if !connects.Less(0, 1) {
		t.Error("expected Less to return true when a is nil")
	}
}

func TestConnects_Sort_NoDependency(t *testing.T) {
	// Two independent clusters. Sort should not panic.
	connects := Connects{
		{
			OriginNamespace:    "clusterA",
			apiServerIPs: []net.IP{net.ParseIP("10.0.0.1")},
		},
		{
			OriginNamespace:    "clusterB",
			apiServerIPs: []net.IP{net.ParseIP("10.0.0.2")},
		},
	}
	sorted := connects.Sort()
	if len(sorted) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(sorted))
	}
}

func TestConnects_Sort_LoopbackIPIgnored(t *testing.T) {
	// Loopback IPs in ExtraCIDR should not create a dependency.
	connects := Connects{
		{
			OriginNamespace:    "clusterA",
			apiServerIPs: []net.IP{net.ParseIP("127.0.0.1")},
		},
		{
			OriginNamespace: "clusterB",
			ExtraRouteInfo: ExtraRouteInfo{
				ExtraCIDR: []string{"127.0.0.0/8"},
			},
			apiServerIPs: []net.IP{net.ParseIP("10.0.0.2")},
		},
	}

	sorted := connects.Sort()
	if len(sorted) != 2 {
		t.Fatalf("expected 2, got %d", len(sorted))
	}
}

func TestConnects_Sort_InvalidCIDRSkipped(t *testing.T) {
	// Invalid CIDR strings should be skipped without panic.
	connects := Connects{
		{
			OriginNamespace:    "clusterA",
			apiServerIPs: []net.IP{net.ParseIP("10.0.0.1")},
		},
		{
			OriginNamespace: "clusterB",
			ExtraRouteInfo: ExtraRouteInfo{
				ExtraCIDR: []string{"not-a-cidr", "also-bad"},
			},
			apiServerIPs: []net.IP{net.ParseIP("10.0.0.2")},
		},
	}

	sorted := connects.Sort()
	if len(sorted) != 2 {
		t.Fatalf("expected 2, got %d", len(sorted))
	}
}

func TestConnects_Sort_ExtraHostLoopbackIgnored(t *testing.T) {
	// Loopback IPs in extraHost should not create a dependency.
	connects := Connects{
		{
			OriginNamespace:    "clusterA",
			apiServerIPs: []net.IP{net.ParseIP("127.0.0.1")},
		},
		{
			OriginNamespace:    "clusterB",
			apiServerIPs: []net.IP{net.ParseIP("10.0.0.2")},
			extraHost:    []dns.Entry{{IP: "127.0.0.1"}},
		},
	}

	sorted := connects.Sort()
	// No dependency should be detected.
	if len(sorted) != 2 {
		t.Fatalf("expected 2, got %d", len(sorted))
	}
}

func TestConnects_Sort_ExtraHostInvalidIPIgnored(t *testing.T) {
	// Invalid IP strings in extraHost should be skipped without panic.
	connects := Connects{
		{
			OriginNamespace:    "clusterA",
			apiServerIPs: []net.IP{net.ParseIP("10.0.0.1")},
		},
		{
			OriginNamespace:    "clusterB",
			apiServerIPs: []net.IP{net.ParseIP("10.0.0.2")},
			extraHost:    []dns.Entry{{IP: "not-an-ip"}},
		},
	}

	sorted := connects.Sort()
	if len(sorted) != 2 {
		t.Fatalf("expected 2, got %d", len(sorted))
	}
}

func TestConnects_Sort_ExactIPMatchInExtraCIDR(t *testing.T) {
	// ExtraCIDR IP exactly matches an API server IP (ip.Equal path in Less).
	connects := Connects{
		{
			OriginNamespace:    "clusterA",
			apiServerIPs: []net.IP{net.ParseIP("10.1.2.3")},
		},
		{
			OriginNamespace: "clusterB",
			ExtraRouteInfo: ExtraRouteInfo{
				ExtraCIDR: []string{"10.1.2.3/32"},
			},
			apiServerIPs: []net.IP{net.ParseIP("172.16.0.1")},
		},
	}

	sorted := connects.Sort()
	if sorted[0].OriginNamespace != "clusterA" {
		t.Errorf("expected clusterA first (dependent on clusterB), got %s", sorted[0].OriginNamespace)
	}
}

// --- Connects.Append tests ---

func TestConnects_Append_NonNil(t *testing.T) {
	var connects Connects
	opt := &ConnectOptions{OriginNamespace: "ns1"}
	result := connects.Append(opt)
	if len(result) != 1 {
		t.Fatalf("expected 1, got %d", len(result))
	}
	if result[0].OriginNamespace != "ns1" {
		t.Errorf("expected ns1, got %s", result[0].OriginNamespace)
	}
}

func TestConnects_Append_Nil(t *testing.T) {
	connects := Connects{
		{OriginNamespace: "existing"},
	}
	result := connects.Append(nil)
	if len(result) != 1 {
		t.Fatalf("expected 1 (nil should not be appended), got %d", len(result))
	}
}

func TestConnects_Append_Multiple(t *testing.T) {
	var connects Connects
	connects = connects.Append(&ConnectOptions{OriginNamespace: "a"})
	connects = connects.Append(nil)
	connects = connects.Append(&ConnectOptions{OriginNamespace: "b"})
	connects = connects.Append(nil)
	connects = connects.Append(&ConnectOptions{OriginNamespace: "c"})
	if len(connects) != 3 {
		t.Fatalf("expected 3, got %d", len(connects))
	}
}

func TestConnects_Append_EmptyToEmpty(t *testing.T) {
	var connects Connects
	result := connects.Append(nil)
	if len(result) != 0 {
		t.Fatalf("expected 0, got %d", len(result))
	}
}

// --- Connects.Len / Swap tests ---

func TestConnects_Len(t *testing.T) {
	connects := Connects{
		{OriginNamespace: "a"},
		{OriginNamespace: "b"},
	}
	if connects.Len() != 2 {
		t.Errorf("expected Len() == 2, got %d", connects.Len())
	}

	var empty Connects
	if empty.Len() != 0 {
		t.Errorf("expected Len() == 0, got %d", empty.Len())
	}
}

func TestConnects_Swap(t *testing.T) {
	a := &ConnectOptions{OriginNamespace: "a"}
	b := &ConnectOptions{OriginNamespace: "b"}
	connects := Connects{a, b}

	connects.Swap(0, 1)
	if connects[0].OriginNamespace != "b" || connects[1].OriginNamespace != "a" {
		t.Errorf("Swap failed: got [%s, %s]", connects[0].OriginNamespace, connects[1].OriginNamespace)
	}
}

// --- ProxyList tests ---

func TestProxyList_Add(t *testing.T) {
	var list ProxyList
	list.Add(&Proxy{workload: "deploy/app1", namespace: "default"})
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}
	list.Add(&Proxy{workload: "deploy/app2", namespace: "default"})
	if len(list) != 2 {
		t.Fatalf("expected 2, got %d", len(list))
	}
}

func TestProxyList_Remove_NilList(t *testing.T) {
	var list *ProxyList
	// Should not panic.
	list.Remove("default", "deploy/app")
}

func TestProxyList_Remove_NotFound(t *testing.T) {
	var list ProxyList
	list.Add(&Proxy{workload: "deploy/app1", namespace: "default"})
	list.Remove("default", "deploy/nonexistent")
	if len(list) != 1 {
		t.Errorf("expected 1 proxy unchanged, got %d", len(list))
	}
}

func TestProxyList_Remove_MultipleMatches(t *testing.T) {
	var list ProxyList
	list.Add(&Proxy{workload: "deploy/app1", namespace: "default"})
	list.Add(&Proxy{workload: "deploy/app1", namespace: "default"})
	list.Add(&Proxy{workload: "deploy/app2", namespace: "default"})
	list.Remove("default", "deploy/app1")
	if len(list) != 1 {
		t.Fatalf("expected 1 proxy after removing duplicates, got %d", len(list))
	}
	if list[0].workload != "deploy/app2" {
		t.Errorf("expected deploy/app2 remaining, got %s", list[0].workload)
	}
}

func TestProxyList_IsMe_Match(t *testing.T) {
	var list ProxyList
	// ConvertUidToWorkload("deployments.apps.productpage") => "deployments.apps/productpage"
	list.Add(&Proxy{
		workload:  "deployments.apps/productpage",
		namespace: "default",
		headers:   map[string]string{"x-user": "test"},
	})

	if !list.IsMe("default", "deployments.apps.productpage", map[string]string{"x-user": "test"}) {
		t.Error("expected IsMe to return true for matching proxy")
	}
}

func TestProxyList_IsMe_NoMatch_DifferentHeaders(t *testing.T) {
	var list ProxyList
	list.Add(&Proxy{
		workload:  "deployments.apps/productpage",
		namespace: "default",
		headers:   map[string]string{"x-user": "test"},
	})

	if list.IsMe("default", "deployments.apps.productpage", map[string]string{"x-user": "other"}) {
		t.Error("expected IsMe to return false for different headers")
	}
}

func TestProxyList_IsMe_NoMatch_DifferentNamespace(t *testing.T) {
	var list ProxyList
	list.Add(&Proxy{
		workload:  "deployments.apps/productpage",
		namespace: "default",
		headers:   map[string]string{},
	})

	if list.IsMe("kube-system", "deployments.apps.productpage", map[string]string{}) {
		t.Error("expected IsMe to return false for different namespace")
	}
}

func TestProxyList_IsMe_NilList(t *testing.T) {
	var list *ProxyList
	if list.IsMe("default", "deployments.apps.productpage", nil) {
		t.Error("expected IsMe on nil list to return false")
	}
}

func TestProxyList_IsMe_EmptyList(t *testing.T) {
	list := make(ProxyList, 0)
	if list.IsMe("default", "deployments.apps.productpage", nil) {
		t.Error("expected IsMe on empty list to return false")
	}
}

func TestProxyList_ToResources_Empty(t *testing.T) {
	var list ProxyList
	resources := list.ToResources()
	if len(resources) != 0 {
		t.Errorf("expected 0 resources from empty list, got %d", len(resources))
	}
}

func TestProxyList_ToResources_PreservesOrder(t *testing.T) {
	var list ProxyList
	list.Add(&Proxy{workload: "deploy/a", namespace: "ns1"})
	list.Add(&Proxy{workload: "deploy/b", namespace: "ns2"})
	list.Add(&Proxy{workload: "deploy/c", namespace: "ns3"})
	resources := list.ToResources()
	if len(resources) != 3 {
		t.Fatalf("expected 3, got %d", len(resources))
	}
	expected := []struct{ ns, wl string }{
		{"ns1", "deploy/a"},
		{"ns2", "deploy/b"},
		{"ns3", "deploy/c"},
	}
	for i, exp := range expected {
		if resources[i].Namespace != exp.ns || resources[i].Workload != exp.wl {
			t.Errorf("index %d: expected {%s,%s}, got {%s,%s}", i, exp.ns, exp.wl, resources[i].Namespace, resources[i].Workload)
		}
	}
}
