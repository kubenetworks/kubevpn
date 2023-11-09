package action

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/tun"
	"github.com/wencaiwulue/kubevpn/pkg/util"
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
		errors.LogErrorf("parse cidr error: %v", err)
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
			errors.LogErrorf("parse route error: %v", err)
			return nil, err
		}
		ctx, sshCancelFunc = context.WithCancel(context.Background())
		go func() {
			err := handler.Run(ctx, servers)
			if err != nil {
				errors.LogErrorf("run route error: %v", err)
			}
		}()

		ctx2, cancelF := context.WithCancel(ctx)
		wait.UntilWithContext(ctx2, func(ctx context.Context) {
			ip, _, _ := net.ParseCIDR(DefaultServerIP)
			ok, err := util.Ping(ip.String())
			if err != nil {
			} else if ok {
				cancelF()
			} else {
				// todo
				cancelF()
			}
		}, time.Millisecond*20)
		if err != nil {
			return nil, err
		}
		serverIP = DefaultServerIP
	}

	serverip, _, err := net.ParseCIDR(serverIP)
	if err != nil {
		err = errors.Wrap(err, "Failed to parse CIDR.")
		return nil, err
	}
	tunDevice, err := util.GetTunDevice(serverip)
	if err != nil {
		err = errors.Wrap(err, "Failed to get Tun device.")
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
		errors.LogErrorf("add route error: %v", err)
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
