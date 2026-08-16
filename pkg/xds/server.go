package xds

import (
	"context"
	"fmt"
	"net"
	"time"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	runtimeservice "github.com/envoyproxy/go-control-plane/envoy/service/runtime/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

const (
	grpcMaxConcurrentStreams = 1000

	// grpcKeepaliveTime is how often the server pings idle clients.
	grpcKeepaliveTime = 15 * time.Second
	// grpcKeepaliveTimeout is how long the server waits for a keepalive ack before closing.
	grpcKeepaliveTimeout = 5 * time.Second
	// grpcKeepaliveMinTime is the minimum client ping interval the server permits.
	grpcKeepaliveMinTime = 15 * time.Second
)

func runServer(ctx context.Context, server serverv3.Server, tunConfig *TunConfigServer, port uint) error {
	grpcOpts := []grpc.ServerOption{
		grpc.MaxConcurrentStreams(grpcMaxConcurrentStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    grpcKeepaliveTime,
			Timeout: grpcKeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             grpcKeepaliveMinTime,
			PermitWithoutStream: true,
		})}
	grpcServer := grpc.NewServer(grpcOpts...)
	grpc_health_v1.RegisterHealthServer(grpcServer, health.NewServer())
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}

	discoverygrpc.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, server)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, server)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, server)
	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, server)
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, server)
	runtimeservice.RegisterRuntimeDiscoveryServiceServer(grpcServer, server)

	// tunConfig is guaranteed non-nil: Main treats its init failure as fatal, so
	// the server never comes up without TunConfigService.
	rpc.RegisterTunConfigServiceServer(grpcServer, tunConfig)
	plog.G(ctx).Infof("TunConfigService registered on port %d", port)

	plog.G(ctx).Infof("Management server listening on %d", port)
	return grpcServer.Serve(listener)
}
