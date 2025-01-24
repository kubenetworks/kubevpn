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
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
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
	defer func() {
		util.InitLoggerForServer(true)
		log.SetOutput(svr.LogFile)
		config.Debug = false
	}()
	out := io.MultiWriter(newProxyWarp(resp), svr.LogFile)
	config.Image = req.Image
	config.Debug = req.Level == int32(log.DebugLevel)
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(out)
	ctx := resp.Context()
	connect := &handler.ConnectOptions{
		Namespace:            req.Namespace,
		Headers:              req.Headers,
		PortMap:              req.PortMap,
		Workloads:            req.Workloads,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		ImagePullSecretName:  req.ImagePullSecretName,
	}
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)

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
	path, err = ssh.SshJump(ctx, sshConf, flags, false)
	if err != nil {
		return err
	}
	err = connect.InitClient(util.InitFactoryByPath(path, req.Namespace))
	if err != nil {
		return err
	}
	var workloads []string
	workloads, err = util.NormalizedResource(ctx, connect.GetFactory(), connect.GetClientset(), connect.Namespace, connect.Workloads)
	if err != nil {
		return err
	}

	defer func() {
		if e != nil && svr.connect != nil {
			_ = svr.connect.LeaveAllProxyResources(context.Background())
		}
	}()

	daemonClient := svr.GetClient(false)
	if daemonClient == nil {
		return fmt.Errorf("daemon is not avaliable")
	}
	if svr.connect != nil {
		isSameCluster, _ := util.IsSameCluster(
			ctx,
			svr.connect.GetClientset().CoreV1().ConfigMaps(svr.connect.Namespace), svr.connect.Namespace,
			connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace), connect.Namespace,
		)
		if isSameCluster {
			// same cluster, do nothing
			log.Infof("Connected to cluster")
		} else {
			log.Infof("Disconnecting from another cluster...")
			var disconnectResp rpc.Daemon_DisconnectClient
			disconnectResp, err = daemonClient.Disconnect(ctx, &rpc.DisconnectRequest{
				KubeconfigBytes: ptr.To(req.KubeconfigBytes),
				Namespace:       ptr.To(connect.Namespace),
				SshJump:         sshConf.ToRPC(),
			})
			if err != nil {
				return err
			}
			err = util.CopyAndConvertGRPCStream[rpc.DisconnectResponse, rpc.ConnectResponse](
				disconnectResp,
				resp,
				func(response *rpc.DisconnectResponse) *rpc.ConnectResponse {
					return &rpc.ConnectResponse{Message: response.Message}
				},
			)
			if err != nil {
				return err
			}
			util.InitLoggerForClient(config.Debug)
			log.SetOutput(out)
		}
	}

	if svr.connect == nil {
		log.Debugf("Connectting to cluster")
		var connResp rpc.Daemon_ConnectClient
		connResp, err = daemonClient.Connect(ctx, req)
		if err != nil {
			return err
		}
		err = util.CopyGRPCStream[rpc.ConnectResponse](connResp, resp)
		if err != nil {
			return err
		}
		util.InitLoggerForClient(config.Debug)
		log.SetOutput(out)
	}

	err = svr.connect.CreateRemoteInboundPod(ctx, workloads, req.Headers, req.PortMap)
	if err != nil {
		log.Errorf("Failed to inject inbound sidecar: %v", err)
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
