package action

import (
	"context"
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/types"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

// Route handles the Route RPC, adding or deleting a CIDR route on the TUN device for the active connection.
func (svr *Server) Route(ctx context.Context, req *rpc.RouteRequest) (*rpc.RouteResponse, error) {
	if !svr.IsSudo {
		svr.connMu.RLock()
		conn, _ := svr.findConnection(svr.currentConnectionID)
		svr.connMu.RUnlock()
		if conn == nil {
			return nil, fmt.Errorf("no connection found")
		}

		tunName, err := conn.GetTunDeviceName()
		if err != nil {
			return nil, err
		}
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
