package xds

import (
	"testing"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/sirupsen/logrus"
)

func TestAgentListenerPort_UsedByInboundAndUDPOnly(t *testing.T) {
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
	var sawInbound, sawUDP, sawTCP bool
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
		if l.Name == "default_deployments.apps.reviews_9080_TCP" {
			sawTCP = true
			if port != 9080 {
				t.Fatalf("TCP original-port listener port = %d, want 9080", port)
			}
		}
		if l.Name == "default_deployments.apps.reviews_20001_TCP" {
			t.Fatal("TCP original-port listener must not share the inbound capture port")
		}
	}
	if !sawInbound || !sawUDP || !sawTCP {
		t.Fatalf("missing listener: inbound=%v udp=%v tcp=%v", sawInbound, sawUDP, sawTCP)
	}
}
