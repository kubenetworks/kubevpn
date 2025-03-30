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
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
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
func (svr *Server) Proxy(req *rpc.ProxyRequest, resp rpc.Daemon_ProxyServer) (e error) {
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newProxyWarp(resp), svr.LogFile))
	config.Image = req.Image
	ctx := plog.WithLogger(resp.Context(), logger)
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
	connect := &handler.ConnectOptions{
		Namespace:            req.Namespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		ImagePullSecretName:  req.ImagePullSecretName,
	}
	err = connect.InitClient(util.InitFactoryByPath(path, req.Namespace))
	if err != nil {
		return err
	}
	var workloads []string
	workloads, err = util.NormalizedResource(ctx, connect.GetFactory(), connect.GetClientset(), req.Namespace, req.Workloads)
	if err != nil {
		return err
	}

	defer func() {
		if e != nil && svr.connect != nil {
			_ = svr.connect.LeaveAllProxyResources(plog.WithLogger(context.Background(), logger))
		}
	}()

	daemonClient := svr.GetClient(false)
	if daemonClient == nil {
		return fmt.Errorf("daemon is not avaliable")
	}
	helmNs, _ := util.GetHelmInstalledNamespace(ctx, connect.GetFactory())
	if helmNs != "" {
		logger.Infof("Using helm namespace: %s", helmNs)
		connect.Namespace = helmNs
	} else {
		logger.Infof("Use namespace: %s", req.Namespace)
	}

	if svr.connect != nil {
		isSameCluster, _ := util.IsSameCluster(
			ctx,
			svr.connect.GetClientset().CoreV1(), svr.connect.Namespace,
			connect.GetClientset().CoreV1(), connect.Namespace,
		)
		if isSameCluster {
			// same cluster, do nothing
			plog.G(ctx).Infof("Connected to cluster")
		} else {
			plog.G(ctx).Infof("Disconnecting from another cluster...")
			var disconnectResp rpc.Daemon_DisconnectClient
			disconnectResp, err = daemonClient.Disconnect(ctx, &rpc.DisconnectRequest{
				KubeconfigBytes: ptr.To(req.KubeconfigBytes),
				Namespace:       ptr.To(req.Namespace),
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
		}
	}

	if svr.connect == nil {
		plog.G(ctx).Debugf("Connectting to cluster")
		var connResp rpc.Daemon_ConnectClient
		connResp, err = daemonClient.Connect(ctx, convert(req, helmNs))
		if err != nil {
			return err
		}
		err = util.CopyGRPCStream[rpc.ConnectResponse](connResp, resp)
		if err != nil {
			return err
		}
	}

	err = svr.connect.CreateRemoteInboundPod(ctx, req.Namespace, workloads, req.Headers, req.PortMap)
	if err != nil {
		plog.G(ctx).Errorf("Failed to inject inbound sidecar: %v", err)
		return err
	}
	return nil
}

type proxyWarp struct {
	server rpc.Daemon_ProxyServer
}

func (r *proxyWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.ProxyResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newProxyWarp(server rpc.Daemon_ProxyServer) io.Writer {
	return &proxyWarp{server: server}
}

func convert(req *rpc.ProxyRequest, ns string) *rpc.ConnectRequest {
	return &rpc.ConnectRequest{
		KubeconfigBytes:      req.KubeconfigBytes,
		Namespace:            util.If(ns != "", ns, req.Namespace),
		Engine:               req.Engine,
		ExtraRoute:           req.ExtraRoute,
		SshJump:              req.SshJump,
		TransferImage:        req.TransferImage,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		Foreground:           req.Foreground,
		Level:                req.Level,
		OriginKubeconfigPath: req.OriginKubeconfigPath,
	}
}
