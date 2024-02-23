package controlplane

import (
	"fmt"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	corev1 "k8s.io/api/core/v1"
)

type Virtual struct {
	Uid   string // group.resource.name
	Ports []corev1.ContainerPort
	Rules []*Rule
}

type Rule struct {
	Headers      map[string]string
	LocalTunIPv4 string
	LocalTunIPv6 string
}

func (a *Virtual) To() (
	listeners []types.Resource,
	clusters []types.Resource,
	routes []types.Resource,
	endpoints []types.Resource,
) {
	//clusters = append(clusters, OriginCluster())
	for _, port := range a.Ports {
		listenerName := fmt.Sprintf("%s_%v_%s", a.Uid, port.ContainerPort, port.Protocol)
		routeName := listenerName
		listeners = append(listeners, ToListener(listenerName, routeName, port.ContainerPort, port.Protocol))

		var rr []*route.Route
		for _, rule := range a.Rules {
			for _, ip := range []string{rule.LocalTunIPv4, rule.LocalTunIPv6} {
				clusterName := fmt.Sprintf("%s_%v", ip, port.ContainerPort)
				clusters = append(clusters, ToCluster(clusterName))
				endpoints = append(endpoints, ToEndPoint(clusterName, ip, port.HostPort))
				rr = append(rr, ToRoute(clusterName, rule.Headers))
			}
		}
		rr = append(rr, DefaultRoute())
		clusters = append(clusters, OriginCluster())
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

func ToListener(listenerName string, routeName string, port int32, p corev1.Protocol) *listener.Listener {
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
		BindToPort:       &wrapperspb.BoolValue{Value: false},
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
