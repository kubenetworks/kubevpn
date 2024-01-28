package webhook

import (
	"context"
	"net"
	"sync"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/webhook/rpc"
)

type dhcpServer struct {
	rpc.UnimplementedDHCPServer

	sync.Mutex
	f         util.Factory
	clientset *kubernetes.Clientset
}

func (d *dhcpServer) RentIP(ctx context.Context, req *rpc.RentIPRequest) (*rpc.RentIPResponse, error) {
	d.Lock()
	defer d.Unlock()

	log.Infof("handling rent ip request, pod name: %s, ns: %s", req.PodName, req.PodNamespace)
	cmi := d.clientset.CoreV1().ConfigMaps(req.PodNamespace)
	dhcp := handler.NewDHCPManager(cmi, req.PodNamespace)
	v4, v6, err := dhcp.RentIPRandom(ctx)
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

func (d *dhcpServer) ReleaseIP(ctx context.Context, req *rpc.ReleaseIPRequest) (*rpc.ReleaseIPResponse, error) {
	d.Lock()
	defer d.Unlock()

	log.Infof("handling release ip request, pod name: %s, ns: %s, ipv4: %s, ipv6: %s", req.PodName, req.PodNamespace, req.IPv4CIDR, req.IPv6CIDR)
	var ips []net.IP
	for _, s := range []string{req.IPv4CIDR, req.IPv6CIDR} {
		ip, _, err := net.ParseCIDR(s)
		if err != nil {
			log.Errorf("ip is invailed, ip: %s, err: %v", ip.String(), err)
			continue
		}
		ips = append(ips, ip)
	}

	cmi := d.clientset.CoreV1().ConfigMaps(req.PodNamespace)
	dhcp := handler.NewDHCPManager(cmi, req.PodNamespace)
	if err := dhcp.ReleaseIP(context.Background(), ips...); err != nil {
		log.Errorf("release ip failed, err: %v", err)
		return nil, err
	}
	return &rpc.ReleaseIPResponse{}, nil
}
