package xds

import (
	"testing"

	httpconnectionmanager "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
)

func TestConvertContainerPort(t *testing.T) {
	ports := ConvertContainerPort(corev1.ContainerPort{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP})
	if len(ports) != 1 || ports[0].Name != "http" || ports[0].ContainerPort != 8080 || ports[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("unexpected conversion: %+v", ports)
	}
}

func TestParseYaml(t *testing.T) {
	result, err := parseYaml(`- Namespace: default
  Uid: deployments.apps.reviews
  Rules:
  - ownerID: owner-a
`)
	if err != nil || len(result) != 1 || result[0].UID != "deployments.apps.reviews" || len(result[0].Rules) != 1 {
		t.Fatalf("valid parse failed: %+v, %v", result, err)
	}
	if result, err = parseYaml(""); err != nil || len(result) != 0 {
		t.Fatalf("empty parse failed: %+v, %v", result, err)
	}
	if _, err = parseYaml("not: [valid: yaml: {{"); err == nil {
		t.Fatal("expected invalid YAML error")
	}
}

func TestBuildFilterChains(t *testing.T) {
	const routeName = "default_services.reviews.nodes.node-a_20001_TCP"
	const fallbackCluster = "10.96.0.10_80"
	chains := buildFilterChains(routeName, fallbackCluster)
	if len(chains) != 2 {
		t.Fatalf("expected HTTP and TCP chains, got %d", len(chains))
	}
	var hcm httpconnectionmanager.HttpConnectionManager
	if err := chains[0].Filters[0].GetTypedConfig().UnmarshalTo(&hcm); err != nil {
		t.Fatal(err)
	}
	if hcm.GetRds().GetRouteConfigName() != routeName || len(hcm.GetHttpFilters()) != 3 {
		t.Fatalf("unexpected RDS or HTTP filter count: %s %d", hcm.GetRds().GetRouteConfigName(), len(hcm.GetHttpFilters()))
	}
	var tcp tcpproxy.TcpProxy
	if err := chains[1].Filters[0].GetTypedConfig().UnmarshalTo(&tcp); err != nil {
		t.Fatal(err)
	}
	if tcp.GetCluster() != fallbackCluster {
		t.Fatalf("TCP fallback cluster = %q", tcp.GetCluster())
	}
	if chains[0].Filters[0].GetName() != wellknown.HTTPConnectionManager {
		t.Fatalf("unexpected HTTP filter: %s", chains[0].Filters[0].GetName())
	}
}

func TestVirtual_To_FiltersEmptyOwnerID(t *testing.T) {
	newVirtual := func(schema int) *Virtual {
		return &Virtual{
			SchemaVersion: schema, Namespace: "default", UID: "deployments.apps.reviews.nodes.node-a",
			Ports:        []ContainerPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP}},
			AgentOrigins: []*AgentOrigin{{PodUID: "pod-a", PodIP: "10.244.0.5", Ports: []AgentOriginPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP, AgentListenerPort: 20001}}}},
			Rules: []*Rule{
				{OwnerID: "owner-a", LocalTunIPv4: "198.18.0.1", PortMap: map[int32]string{9080: "9080"}},
				{OwnerID: "", LocalTunIPv4: "198.18.0.2", PortMap: map[int32]string{9080: "9080"}},
			},
		}
	}
	_, clusters, _, _ := To(newVirtual(CurrentSchemaVersion), false, logrus.NewEntry(logrus.New()))
	if len(clusters) != 2 {
		t.Fatalf("schema v2 must filter empty OwnerID: got %d clusters", len(clusters))
	}
	_, clusters, _, _ = To(newVirtual(0), false, logrus.NewEntry(logrus.New()))
	if len(clusters) != 3 {
		t.Fatalf("legacy rule must be preserved: got %d clusters", len(clusters))
	}
}
