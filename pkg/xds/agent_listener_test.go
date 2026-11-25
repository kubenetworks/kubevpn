package xds

import (
	"testing"

	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/sirupsen/logrus"
)

func TestAgentListenerPort_UsedByInboundAndUDP(t *testing.T) {
	virtual := &Virtual{
		Namespace:         "default",
		UID:               "deployments.apps.reviews",
		Selector:          "app=reviews",
		AgentListenerPort: 20001,
		Ports: []ContainerPort{
			{ContainerPort: 9080, Protocol: "TCP"},
			{ContainerPort: 9081, Protocol: "UDP"},
		},
		Rules: []*Rule{{
			OwnerID:      "owner-1",
			LocalTunIPv4: "198.18.0.1",
			LocalTunIPv6: "2001:2::1",
			IsAgent:      true,
		}},
	}
	listeners, _, _, _ := To(virtual, true, logrus.NewEntry(logrus.New()))
	var sawInbound, sawUDP bool
	for _, resource := range listeners {
		l := resource.(*listener.Listener)
		port := l.Address.GetSocketAddress().GetPortSpecifier().(*core.SocketAddress_PortValue).PortValue
		if l.Name == "default_deployments.apps.reviews_inbound" {
			sawInbound = true
			if port != 20001 {
				t.Fatalf("inbound listener port = %d, want 20001", port)
			}
		}
		if l.Name == "default_deployments.apps.reviews_20001_UDP" {
			sawUDP = true
			if port != 20001 {
				t.Fatalf("UDP listener port = %d, want 20001", port)
			}
		}
	}
	if !sawInbound || !sawUDP {
		t.Fatalf("missing inbound or UDP listener: inbound=%v udp=%v", sawInbound, sawUDP)
	}
}
