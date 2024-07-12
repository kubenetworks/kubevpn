package action

import (
	"context"
	"net"
	"sync"

	"github.com/containernetworking/cni/pkg/types"
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/core"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var _, bits = config.DockerCIDR.Mask.Size()
var DefaultServerIP = (&net.IPNet{IP: config.DockerRouterIP, Mask: net.CIDRMask(bits, bits)}).String()

var serverIP string
var mux sync.Mutex
var sshCancelFunc context.CancelFunc

func (svr *Server) SshStart(ctx context.Context, req *rpc.SshStartRequest) (*rpc.SshStartResponse, error) {
	mux.Lock()
	defer mux.Unlock()

	clientIP, clientCIDR, err := net.ParseCIDR(req.ClientIP)
	if err != nil {
		log.Errorf("parse cidr error: %v", err)
		return nil, err
	}
	if serverIP == "" {
		r := core.Route{
			ServeNodes: []string{
				"tun://127.0.0.1:8422?net=" + DefaultServerIP,
				"tcp://:10800",
			},
			Retries: 5,
		}
		servers, err := handler.Parse(r)
		if err != nil {
			log.Errorf("parse route error: %v", err)
			return nil, err
		}
		var ctx1 context.Context
		ctx1, sshCancelFunc = context.WithCancel(context.Background())
		go func() {
			err := handler.Run(ctx1, servers)
			if err != nil {
				log.Errorf("run route error: %v", err)
			}
		}()
		serverIP = DefaultServerIP
	}

	serverip, _, err := net.ParseCIDR(serverIP)
	if err != nil {
		return nil, err
	}
	tunDevice, err := util.GetTunDevice(serverip)
	if err != nil {
		return nil, err
	}
	err = tun.AddRoutes(tunDevice.Name, types.Route{
		Dst: net.IPNet{
			IP:   clientIP,
			Mask: clientCIDR.Mask,
		},
		GW: nil,
	})
	if err != nil {
		log.Errorf("add route error: %v", err)
		return nil, err
	}

	return &rpc.SshStartResponse{ServerIP: serverIP}, nil
}

func (svr *Server) SshStop(ctx context.Context, req *rpc.SshStopRequest) (*rpc.SshStopResponse, error) {
	if sshCancelFunc != nil {
		sshCancelFunc()
	}
	return &rpc.SshStopResponse{}, nil
}
