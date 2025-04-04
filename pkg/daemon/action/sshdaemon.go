package action

import (
	"context"
	"net"
	"sync"

	"github.com/containernetworking/cni/pkg/types"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var _, bits = config.DockerCIDR.Mask.Size()
var DefaultServerIP = (&net.IPNet{IP: config.DockerRouterIP, Mask: net.CIDRMask(bits, bits)}).String()

var serverIP string
var mux sync.Mutex
var sshCancelFunc context.CancelFunc

func (svr *Server) SshStart(ctx context.Context, req *rpc.SshStartRequest) (resp *rpc.SshStartResponse, err error) {
	mux.Lock()
	defer mux.Unlock()

	var clientIP net.IP
	var clientCIDR *net.IPNet
	clientIP, clientCIDR, err = net.ParseCIDR(req.ClientIP)
	if err != nil {
		plog.G(ctx).Errorf("Failed to parse network CIDR: %v", err)
		return
	}
	if serverIP == "" {
		defer func() {
			if err != nil {
				if sshCancelFunc != nil {
					sshCancelFunc()
				}
				serverIP = ""
			}
		}()

		r := core.Route{
			ServeNodes: []string{
				"tun://127.0.0.1:8422?net=" + DefaultServerIP,
				"tcp://:10800",
			},
			Retries: 5,
		}
		var servers []core.Server
		servers, err = handler.Parse(r)
		if err != nil {
			plog.G(ctx).Errorf("Failed to parse route: %v", err)
			return
		}
		var ctx1 context.Context
		ctx1, sshCancelFunc = context.WithCancel(context.Background())
		go func() {
			err := handler.Run(ctx1, servers)
			if err != nil {
				plog.G(ctx).Errorf("Failed to run route: %v", err)
			}
		}()
		serverIP = DefaultServerIP
	}

	var serverip net.IP
	serverip, _, err = net.ParseCIDR(serverIP)
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
		GW: nil,
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to add route: %v", err)
		return
	}

	resp = &rpc.SshStartResponse{ServerIP: serverIP}
	return
}

func (svr *Server) SshStop(ctx context.Context, req *rpc.SshStopRequest) (*rpc.SshStopResponse, error) {
	if sshCancelFunc != nil {
		sshCancelFunc()
	}
	return &rpc.SshStopResponse{}, nil
}
