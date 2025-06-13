package dhcp

import (
	"context"
	"net"
	"sync"

	"k8s.io/client-go/kubernetes"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type Server struct {
	rpc.UnimplementedDHCPServer

	sync.Mutex
	clientset *kubernetes.Clientset
}

func NewServer(clientset *kubernetes.Clientset) *Server {
	return &Server{
		clientset: clientset,
	}
}

func (s *Server) RentIP(ctx context.Context, req *rpc.RentIPRequest) (*rpc.RentIPResponse, error) {
	s.Lock()
	defer s.Unlock()

	plog.G(ctx).Infof("Handling rent IP request, pod name: %s, ns: %s", req.PodName, req.PodNamespace)
	manager := NewDHCPManager(s.clientset, req.PodNamespace)
	v4, v6, err := manager.RentIP(ctx)
	if err != nil {
		plog.G(ctx).Errorf("Failed to rent IP: %v", err)
		return nil, err
	}
	// todo patch annotation
	resp := &rpc.RentIPResponse{
		IPv4CIDR: v4.String(),
		IPv6CIDR: v6.String(),
	}
	return resp, nil
}

func (s *Server) ReleaseIP(ctx context.Context, req *rpc.ReleaseIPRequest) (*rpc.ReleaseIPResponse, error) {
	s.Lock()
	defer s.Unlock()

	plog.G(ctx).Infof("Handling release IP request, pod name: %s, ns: %s, IPv4: %s, IPv6: %s", req.PodName, req.PodNamespace, req.IPv4CIDR, req.IPv6CIDR)

	ipv4, _, err := net.ParseCIDR(req.IPv4CIDR)
	if err != nil {
		plog.G(ctx).Errorf("IP %s is invailed: %v", req.IPv4CIDR, err)
	}
	var ipv6 net.IP
	ipv6, _, err = net.ParseCIDR(req.IPv6CIDR)
	if err != nil {
		plog.G(ctx).Errorf("IP %s is invailed: %v", req.IPv6CIDR, err)
	}

	manager := NewDHCPManager(s.clientset, req.PodNamespace)
	if err = manager.ReleaseIP(ctx, ipv4, ipv6); err != nil {
		plog.G(ctx).Errorf("Failed to release IP: %v", err)
		return nil, err
	}
	return &rpc.ReleaseIPResponse{}, nil
}
