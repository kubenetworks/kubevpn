package action

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type proxyWarp struct {
	server rpc.Daemon_ProxyServer
}

func (r *proxyWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.ConnectResponse{
		Message: string(p),
	})
	return len(p), err
}

func newProxyWarp(server rpc.Daemon_ProxyServer) io.Writer {
	return &proxyWarp{server: server}
}

// 1. if not connect to cluster
//		1.1 connect to cluster
//		1.2 proxy workloads
// 2. if already connect to cluster
//		2.1 disconnect from cluster
// 		2.2 same as step 1

func (svr *Server) Proxy(req *rpc.ConnectRequest, resp rpc.Daemon_ProxyServer) error {
	out := newProxyWarp(resp)
	log.SetOutput(out)
	defer func() {
		log.SetOutput(os.Stdout)
		log.SetLevel(log.DebugLevel)
	}()
	ctx := context.Background()
	util.InitLogger(false)
	connect := &handler.ConnectOptions{
		Namespace:   req.Namespace,
		Headers:     req.Headers,
		Workloads:   req.Workloads,
		ExtraCIDR:   req.ExtraCIDR,
		ExtraDomain: req.ExtraDomain,
		UseLocalDNS: req.UseLocalDNS,
		Engine:      config.Engine(req.Engine),
	}
	var sshConf = &util.SshConfig{
		Addr:             req.Addr,
		User:             req.User,
		Password:         req.Password,
		Keyfile:          req.Keyfile,
		ConfigAlias:      req.ConfigAlias,
		RemoteKubeconfig: req.RemoteKubeconfig,
	}
	err := handler.SshJump(ctx, sshConf, nil)
	if err != nil {
		return err
	}
	err = connect.InitClient(InitFactory(req.KubeconfigBytes, req.Namespace))
	if err != nil {
		return err
	}
	err = connect.PreCheckResource()
	if err != nil {
		return err
	}

	if svr.connect != nil {
		isSameCluster, err := util.IsSameCluster(
			svr.connect.GetClientset().CoreV1().ConfigMaps(svr.connect.Namespace), svr.connect.Namespace,
			connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace), connect.Namespace,
		)
		if err == nil && isSameCluster && svr.connect.Equal(connect) {
			// same cluster, do nothing
			log.Debugf("already connect to cluster")
		} else {
			log.Debugf("try to disconnect from another cluster")
			disconnect, err := svr.GetClient(true).Disconnect(ctx, &rpc.DisconnectRequest{})
			if err != nil {
				return err
			}
			for {
				recv, err := disconnect.Recv()
				if err == io.EOF {
					break
				} else if err != nil {
					return err
				}
				err = resp.Send(&rpc.ConnectResponse{Message: recv.Message})
				if err != nil {
					return err
				}
			}
			svr.connect = nil
		}
	}

	if svr.connect == nil {
		svr.connect = connect
		ctx, err = connect.RentInnerIP(ctx)
		if err != nil {
			return err
		}
		log.Debugf("connect to cluster")
		connResp, err := svr.GetClient(true).Connect(ctx, req)
		if err != nil {
			return err
		}
		for {
			recv, err := connResp.Recv()
			if err == io.EOF {
				break
			} else if err != nil {
				return err
			}
			err = resp.Send(recv)
			if err != nil {
				return err
			}
		}
	}

	err = svr.connect.CreateRemoteInboundPod(ctx)
	return err
}

func (svr *Server) redirectToSudoDaemon1(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer) error {
	cli := svr.GetClient(true)
	if cli == nil {
		return fmt.Errorf("sudo daemon not start")
	}
	connResp, err := cli.Connect(resp.Context(), req)
	if err != nil {
		return err
	}
	for {
		recv, err := connResp.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		err = resp.Send(recv)
		if err != nil {
			return err
		}
	}

	svr.t = time.Now()
	svr.connect = &handler.ConnectOptions{
		Namespace:   req.Namespace,
		Headers:     req.Headers,
		Workloads:   req.Workloads,
		ExtraCIDR:   req.ExtraCIDR,
		ExtraDomain: req.ExtraDomain,
		UseLocalDNS: req.UseLocalDNS,
		Engine:      config.Engine(req.Engine),
	}
	var sshConf = &util.SshConfig{
		Addr:             req.Addr,
		User:             req.User,
		Password:         req.Password,
		Keyfile:          req.Keyfile,
		ConfigAlias:      req.ConfigAlias,
		RemoteKubeconfig: req.RemoteKubeconfig,
	}
	err = handler.SshJump(context.Background(), sshConf, nil)
	if err != nil {
		return err
	}
	err = svr.connect.InitClient(InitFactory(req.KubeconfigBytes, req.Namespace))
	if err != nil {
		return err
	}
	err = svr.connect.PreCheckResource()
	if err != nil {
		return err
	}
	return nil
}
