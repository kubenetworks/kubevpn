package action

import (
	"context"
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/types"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var defaultServerIP = func() string {
	_, bits := config.DockerCIDR.Mask.Size()
	return (&net.IPNet{IP: config.DockerRouterIP, Mask: net.CIDRMask(bits, bits)}).String()
}()

// SshStart handles the SshStart RPC, creating a TUN server and adding a route for the SSH client's IP.
func (svr *Server) SshStart(ctx context.Context, req *rpc.SshStartRequest) (resp *rpc.SshStartResponse, err error) {
	svr.Lock.Lock()
	defer svr.Lock.Unlock()

	var clientIP net.IP
	var clientCIDR *net.IPNet
	clientIP, clientCIDR, err = net.ParseCIDR(req.ClientIP)
	if err != nil {
		plog.G(ctx).Errorf("Failed to parse network CIDR: %v", err)
		return
	}
	if svr.sshServerIP == "" {
		defer func() {
			if err != nil {
				if svr.sshCancelFunc != nil {
					svr.sshCancelFunc()
				}
				svr.sshServerIP = ""
			}
		}()

		nodes := []*core.Node{
			core.NewNode("tun", "").WithParam("net", defaultServerIP),
			core.NewNode("gtcp", fmt.Sprintf(":%d", config.PortTCP)),
		}
		var servers []core.Server
		servers, err = core.GenerateServersFromNodes(nodes, core.NewRouteHub())
		if err != nil {
			plog.G(ctx).Errorf("Failed to parse route: %v", err)
			return
		}
		var sshCtx context.Context
		sshCtx, svr.sshCancelFunc = context.WithCancel(context.Background())
		go func() {
			if err := handler.Run(sshCtx, servers); err != nil {
				plog.G(ctx).Errorf("Failed to run route: %v", err)
			}
		}()
		svr.sshServerIP = defaultServerIP
	}

	var serverip net.IP
	serverip, _, err = net.ParseCIDR(svr.sshServerIP)
	if err != nil {
		return
	}
	var tunDevice *net.Interface
	tunDevice, err = util.GetTunDevice(serverip)
	if err != nil {
		return
	}
	err = tun.AddRoutes(tunDevice.Name, types.Route{
		Dst: net.IPNet{
			IP:   clientIP,
			Mask: clientCIDR.Mask,
		},
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to add route: %v", err)
		return
	}

	resp = &rpc.SshStartResponse{ServerIP: svr.sshServerIP}
	return
}

// SshStop handles the SshStop RPC, shutting down the SSH TUN server.
func (svr *Server) SshStop(ctx context.Context, req *rpc.SshStopRequest) (*rpc.SshStopResponse, error) {
	if svr.sshCancelFunc != nil {
		svr.sshCancelFunc()
	}
	return &rpc.SshStopResponse{}, nil
}
