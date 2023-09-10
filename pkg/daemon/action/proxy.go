package action

import (
	"fmt"
	"io"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

// Proxy
//  1. if not connect to cluster
//     1.1 connect to cluster
//     1.2 proxy workloads
//  2. if already connect to cluster
//     2.1 disconnect from cluster
//     2.2 same as step 1
func (svr *Server) Proxy(req *rpc.ConnectRequest, resp rpc.Daemon_ProxyServer) error {
	origin := log.StandardLogger().Out
	out := io.MultiWriter(newProxyWarp(resp), origin)
	log.SetOutput(out)
	defer func() {
		log.SetOutput(origin)
		log.SetLevel(log.DebugLevel)
	}()
	util.InitLogger(false)
	ctx := resp.Context()
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

	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	flags.AddFlag(&pflag.Flag{
		Name:     "kubeconfig",
		DefValue: file,
	})
	err = handler.SshJump(ctx, sshConf, flags)
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

	daemonClient := svr.GetClient(false)
	if daemonClient == nil {
		return fmt.Errorf("daemon is not avaliable")
	}
	if svr.connect != nil {
		var isSameCluster bool
		isSameCluster, err = util.IsSameCluster(
			svr.connect.GetClientset().CoreV1().ConfigMaps(svr.connect.Namespace), svr.connect.Namespace,
			connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace), connect.Namespace,
		)
		if err == nil && isSameCluster && svr.connect.Equal(connect) {
			// same cluster, do nothing
			log.Debugf("already connect to cluster")
		} else {
			log.Debugf("try to disconnect from another cluster")
			var disconnect rpc.Daemon_DisconnectClient
			disconnect, err = daemonClient.Disconnect(ctx, &rpc.DisconnectRequest{})
			if err != nil {
				return err
			}
			var recv *rpc.DisconnectResponse
			for {
				recv, err = disconnect.Recv()
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
		}
	}

	if svr.connect == nil {
		log.Debugf("connectting to cluster")
		var connResp rpc.Daemon_ConnectClient
		connResp, err = daemonClient.Connect(ctx, req)
		if err != nil {
			return err
		}
		var recv *rpc.ConnectResponse
		for {
			recv, err = connResp.Recv()
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

	log.Debugf("proxy resource...")
	err = svr.connect.CreateRemoteInboundPod(ctx)
	if err != nil {
		return err
	}
	log.Debugf("proxy resource done")
	util.Print(out, "Now you can access resources in the kubernetes cluster, enjoy it :)")
	return nil
}

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
