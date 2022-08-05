package controlplane

import (
	"fmt"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	httpinspector "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/http_inspector/v3"
	httpconnectionmanager "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxy "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	matcher "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/golang/protobuf/ptypes/wrappers"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
)

type Virtual struct {
	Uid   string // group.resource.name
	Ports []corev1.ContainerPort
	Rules []*Rule
}

type Rule struct {
	Headers    map[string]string
	LocalTunIP string
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
			clusterName := fmt.Sprintf("%s_%v", rule.LocalTunIP, port.ContainerPort)
			clusters = append(clusters, ToCluster(clusterName))
			endpoints = append(endpoints, ToEndPoint(clusterName, rule.LocalTunIP, port.ContainerPort))
			rr = append(rr, ToRoute(clusterName, rule.Headers))
		}
		rr = append(rr, DefaultRoute())
		routes = append(routes, &route.RouteConfiguration{
			Name: routeName,
			VirtualHosts: []*route.VirtualHost{
				{
					Name:    "local_service",
					Domains: []string{"*"},
					Routes:  rr,
				},
			},
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
	return &cluster.Cluster{
		Name:                 clusterName,
		ConnectTimeout:       durationpb.New(5 * time.Second),
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS},
		LbPolicy:             cluster.Cluster_ROUND_ROBIN,
		DnsLookupFamily:      cluster.Cluster_V4_ONLY,
		EdsClusterConfig: &cluster.Cluster_EdsClusterConfig{
			EdsConfig: &core.ConfigSource{
				ResourceApiVersion: resource.DefaultAPIVersion,
				ConfigSourceSpecifier: &core.ConfigSource_Ads{
					Ads: &core.AggregatedConfigSource{},
				},
			},
		},
	}
}

func OriginCluster() *cluster.Cluster {
	return &cluster.Cluster{
		Name:           "origin_cluster",
		ConnectTimeout: durationpb.New(time.Second * 5),
		LbPolicy:       cluster.Cluster_CLUSTER_PROVIDED,
		ClusterDiscoveryType: &cluster.Cluster_Type{
			Type: cluster.Cluster_ORIGINAL_DST,
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

	any := func(m proto.Message) *anypb.Any {
		pbst, _ := anypb.New(m)
		return pbst
	}

	httpManager := &httpconnectionmanager.HttpConnectionManager{
		CodecType:  httpconnectionmanager.HttpConnectionManager_AUTO,
		StatPrefix: "http",
		HttpFilters: []*httpconnectionmanager.HttpFilter{{
			Name: wellknown.Router,
		}},
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
		BindToPort:       &wrappers.BoolValue{Value: false},
		UseOriginalDst:   &wrappers.BoolValue{Value: true},

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
							TypedConfig: any(httpManager),
						},
					},
				},
			},
			{
				Filters: []*listener.Filter{
					{
						Name: wellknown.TCPProxy,
						ConfigType: &listener.Filter_TypedConfig{
							TypedConfig: any(tcpConfig),
						},
					},
				},
			},
		},
		ListenerFilters: []*listener.ListenerFilter{
			{
				Name: wellknown.HttpInspector,
				ConfigType: &listener.ListenerFilter_TypedConfig{
					TypedConfig: any(&httpinspector.HttpInspector{}),
				},
			},
		},
	}
}
