package handler

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func Complete(ctx context.Context, route *core.Route) error {
	if v, ok := os.LookupEnv(config.EnvInboundPodTunIPv4); !ok || v != "" {
		return nil
	}

	ns := os.Getenv(config.EnvPodNamespace)
	if ns == "" {
		return fmt.Errorf("can not get namespace")
	}
	client, err := getDHCPServerClient(ctx, ns)
	if err != nil {
		return err
	}
	var resp *rpc.RentIPResponse
	resp, err = client.RentIP(ctx, &rpc.RentIPRequest{
		PodName:      os.Getenv(config.EnvPodName),
		PodNamespace: ns,
	})
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		_, err2 := client.ReleaseIP(context.Background(), &rpc.ReleaseIPRequest{
			PodName:      os.Getenv(config.EnvPodName),
			PodNamespace: os.Getenv(config.EnvPodNamespace),
			IPv4CIDR:     os.Getenv(config.EnvInboundPodTunIPv4),
			IPv6CIDR:     os.Getenv(config.EnvInboundPodTunIPv6),
		})
		if err2 != nil {
			plog.G(ctx).Errorf("Failed to release IP %s and %s: %v", resp.IPv4CIDR, resp.IPv6CIDR, err2)
		} else {
			plog.G(ctx).Debugf("Release IP %s and %s", resp.IPv4CIDR, resp.IPv6CIDR)
		}
	}()

	plog.G(ctx).Infof("Rent an IPv4: %s, IPv6: %s", resp.IPv4CIDR, resp.IPv6CIDR)
	if err = os.Setenv(config.EnvInboundPodTunIPv4, resp.IPv4CIDR); err != nil {
		plog.G(ctx).Errorf("Failed to set IP: %v", err)
		return err
	}
	if err = os.Setenv(config.EnvInboundPodTunIPv6, resp.IPv6CIDR); err != nil {
		plog.G(ctx).Errorf("Failed to set IP: %v", err)
		return err
	}
	for i := 0; i < len(route.ServeNodes); i++ {
		var node *core.Node
		node, err = core.ParseNode(route.ServeNodes[i])
		if err != nil {
			return err
		}
		if node.Protocol == "tun" {
			if get := node.Get("net"); get == "" {
				route.ServeNodes[i] = route.ServeNodes[i] + "&net=" + resp.IPv4CIDR
			}
		}
	}

	return nil
}

func getDHCPServerClient(ctx context.Context, namespace string) (rpc.DHCPClient, error) {
	cert, ok := os.LookupEnv(config.TLSCertKey)
	if !ok {
		return nil, fmt.Errorf("can not get %s from env", config.TLSCertKey)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM([]byte(cert))

	conn, err := grpc.DialContext(ctx,
		fmt.Sprintf("%s:80", util.GetTlsDomain(namespace)),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs: caCertPool,
		})),
	)
	if err != nil {
		return nil, err
	}
	healthClient := grpc_health_v1.NewHealthClient(conn)
	var resp *grpc_health_v1.HealthCheckResponse
	resp, err = healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return nil, err
	}
	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return nil, fmt.Errorf("grpc service is not heath: %s", resp.Status)
	}

	cli := rpc.NewDHCPClient(conn)
	return cli, nil
}
