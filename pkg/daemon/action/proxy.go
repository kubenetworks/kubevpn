package action

import (
	"context"
	"io"
	"os"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"

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
func (svr *Server) Proxy(resp rpc.Daemon_ProxyServer) (err error) {
	var req *rpc.ProxyRequest
	req, err = resp.Recv()
	if err != nil {
		return err
	}

	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newProxyWarp(resp), svr.LogFile))
	ctx := plog.WithLogger(resp.Context(), logger)
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)

	var file string
	file, err = util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
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
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		Request:              convert(req),
	}
	err = connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
	if err != nil {
		return err
	}

	var workloads []string
	workloads, _, err = util.NormalizedResource(connect.GetFactory(), req.Namespace, req.Workloads)
	if err != nil {
		return err
	}

	var cli rpc.DaemonClient
	cli, err = svr.GetClient(false)
	if err != nil {
		return errors.Wrap(err, "daemon is not available")
	}

	var connResp grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse]
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
	var connectionID string
	connectionID, err = util.CopyGRPCConnStream(connResp, resp)
	if err != nil {
		return err
	}

	var options *handler.ConnectOptions
	for _, connection := range svr.connections {
		if connection.GetConnectionID() == connectionID {
			options = connection
			break
		}
	}

	if options == nil {
		return errors.New("Failed to connect to cluster")
	}

	defer func() {
		if err != nil {
			_ = options.LeaveAllProxyResources(plog.WithLogger(context.Background(), logger))
		}
	}()

	var podList []v1.Pod
	podList, err = options.GetRunningPodList(cancel)
	if err != nil {
		return err
	}
	image := podList[0].Spec.Containers[0].Image
	err = options.CreateRemoteInboundPod(plog.WithLogger(cancel, logger), req.Namespace, workloads, req.Headers, req.PortMap, image)
	if err != nil {
		plog.G(ctx).Errorf("Failed to inject inbound sidecar: %v", err)
		return err
	}
	svr.currentConnectionID = connectionID
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
