package controlplane

import (
	"testing"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	httpconnectionmanager "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
)

func TestVirtual_IsFargateMode_ExplicitField(t *testing.T) {
	v := &Virtual{FargateMode: true}
	if !v.IsFargateMode() {
		t.Fatal("expected fargate mode with explicit field")
	}
}

func TestVirtual_IsFargateMode_LegacyFallback(t *testing.T) {
	v := &Virtual{
		Ports: []ContainerPort{{EnvoyListenerPort: 12345, ContainerPort: 8080}},
	}
	if !v.IsFargateMode() {
		t.Fatal("expected fargate mode via legacy EnvoyListenerPort != 0")
	}
}

func TestVirtual_IsFargateMode_False(t *testing.T) {
	v := &Virtual{
		Ports: []ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}
	if v.IsFargateMode() {
		t.Fatal("expected non-fargate mode")
	}
}

func TestConvertContainerPort(t *testing.T) {
	ports := ConvertContainerPort(
		corev1.ContainerPort{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		corev1.ContainerPort{Name: "grpc", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
	)
	if len(ports) != 2 {
		t.Fatalf("expected 2 ports, got %d", len(ports))
	}
	for _, p := range ports {
		if p.EnvoyListenerPort != 0 {
			t.Fatalf("EnvoyListenerPort should be 0, got %d", p.EnvoyListenerPort)
		}
	}
	if ports[0].Name != "http" || ports[0].ContainerPort != 8080 {
		t.Fatalf("port 0 mismatch: %+v", ports[0])
	}
}

func TestVirtual_To_MeshMode(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.reviews",
		Ports: []ContainerPort{
			{Name: "http", ContainerPort: 9080, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{"env": "test"},
				LocalTunIPv4: "198.18.0.1",
				LocalTunIPv6: "2001:2::1",
				PortMap:      map[int32]string{9080: "9080"},
			},
		},
	}

	logger := log.NewEntry(log.New())
	listeners, clusters, routes, endpoints := v.To(false, logger)

	if len(listeners) == 0 {
		t.Fatal("expected at least 1 listener")
	}
	if len(clusters) == 0 {
		t.Fatal("expected at least 1 cluster")
	}
	if len(routes) == 0 {
		t.Fatal("expected at least 1 route")
	}
	if len(endpoints) == 0 {
		t.Fatal("expected at least 1 endpoint")
	}
	t.Logf("Mesh mode: %d listeners, %d clusters, %d routes, %d endpoints",
		len(listeners), len(clusters), len(routes), len(endpoints))
}

func TestVirtual_To_FargateMode(t *testing.T) {
	v := &Virtual{
		Namespace:   "default",
		UID:         "services.reviews",
		FargateMode: true,
		Ports: []ContainerPort{
			{Name: "http", ContainerPort: 9080, EnvoyListenerPort: 38721, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{"env": "test"},
				LocalTunIPv4: "127.0.0.1",
				LocalTunIPv6: "::1",
				PortMap:      map[int32]string{9080: "29450:19080"},
			},
		},
	}

	logger := log.NewEntry(log.New())
	listeners, clusters, routes, endpoints := v.To(false, logger)

	if len(listeners) == 0 {
		t.Fatal("expected at least 1 listener")
	}
	// Fargate should have default route clusters + rule clusters
	if len(clusters) < 2 {
		t.Fatalf("expected at least 2 clusters (rule + default), got %d", len(clusters))
	}
	if len(routes) == 0 {
		t.Fatal("expected at least 1 route config")
	}
	if len(endpoints) < 2 {
		t.Fatalf("expected at least 2 endpoints, got %d", len(endpoints))
	}
	t.Logf("Fargate mode: %d listeners, %d clusters, %d routes, %d endpoints",
		len(listeners), len(clusters), len(routes), len(endpoints))
}

func TestVirtual_To_IPv6(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.ratings",
		Ports: []ContainerPort{
			{ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{"user": "dev"},
				LocalTunIPv4: "198.18.0.5",
				LocalTunIPv6: "2001:2::5",
				PortMap:      map[int32]string{8080: "8080"},
			},
		},
	}

	logger := log.NewEntry(log.New())
	listeners, clusters, _, endpoints := v.To(true, logger)

	if len(listeners) != 1 {
		t.Fatalf("expected 1 listener, got %d", len(listeners))
	}
	// IPv6 mode: each rule generates 2 clusters (v4 + v6) + 1 origin cluster
	if len(clusters) < 3 {
		t.Fatalf("expected at least 3 clusters for IPv6 mode, got %d", len(clusters))
	}
	if len(endpoints) < 2 {
		t.Fatalf("expected at least 2 endpoints for IPv6, got %d", len(endpoints))
	}
}

func TestVirtual_To_MultipleRules(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.productpage",
		Ports: []ContainerPort{
			{ContainerPort: 9080, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{Headers: map[string]string{"env": "test"}, LocalTunIPv4: "198.18.0.1", PortMap: map[int32]string{9080: "9080"}},
			{Headers: map[string]string{"env": "dev"}, LocalTunIPv4: "198.18.0.2", PortMap: map[int32]string{9080: "9080"}},
			{Headers: map[string]string{}, LocalTunIPv4: "198.18.0.3", PortMap: map[int32]string{9080: "9080"}},
		},
	}

	logger := log.NewEntry(log.New())
	_, clusters, routes, _ := v.To(false, logger)

	// 3 rules + 1 origin = 4 clusters minimum
	if len(clusters) < 4 {
		t.Fatalf("expected at least 4 clusters for 3 rules, got %d", len(clusters))
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route config, got %d", len(routes))
	}
}

func TestVirtual_To_EmptyRules(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.empty",
		Ports: []ContainerPort{
			{ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{},
	}

	logger := log.NewEntry(log.New())
	listeners, clusters, routes, _ := v.To(false, logger)

	if len(listeners) != 1 {
		t.Fatalf("expected 1 listener even with empty rules, got %d", len(listeners))
	}
	// Should still have origin cluster for default route
	if len(clusters) != 1 {
		t.Fatalf("expected 1 origin cluster, got %d", len(clusters))
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route config, got %d", len(routes))
	}
}

func TestParseYaml_Valid(t *testing.T) {
	content := `
- namespace: default
  Uid: deployments.apps.reviews
  ports:
    - containerPort: 9080
      protocol: TCP
  rules:
    - headers:
        env: test
      localTunIPv4: "198.18.0.1"
      localTunIPv6: "2001:2::1"
`
	result, err := parseYaml(content)
	if err != nil {
		t.Fatalf("parseYaml: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(result))
	}
	if result[0].UID != "deployments.apps.reviews" {
		t.Fatalf("UID: want deployments.apps.reviews, got %s", result[0].UID)
	}
	if len(result[0].Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(result[0].Rules))
	}
}

func TestParseYaml_Empty(t *testing.T) {
	result, err := parseYaml("")
	if err != nil {
		t.Fatalf("parseYaml empty: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 virtuals, got %d", len(result))
	}
}

func TestParseYaml_Invalid(t *testing.T) {
	_, err := parseYaml("not: [valid: yaml: {{")
	if err == nil {
		t.Fatal("expected error for invalid yaml")
	}
}

func TestVirtual_To_UDPProtocol(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.dns",
		Ports: []ContainerPort{
			{ContainerPort: 53, Protocol: corev1.ProtocolUDP},
		},
		Rules: []*Rule{
			{Headers: map[string]string{"env": "test"}, LocalTunIPv4: "198.18.0.1", PortMap: map[int32]string{53: "53"}},
		},
	}
	logger := log.NewEntry(log.New())
	listeners, _, _, _ := v.To(false, logger)
	if len(listeners) != 1 {
		t.Fatalf("expected 1 UDP listener, got %d", len(listeners))
	}
}

func TestVirtual_To_MultiplePortsTCP(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.web",
		Ports: []ContainerPort{
			{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
			{Name: "grpc", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{"env": "dev"},
				LocalTunIPv4: "198.18.0.10",
				PortMap:      map[int32]string{8080: "8080", 9090: "9090"},
			},
		},
	}

	logger := log.NewEntry(log.New())
	listeners, clusters, routes, endpoints := v.To(false, logger)

	// Each port should produce one listener
	if len(listeners) != 2 {
		t.Fatalf("expected 2 listeners for 2 TCP ports, got %d", len(listeners))
	}

	// Verify both listeners are TCP on the correct ports
	ports := map[uint32]bool{}
	for _, res := range listeners {
		l := res.(*listener.Listener)
		addr := l.GetAddress().GetSocketAddress()
		if addr.GetProtocol() != core.SocketAddress_TCP {
			t.Fatalf("expected TCP protocol, got %v for listener %s", addr.GetProtocol(), l.GetName())
		}
		ports[addr.GetPortValue()] = true
	}
	if !ports[8080] {
		t.Fatal("missing listener on port 8080")
	}
	if !ports[9090] {
		t.Fatal("missing listener on port 9090")
	}

	// Each port: 1 rule cluster + 1 origin cluster = 2 clusters per port = 4 total
	if len(clusters) < 4 {
		t.Fatalf("expected at least 4 clusters for 2 ports with 1 rule, got %d", len(clusters))
	}

	// Should have 2 route configs (one per port)
	if len(routes) != 2 {
		t.Fatalf("expected 2 route configs, got %d", len(routes))
	}

	// Should have at least 2 endpoints (one per rule per port)
	if len(endpoints) < 2 {
		t.Fatalf("expected at least 2 endpoints, got %d", len(endpoints))
	}
}

func TestVirtual_To_MixedProtocols(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.mixed",
		Ports: []ContainerPort{
			{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
			{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{"env": "staging"},
				LocalTunIPv4: "198.18.0.20",
				PortMap:      map[int32]string{8080: "8080", 53: "53"},
			},
		},
	}

	logger := log.NewEntry(log.New())
	listeners, _, _, _ := v.To(false, logger)

	if len(listeners) != 2 {
		t.Fatalf("expected 2 listeners (1 TCP + 1 UDP), got %d", len(listeners))
	}

	var tcpCount, udpCount int
	for _, res := range listeners {
		l := res.(*listener.Listener)
		addr := l.GetAddress().GetSocketAddress()
		switch addr.GetProtocol() {
		case core.SocketAddress_TCP:
			tcpCount++
			if addr.GetPortValue() != 8080 {
				t.Fatalf("TCP listener expected on port 8080, got %d", addr.GetPortValue())
			}
		case core.SocketAddress_UDP:
			udpCount++
			if addr.GetPortValue() != 53 {
				t.Fatalf("UDP listener expected on port 53, got %d", addr.GetPortValue())
			}
		default:
			t.Fatalf("unexpected protocol: %v", addr.GetProtocol())
		}
	}

	if tcpCount != 1 {
		t.Fatalf("expected 1 TCP listener, got %d", tcpCount)
	}
	if udpCount != 1 {
		t.Fatalf("expected 1 UDP listener, got %d", udpCount)
	}
}

func TestVirtual_To_FargateDefaultRoute(t *testing.T) {
	v := &Virtual{
		Namespace:   "default",
		UID:         "services.myapp",
		FargateMode: true,
		Ports: []ContainerPort{
			{Name: "http", ContainerPort: 8080, EnvoyListenerPort: 38000, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{"env": "test"},
				LocalTunIPv4: "127.0.0.1",
				PortMap:      map[int32]string{8080: "29000:19080"},
			},
		},
	}

	logger := log.NewEntry(log.New())
	_, _, _, endpoints := v.To(false, logger)

	// In fargate mode with 1 rule (IPv4 only):
	// - 1 endpoint for the rule (127.0.0.1:29000)
	// - 1 default route endpoint (127.0.0.1:8080 = containerPort)
	if len(endpoints) < 2 {
		t.Fatalf("expected at least 2 endpoints in fargate mode, got %d", len(endpoints))
	}

	// Verify default route endpoint points to 127.0.0.1:containerPort
	foundDefaultRoute := false
	for _, res := range endpoints {
		ep := res.(*endpoint.ClusterLoadAssignment)
		for _, locality := range ep.GetEndpoints() {
			for _, lbEndpoint := range locality.GetLbEndpoints() {
				addr := lbEndpoint.GetEndpoint().GetAddress().GetSocketAddress()
				if addr.GetAddress() == "127.0.0.1" && addr.GetPortValue() == 8080 {
					foundDefaultRoute = true
				}
			}
		}
	}
	if !foundDefaultRoute {
		t.Fatal("expected default route endpoint pointing to 127.0.0.1:8080 (containerPort)")
	}
}

func TestBuildFilterChains_NonEmptyRouteName(t *testing.T) {
	routeName := "default_deployments.apps.reviews_9080_TCP"
	chains := buildFilterChains(routeName)

	if len(chains) != 2 {
		t.Fatalf("expected 2 filter chains (HTTP + TCP fallback), got %d", len(chains))
	}

	httpChain := chains[0]
	if httpChain.FilterChainMatch == nil {
		t.Fatal("expected FilterChainMatch on HTTP filter chain")
	}
	protos := httpChain.FilterChainMatch.GetApplicationProtocols()
	expectedProtos := []string{"http/1.0", "http/1.1", "h2c"}
	if len(protos) != len(expectedProtos) {
		t.Fatalf("expected %d application protocols, got %d", len(expectedProtos), len(protos))
	}
	for i, p := range protos {
		if p != expectedProtos[i] {
			t.Fatalf("protocol[%d]: want %q, got %q", i, expectedProtos[i], p)
		}
	}

	if len(httpChain.Filters) != 1 {
		t.Fatalf("expected 1 filter in HTTP chain, got %d", len(httpChain.Filters))
	}
	if httpChain.Filters[0].GetName() != wellknown.HTTPConnectionManager {
		t.Fatalf("expected filter name %q, got %q", wellknown.HTTPConnectionManager, httpChain.Filters[0].GetName())
	}

	var hcm httpconnectionmanager.HttpConnectionManager
	if err := httpChain.Filters[0].GetTypedConfig().UnmarshalTo(&hcm); err != nil {
		t.Fatalf("failed to unmarshal HttpConnectionManager: %v", err)
	}
	rds := hcm.GetRds()
	if rds == nil {
		t.Fatal("expected RDS route specifier")
	}
	if rds.GetRouteConfigName() != routeName {
		t.Fatalf("RDS route config name: want %q, got %q", routeName, rds.GetRouteConfigName())
	}

	httpFilters := hcm.GetHttpFilters()
	if len(httpFilters) != 3 {
		t.Fatalf("expected 3 HTTP filters, got %d", len(httpFilters))
	}
	expectedFilterNames := []string{wellknown.GRPCWeb, wellknown.CORS, wellknown.Router}
	for i, f := range httpFilters {
		if f.GetName() != expectedFilterNames[i] {
			t.Fatalf("HTTP filter[%d]: want %q, got %q", i, expectedFilterNames[i], f.GetName())
		}
	}

	tcpChain := chains[1]
	if tcpChain.FilterChainMatch != nil {
		t.Fatal("expected no FilterChainMatch on TCP fallback chain")
	}
	var tcp tcpproxy.TcpProxy
	if err := tcpChain.Filters[0].GetTypedConfig().UnmarshalTo(&tcp); err != nil {
		t.Fatalf("failed to unmarshal TcpProxy: %v", err)
	}
	if tcp.GetCluster() != "origin_cluster" {
		t.Fatalf("TCP proxy cluster: want %q, got %q", "origin_cluster", tcp.GetCluster())
	}
}

func TestBuildFilterChains_EmptyRouteName(t *testing.T) {
	chains := buildFilterChains("")
	if len(chains) != 2 {
		t.Fatalf("expected 2 filter chains even with empty route name, got %d", len(chains))
	}

	var hcm httpconnectionmanager.HttpConnectionManager
	if err := chains[0].Filters[0].GetTypedConfig().UnmarshalTo(&hcm); err != nil {
		t.Fatalf("failed to unmarshal HttpConnectionManager: %v", err)
	}
	if rdsName := hcm.GetRds().GetRouteConfigName(); rdsName != "" {
		t.Fatalf("expected empty RDS route config name, got %q", rdsName)
	}
}

func TestBuildFilterChains_HTTPManagerConfig(t *testing.T) {
	chains := buildFilterChains("test-route")

	var hcm httpconnectionmanager.HttpConnectionManager
	if err := chains[0].Filters[0].GetTypedConfig().UnmarshalTo(&hcm); err != nil {
		t.Fatalf("failed to unmarshal HttpConnectionManager: %v", err)
	}

	if hcm.GetCodecType() != httpconnectionmanager.HttpConnectionManager_AUTO {
		t.Fatalf("expected AUTO codec type, got %v", hcm.GetCodecType())
	}
	if hcm.GetStatPrefix() != "http" {
		t.Fatalf("expected stat prefix %q, got %q", "http", hcm.GetStatPrefix())
	}

	upgrades := hcm.GetUpgradeConfigs()
	if len(upgrades) != 1 || upgrades[0].GetUpgradeType() != "websocket" {
		t.Fatalf("expected 1 websocket upgrade config, got %v", upgrades)
	}

	accessLogs := hcm.GetAccessLog()
	if len(accessLogs) != 1 {
		t.Fatalf("expected 1 access log, got %d", len(accessLogs))
	}
	if accessLogs[0].GetName() != wellknown.FileAccessLog {
		t.Fatalf("expected access log name %q, got %q", wellknown.FileAccessLog, accessLogs[0].GetName())
	}

	if hcm.GetStreamIdleTimeout().GetSeconds() != 0 {
		t.Fatalf("expected stream idle timeout 0, got %v", hcm.GetStreamIdleTimeout())
	}
}
