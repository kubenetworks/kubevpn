package action

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/containernetworking/cni/pkg/types"
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

func (svr *Server) Route(ctx context.Context, req *rpc.RouteRequest) (*rpc.RouteResponse, error) {
	if !svr.IsSudo {
		logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(svr.LogFile))
		var index = -1
		for i, connection := range svr.connections {
			if connection.GetConnectionID() == svr.currentConnectionID {
				index = i
				break
			}
		}
		if index == -1 {
			logger.Infof("No connection found")
			return nil, fmt.Errorf("no connection found")
		}

		tunName, err := svr.connections[index].GetTunDeviceName()
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
	} else {
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
		return &rpc.RouteResponse{
			Message: "ok",
		}, nil
	}
}
