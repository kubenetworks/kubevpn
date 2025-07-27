package action

import (
	"context"
	"io"
	"os"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Clone(resp rpc.Daemon_CloneServer) (err error) {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger := plog.GetLoggerForClient(req.Level, io.MultiWriter(newCloneWarp(resp), svr.LogFile))

	var sshConf = ssh.ParseSshFromRPC(req.SshJump)
	connReq := &rpc.ConnectRequest{
		KubeconfigBytes:      req.KubeconfigBytes,
		Namespace:            req.Namespace,
		ExtraRoute:           req.ExtraRoute,
		SshJump:              req.SshJump,
		TransferImage:        req.TransferImage,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		Level:                req.Level,
		OriginKubeconfigPath: req.OriginKubeconfigPath,
	}
	cli, err := svr.GetClient(false)
	if err != nil {
		return err
	}

	var connResp grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse]
	var disconnectResp rpc.Daemon_DisconnectClient
	sshCtx, sshFunc := context.WithCancel(context.Background())
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
		sshFunc()
	}()

	connResp, err = cli.Connect(context.Background())
	if err != nil {
		return err
	}
	err = connResp.Send(connReq)
	if err != nil {
		return err
	}
	err = util.CopyAndConvertGRPCStream[rpc.ConnectResponse, rpc.CloneResponse](
		connResp,
		resp,
		func(r *rpc.ConnectResponse) *rpc.CloneResponse {
			_, _ = svr.LogFile.Write([]byte(r.Message))
			return &rpc.CloneResponse{Message: r.Message}
		})
	if err != nil {
		return err
	}

	options := &handler.CloneOptions{
		Namespace:            req.Namespace,
		Headers:              req.Headers,
		Workloads:            req.Workloads,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,

		TargetContainer:     req.TargetContainer,
		TargetImage:         req.TargetImage,
		TargetWorkloadNames: map[string]string{},
		LocalDir:            req.LocalDir,
		RemoteDir:           req.RemoteDir,
		Request:             req,
	}
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = options.Cleanup(sshCtx)
			sshFunc()
		}
	}()
	options.AddRollbackFunc(func() error {
		sshFunc()
		_ = os.Remove(file)
		return nil
	})
	if !sshConf.IsEmpty() {
		file, err = ssh.SshJump(sshCtx, sshConf, file, false)
		if err != nil {
			return err
		}
	}
	f := util.InitFactoryByPath(file, req.Namespace)
	err = options.InitClient(f)
	if err != nil {
		plog.G(context.Background()).Errorf("Failed to init client: %v", err)
		return err
	}
	logger.Infof("Clone workloads...")
	options.SetContext(sshCtx)
	err = options.DoClone(plog.WithLogger(sshCtx, logger), []byte(req.KubeconfigBytes), req.Image)
	if err != nil {
		plog.G(context.Background()).Errorf("Clone workloads failed: %v", err)
		return err
	}
	svr.clone = options
	return nil
}

type cloneWarp struct {
	server rpc.Daemon_CloneServer
}

func (r *cloneWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.CloneResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newCloneWarp(server rpc.Daemon_CloneServer) io.Writer {
	return &cloneWarp{server: server}
}
