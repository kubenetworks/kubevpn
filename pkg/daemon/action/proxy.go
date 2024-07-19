package action

import (
	"context"
	"fmt"
	"io"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Proxy
//  1. if not connect to cluster
//     1.1 connect to cluster
//     1.2 proxy workloads
//  2. if already connect to cluster
//     2.1 disconnect from cluster
//     2.2 same as step 1
func (svr *Server) Proxy(req *rpc.ConnectRequest, resp rpc.Daemon_ProxyServer) (e error) {
	out := io.MultiWriter(newProxyWarp(resp), svr.LogFile)
	log.SetOutput(out)
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	config.Image = req.Image
	config.Debug = req.Level == int32(log.DebugLevel)
	log.SetLevel(log.InfoLevel)
	ctx := resp.Context()
	connect := &handler.ConnectOptions{
		Namespace:            req.Namespace,
		Headers:              req.Headers,
		PortMap:              req.PortMap,
		Workloads:            req.Workloads,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
	}
	var sshConf = util.ParseSshFromRPC(req.SshJump)

	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	flags.AddFlag(&pflag.Flag{
		Name:     "kubeconfig",
		DefValue: file,
	})
	var path string
	path, err = util.SshJump(ctx, sshConf, flags, false)
	if err != nil {
		return err
	}
	err = connect.InitClient(util.InitFactoryByPath(path, req.Namespace))
	if err != nil {
		return err
	}
	err = connect.PreCheckResource()
	if err != nil {
		return err
	}

	defer func() {
		if e != nil && svr.connect != nil {
			_ = svr.connect.LeaveProxyResources(context.Background())
		}
	}()

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
			log.Infof("already connect to cluster")
		} else {
			log.Infof("try to disconnect from another cluster")
			var disconnectResp rpc.Daemon_DisconnectClient
			disconnectResp, err = daemonClient.Disconnect(ctx, &rpc.DisconnectRequest{
				KubeconfigBytes: ptr.To(req.KubeconfigBytes),
				Namespace:       ptr.To(connect.Namespace),
				SshJump:         sshConf.ToRPC(),
			})
			if err != nil {
				return err
			}
			var recv *rpc.DisconnectResponse
			for {
				recv, err = disconnectResp.Recv()
				if err == io.EOF {
					break
				} else if err != nil {
					log.Errorf("recv from disconnect failed, %v", err)
					return err
				}
				err = resp.Send(&rpc.ConnectResponse{Message: recv.Message})
				if err != nil {
					return err
				}
			}
			log.SetOutput(out)
		}
	}

	if svr.connect == nil {
		log.Infof("connectting to cluster")
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
		log.SetOutput(out)
	}

	svr.connect.Workloads = req.Workloads
	svr.connect.Headers = req.Headers
	svr.connect.PortMap = req.PortMap
	err = svr.connect.CreateRemoteInboundPod(ctx)
	if err != nil {
		log.Errorf("create remote inbound pod failed: %s", err.Error())
		return err
	}
	return nil
}

type proxyWarp struct {
	server rpc.Daemon_ProxyServer
}

func (r *proxyWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.ConnectResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newProxyWarp(server rpc.Daemon_ProxyServer) io.Writer {
	return &proxyWarp{server: server}
}
