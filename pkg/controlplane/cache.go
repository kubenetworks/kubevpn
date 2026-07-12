package controlplane

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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
	preservecasev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/http/header_formatters/preserve_case/v3"
	udpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
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
)

// CurrentSchemaVersion is the latest schema version for Virtual configs stored in ConfigMaps.
// Bump this when making breaking changes to the Virtual/Rule struct layout.
// A zero value means a legacy config created before versioning was introduced.
// Version 2: OwnerID is required on all rules (no backward compat with empty OwnerID).
const CurrentSchemaVersion = 2

// Virtual represents an envoy xDS configuration for a single proxied workload.
type Virtual struct {
	// SchemaVersion tracks the config schema revision. Zero means legacy (pre-versioning).
	SchemaVersion int    `yaml:"schemaVersion,omitempty" json:"schemaVersion,omitempty"`
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

// parsePort converts a port string to int32, returning 0 on parse failure.
func parsePort(s string) (int32, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

func (a *Virtual) To(enableIPv6 bool, logger *log.Entry) (
	listeners []types.Resource,
	clusters []types.Resource,
	routes []types.Resource,
	endpoints []types.Resource,
) {
	fargate := a.IsFargateMode()
	for _, port := range a.Ports {
		listenPort := port.ContainerPort
		if fargate {
			listenPort = port.EnvoyListenerPort
		}

		listenerName := fmt.Sprintf("%s_%s_%v_%s", a.Namespace, a.UID, listenPort, port.Protocol)

		// UDP ports use udp_proxy filter (no HTTP/TCP filter chain, no header routing).
		if port.Protocol == corev1.ProtocolUDP {
			for _, rule := range a.Rules {
				var envoyRulePort int32
				for _, pm := range rule.ParsePortMap() {
					if pm.ContainerPort == port.ContainerPort {
						envoyRulePort = pm.EnvoyPort
						break
					}
				}
				ips := []string{rule.LocalTunIPv4}
				if enableIPv6 {
					ips = append(ips, rule.LocalTunIPv6)
				}
				for _, ip := range ips {
					clusterName := fmt.Sprintf("%s_%v", ip, envoyRulePort)
					clusters = append(clusters, toCluster(clusterName))
					endpoints = append(endpoints, toEndPoint(clusterName, ip, envoyRulePort))
					listeners = append(listeners, toUDPListener(listenerName, listenPort, clusterName))
				}
			}
			continue
		}

		// TCP ports use the existing HTTP connection manager with header-based routing.
		routeName := listenerName
		listeners = append(listeners, toListener(listenerName, routeName, listenPort, port.Protocol, fargate))

		var rr []*route.Route
		for _, rule := range a.Rules {
			var ips []string
			if enableIPv6 {
				ips = []string{rule.LocalTunIPv4, rule.LocalTunIPv6}
			} else {
				ips = []string{rule.LocalTunIPv4}
			}
			var envoyRulePort int32
			for _, pm := range rule.ParsePortMap() {
				if pm.ContainerPort == port.ContainerPort {
					envoyRulePort = pm.EnvoyPort
					break
				}
			}
			for _, ip := range ips {
				clusterName := fmt.Sprintf("%s_%v", ip, envoyRulePort)
				clusters = append(clusters, toCluster(clusterName))
				endpoints = append(endpoints, toEndPoint(clusterName, ip, envoyRulePort))
				rr = append(rr, toRoute(clusterName, rule.Headers))
			}
		}
		// fargate needs explicit default route to container port because use_original_dst does not work
		if fargate {
			// all ips should is IPv4 127.0.0.1 and ::1
			var ips = sets.New[string]()
			for _, rule := range a.Rules {
				if enableIPv6 {
					ips.Insert(rule.LocalTunIPv4, rule.LocalTunIPv6)
				} else {
					ips.Insert(rule.LocalTunIPv4)
				}
			}
			for _, ip := range ips.UnsortedList() {
				defaultClusterName := fmt.Sprintf("%s_%v", ip, port.ContainerPort)
				clusters = append(clusters, toCluster(defaultClusterName))
				endpoints = append(endpoints, toEndPoint(defaultClusterName, ip, port.ContainerPort))
				rr = append(rr, defaultRouteToCluster(defaultClusterName))
			}
		} else {
			rr = append(rr, defaultRouteToCluster("origin_cluster"))
			clusters = append(clusters, originCluster())
		}
		routes = append(routes, &route.RouteConfiguration{
			Name: routeName,
			VirtualHosts: []*route.VirtualHost{
				{
					Name:    "local_service",
					Domains: []string{"*"},
					Routes:  rr,
				},
			},
			MaxDirectResponseBodySizeBytes: nil,
		})
	}
	return
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
		ConnectTimeout: durationpb.New(5 * time.Second),
		LbPolicy:       cluster.Cluster_ROUND_ROBIN,
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": mustMarshalAny(&httpv3.HttpProtocolOptions{
				CommonHttpProtocolOptions: &core.HttpProtocolOptions{
					IdleTimeout: durationpb.New(time.Second * 10),
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
		Name:           "origin_cluster",
		ConnectTimeout: durationpb.New(time.Second * 5),
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
			Cluster: "origin_cluster",
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

// toUDPListener creates a UDP listener with the udp_proxy filter routing to the given cluster.
// UDP does not support use_original_dst or header-based routing — all traffic goes to the cluster.
func toUDPListener(name string, port int32, clusterName string) *listener.Listener {
	return &listener.Listener{
		Name:             name,
		TrafficDirection: core.TrafficDirection_INBOUND,
		Address: &core.Address{
			Address: &core.Address_SocketAddress{
				SocketAddress: &core.SocketAddress{
					Protocol: core.SocketAddress_UDP,
					Address:  "0.0.0.0",
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
						RouteSpecifier: &udpproxyv3.UdpProxyConfig_Cluster{
							Cluster: clusterName,
						},
						IdleTimeout: durationpb.New(5 * time.Minute),
					}),
				},
			},
		},
	}
}
