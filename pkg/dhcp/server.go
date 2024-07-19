package dhcp

import (
	"context"
	"net"
	"sync"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp/rpc"
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

	log.Infof("handling rent ip request, pod name: %s, ns: %s", req.PodName, req.PodNamespace)
	cmi := s.clientset.CoreV1().ConfigMaps(req.PodNamespace)
	manager := NewDHCPManager(cmi, req.PodNamespace)
	v4, v6, err := manager.RentIP(ctx)
	if err != nil {
		log.Errorf("rent ip failed, err: %v", err)
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

	log.Infof("handling release ip request, pod name: %s, ns: %s, ipv4: %s, ipv6: %s", req.PodName, req.PodNamespace, req.IPv4CIDR, req.IPv6CIDR)
	var ips []net.IP
	for _, ipStr := range []string{req.IPv4CIDR, req.IPv6CIDR} {
		ip, _, err := net.ParseCIDR(ipStr)
		if err != nil {
			log.Errorf("ip is invailed, ip: %s, err: %v", ipStr, err)
			continue
		}
		ips = append(ips, ip)
	}

	cmi := s.clientset.CoreV1().ConfigMaps(req.PodNamespace)
	manager := NewDHCPManager(cmi, req.PodNamespace)
	if err := manager.ReleaseIP(ctx, ips...); err != nil {
		log.Errorf("release ip failed, err: %v", err)
		return nil, err
	}
	return &rpc.ReleaseIPResponse{}, nil
}
