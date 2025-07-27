package action

import (
	"context"
	"fmt"
	"io"
	"os"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Sync(resp rpc.Daemon_SyncServer) (err error) {
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
	var connectionID string
	err = util.CopyAndConvertGRPCStream[rpc.ConnectResponse, rpc.SyncResponse](
		connResp,
		resp,
		func(r *rpc.ConnectResponse) *rpc.SyncResponse {
			if r.ConnectionID != "" {
				connectionID = r.ConnectionID
			}
			_, _ = svr.LogFile.Write([]byte(r.Message))
			return &rpc.SyncResponse{Message: r.Message}
		})
	if err != nil {
		return err
	}

	options := &handler.SyncOptions{
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
	logger.Infof("Sync workloads...")
	options.SetContext(sshCtx)
	err = options.DoClone(plog.WithLogger(sshCtx, logger), []byte(req.KubeconfigBytes), req.Image)
	if err != nil {
		plog.G(context.Background()).Errorf("Sync workloads failed: %v", err)
		return err
	}
	var opt *handler.ConnectOptions
	for _, connection := range svr.connections {
		if connection.GetConnectionID() == connectionID {
			opt = connection
			break
		}
	}
	if opt == nil {
		return fmt.Errorf("cluster %s not found", connectionID)
	}
	opt.Sync = options
	return nil
}

type cloneWarp struct {
	server rpc.Daemon_SyncServer
}

func (r *cloneWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.SyncResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newCloneWarp(server rpc.Daemon_SyncServer) io.Writer {
	return &cloneWarp{server: server}
}
