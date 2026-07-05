package action

import (
	"context"
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/types"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// Route handles the Route RPC, adding or deleting a CIDR route on the TUN device for the active connection.
func (svr *Server) Route(ctx context.Context, req *rpc.RouteRequest) (*rpc.RouteResponse, error) {
	if !svr.IsSudo {
		svr.connMu.RLock()
		conn, _ := svr.findConnection(svr.currentConnectionID)
		svr.connMu.RUnlock()
		if conn == nil {
			return nil, config.ErrConnectionNotFound
		}

		ips := svr.getSudoTunIPs(ctx)
		v4, v6 := resolveTunIP(conn, ips)
		var tunIPs []net.IP
		if v4 != "" {
			tunIPs = append(tunIPs, net.ParseIP(v4))
		}
		if v6 != "" {
			tunIPs = append(tunIPs, net.ParseIP(v6))
		}
		if len(tunIPs) == 0 {
			return nil, fmt.Errorf("no TUN IP available")
		}
		dev, err := netutil.GetTunDevice(tunIPs...)
		if err != nil {
			return nil, err
		}
		tunName := dev.Name
		_, ipNet, err := net.ParseCIDR(req.Cidr)
		if err != nil {
			return nil, err
		}
		client, err := svr.GetClient(true)
		if err != nil {
			return nil, err
		}
		return client.Route(ctx, &rpc.RouteRequest{
			Cidr: ipNet.String(),
			Type: req.Type,
			Dev:  tunName,
		})
	}

	_, ipNet, err := net.ParseCIDR(req.Cidr)
	if err != nil {
		return nil, err
	}
	switch req.Type {
	case rpc.RouteType_ROUTE_ADD:
		err = tun.AddRoutes(req.Dev, types.Route{Dst: *ipNet})
	case rpc.RouteType_ROUTE_DELETE:
		err = tun.DeleteRoutes(req.Dev, types.Route{Dst: *ipNet})
	}
	if err != nil {
		return nil, err
	}
	return &rpc.RouteResponse{}, nil
}
