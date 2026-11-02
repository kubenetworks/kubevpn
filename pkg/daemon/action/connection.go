package action

import (
	"context"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// siblingTunIPs reads session IPs outside the registry lock.
func (svr *Server) siblingTunIPs() []net.IP {
	connections, _ := svr.registry.snapshot()
	var ips []net.IP
	for _, conn := range connections {
		if dp, ok := conn.(handler.DataPlane); ok {
			v4, v6 := dp.GetLocalTunIP()
			if ip := net.ParseIP(v4); ip != nil {
				ips = append(ips, ip)
			}
			if ip := net.ParseIP(v6); ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	return ips
}

// ConnectionList handles the ConnectionList RPC without holding membership locks
// across the peer-daemon health query or session status reads.
func (svr *Server) ConnectionList(ctx context.Context, req *rpc.ConnectionListRequest) (*rpc.ConnectionListResponse, error) {
	connections, currentID := svr.registry.snapshot()
	var ips map[string]tunIP
	if !svr.IsSudo {
		ips = svr.sudoHealthSnapshot(ctx)
	}
	var list []*rpc.Status
	for _, c := range connections {
		list = append(list, buildConnectionStatus(c, ips))
	}
	return &rpc.ConnectionListResponse{List: list, CurrentConnectionID: currentID}, nil
}

func cleanupConnection(ctx context.Context, conn handler.Connection) {
	if conn == nil {
		return
	}
	if pc, ok := conn.(handler.ProxyController); ok {
		if sync := pc.GetSync(); sync != nil {
			_ = sync.Cleanup(ctx)
		}
	}
	conn.Cleanup(ctx)
}

func (svr *Server) ConnectionUse(ctx context.Context, req *rpc.ConnectionUseRequest) (*rpc.ConnectionUseResponse, error) {
	if !svr.registry.use(req.ConnectionID) {
		return nil, config.ErrConnectionNotFound
	}
	return &rpc.ConnectionUseResponse{}, nil
}
