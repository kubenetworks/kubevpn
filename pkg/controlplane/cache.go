package controlplane

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v31 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
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

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type Virtual struct {
	Namespace string
	Uid       string // group.resource.name
	Ports     []ContainerPort
	Rules     []*Rule
}

type ContainerPort struct {
	// If specified, this must be an IANA_SVC_NAME and unique within the pod. Each
	// named port in a pod must have a unique name. Name for the port that can be
	// referred to by services.
	// +optional
	Name string `json:"name,omitempty"`
	// Number of port to expose on the host.
	// If specified, this must be a valid port number, 0 < x < 65536.
	// envoy listener port, if is not 0, means fargate mode
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

type Rule struct {
	Headers      map[string]string
	LocalTunIPv4 string
	LocalTunIPv6 string
	// for no privileged mode (AWS Fargate mode), don't have cap NET_ADMIN and privileged: true. so we can not use OSI layer 3 proxy
	// containerPort -> envoyRulePort:localPort
	// envoyRulePort for envoy forward to localhost:envoyRulePort
	// localPort for local pc listen localhost:localPort
	// use ssh reverse tunnel, envoy rule endpoint localhost:envoyRulePort will forward to local pc localhost:localPort
	// localPort is required and envoyRulePort is optional
	PortMap map[int32]string
}

func (a *Virtual) To(enableIPv6 bool, logger *log.Logger) (
	listeners []types.Resource,
	clusters []types.Resource,
	routes []types.Resource,
	endpoints []types.Resource,
) {
	//clusters = append(clusters, OriginCluster())
	for _, port := range a.Ports {
		isFargateMode := port.EnvoyListenerPort != 0

		listenerName := fmt.Sprintf("%s_%s_%v_%s", a.Namespace, a.Uid, util.If(isFargateMode, port.EnvoyListenerPort, port.ContainerPort), port.Protocol)
		routeName := listenerName
		listeners = append(listeners, ToListener(listenerName, routeName, util.If(isFargateMode, port.EnvoyListenerPort, port.ContainerPort), port.Protocol, isFargateMode))

		var rr []*route.Route
		for _, rule := range a.Rules {
			var ips []string
			if enableIPv6 {
				ips = []string{rule.LocalTunIPv4, rule.LocalTunIPv6}
			} else {
				ips = []string{rule.LocalTunIPv4}
			}
			ports := rule.PortMap[port.ContainerPort]
			if isFargateMode {
				if strings.Index(ports, ":") > 0 {
					ports = strings.Split(ports, ":")[0]
				} else {
					logger.Errorf("fargate mode port should have two pair: %s", ports)
				}
			}
			envoyRulePort, _ := strconv.Atoi(ports)
			for _, ip := range ips {
				clusterName := fmt.Sprintf("%s_%v", ip, envoyRulePort)
				clusters = append(clusters, ToCluster(clusterName))
				endpoints = append(endpoints, ToEndPoint(clusterName, ip, int32(envoyRulePort)))
				rr = append(rr, ToRoute(clusterName, rule.Headers))
			}
		}
		// if isFargateMode is true, needs to add default route to container port, because use_original_dst not work
		if isFargateMode {
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
				clusters = append(clusters, ToCluster(defaultClusterName))
				endpoints = append(endpoints, ToEndPoint(defaultClusterName, ip, port.ContainerPort))
				rr = append(rr, DefaultRouteToCluster(defaultClusterName))
			}
		} else {
			rr = append(rr, DefaultRoute())
			clusters = append(clusters, OriginCluster())
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

func ToEndPoint(clusterName string, localTunIP string, port int32) *endpoint.ClusterLoadAssignment {
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

func ToCluster(clusterName string) *cluster.Cluster {
	anyFunc := func(m proto.Message) *anypb.Any {
		pbst, _ := anypb.New(m)
		return pbst
	}
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
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": anyFunc(&httpv3.HttpProtocolOptions{
				CommonHttpProtocolOptions: &corev3.HttpProtocolOptions{
					IdleTimeout: durationpb.New(time.Second * 10),
				},
				UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_UseDownstreamProtocolConfig{
					UseDownstreamProtocolConfig: &httpv3.HttpProtocolOptions_UseDownstreamHttpConfig{},
				},
			}),
		},
		DnsLookupFamily: cluster.Cluster_ALL,
	}
}

func OriginCluster() *cluster.Cluster {
	anyFunc := func(m proto.Message) *anypb.Any {
		pbst, _ := anypb.New(m)
		return pbst
	}
	return &cluster.Cluster{
		Name:           "origin_cluster",
		ConnectTimeout: durationpb.New(time.Second * 5),
		LbPolicy:       cluster.Cluster_CLUSTER_PROVIDED,
		ClusterDiscoveryType: &cluster.Cluster_Type{
			Type: cluster.Cluster_ORIGINAL_DST,
		},
		TypedExtensionProtocolOptions: map[string]*anypb.Any{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": anyFunc(&httpv3.HttpProtocolOptions{
				UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_UseDownstreamProtocolConfig{
					UseDownstreamProtocolConfig: &httpv3.HttpProtocolOptions_UseDownstreamHttpConfig{},
				},
			}),
		},
	}
}

func ToRoute(clusterName string, headers map[string]string) *route.Route {
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

func DefaultRoute() *route.Route {
	return &route.Route{
		Match: &route.RouteMatch{
			PathSpecifier: &route.RouteMatch_Prefix{
				Prefix: "/",
			},
		},
		Action: &route.Route_Route{
			Route: &route.RouteAction{
				ClusterSpecifier: &route.RouteAction_Cluster{
					Cluster: "origin_cluster",
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

func DefaultRouteToCluster(clusterName string) *route.Route {
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

func ToListener(listenerName string, routeName string, port int32, p corev1.Protocol, isFargateMode bool) *listener.Listener {
	var protocol core.SocketAddress_Protocol
	switch p {
	case corev1.ProtocolTCP:
		protocol = core.SocketAddress_TCP
	case corev1.ProtocolUDP:
		protocol = core.SocketAddress_UDP
	case corev1.ProtocolSCTP:
		protocol = core.SocketAddress_TCP
	}

	anyFunc := func(m proto.Message) *anypb.Any {
		pbst, _ := anypb.New(m)
		return pbst
	}

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
		// "details": "Error: terminal filter named envoy.filters.http.router of type envoy.filters.http.router must be the last filter in a http filter chain."
		HttpFilters: []*httpconnectionmanager.HttpFilter{
			{
				Name: wellknown.GRPCWeb,
				ConfigType: &httpconnectionmanager.HttpFilter_TypedConfig{
					TypedConfig: anyFunc(&grpcwebv3.GrpcWeb{}),
				},
			},
			{
				Name: wellknown.CORS,
				ConfigType: &httpconnectionmanager.HttpFilter_TypedConfig{
					TypedConfig: anyFunc(&corsv3.Cors{}),
				},
			},
			{
				Name: wellknown.Router,
				ConfigType: &httpconnectionmanager.HttpFilter_TypedConfig{
					TypedConfig: anyFunc(&routerv3.Router{}),
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
				TypedConfig: anyFunc(&accesslogfilev3.FileAccessLog{
					Path: "/dev/stdout",
				}),
			},
		}},
	}

	tcpConfig := &tcpproxy.TcpProxy{
		StatPrefix: "tcp",
		ClusterSpecifier: &tcpproxy.TcpProxy_Cluster{
			Cluster: "origin_cluster",
		},
	}

	return &listener.Listener{
		Name:             listenerName,
		TrafficDirection: core.TrafficDirection_INBOUND,
		BindToPort:       &wrapperspb.BoolValue{Value: util.If(isFargateMode, true, false)},
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
		FilterChains: []*listener.FilterChain{
			{
				FilterChainMatch: &listener.FilterChainMatch{
					ApplicationProtocols: []string{"http/1.0", "http/1.1", "h2c"},
				},
				Filters: []*listener.Filter{
					{
						Name: wellknown.HTTPConnectionManager,
						ConfigType: &listener.Filter_TypedConfig{
							TypedConfig: anyFunc(httpManager),
						},
					},
				},
			},
			{
				Filters: []*listener.Filter{
					{
						Name: wellknown.TCPProxy,
						ConfigType: &listener.Filter_TypedConfig{
							TypedConfig: anyFunc(tcpConfig),
						},
					},
				},
			},
		},
		ListenerFilters: []*listener.ListenerFilter{
			{
				Name: wellknown.HttpInspector,
				ConfigType: &listener.ListenerFilter_TypedConfig{
					TypedConfig: anyFunc(&httpinspector.HttpInspector{}),
				},
			},
			{
				Name: wellknown.OriginalDestination,
				ConfigType: &listener.ListenerFilter_TypedConfig{
					TypedConfig: anyFunc(&dstv3inspector.OriginalDst{}),
				},
			},
		},
	}
}
