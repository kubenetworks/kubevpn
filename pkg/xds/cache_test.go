package xds

import (
	"strings"
	"testing"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	httpconnectionmanager "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// nonCaptureListeners returns the per-port/UDP listeners, excluding the mesh-mode
// virtual-inbound capture listener bound on config.PortEnvoyInbound (:15006) that
// Virtual.To adds for non-fargate Virtuals.
func nonCaptureListeners(listeners []types.Resource) []*listener.Listener {
	var out []*listener.Listener
	for _, res := range listeners {
		l := res.(*listener.Listener)
		if l.GetAddress().GetSocketAddress().GetPortValue() == uint32(config.PortEnvoyInbound) {
			continue
		}
		out = append(out, l)
	}
	return out
}

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
	listeners, clusters, routes, endpoints := To(v, false, logger)

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

// TestVirtual_To_RDSUsesADS guards the xDS resilience fix: route config must be
// delivered over the aggregated (ADS) stream, not a separate RDS gRPC stream, so a
// traffic-manager restart reconnects all resource types atomically instead of
// leaving the listener up with empty routes.
func TestVirtual_To_RDSUsesADS(t *testing.T) {
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
				PortMap:      map[int32]string{9080: "9080"},
			},
		},
	}

	logger := log.NewEntry(log.New())
	listeners, _, _, _ := To(v, false, logger)

	checked := 0
	for _, l := range nonCaptureListeners(listeners) {
		for _, fc := range l.GetFilterChains() {
			for _, f := range fc.GetFilters() {
				if f.GetName() != wellknown.HTTPConnectionManager {
					continue
				}
				hcm := &httpconnectionmanager.HttpConnectionManager{}
				if err := f.GetTypedConfig().UnmarshalTo(hcm); err != nil {
					t.Fatalf("unmarshal HttpConnectionManager: %v", err)
				}
				rds, ok := hcm.GetRouteSpecifier().(*httpconnectionmanager.HttpConnectionManager_Rds)
				if !ok {
					t.Fatalf("expected Rds route specifier, got %T", hcm.GetRouteSpecifier())
				}
				if _, ok := rds.Rds.GetConfigSource().GetConfigSourceSpecifier().(*core.ConfigSource_Ads); !ok {
					t.Fatalf("expected RDS ConfigSource to use ADS, got %T", rds.Rds.GetConfigSource().GetConfigSourceSpecifier())
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no HttpConnectionManager filter found to verify RDS uses ADS")
	}
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
	listeners, clusters, routes, endpoints := To(v, false, logger)

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
	listeners, clusters, _, endpoints := To(v, true, logger)

	if got := len(nonCaptureListeners(listeners)); got != 1 {
		t.Fatalf("expected 1 listener, got %d", got)
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
	_, clusters, routes, _ := To(v, false, logger)

	// 3 rules + 1 origin = 4 clusters minimum
	if len(clusters) < 4 {
		t.Fatalf("expected at least 4 clusters for 3 rules, got %d", len(clusters))
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route config, got %d", len(routes))
	}
}

// emptyEndpointAddrs returns the names of any generated endpoints whose socket address is
// empty — an upstream envoy can never connect to, making it fail every routed request with
// 503 "connection refused".
func emptyEndpointAddrs(endpoints []types.Resource) []string {
	var bad []string
	for _, res := range endpoints {
		cla := res.(*endpoint.ClusterLoadAssignment)
		for _, lbe := range cla.GetEndpoints() {
			for _, e := range lbe.GetLbEndpoints() {
				if e.GetEndpoint().GetAddress().GetSocketAddress().GetAddress() == "" {
					bad = append(bad, cla.GetClusterName())
				}
			}
		}
	}
	return bad
}

// TestVirtual_To_SkipsEmptyTunIP_TCP guards against routing TCP traffic to an empty upstream
// address when a rule's TUN IP is not yet allocated/propagated. A full-proxy (empty-headers)
// rule with an empty IP would otherwise produce a match-all route to a broken cluster, so
// envoy fails ALL requests with 503. TCP must mirror addUDPPort's empty-IP guard.
func TestVirtual_To_SkipsEmptyTunIP_TCP(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.authors",
		Ports: []ContainerPort{
			{ContainerPort: 9080, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			// Full-proxy rule whose TUN IP has not propagated yet.
			{Headers: map[string]string{}, LocalTunIPv4: "", PortMap: map[int32]string{9080: "9080"}},
		},
	}

	logger := log.NewEntry(log.New())
	_, clusters, _, endpoints := To(v, false, logger)

	if bad := emptyEndpointAddrs(endpoints); len(bad) != 0 {
		t.Fatalf("empty-address endpoints generated for empty TUN IP: %v", bad)
	}
	// No per-rule TUN cluster is created for the empty IP. The only clusters left are the
	// shared origin_cluster (capture passthrough) and the per-port loopback_<port> cluster
	// (the mesh default route target) — neither routes to a broken empty address.
	for _, res := range clusters {
		name := res.(*cluster.Cluster).GetName()
		if name != originClusterName && !strings.HasPrefix(name, "loopback_") {
			t.Fatalf("unexpected cluster %q for empty TUN IP (want only origin_cluster / loopback_*)", name)
		}
	}
}

// TestVirtual_To_SkipsEmptyTunIP_IPv6Missing guards the v4-only rule in an IPv6-enabled
// snapshot: rule.tunIPs returns [v4, ""], and the empty IPv6 entry must not yield a broken
// v6 endpoint.
func TestVirtual_To_SkipsEmptyTunIP_IPv6Missing(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.authors",
		Ports: []ContainerPort{
			{ContainerPort: 9080, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{Headers: map[string]string{"env": "test"}, LocalTunIPv4: "198.18.0.1", LocalTunIPv6: "", PortMap: map[int32]string{9080: "9080"}},
		},
	}

	logger := log.NewEntry(log.New())
	_, _, _, endpoints := To(v, true, logger)

	if bad := emptyEndpointAddrs(endpoints); len(bad) != 0 {
		t.Fatalf("empty-address endpoints generated for missing IPv6: %v", bad)
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
	listeners, clusters, routes, _ := To(v, false, logger)

	if got := len(nonCaptureListeners(listeners)); got != 1 {
		t.Fatalf("expected 1 listener even with empty rules, got %d", got)
	}
	// Even with no rules: origin_cluster (capture passthrough) + the per-port
	// loopback_8080 cluster (the mesh default route target for declared port 8080).
	names := map[string]bool{}
	for _, res := range clusters {
		names[res.(*cluster.Cluster).GetName()] = true
	}
	if !names[originClusterName] {
		t.Fatalf("expected origin_cluster present, got %v", names)
	}
	if !names[loopbackClusterName(8080)] {
		t.Fatalf("expected %s present, got %v", loopbackClusterName(8080), names)
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
	listeners, _, _, _ := To(v, false, logger)
	if got := len(nonCaptureListeners(listeners)); got != 1 {
		t.Fatalf("expected 1 UDP listener, got %d", got)
	}
}

// TestVirtual_To_UDPDualStack verifies that with IPv6 enabled, a UDP port emits
// two listeners with DISTINCT names (one per IP family) bound to the matching
// family address. A shared name would collapse them to one in the xDS snapshot,
// leaving the surviving listener bound to the wrong family — so the UDP listener
// would never serve the requested family (observed as the pod refusing UDP).
func TestVirtual_To_UDPDualStack(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.dns",
		Ports: []ContainerPort{
			{ContainerPort: 53, Protocol: corev1.ProtocolUDP},
		},
		Rules: []*Rule{
			{LocalTunIPv4: "198.18.0.1", LocalTunIPv6: "2001:2::1", PortMap: map[int32]string{53: "53"}},
		},
	}
	logger := log.NewEntry(log.New())
	listeners, _, _, _ := To(v, true, logger)
	udpListeners := nonCaptureListeners(listeners)
	if len(udpListeners) != 2 {
		t.Fatalf("expected 2 UDP listeners (v4 + v6), got %d", len(udpListeners))
	}
	names := map[string]string{} // name -> bind address
	for _, l := range udpListeners {
		addr := l.GetAddress().GetSocketAddress()
		if addr.GetProtocol() != core.SocketAddress_UDP {
			t.Fatalf("expected UDP protocol, got %v for listener %s", addr.GetProtocol(), l.GetName())
		}
		if prev, dup := names[l.GetName()]; dup {
			t.Fatalf("duplicate UDP listener name %q (would collapse in snapshot); existing bind %s", l.GetName(), prev)
		}
		names[l.GetName()] = addr.GetAddress()
	}
	var sawV4, sawV6 bool
	for _, bind := range names {
		switch bind {
		case "0.0.0.0":
			sawV4 = true
		case "::":
			sawV6 = true
		}
	}
	if !sawV4 || !sawV6 {
		t.Fatalf("expected one 0.0.0.0 and one :: UDP listener, got binds %v", names)
	}
}

// TestVirtual_To_UDPSkipsEmptyIPv6 verifies that an empty LocalTunIPv6 (IPv6
// enabled but the rule's v6 TUN IP unpopulated) does NOT emit a second listener
// with an invalid empty-address endpoint — only the valid v4 listener is emitted.
func TestVirtual_To_UDPSkipsEmptyIPv6(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.dns",
		Ports: []ContainerPort{
			{ContainerPort: 53, Protocol: corev1.ProtocolUDP},
		},
		Rules: []*Rule{
			{LocalTunIPv4: "198.18.0.1", LocalTunIPv6: "", PortMap: map[int32]string{53: "53"}},
		},
	}
	logger := log.NewEntry(log.New())
	listeners, _, _, endpoints := To(v, true, logger)
	if got := len(nonCaptureListeners(listeners)); got != 1 {
		t.Fatalf("expected 1 UDP listener (empty v6 skipped), got %d", got)
	}
	for _, res := range endpoints {
		cla := res.(*endpoint.ClusterLoadAssignment)
		for _, ep := range cla.GetEndpoints() {
			for _, lb := range ep.GetLbEndpoints() {
				addr := lb.GetEndpoint().GetAddress().GetSocketAddress().GetAddress()
				if addr == "" {
					t.Fatalf("endpoint %s has empty address (invalid)", cla.GetClusterName())
				}
			}
		}
	}
}

// TestVirtual_To_MeshInboundCaptureListener verifies mesh mode emits the virtual-inbound
// capture listener bound on config.PortEnvoyInbound (:15006) — the entry point the sidecar
// iptables DNATs TCP to. Without it, DNAT'd connections hit a closed port and the kernel
// resets them (observed as "connection reset by peer"). Fargate binds listeners directly
// and must NOT emit it.
func TestVirtual_To_MeshInboundCaptureListener(t *testing.T) {
	mkVirtual := func(fargate bool) *Virtual {
		v := &Virtual{
			Namespace: "default",
			UID:       "deployments.apps.reviews",
			Ports:     []ContainerPort{{ContainerPort: 9080, Protocol: corev1.ProtocolTCP}},
			Rules:     []*Rule{{Headers: map[string]string{"env": "test"}, LocalTunIPv4: "198.18.0.1", PortMap: map[int32]string{9080: "9080"}}},
		}
		if fargate {
			v.FargateMode = true
			v.Ports[0].EnvoyListenerPort = 15080
		}
		return v
	}

	logger := log.NewEntry(log.New())

	// Mesh mode: capture listener present and correctly shaped.
	listeners, clusters, _, _ := To(mkVirtual(false), false, logger)
	var capture *listener.Listener
	for _, res := range listeners {
		l := res.(*listener.Listener)
		if l.GetAddress().GetSocketAddress().GetPortValue() == uint32(config.PortEnvoyInbound) {
			capture = l
		}
	}
	if capture == nil {
		t.Fatal("mesh mode must emit the :15006 inbound capture listener")
	}
	if capture.GetAddress().GetSocketAddress().GetProtocol() != core.SocketAddress_TCP {
		t.Error("capture listener must be TCP")
	}
	if capture.GetBindToPort() == nil || !capture.GetBindToPort().GetValue() {
		t.Error("capture listener must bind to port")
	}
	if !capture.GetUseOriginalDst().GetValue() {
		t.Error("capture listener must set use_original_dst")
	}
	var hasOrigDstFilter bool
	for _, lf := range capture.GetListenerFilters() {
		if lf.GetName() == wellknown.OriginalDestination {
			hasOrigDstFilter = true
		}
	}
	if !hasOrigDstFilter {
		t.Error("capture listener must have the original_dst listener filter")
	}
	// The passthrough filter chain routes to origin_cluster, which must exist.
	var hasOrigin bool
	for _, res := range clusters {
		if c, ok := res.(*cluster.Cluster); ok && c.GetName() == "origin_cluster" {
			hasOrigin = true
		}
	}
	if !hasOrigin {
		t.Error("origin_cluster must exist for the capture passthrough")
	}

	// Fargate mode: NO capture listener (listeners bind directly).
	flisteners, _, _, _ := To(mkVirtual(true), false, logger)
	for _, res := range flisteners {
		l := res.(*listener.Listener)
		if l.GetAddress().GetSocketAddress().GetPortValue() == uint32(config.PortEnvoyInbound) {
			t.Error("fargate mode must NOT emit the :15006 capture listener")
		}
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
	listeners, clusters, routes, endpoints := To(v, false, logger)

	// Each port should produce one listener
	portListeners := nonCaptureListeners(listeners)
	if len(portListeners) != 2 {
		t.Fatalf("expected 2 listeners for 2 TCP ports, got %d", len(portListeners))
	}

	// Verify both listeners are TCP on the correct ports
	ports := map[uint32]bool{}
	for _, l := range portListeners {
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

	// 1 rule cluster per port (2) + 1 loopback cluster per port (2) + 1 shared
	// origin_cluster (created once per Virtual) = 5
	if len(clusters) < 3 {
		t.Fatalf("expected at least 3 clusters for 2 ports with 1 rule, got %d", len(clusters))
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
	listeners, _, _, _ := To(v, false, logger)

	portListeners := nonCaptureListeners(listeners)
	if len(portListeners) != 2 {
		t.Fatalf("expected 2 listeners (1 TCP + 1 UDP), got %d", len(portListeners))
	}

	var tcpCount, udpCount int
	for _, l := range portListeners {
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
	_, _, _, endpoints := To(v, false, logger)

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

// TestVirtual_To_MeshDefaultRouteLoopback locks in the loopback origin回源 for mesh
// declared ports: the no-header-match default route and the per-port loopback cluster must
// target the real app over 127.0.0.1:<containerPort> (not origin_cluster/ORIGINAL_DST), so
// the return path goes through lo and never re-enters the sidecar PREROUTING DNAT. The
// shared origin_cluster must still exist for the capture listener's undeclared-port passthrough.
func TestVirtual_To_MeshDefaultRouteLoopback(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.reviews",
		Ports: []ContainerPort{
			{ContainerPort: 9080, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{Headers: map[string]string{"env": "test"}, LocalTunIPv4: "198.18.0.1", PortMap: map[int32]string{9080: "9080"}},
		},
	}

	logger := log.NewEntry(log.New())
	_, clusters, routes, _ := To(v, false, logger)

	// loopback_9080 must exist, be STATIC, and point at 127.0.0.1:9080.
	var lb *cluster.Cluster
	var hasOrigin bool
	for _, res := range clusters {
		c := res.(*cluster.Cluster)
		switch c.GetName() {
		case loopbackClusterName(9080):
			lb = c
		case originClusterName:
			hasOrigin = true
		}
	}
	if lb == nil {
		t.Fatal("expected loopback_9080 cluster for mesh declared port")
	}
	if lb.GetType() != cluster.Cluster_STATIC {
		t.Fatalf("loopback cluster must be STATIC, got %v", lb.GetType())
	}
	ep := lb.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint()
	addr := ep.GetAddress().GetSocketAddress()
	if addr.GetAddress() != "127.0.0.1" || addr.GetPortValue() != 9080 {
		t.Fatalf("loopback endpoint: want 127.0.0.1:9080, got %s:%d", addr.GetAddress(), addr.GetPortValue())
	}
	// IPv6 disabled: no ::1 happy-eyeballs additional address.
	if n := len(ep.GetAdditionalAddresses()); n != 0 {
		t.Fatalf("expected no additional addresses with IPv6 disabled, got %d", n)
	}
	if !hasOrigin {
		t.Error("origin_cluster must still exist for the capture passthrough (undeclared ports)")
	}

	// The default route (no header match) must target loopback_9080.
	rc := routes[0].(*route.RouteConfiguration)
	var defaultCluster string
	for _, r := range rc.GetVirtualHosts()[0].GetRoutes() {
		if len(r.GetMatch().GetHeaders()) == 0 {
			defaultCluster = r.GetRoute().GetCluster()
		}
	}
	if defaultCluster != loopbackClusterName(9080) {
		t.Fatalf("mesh default route: want %s, got %q", loopbackClusterName(9080), defaultCluster)
	}
}

// TestVirtual_To_MeshLoopbackDualStack verifies that with IPv6 enabled the loopback cluster
// carries both 127.0.0.1 (primary) and ::1 (additional address) so envoy's happy-eyeballs
// reaches the app whether it listens on v4, v6, or dual-stack.
func TestVirtual_To_MeshLoopbackDualStack(t *testing.T) {
	v := &Virtual{
		Namespace: "default",
		UID:       "deployments.apps.reviews",
		Ports: []ContainerPort{
			{ContainerPort: 9080, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{Headers: map[string]string{"env": "test"}, LocalTunIPv4: "198.18.0.1", LocalTunIPv6: "2001:2::1", PortMap: map[int32]string{9080: "9080"}},
		},
	}

	logger := log.NewEntry(log.New())
	_, clusters, _, _ := To(v, true, logger)

	var lb *cluster.Cluster
	for _, res := range clusters {
		if c := res.(*cluster.Cluster); c.GetName() == loopbackClusterName(9080) {
			lb = c
		}
	}
	if lb == nil {
		t.Fatal("expected loopback_9080 cluster")
	}
	ep := lb.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint()
	if a := ep.GetAddress().GetSocketAddress(); a.GetAddress() != "127.0.0.1" || a.GetPortValue() != 9080 {
		t.Fatalf("primary loopback addr: want 127.0.0.1:9080, got %s:%d", a.GetAddress(), a.GetPortValue())
	}
	add := ep.GetAdditionalAddresses()
	if len(add) != 1 {
		t.Fatalf("expected 1 additional (::1) address with IPv6 enabled, got %d", len(add))
	}
	if a := add[0].GetAddress().GetSocketAddress(); a.GetAddress() != "::1" || a.GetPortValue() != 9080 {
		t.Fatalf("additional loopback addr: want [::1]:9080, got %s:%d", a.GetAddress(), a.GetPortValue())
	}
}

func TestBuildFilterChains_NonEmptyRouteName(t *testing.T) {
	routeName := "default_deployments.apps.reviews_9080_TCP"
	chains := buildFilterChains(routeName, loopbackClusterName(9080))

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
	if want := loopbackClusterName(9080); tcp.GetCluster() != want {
		t.Fatalf("TCP proxy cluster: want %q, got %q", want, tcp.GetCluster())
	}
}

func TestBuildFilterChains_EmptyRouteName(t *testing.T) {
	chains := buildFilterChains("", originClusterName)
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
	chains := buildFilterChains("test-route", originClusterName)

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
