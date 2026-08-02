package handler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/xds"
)

func TestParsePortMap_ColonSeparated(t *testing.T) {
	rule := &xds.Rule{
		PortMap: map[int32]string{
			8080: "29450:19080",
		},
	}
	mappings := rule.ParsePortMap()
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}
	pm := mappings[0]
	if pm.ContainerPort != 8080 || pm.EnvoyPort != 29450 || pm.LocalPort != 19080 {
		t.Errorf("got %+v", pm)
	}
}

func TestParsePortMap_PlainNumber(t *testing.T) {
	rule := &xds.Rule{
		PortMap: map[int32]string{8080: "9090"},
	}
	mappings := rule.ParsePortMap()
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}
	pm := mappings[0]
	if pm.EnvoyPort != 9090 {
		t.Errorf("EnvoyPort: want 9090, got %d", pm.EnvoyPort)
	}
	if pm.LocalPort != 8080 {
		t.Errorf("LocalPort: want 8080 (containerPort fallback), got %d", pm.LocalPort)
	}
}

func TestParsePortMap_Invalid(t *testing.T) {
	rule := &xds.Rule{
		PortMap: map[int32]string{8080: "not-a-number"},
	}
	mappings := rule.ParsePortMap()
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}
	if mappings[0].EnvoyPort != 0 {
		t.Errorf("EnvoyPort: want 0 for invalid, got %d", mappings[0].EnvoyPort)
	}
}

func TestProxyList_AddRemove(t *testing.T) {
	var list ProxyList
	list.Add(&Proxy{workload: "deploy/app1", namespace: "default"})
	list.Add(&Proxy{workload: "deploy/app2", namespace: "default"})
	if len(list) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(list))
	}
	list.Remove("default", "deploy/app1")
	if len(list) != 1 {
		t.Fatalf("expected 1 proxy after remove, got %d", len(list))
	}
}

func TestProxyList_ToResources(t *testing.T) {
	var list ProxyList
	list.Add(&Proxy{workload: "deploy/app1", namespace: "ns1"})
	list.Add(&Proxy{workload: "deploy/app2", namespace: "ns2"})
	resources := list.ToResources()
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(resources))
	}
}

func TestCancelAllTunnels(t *testing.T) {
	tunnels := &sync.Map{}
	var cancelled atomic.Int32
	for i := 0; i < 3; i++ {
		_, cancel := context.WithCancel(context.Background())
		tunnels.Store(i, context.CancelFunc(func() {
			cancel()
			cancelled.Add(1)
		}))
	}
	cancelAllTunnels(tunnels)
	if cancelled.Load() != 3 {
		t.Errorf("expected 3 cancels, got %d", cancelled.Load())
	}
}
