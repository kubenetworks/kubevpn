package handler

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// TestNextPortForwardDelay covers the reconnect backoff across both dimensions that decide it:
// how long the session lasted, and whether it ever carried data-plane traffic.
func TestNextPortForwardDelay(t *testing.T) {
	for _, tc := range []struct {
		name            string
		cur             time.Duration
		sessionDuration time.Duration
		primed          bool
		want            time.Duration
		why             string
	}{{
		name: "primed and long enough resets to the floor",
		cur:  portForwardReconnectMaxDelay, sessionDuration: portForwardHealthySession, primed: true,
		want: portForwardReconnectDelay,
		why:  "a session that worked and then dropped (pod recreation) must reconnect fast",
	}, {
		name: "primed and much longer resets to the floor",
		cur:  2 * time.Second, sessionDuration: portForwardHealthySession + time.Minute, primed: true,
		want: portForwardReconnectDelay,
	}, {
		// The regression this whole change exists for. livenessStartupDeadline ==
		// portForwardHealthySession, so a black-holed session always reaches the "healthy"
		// duration; keying only off duration reset the backoff forever.
		name: "never primed at exactly the healthy duration must NOT reset",
		cur:  time.Second, sessionDuration: portForwardHealthySession, primed: false,
		want: 2 * time.Second,
		why:  "a black-holed session reaches this duration by definition; it is not evidence of health",
	}, {
		name: "never primed and long must NOT reset",
		cur:  4 * time.Second, sessionDuration: 10 * time.Minute, primed: false,
		want: 8 * time.Second,
	}, {
		name: "primed but short-lived doubles, capped at the normal max",
		cur:  portForwardReconnectMaxDelay, sessionDuration: time.Second, primed: true,
		want: portForwardReconnectMaxDelay,
	}, {
		name: "first backoff doubles from the floor",
		cur:  portForwardReconnectDelay, sessionDuration: time.Second, primed: true,
		want: 2 * portForwardReconnectDelay,
	}, {
		name: "a primed session clamps a grown black-hole delay back down",
		cur:  portForwardBlackHoleMaxDelay, sessionDuration: time.Second, primed: true,
		want: portForwardReconnectMaxDelay,
		why:  "recovery must not stay stuck at the black-hole ceiling",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextPortForwardDelay(tc.cur, tc.sessionDuration, tc.primed); got != tc.want {
				t.Errorf("nextPortForwardDelay(%v, %v, primed=%v) = %v, want %v. %s",
					tc.cur, tc.sessionDuration, tc.primed, got, tc.want, tc.why)
			}
		})
	}

	// Consecutive primed-but-short failures double, capped at the normal max, monotonic.
	t.Run("primed short sessions converge to the normal cap", func(t *testing.T) {
		prev, last := portForwardReconnectDelay, time.Duration(0)
		for i := 0; i < 20; i++ {
			next := nextPortForwardDelay(prev, time.Second, true)
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
	})

	// A sustained black hole must converge to the LARGER ceiling, so the control plane that shares
	// this port-forward session gets usable stretches of uptime between attempts.
	t.Run("never-primed sessions converge to the black-hole cap", func(t *testing.T) {
		delay := portForwardReconnectDelay
		for i := 0; i < 20; i++ {
			next := nextPortForwardDelay(delay, portForwardHealthySession, false)
			if next > portForwardBlackHoleMaxDelay {
				t.Fatalf("backoff exceeded black-hole cap: %v > %v", next, portForwardBlackHoleMaxDelay)
			}
			delay = next
		}
		if delay != portForwardBlackHoleMaxDelay {
			t.Errorf("sustained black hole should reach %v, got %v", portForwardBlackHoleMaxDelay, delay)
		}
	})

	// Never returns below the initial floor, whatever the inputs.
	for _, primed := range []bool{true, false} {
		if got := nextPortForwardDelay(0, time.Second, primed); got < portForwardReconnectDelay {
			t.Errorf("backoff below floor with primed=%v: %v", primed, got)
		}
	}
}

type routeFrameSink struct {
	addedCIDRs      [][]string
	addedServiceIPs [][]string
	dnsCalls        [][]v1.Service
}

func (s *routeFrameSink) addCIDR(c []string) { s.addedCIDRs = append(s.addedCIDRs, c) }
func (s *routeFrameSink) addServiceIP(ips []string) {
	s.addedServiceIPs = append(s.addedServiceIPs, ips)
}
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
	}, services, sink.addCIDR, sink.addServiceIP, sink.setDNS)

	if len(sink.addedCIDRs) != 1 || len(sink.addedCIDRs[0]) != 2 {
		t.Fatalf("snapshot: expected one addCIDR call with 2 prefixes, got %v", sink.addedCIDRs)
	}
	if len(services) != 1 || services["ns/web"] == nil {
		t.Fatalf("snapshot: services=%v, want {ns/web}", services)
	}
	if len(sink.dnsCalls) != 1 || len(sink.dnsCalls[0]) != 1 {
		t.Fatalf("snapshot: expected one DNS push with 1 service, got %v", sink.dnsCalls)
	}
	// Service ClusterIPs must be routed, not only fed to DNS: a resolvable name whose
	// ClusterIP has no route resolves but cannot connect. Snapshot routes web's IP.
	if len(sink.addedServiceIPs) != 1 || len(sink.addedServiceIPs[0]) != 1 || sink.addedServiceIPs[0][0] != "10.96.0.10" {
		t.Fatalf("snapshot: expected service IP 10.96.0.10 routed, got %v", sink.addedServiceIPs)
	}

	// Delta: add a service + a new pod CIDR.
	applyRouteFrame(&rpc.NamespaceRoutesResponse{
		Enabled:          true,
		AddedPodCIDRs:    []string{"10.244.3.0/24"},
		UpsertedServices: []*rpc.ServiceRecord{svcRec("api", "ns", "10.96.0.20")},
		Version:          2,
	}, services, sink.addCIDR, sink.addServiceIP, sink.setDNS)

	if len(services) != 2 {
		t.Fatalf("delta add: services=%v, want 2", services)
	}
	if got := sink.addedCIDRs[len(sink.addedCIDRs)-1]; len(got) != 1 || got[0] != "10.244.3.0/24" {
		t.Fatalf("delta add: last addCIDR=%v, want [10.244.3.0/24]", got)
	}
	// Delta upsert must route the new service's ClusterIP too.
	if got := sink.addedServiceIPs[len(sink.addedServiceIPs)-1]; len(got) != 1 || got[0] != "10.96.0.20" {
		t.Fatalf("delta add: last routed service IP=%v, want [10.96.0.20]", got)
	}

	// Delta: remove a service. Routes are add-only, so RemovedPodCIDRs must NOT call addCIDR
	// nor unroute — only DNS updates.
	cidrCallsBefore := len(sink.addedCIDRs)
	svcIPCallsBefore := len(sink.addedServiceIPs)
	applyRouteFrame(&rpc.NamespaceRoutesResponse{
		Enabled:            true,
		RemovedPodCIDRs:    []string{"10.244.1.0/24"},
		RemovedServiceKeys: []string{"ns/web"},
		Version:            3,
	}, services, sink.addCIDR, sink.addServiceIP, sink.setDNS)

	if _, ok := services["ns/web"]; ok {
		t.Fatalf("delta remove: ns/web should be gone, services=%v", services)
	}
	if len(services) != 1 || services["ns/api"] == nil {
		t.Fatalf("delta remove: services=%v, want {ns/api}", services)
	}
	if len(sink.addedCIDRs) != cidrCallsBefore {
		t.Fatalf("delta remove: routes are add-only, addCIDR must not be called for RemovedPodCIDRs")
	}
	if len(sink.addedServiceIPs) != svcIPCallsBefore {
		t.Fatalf("delta remove: no upserts, service-IP routing must not be called")
	}

	// A no-op delta (nothing added/removed) must not push DNS.
	dnsBefore := len(sink.dnsCalls)
	applyRouteFrame(&rpc.NamespaceRoutesResponse{Enabled: true, Version: 4}, services, sink.addCIDR, sink.addServiceIP, sink.setDNS)
	if len(sink.dnsCalls) != dnsBefore {
		t.Fatalf("no-op delta must not push DNS")
	}

	// Snapshot again resets the service map (in place) before reapplying.
	applyRouteFrame(&rpc.NamespaceRoutesResponse{
		Snapshot:         true,
		Enabled:          true,
		UpsertedServices: []*rpc.ServiceRecord{svcRec("only", "ns", "10.96.0.30")},
		Version:          5,
	}, services, sink.addCIDR, sink.addServiceIP, sink.setDNS)
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
