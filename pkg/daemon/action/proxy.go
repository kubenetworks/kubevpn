package action

import (
	"context"
	"io"
	"os"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/cli-runtime/pkg/resource"
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
func (svr *Server) Proxy(resp rpc.Daemon_ProxyServer) (e error) {
	req, err := resp.Recv()
	if err != nil {
		return err
	}

	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newProxyWarp(resp), svr.LogFile))
	config.Image = req.Image
	ctx := plog.WithLogger(resp.Context(), logger)
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)

	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	defer os.Remove(file)
	if !sshConf.IsEmpty() {
		file, err = ssh.SshJump(ctx, sshConf, file, false)
		if err != nil {
			return err
		}
	}
	connect := &handler.ConnectOptions{
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		ImagePullSecretName:  req.ImagePullSecretName,
	}
	err = connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
	if err != nil {
		return err
	}
	var workloads []string
	var objectList []*resource.Info
	workloads, objectList, err = util.NormalizedResource(connect.GetFactory(), req.Namespace, req.Workloads)
	if err != nil {
		return err
	}
	// netstack gvisor only support k8s service
	if config.Engine(req.Engine) == config.EngineGvisor {
		for _, info := range objectList {
			if !util.IsK8sService(info) {
				return errors.Errorf("netstack gvisor mode only support k8s services, but got %s", info.Object.GetObjectKind().GroupVersionKind().Kind)
			}
		}
	}

	defer func() {
		if e != nil && svr.connect != nil {
			_ = svr.connect.LeaveAllProxyResources(plog.WithLogger(context.Background(), logger))
		}
	}()

	cli, err := svr.GetClient(false)
	if err != nil {
		return errors.Wrap(err, "daemon is not available")
	}

	var connResp, reConnResp grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse]
	var disconnectResp rpc.Daemon_DisconnectClient
	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	go func() {
		var s rpc.Cancel
		err = resp.RecvMsg(&s)
		if err != nil {
			return
		}
		if connResp != nil {
			_ = connResp.SendMsg(&s)
		}
		if disconnectResp != nil {
			_ = disconnectResp.SendMsg(&s)
		}
		if reConnResp != nil {
			_ = reConnResp.SendMsg(&s)
		}
		cancelFunc()
	}()

	plog.G(ctx).Debugf("Connecting to cluster")
	connResp, err = cli.Connect(context.Background())
	if err != nil {
		return err
	}
	err = connResp.Send(convert(req))
	if err != nil {
		return err
	}
	err = util.CopyGRPCStream[rpc.ConnectResponse](connResp, resp)
	if err != nil {
		if status.Code(err) != codes.AlreadyExists {
			return err
		}
		plog.G(ctx).Infof("Disconnecting from another cluster...")
		disconnectResp, err = cli.Disconnect(context.Background())
		if err != nil {
			return err
		}
		err = disconnectResp.Send(&rpc.DisconnectRequest{ID: ptr.To[int32](0)})
		if err != nil {
			return err
		}
		err = util.CopyAndConvertGRPCStream[rpc.DisconnectResponse, rpc.ConnectResponse](
			disconnectResp,
			resp,
			func(response *rpc.DisconnectResponse) *rpc.ConnectResponse {
				_, _ = svr.LogFile.Write([]byte(response.Message))
				return &rpc.ConnectResponse{Message: response.Message}
			},
		)
		if err != nil {
			return err
		}
		reConnResp, err = cli.Connect(context.Background())
		if err != nil {
			return err
		}
		err = reConnResp.Send(convert(req))
		if err != nil {
			return err
		}
		err = util.CopyGRPCStream[rpc.ConnectResponse](reConnResp, resp)
		if err != nil {
			return err
		}
	}

	err = svr.connect.CreateRemoteInboundPod(plog.WithLogger(cancel, logger), req.Namespace, workloads, req.Headers, req.PortMap)
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

func convert(req *rpc.ProxyRequest) *rpc.ConnectRequest {
	return &rpc.ConnectRequest{
		KubeconfigBytes:      req.KubeconfigBytes,
		Namespace:            req.Namespace,
		Engine:               req.Engine,
		ExtraRoute:           req.ExtraRoute,
		SshJump:              req.SshJump,
		TransferImage:        req.TransferImage,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		Foreground:           req.Foreground,
		Level:                req.Level,
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		ManagerNamespace:     req.ManagerNamespace,
	}
}
