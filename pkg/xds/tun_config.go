package xds

import (
	"context"
	"net"

	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds/internal/discovery"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds/internal/envoy"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds/internal/leases"
)

const XDSPort uint = config.PortXDS
const LeaseDuration = leases.LeaseDuration

// TunConfigServer adapts the RPC protocol to the lease and discovery domains.
type TunConfigServer struct {
	rpc.UnimplementedTunConfigServiceServer
	clientset kubernetes.Interface
	namespace string
	factory   cmdutil.Factory
	leases    *leases.Manager
	routes    *discovery.Broadcaster
}

// NewTunConfigServer initializes the mandatory allocator before serving RPCs.
func NewTunConfigServer(ctx context.Context, clientset kubernetes.Interface, namespace string) (*TunConfigServer, error) {
	s := &TunConfigServer{clientset: clientset, namespace: namespace}
	rules := envoy.New(clientset, namespace)
	manager, err := leases.New(ctx, clientset, namespace, leases.Callbacks{
		Sync:   rules.Sync,
		Remove: rules.Remove,
	})
	if err != nil {
		return nil, err
	}
	s.leases = manager
	s.routes = discovery.New(ctx, clientset)
	return s, nil
}

func parseExcludeIPs(raw []string) []net.IP {
	ips := make([]net.IP, 0, len(raw))
	for _, rawIP := range raw {
		if ip := net.ParseIP(rawIP); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

func tunIPResponse(event *leases.Event) *rpc.TunIPResponse {
	return &rpc.TunIPResponse{IPv4: event.IPv4, IPv6: event.IPv6, Version: event.Version, DryRun: event.DryRun}
}

func (s *TunConfigServer) GetTunIP(ctx context.Context, req *rpc.TunIPRequest) (*rpc.TunIPResponse, error) {
	event, err := s.leases.GetTunIP(ctx, &leases.Request{OwnerID: req.OwnerID, ExcludeIPs: parseExcludeIPs(req.ExcludeIPs), ConfirmIP: net.ParseIP(req.ConfirmIP), Hostname: req.Hostname})
	if err != nil {
		return nil, err
	}
	return tunIPResponse(event), nil
}

func (s *TunConfigServer) WatchTunIP(req *rpc.TunIPRequest, stream rpc.TunConfigService_WatchTunIPServer) error {
	return s.leases.Watch(stream.Context(), req.OwnerID, func(event *leases.Event) error { return stream.Send(tunIPResponse(event)) })
}

func (s *TunConfigServer) WatcherCount(owner string) int { return s.leases.WatcherCount(owner) }
func (s *TunConfigServer) NotifyIPChange(owner string, v4, v6 *net.IPNet) {
	s.leases.NotifyIPChange(owner, v4, v6)
}
func (s *TunConfigServer) StartLeaseReaper(ctx context.Context) { s.leases.StartLeaseReaper(ctx) }
func (s *TunConfigServer) ReconcileDHCP(ctx context.Context)    { s.leases.ReconcileDHCP(ctx) }
func (s *TunConfigServer) ReconcileAllocsFromConfigMap(ctx context.Context) {
	s.leases.ReconcileAllocsFromConfigMap(ctx)
}

// Close joins all domain workers after the RPC and ConfigMap watcher have stopped.
func (s *TunConfigServer) Close() {
	s.routes.Close()
	s.leases.Close()
}
