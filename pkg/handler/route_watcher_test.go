package handler

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// TestNextPortForwardDelay covers the reconnect backoff: healthy sessions reset to the
// initial delay, short-lived failures double up to the cap.
func TestNextPortForwardDelay(t *testing.T) {
	// A healthy session (>= threshold) that then drops resets to the fast initial delay.
	if got := nextPortForwardDelay(portForwardReconnectMaxDelay, portForwardHealthySession); got != portForwardReconnectDelay {
		t.Errorf("healthy session: got %v, want reset to %v", got, portForwardReconnectDelay)
	}
	if got := nextPortForwardDelay(2*time.Second, portForwardHealthySession+time.Minute); got != portForwardReconnectDelay {
		t.Errorf("long session: got %v, want reset to %v", got, portForwardReconnectDelay)
	}

	// Consecutive short-lived failures double, capped at max, monotonic non-decreasing.
	prev := portForwardReconnectDelay
	last := time.Duration(0)
	for i := 0; i < 20; i++ {
		next := nextPortForwardDelay(prev, time.Second) // short session -> back off
		if next < prev && next != portForwardReconnectMaxDelay {
			t.Fatalf("backoff not monotonic: prev=%v next=%v", prev, next)
		}
		if next > portForwardReconnectMaxDelay {
			t.Fatalf("backoff exceeded cap: %v > %v", next, portForwardReconnectMaxDelay)
		}
		prev, last = next, next
	}
	if last != portForwardReconnectMaxDelay {
		t.Errorf("after many failures delay should reach cap %v, got %v", portForwardReconnectMaxDelay, last)
	}

	// First failure from the initial delay doubles.
	if got := nextPortForwardDelay(portForwardReconnectDelay, time.Second); got != 2*portForwardReconnectDelay {
		t.Errorf("first backoff: got %v, want %v", got, 2*portForwardReconnectDelay)
	}
	// Never returns below the initial floor.
	if got := nextPortForwardDelay(0, time.Second); got < portForwardReconnectDelay {
		t.Errorf("backoff below floor: %v", got)
	}
}

type routeFrameSink struct {
	addedCIDRs [][]string
	dnsCalls   [][]v1.Service
}

func (s *routeFrameSink) addCIDR(c []string)    { s.addedCIDRs = append(s.addedCIDRs, c) }
func (s *routeFrameSink) setDNS(v []v1.Service) { s.dnsCalls = append(s.dnsCalls, v) }

func svcRec(name, ns, clusterIP string) *rpc.ServiceRecord {
	r := &rpc.ServiceRecord{Name: name, Namespace: ns}
	if clusterIP != "" {
		r.ClusterIPs = []string{clusterIP}
	}
	return r
}

// TestApplyRouteFrame drives snapshot + delta frames through applyRouteFrame and asserts
// the maintained service map, add-only routing, DNS pushes, and snapshot reset.
func TestApplyRouteFrame(t *testing.T) {
	services := map[string]*rpc.ServiceRecord{}
	sink := &routeFrameSink{}

	// Snapshot: seed pods + one service.
	applyRouteFrame(&rpc.NamespaceRoutesResponse{
		Snapshot:         true,
		Enabled:          true,
		AddedPodCIDRs:    []string{"10.244.1.0/24", "10.244.2.0/24"},
		UpsertedServices: []*rpc.ServiceRecord{svcRec("web", "ns", "10.96.0.10")},
		Version:          1,
	}, services, sink.addCIDR, sink.setDNS)

	if len(sink.addedCIDRs) != 1 || len(sink.addedCIDRs[0]) != 2 {
		t.Fatalf("snapshot: expected one addCIDR call with 2 prefixes, got %v", sink.addedCIDRs)
	}
	if len(services) != 1 || services["ns/web"] == nil {
		t.Fatalf("snapshot: services=%v, want {ns/web}", services)
	}
	if len(sink.dnsCalls) != 1 || len(sink.dnsCalls[0]) != 1 {
		t.Fatalf("snapshot: expected one DNS push with 1 service, got %v", sink.dnsCalls)
	}

	// Delta: add a service + a new pod CIDR.
	applyRouteFrame(&rpc.NamespaceRoutesResponse{
		Enabled:          true,
		AddedPodCIDRs:    []string{"10.244.3.0/24"},
		UpsertedServices: []*rpc.ServiceRecord{svcRec("api", "ns", "10.96.0.20")},
		Version:          2,
	}, services, sink.addCIDR, sink.setDNS)

	if len(services) != 2 {
		t.Fatalf("delta add: services=%v, want 2", services)
	}
	if got := sink.addedCIDRs[len(sink.addedCIDRs)-1]; len(got) != 1 || got[0] != "10.244.3.0/24" {
		t.Fatalf("delta add: last addCIDR=%v, want [10.244.3.0/24]", got)
	}

	// Delta: remove a service. Routes are add-only, so RemovedPodCIDRs must NOT call addCIDR
	// nor unroute — only DNS updates.
	cidrCallsBefore := len(sink.addedCIDRs)
	applyRouteFrame(&rpc.NamespaceRoutesResponse{
		Enabled:            true,
		RemovedPodCIDRs:    []string{"10.244.1.0/24"},
		RemovedServiceKeys: []string{"ns/web"},
		Version:            3,
	}, services, sink.addCIDR, sink.setDNS)

	if _, ok := services["ns/web"]; ok {
		t.Fatalf("delta remove: ns/web should be gone, services=%v", services)
	}
	if len(services) != 1 || services["ns/api"] == nil {
		t.Fatalf("delta remove: services=%v, want {ns/api}", services)
	}
	if len(sink.addedCIDRs) != cidrCallsBefore {
		t.Fatalf("delta remove: routes are add-only, addCIDR must not be called for RemovedPodCIDRs")
	}

	// A no-op delta (nothing added/removed) must not push DNS.
	dnsBefore := len(sink.dnsCalls)
	applyRouteFrame(&rpc.NamespaceRoutesResponse{Enabled: true, Version: 4}, services, sink.addCIDR, sink.setDNS)
	if len(sink.dnsCalls) != dnsBefore {
		t.Fatalf("no-op delta must not push DNS")
	}

	// Snapshot again resets the service map (in place) before reapplying.
	applyRouteFrame(&rpc.NamespaceRoutesResponse{
		Snapshot:         true,
		Enabled:          true,
		UpsertedServices: []*rpc.ServiceRecord{svcRec("only", "ns", "10.96.0.30")},
		Version:          5,
	}, services, sink.addCIDR, sink.setDNS)
	if len(services) != 1 || services["ns/only"] == nil {
		t.Fatalf("snapshot reset: services=%v, want only {ns/only}", services)
	}
}

// TestServiceRecordsToServices verifies the wire->corev1 conversion the DNS layer consumes.
func TestServiceRecordsToServices(t *testing.T) {
	recs := map[string]*rpc.ServiceRecord{
		"ns/web": {Name: "web", Namespace: "ns", ClusterIPs: []string{"10.96.0.10", "fd00::10"}},
		"ns/ext": {Name: "ext", Namespace: "ns", ExternalName: "example.com"},
	}
	out := serviceRecordsToServices(recs)
	if len(out) != 2 {
		t.Fatalf("got %d services, want 2", len(out))
	}
	byName := map[string]v1.Service{}
	for _, s := range out {
		byName[s.Name] = s
	}
	if web := byName["web"]; web.Spec.ClusterIP != "10.96.0.10" || len(web.Spec.ClusterIPs) != 2 {
		t.Errorf("web conversion wrong: %+v", web.Spec)
	}
	if ext := byName["ext"]; ext.Spec.ExternalName != "example.com" || ext.Spec.ClusterIP != "" {
		t.Errorf("ext conversion wrong: %+v", ext.Spec)
	}
}
