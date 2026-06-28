package controlplane

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	xdscore "github.com/cncf/xds/go/xds/core/v3"
	xdsmatcher "github.com/cncf/xds/go/xds/type/matcher/v3"
	v31 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	accesslogfilev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/file/v3"
	corsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/cors/v3"
	grpcwebv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/grpc_web/v3"
	routerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	httpinspector "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/http_inspector/v3"
	dstv3inspector "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/original_dst/v3"
	httpconnectionmanager "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	udpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	preservecasev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/http/header_formatters/preserve_case/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	matcher "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// CurrentSchemaVersion is the latest schema version for Virtual configs stored in ConfigMaps.
// Bump this when making breaking changes to the Virtual/Rule struct layout.
// A zero value means a legacy config created before versioning was introduced.
// Version 2: OwnerID is required on all rules (no backward compat with empty OwnerID).
const CurrentSchemaVersion = 2

// originClusterName is the name of the shared ORIGINAL_DST cluster that forwards a
// connection to its original destination (the local app). It backs both the mesh TCP
// default route (no header match) and the inbound capture listener's passthrough.
const originClusterName = "origin_cluster"

const (
	// envoyClusterConnectTimeout is the upstream connect timeout for generated Envoy clusters.
	envoyClusterConnectTimeout = 5 * time.Second
	// envoyClusterIdleTimeout is the upstream HTTP idle timeout for generated Envoy clusters.
	envoyClusterIdleTimeout = 10 * time.Second
	// envoyUDPProxyIdleTimeout is the idle timeout for the Envoy UDP proxy listener.
	envoyUDPProxyIdleTimeout = 5 * time.Minute
)

// Virtual represents an envoy xDS configuration for a single proxied workload.
type Virtual struct {
	// SchemaVersion tracks the config schema revision. Zero means legacy (pre-versioning).
	SchemaVersion int `yaml:"schemaVersion,omitempty" json:"schemaVersion,omitempty"`
	Namespace     string
	UID           string `yaml:"Uid" json:"Uid"` // group.resource.name
	FargateMode   bool   `yaml:"fargateMode,omitempty" json:"fargateMode,omitempty"`
	Ports         []ContainerPort
	Rules         []*Rule
}

// ContainerPort describes a port on a container with optional envoy listener binding.
type ContainerPort struct {
	// If specified, this must be an IANA_SVC_NAME and unique within the pod. Each
	// named port in a pod must have a unique name. Name for the port that can be
	// referred to by services.
	// +optional
	Name string `json:"name,omitempty"`
	// EnvoyListenerPort is the port envoy binds to in fargate mode (BindToPort=true).
	// In mesh mode this is 0 (envoy uses iptables-redirected traffic instead).
	// +optional
	EnvoyListenerPort int32 `json:"envoyListenerPort,omitempty"`
	// Number of port to expose on the pod's IP address.
	// This must be a valid port number, 0 < x < 65536.
	ContainerPort int32 `json:"containerPort"`
	// Protocol for port. Must be UDP, TCP, or SCTP.
	// Defaults to "TCP".
	// +optional
	// +default="TCP"
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

// IsFargateMode returns true if this Virtual uses Fargate/Service mode
// (no iptables, SSH tunnels instead of VPN sidecar).
// Checks the explicit FargateMode field first, with a fallback to the legacy
// heuristic (EnvoyListenerPort != 0) for backward compatibility with existing ConfigMaps.
func (a *Virtual) IsFargateMode() bool {
	if a.FargateMode {
		return true
	}
	for _, port := range a.Ports {
		if port.EnvoyListenerPort != 0 {
			return true
		}
	}
	return false
}

// ConvertContainerPort converts Kubernetes ContainerPort values to controlplane ContainerPort with EnvoyListenerPort=0.
func ConvertContainerPort(ports ...corev1.ContainerPort) []ContainerPort {
	var result []ContainerPort
	for _, port := range ports {
		result = append(result, ContainerPort{
			Name:              port.Name,
			EnvoyListenerPort: 0,
			ContainerPort:     port.ContainerPort,
			Protocol:          port.Protocol,
		})
	}
	return result
}

// mustMarshalAny marshals a protobuf message into an Any.
// All call sites pass statically-typed envoy config messages that are always valid.
func mustMarshalAny(m proto.Message) *anypb.Any {
	pbst, _ := anypb.New(m)
	return pbst
}

// createPreserveCaseConfig creates a TypedExtensionConfig for preserving header case
func createPreserveCaseConfig() *core.TypedExtensionConfig {
	return &core.TypedExtensionConfig{
		Name:        "preserve_case",
		TypedConfig: mustMarshalAny(&preservecasev3.PreserveCaseFormatterConfig{}),
	}
}

// Rule defines a header-based routing rule for envoy traffic splitting.
type Rule struct {
	Headers      map[string]string
	LocalTunIPv4 string
	LocalTunIPv6 string
	// OwnerID identifies the connection that owns this rule (a UUID prefix).
	// Required — used as the primary key for rule matching in addVirtualRule and removeEnvoyConfig.
	OwnerID string `yaml:"ownerID" json:"ownerID"`
	// for no privileged mode (AWS Fargate mode), don't have cap NET_ADMIN and privileged: true. so we cannot use OSI layer 3 proxy
	// containerPort -> envoyRulePort:localPort
	// envoyRulePort for envoy forward to localhost:envoyRulePort
	// localPort for local pc listen localhost:localPort
	// use ssh reverse tunnel, envoy rule endpoint localhost:envoyRulePort will forward to local pc localhost:localPort
	// localPort is required and envoyRulePort is optional
	PortMap map[int32]string
}

// PortMapping represents a parsed port mapping from the PortMap string encoding.
type PortMapping struct {
	// ContainerPort is the original container port (the map key in PortMap).
	ContainerPort int32
	// EnvoyPort is the port envoy forwards to (the first number in the string value).
	EnvoyPort int32
	// LocalPort is the port the local PC listens on.
	// In non-fargate mode (plain "envoyPort" format), LocalPort equals ContainerPort.
	// In fargate mode ("envoyPort:localPort" format), LocalPort is the second number.
	LocalPort int32
}

// ParsePortMap parses the string-encoded PortMap into typed PortMapping values.
// The string value is either "envoyPort" (plain number) or "envoyPort:localPort" (colon-separated pair).
func (r *Rule) ParsePortMap() []PortMapping {
	result := make([]PortMapping, 0, len(r.PortMap))
	for containerPort, portStr := range r.PortMap {
		pm := PortMapping{ContainerPort: containerPort}
		if before, after, ok := strings.Cut(portStr, ":"); ok {
			pm.EnvoyPort, _ = parsePort(before)
			pm.LocalPort, _ = parsePort(after)
		} else {
			pm.EnvoyPort, _ = parsePort(portStr)
			pm.LocalPort = containerPort
		}
		result = append(result, pm)
	}
	return result
}

// tunIPs returns the rule's local TUN IPs: IPv4 only, or IPv4 + IPv6 when enabled.
func (r *Rule) tunIPs(enableIPv6 bool) []string {
	if enableIPv6 {
		return []string{r.LocalTunIPv4, r.LocalTunIPv6}
	}
	return []string{r.LocalTunIPv4}
}

// envoyPort returns the envoy upstream port mapped for the given container port,
// or 0 if the rule has no mapping for it.
func (r *Rule) envoyPort(containerPort int32) int32 {
	for _, pm := range r.ParsePortMap() {
		if pm.ContainerPort == containerPort {
			return pm.EnvoyPort
		}
	}
	return 0
}

// parsePort converts a port string to int32, returning 0 on parse failure.
func parsePort(s string) (int32, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

// xdsResources accumulates the four Envoy xDS resource lists a Virtual produces. The
// per-port builders write into it so To() does not have to spread/merge slices by hand.
type xdsResources struct {
	listeners []types.Resource
	clusters  []types.Resource
	routes    []types.Resource
	endpoints []types.Resource
}

// addTunCluster appends the cluster + endpoint routing to ip:port and returns the shared
// cluster name (also used as the route target). This is the one place the cluster/endpoint
// pair is created, for both the TCP and UDP builders.
func (r *xdsResources) addTunCluster(ip string, port int32) string {
	name := fmt.Sprintf("%s_%v", ip, port)
	r.clusters = append(r.clusters, toCluster(name))
	r.endpoints = append(r.endpoints, toEndPoint(name, ip, port))
	return name
}

// To converts the Virtual into Envoy xDS resources (listeners, clusters, routes,
// endpoints). Each port is turned into per-protocol resources; in mesh mode a single
// virtual-inbound capture listener is added as the bound entry point for the sidecar's
// iptables DNAT.
func (a *Virtual) To(enableIPv6 bool, logger *log.Entry) (
	listeners []types.Resource,
	clusters []types.Resource,
	routes []types.Resource,
	endpoints []types.Resource,
) {
	var res xdsResources
	fargate := a.IsFargateMode()
	logger.Debugf("[xDS] convert %s/%s: fargate=%v ipv6=%v ports=%d rules=%d",
		a.Namespace, a.UID, fargate, enableIPv6, len(a.Ports), len(a.Rules))
	for _, port := range a.Ports {
		// Mesh binds the container port; fargate binds the dedicated envoy listener port.
		listenPort := port.ContainerPort
		if fargate {
			listenPort = port.EnvoyListenerPort
		}
		logger.Debugf("[xDS] %s/%s port %d/%s -> listen on %d", a.Namespace, a.UID, port.ContainerPort, port.Protocol, listenPort)
		if port.Protocol == corev1.ProtocolUDP {
			a.addUDPPort(&res, port, listenPort, enableIPv6)
		} else {
			a.addTCPPort(&res, port, listenPort, fargate, enableIPv6)
		}
	}

	// Mesh mode adds two shared, per-Virtual resources:
	//   - the :15006 virtual-inbound capture listener: the bound entry point the sidecar
	//     iptables DNATs to, since the per-port TCP listeners are BindToPort=false.
	//   - origin_cluster: the ORIGINAL_DST cluster that both the capture passthrough and
	//     every TCP default route (no header match) forward to. It is referenced by name
	//     in many places but created exactly once, here.
	// Fargate binds each listener directly and routes to explicit per-IP clusters, so it
	// needs neither.
	if !fargate {
		res.listeners = append(res.listeners, toInboundCaptureListener(fmt.Sprintf("%s_%s_inbound", a.Namespace, a.UID)))
		res.clusters = append(res.clusters, originCluster())
		logger.Debugf("[xDS] %s/%s added mesh inbound capture listener :%d + origin_cluster", a.Namespace, a.UID, config.PortEnvoyInbound)
	}
	logger.Debugf("[xDS] %s/%s generated %d listeners, %d clusters, %d routes, %d endpoints",
		a.Namespace, a.UID, len(res.listeners), len(res.clusters), len(res.routes), len(res.endpoints))
	return res.listeners, res.clusters, res.routes, res.endpoints
}

// addUDPPort builds the udp_proxy listener plus its cluster/endpoint for a UDP port, one set
// per rule per IP family. UDP has no headers, so every datagram is forwarded to the rule's
// TUN IP. The listener name and bind address are made family-distinct so the v4 and v6
// listeners do not collapse to one in the xDS snapshot (which would leave the survivor bound
// to the wrong family); unpopulated IP families are skipped to avoid an endpoint with an
// invalid empty address.
func (a *Virtual) addUDPPort(res *xdsResources, port ContainerPort, listenPort int32, enableIPv6 bool) {
	listenerName := fmt.Sprintf("%s_%s_%v_%s", a.Namespace, a.UID, listenPort, port.Protocol)
	for _, rule := range a.Rules {
		envoyRulePort := rule.envoyPort(port.ContainerPort)
		for _, ip := range rule.tunIPs(enableIPv6) {
			if ip == "" {
				continue
			}
			bindAddr, name := "0.0.0.0", listenerName
			if strings.Contains(ip, ":") {
				bindAddr, name = "::", listenerName+"_v6"
			}
			clusterName := res.addTunCluster(ip, envoyRulePort)
			res.listeners = append(res.listeners, toUDPListener(name, bindAddr, listenPort, clusterName))
		}
	}
}

// addTCPPort builds the TCP listener, route config, and per-rule clusters/endpoints for a
// TCP port, with header-based routing to each rule's TUN IP. The default route falls back to
// origin_cluster (mesh, via use_original_dst) or to an explicit per-IP container-port cluster
// (fargate, where use_original_dst does not work).
func (a *Virtual) addTCPPort(res *xdsResources, port ContainerPort, listenPort int32, fargate, enableIPv6 bool) {
	listenerName := fmt.Sprintf("%s_%s_%v_%s", a.Namespace, a.UID, listenPort, port.Protocol)
	// The listener resolves its routes from an RDS config of the same name.
	routeName := listenerName
	res.listeners = append(res.listeners, toListener(listenerName, routeName, listenPort, port.Protocol, fargate))

	var rr []*route.Route
	for _, rule := range a.Rules {
		envoyRulePort := rule.envoyPort(port.ContainerPort)
		for _, ip := range rule.tunIPs(enableIPv6) {
			rr = append(rr, toRoute(res.addTunCluster(ip, envoyRulePort), rule.Headers))
		}
	}

	if fargate {
		// Fargate needs an explicit default route to the container port (deduped across
		// rules) because use_original_dst does not work there.
		ips := sets.New[string]()
		for _, rule := range a.Rules {
			ips.Insert(rule.tunIPs(enableIPv6)...)
		}
		for _, ip := range ips.UnsortedList() {
			rr = append(rr, defaultRouteToCluster(res.addTunCluster(ip, port.ContainerPort)))
		}
	} else {
		// Mesh: no header match falls back to origin_cluster (created once in To()).
		rr = append(rr, defaultRouteToCluster(originClusterName))
	}

	res.routes = append(res.routes, &route.RouteConfiguration{
		Name: routeName,
		VirtualHosts: []*route.VirtualHost{{
			Name:    "local_service",
			Domains: []string{"*"},
			Routes:  rr,
		}},
	})
}

func toEndPoint(clusterName string, localTunIP string, port int32) *endpoint.ClusterLoadAssignment {
	return &endpoint.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints: []*endpoint.LocalityLbEndpoints{{
			LbEndpoints: []*endpoint.LbEndpoint{{
				HostIdentifier: &endpoint.LbEndpoint_Endpoint{
					Endpoint: &endpoint.Endpoint{
						Address: &core.Address{
							Address: &core.Address_SocketAddress{
								SocketAddress: &core.SocketAddress{
									Address: localTunIP,
									PortSpecifier: &core.SocketAddress_PortValue{
										PortValue: uint32(port),
									},
								},
							},
						},
					},
				},
			}},
		}},
	}
}

func toCluster(clusterName string) *cluster.Cluster {
	return &cluster.Cluster{
		Name:                 clusterName,
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS},
		EdsClusterConfig: &cluster.Cluster_EdsClusterConfig{
			EdsConfig: &core.ConfigSource{
				ResourceApiVersion: resource.DefaultAPIVersion,
				ConfigSourceSpecifier: &core.ConfigSource_Ads{
					Ads: &core.AggregatedConfigSource{},
				},
			},
		},
		ConnectTimeout: durationpb.New(envoyClusterConnectTimeout),
		LbPolicy:       cluster.Cluster_ROUND_ROBIN,
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustMarshalAny(&httpv3.HttpProtocolOptions{
				CommonHttpProtocolOptions: &core.HttpProtocolOptions{
					IdleTimeout: durationpb.New(envoyClusterIdleTimeout),
				},
				UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_UseDownstreamProtocolConfig{
					UseDownstreamProtocolConfig: &httpv3.HttpProtocolOptions_UseDownstreamHttpConfig{
						HttpProtocolOptions: &core.Http1ProtocolOptions{
							HeaderKeyFormat: &core.Http1ProtocolOptions_HeaderKeyFormat{
								HeaderFormat: &core.Http1ProtocolOptions_HeaderKeyFormat_StatefulFormatter{
									StatefulFormatter: createPreserveCaseConfig(),
								},
							},
						},
					},
				},
			}),
		},
		DnsLookupFamily: cluster.Cluster_ALL,
	}
}

func originCluster() *cluster.Cluster {
	return &cluster.Cluster{
		Name:           originClusterName,
		ConnectTimeout: durationpb.New(envoyClusterConnectTimeout),
		LbPolicy:       cluster.Cluster_CLUSTER_PROVIDED,
		ClusterDiscoveryType: &cluster.Cluster_Type{
			Type: cluster.Cluster_ORIGINAL_DST,
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustMarshalAny(&httpv3.HttpProtocolOptions{
				UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_UseDownstreamProtocolConfig{
					UseDownstreamProtocolConfig: &httpv3.HttpProtocolOptions_UseDownstreamHttpConfig{
						HttpProtocolOptions: &core.Http1ProtocolOptions{
							HeaderKeyFormat: &core.Http1ProtocolOptions_HeaderKeyFormat{
								HeaderFormat: &core.Http1ProtocolOptions_HeaderKeyFormat_StatefulFormatter{
									StatefulFormatter: createPreserveCaseConfig(),
								},
							},
						},
					},
				},
			}),
		},
	}
}

func toRoute(clusterName string, headers map[string]string) *route.Route {
	var r []*route.HeaderMatcher
	for k, v := range headers {
		r = append(r, &route.HeaderMatcher{
			Name: k,
			HeaderMatchSpecifier: &route.HeaderMatcher_StringMatch{
				StringMatch: &matcher.StringMatcher{
					MatchPattern: &matcher.StringMatcher_Exact{
						Exact: v,
					},
					IgnoreCase: true,
				},
			},
		})
	}
	return &route.Route{
		Match: &route.RouteMatch{
			PathSpecifier: &route.RouteMatch_Prefix{
				Prefix: "/",
			},
			Headers: r,
		},
		Action: &route.Route_Route{
			Route: &route.RouteAction{
				ClusterSpecifier: &route.RouteAction_Cluster{
					Cluster: clusterName,
				},
				Timeout:     durationpb.New(0),
				IdleTimeout: durationpb.New(0),
				MaxStreamDuration: &route.RouteAction_MaxStreamDuration{
					MaxStreamDuration:    durationpb.New(0),
					GrpcTimeoutHeaderMax: durationpb.New(0),
				},
			},
		},
	}
}

func defaultRouteToCluster(clusterName string) *route.Route {
	return &route.Route{
		Match: &route.RouteMatch{
			PathSpecifier: &route.RouteMatch_Prefix{
				Prefix: "/",
			},
		},
		Action: &route.Route_Route{
			Route: &route.RouteAction{
				ClusterSpecifier: &route.RouteAction_Cluster{
					Cluster: clusterName,
				},
				Timeout:     durationpb.New(0),
				IdleTimeout: durationpb.New(0),
				MaxStreamDuration: &route.RouteAction_MaxStreamDuration{
					MaxStreamDuration:    durationpb.New(0),
					GrpcTimeoutHeaderMax: durationpb.New(0),
				},
			},
		},
	}
}

// buildFilterChains creates the HTTP connection manager filter chain and a TCP proxy
// fallback filter chain. Used for all protocol types (TCP, UDP, SCTP).
func buildFilterChains(routeName string) []*listener.FilterChain {
	httpManager := &httpconnectionmanager.HttpConnectionManager{
		CodecType:  httpconnectionmanager.HttpConnectionManager_AUTO,
		StatPrefix: "http",
		RouteSpecifier: &httpconnectionmanager.HttpConnectionManager_Rds{
			Rds: &httpconnectionmanager.Rds{
				ConfigSource: &core.ConfigSource{
					ResourceApiVersion: resource.DefaultAPIVersion,
					ConfigSourceSpecifier: &core.ConfigSource_ApiConfigSource{
						ApiConfigSource: &core.ApiConfigSource{
							TransportApiVersion:       resource.DefaultAPIVersion,
							ApiType:                   core.ApiConfigSource_GRPC,
							SetNodeOnFirstMessageOnly: true,
							GrpcServices: []*core.GrpcService{{
								TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
									EnvoyGrpc: &core.GrpcService_EnvoyGrpc{ClusterName: "xds_cluster"},
								},
							}},
						},
					},
				},
				RouteConfigName: routeName,
			},
		},
		// terminal filter envoy.filters.http.router must be the last filter in a http filter chain
		HttpFilters: []*httpconnectionmanager.HttpFilter{
			{
				Name: wellknown.GRPCWeb,
				ConfigType: &httpconnectionmanager.HttpFilter_TypedConfig{
					TypedConfig: mustMarshalAny(&grpcwebv3.GrpcWeb{}),
				},
			},
			{
				Name: wellknown.CORS,
				ConfigType: &httpconnectionmanager.HttpFilter_TypedConfig{
					TypedConfig: mustMarshalAny(&corsv3.Cors{}),
				},
			},
			{
				Name: wellknown.Router,
				ConfigType: &httpconnectionmanager.HttpFilter_TypedConfig{
					TypedConfig: mustMarshalAny(&routerv3.Router{}),
				},
			},
		},
		StreamIdleTimeout: durationpb.New(0),
		UpgradeConfigs: []*httpconnectionmanager.HttpConnectionManager_UpgradeConfig{{
			UpgradeType: "websocket",
		}},
		AccessLog: []*v31.AccessLog{{
			Name: wellknown.FileAccessLog,
			ConfigType: &v31.AccessLog_TypedConfig{
				TypedConfig: mustMarshalAny(&accesslogfilev3.FileAccessLog{
					Path: "/dev/stdout",
				}),
			},
		}},
		HttpProtocolOptions: &core.Http1ProtocolOptions{
			HeaderKeyFormat: &core.Http1ProtocolOptions_HeaderKeyFormat{
				HeaderFormat: &core.Http1ProtocolOptions_HeaderKeyFormat_StatefulFormatter{
					StatefulFormatter: createPreserveCaseConfig(),
				},
			},
		},
	}

	tcpConfig := &tcpproxy.TcpProxy{
		StatPrefix: "tcp",
		ClusterSpecifier: &tcpproxy.TcpProxy_Cluster{
			Cluster: originClusterName,
		},
	}

	return []*listener.FilterChain{
		{
			FilterChainMatch: &listener.FilterChainMatch{
				ApplicationProtocols: []string{"http/1.0", "http/1.1", "h2c"},
			},
			Filters: []*listener.Filter{
				{
					Name: wellknown.HTTPConnectionManager,
					ConfigType: &listener.Filter_TypedConfig{
						TypedConfig: mustMarshalAny(httpManager),
					},
				},
			},
		},
		{
			Filters: []*listener.Filter{
				{
					Name: wellknown.TCPProxy,
					ConfigType: &listener.Filter_TypedConfig{
						TypedConfig: mustMarshalAny(tcpConfig),
					},
				},
			},
		},
	}
}

func toListener(listenerName string, routeName string, port int32, p corev1.Protocol, isFargateMode bool) *listener.Listener {
	var protocol core.SocketAddress_Protocol
	switch p {
	case corev1.ProtocolTCP:
		protocol = core.SocketAddress_TCP
	case corev1.ProtocolUDP:
		protocol = core.SocketAddress_UDP
	case corev1.ProtocolSCTP:
		protocol = core.SocketAddress_TCP
	}

	return &listener.Listener{
		Name:             listenerName,
		TrafficDirection: core.TrafficDirection_INBOUND,
		BindToPort:       &wrapperspb.BoolValue{Value: isFargateMode},
		UseOriginalDst:   &wrapperspb.BoolValue{Value: true},
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol: protocol,
					Address:  "0.0.0.0",
					PortSpecifier: &core.SocketAddress_PortValue{
						PortValue: uint32(port),
					},
				},
			},
		},
		FilterChains: buildFilterChains(routeName),
		ListenerFilters: []*listener.ListenerFilter{
			{
				Name: wellknown.HttpInspector,
				ConfigType: &listener.ListenerFilter_TypedConfig{
					TypedConfig: mustMarshalAny(&httpinspector.HttpInspector{}),
				},
			},
			{
				Name: wellknown.OriginalDestination,
				ConfigType: &listener.ListenerFilter_TypedConfig{
					TypedConfig: mustMarshalAny(&dstv3inspector.OriginalDst{}),
				},
			},
		},
	}
}

// toInboundCaptureListener creates the mesh-mode virtual-inbound listener bound on
// config.PortEnvoyInbound (:15006). The sidecar iptables DNATs all inbound TCP to this
// port; this listener restores the original destination via the original_dst listener
// filter + use_original_dst and redirects each connection to the per-port virtual
// listener (BindToPort=false) that matches the original port. Connections whose original
// port has no per-port listener fall through to this listener's passthrough filter chain,
// which TCP-proxies to origin_cluster (ORIGINAL_DST) so the app still receives them.
func toInboundCaptureListener(name string) *listener.Listener {
	passthrough := &tcpproxy.TcpProxy{
		StatPrefix:       "inbound_passthrough",
		ClusterSpecifier: &tcpproxy.TcpProxy_Cluster{Cluster: originClusterName},
	}
	return &listener.Listener{
		Name:             name,
		TrafficDirection: core.TrafficDirection_INBOUND,
		BindToPort:       &wrapperspb.BoolValue{Value: true},
		UseOriginalDst:   &wrapperspb.BoolValue{Value: true},
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol: core.SocketAddress_TCP,
					Address:  "0.0.0.0",
					PortSpecifier: &core.SocketAddress_PortValue{
						PortValue: uint32(config.PortEnvoyInbound),
					},
				},
			},
		},
		ListenerFilters: []*listener.ListenerFilter{
			{
				Name: wellknown.OriginalDestination,
				ConfigType: &listener.ListenerFilter_TypedConfig{
					TypedConfig: mustMarshalAny(&dstv3inspector.OriginalDst{}),
				},
			},
		},
		FilterChains: []*listener.FilterChain{
			{
				Filters: []*listener.Filter{
					{
						Name: wellknown.TCPProxy,
						ConfigType: &listener.Filter_TypedConfig{
							TypedConfig: mustMarshalAny(passthrough),
						},
					},
				},
			},
		},
	}
}

// toUDPListener creates a UDP listener with the udp_proxy filter routing to the given cluster.
// UDP does not support use_original_dst or header-based routing — all traffic goes to the cluster.
func toUDPListener(name string, bindAddr string, port int32, clusterName string) *listener.Listener {
	return &listener.Listener{
		Name:             name,
		TrafficDirection: core.TrafficDirection_INBOUND,
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol: core.SocketAddress_UDP,
					Address:  bindAddr,
					PortSpecifier: &core.SocketAddress_PortValue{
						PortValue: uint32(port),
					},
				},
			},
		},
		ListenerFilters: []*listener.ListenerFilter{
			{
				Name: "envoy.filters.udp_listener.udp_proxy",
				ConfigType: &listener.ListenerFilter_TypedConfig{
					TypedConfig: mustMarshalAny(&udpproxyv3.UdpProxyConfig{
						StatPrefix: "udp_proxy",
						// The single-cluster `cluster` field is deprecated; route via the
						// matcher API instead. With no match criteria, every datagram falls
						// through to OnNoMatch -> the udp_proxy Route action for the cluster.
						RouteSpecifier: &udpproxyv3.UdpProxyConfig_Matcher{
							Matcher: &xdsmatcher.Matcher{
								OnNoMatch: &xdsmatcher.Matcher_OnMatch{
									OnMatch: &xdsmatcher.Matcher_OnMatch_Action{
										Action: &xdscore.TypedExtensionConfig{
											Name:        "route",
											TypedConfig: mustMarshalAny(&udpproxyv3.Route{Cluster: clusterName}),
										},
									},
								},
							},
						},
						IdleTimeout: durationpb.New(envoyUDPProxyIdleTimeout),
					}),
				},
			},
		},
	}
}
