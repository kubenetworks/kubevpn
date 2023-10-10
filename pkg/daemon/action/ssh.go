package action

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/tun"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

var serverIP string
var mux sync.Mutex

func (svr *Server) SshStart(ctx context.Context, req *rpc.SshStartRequest) (*rpc.SshStartResponse, error) {
	mux.Lock()
	defer mux.Unlock()

	cidr, ipNet, err := net.ParseCIDR(req.ClientIP)
	if err != nil {
		log.Errorf("parse cidr error: %v", err)
		return nil, err
	}
	if serverIP == "" {
		r := core.Route{
			ServeNodes: []string{
				"tun://127.0.0.1:8422?net=223.254.0.123/32",
				"tcp://:10800",
			},
			Retries: 5,
		}
		servers, err := handler.Parse(r)
		if err != nil {
			log.Errorf("parse route error: %v", err)
			return nil, err
		}
		go func() {
			err = handler.Run(context.Background(), servers)
			if err != nil {
				log.Errorf("run route error: %v", err)
			}
		}()

		ctx2, cancelF := context.WithCancel(ctx)
		wait.UntilWithContext(ctx2, func(ctx context.Context) {
			ok, err := util.Ping("223.254.0.123")
			if err != nil {
			} else if ok {
				cancelF()
			} else {
				// todo
				cancelF()
			}
		}, time.Second)
		if err != nil {
			return nil, err
		}
		serverIP = "223.254.0.123/32"
	}

	err = tun.AddRoutes(types.Route{
		Dst: net.IPNet{
			IP:   cidr,
			Mask: ipNet.Mask,
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
	// todo remove route
	return &rpc.SshStopResponse{}, nil
}
