package xds

import (
	"testing"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/sirupsen/logrus"
)

func TestAgentOrigins_DirectListenersAndExplicitPodRoutes(t *testing.T) {
	virtual := &Virtual{
		Namespace:         "default",
		UID:               "deployments.apps.reviews.nodes.node-a",
		AgentListenerPort: 20000,
		Ports: []ContainerPort{
			{ContainerPort: 9080, Protocol: "TCP"},
			{ContainerPort: 9081, Protocol: "UDP"},
		},
		AgentOrigins: []*AgentOrigin{
			{PodUID: "pod-a", PodIP: "10.244.0.5", Ports: []AgentOriginPort{
				{ContainerPort: 9080, Protocol: "TCP", AgentListenerPort: 20001},
				{ContainerPort: 9081, Protocol: "UDP", AgentListenerPort: 20002},
			}},
			{PodUID: "pod-b", PodIP: "10.244.0.6", Ports: []AgentOriginPort{
				{ContainerPort: 9080, Protocol: "TCP", AgentListenerPort: 20003},
				{ContainerPort: 9081, Protocol: "UDP", AgentListenerPort: 20004},
			}},
		},
		Rules: []*Rule{{
			OwnerID:      "owner-1",
			LocalTunIPv4: "198.18.0.1",
			LocalTunIPv6: "2001:2::1",
			PortMap:      map[int32]string{9080: "9080", 9081: "9081"},
			IsAgent:      true,
		}},
	}
	listeners, clusters, routes, endpoints := To(virtual, true, logrus.NewEntry(logrus.New()))

	wantTCP := map[string]int32{"10.244.0.5": 20001, "10.244.0.6": 20003}
	wantUDP := map[uint32]bool{20002: true, 20004: true}
	for _, resource := range listeners {
		l := resource.(*listener.Listener)
		if l.Name == "default_deployments.apps.reviews.nodes.node-a_inbound" {
			t.Fatal("node-boundary virtual must not use the sidecar capture listener")
		}
		port := l.Address.GetSocketAddress().GetPortSpecifier().(*core.SocketAddress_PortValue).PortValue
		switch port {
		case 20001, 20003:
			if !l.BindToPort.Value || l.UseOriginalDst.Value {
				t.Fatalf("TCP listener %s is not direct: bind=%v originalDst=%v", l.Name, l.BindToPort.Value, l.UseOriginalDst.Value)
			}
			if len(l.ListenerFilters) != 1 {
				t.Fatalf("direct TCP listener must not use original_dst filter: %s", l.Name)
			}
		case 20002, 20004:
			delete(wantUDP, port)
		}
	}
	if len(wantUDP) != 0 {
		t.Fatalf("missing per-pod UDP listeners: %v", wantUDP)
	}

	for _, resource := range routes {
		rc := resource.(*route.RouteConfiguration)
		var originIP string
		switch rc.Name {
		case "default_deployments.apps.reviews.nodes.node-a_20001_TCP":
			originIP = "10.244.0.5"
		case "default_deployments.apps.reviews.nodes.node-a_20003_TCP":
			originIP = "10.244.0.6"
		default:
			continue
		}
		routeList := rc.VirtualHosts[0].Routes
		gotCluster := routeList[len(routeList)-1].GetRoute().GetCluster()
		wantCluster := originIP + "_9080"
		if gotCluster != wantCluster {
			t.Fatalf("default route cluster = %q, want explicit pod origin %q", gotCluster, wantCluster)
		}
		delete(wantTCP, originIP)
	}
	if len(wantTCP) != 0 {
		t.Fatalf("missing per-pod TCP routes: %v", wantTCP)
	}

	clusterNames := map[string]bool{}
	for _, resource := range clusters {
		name := resource.(*cluster.Cluster).Name
		if clusterNames[name] {
			t.Fatalf("duplicate cluster in xDS snapshot: %s", name)
		}
		clusterNames[name] = true
	}

	wantEndpoints := map[string]bool{"10.244.0.5_9080": true, "10.244.0.6_9080": true}
	endpointNames := map[string]bool{}
	for _, resource := range endpoints {
		cla := resource.(*endpoint.ClusterLoadAssignment)
		if endpointNames[cla.ClusterName] {
			t.Fatalf("duplicate endpoint in xDS snapshot: %s", cla.ClusterName)
		}
		endpointNames[cla.ClusterName] = true
		if !wantEndpoints[cla.ClusterName] {
			continue
		}
		address := cla.Endpoints[0].LbEndpoints[0].GetEndpoint().Address.GetSocketAddress()
		if address.Address+"_9080" != cla.ClusterName || address.GetPortValue() != 9080 {
			t.Fatalf("origin endpoint does not target the real pod: %s", cla.ClusterName)
		}
		delete(wantEndpoints, cla.ClusterName)
	}
	if len(wantEndpoints) != 0 {
		t.Fatalf("missing explicit pod origin endpoints: %v", wantEndpoints)
	}
}
