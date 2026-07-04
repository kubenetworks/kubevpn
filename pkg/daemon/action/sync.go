package action

import (
	"context"
	"fmt"
	"io"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Sync handles the bidirectional file sync RPC, connecting to the cluster and synchronizing local directories with remote workloads.
func (svr *Server) Sync(resp rpc.Daemon_SyncServer) (err error) {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger := plog.GetLoggerForClient(req.Level, io.MultiWriter(newStreamWriter(func(msg string) error {
		return resp.Send(&rpc.SyncResponse{Message: msg})
	}), svr.LogFile))

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
	session := NewSessionLifecycle(logger)
	go func() {
		var s rpc.Cancel
		err = resp.RecvMsg(&s)
		if err != nil {
			return
		}
		if connResp != nil {
			_ = connResp.SendMsg(&s)
		}
		session.Cancel()
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
	defer func() {
		if err != nil {
			_ = options.Cleanup(session.Ctx)
			session.Cancel()
		}
	}()
	var file string
	session.AddTempFile(&file)
	options.AddRollbackFunc(func() error {
		session.RunCleanups()
		return nil
	})
	file, err = resolveKubeconfig(session.Ctx, req.SshJump, req.KubeconfigBytes, false)
	if err != nil {
		return err
	}
	f := util.InitFactoryByPath(file, req.Namespace)
	err = options.InitClient(f)
	if err != nil {
		plog.G(resp.Context()).Errorf("Failed to init client: %v", err)
		return err
	}
	logger.Infof("Sync workloads...")
	options.SetContext(session.Ctx)
	newKubeconfigBytes, err := options.ConvertApiServerToNodeIP(resp.Context(), []byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	err = options.DoSync(plog.WithLogger(session.Ctx, logger), newKubeconfigBytes, req.Image)
	if err != nil {
		plog.G(resp.Context()).Errorf("Sync workloads failed: %v", err)
		return err
	}
	opt, _ := svr.findConnection(connectionID)
	if opt == nil {
		return fmt.Errorf("cluster %s not found", connectionID)
	}
	opt.Sync = options
	return nil
}

