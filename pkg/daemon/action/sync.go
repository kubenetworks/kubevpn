package action

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
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
	logger := newServerStreamLogger(svr.LogFile, req.Level, func(msg string) error {
		return resp.Send(&rpc.SyncResponse{Message: msg})
	})

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
	var connRespMu sync.Mutex
	session := NewSessionLifecycle(logger)
	go func() {
		var s rpc.Cancel
		if recvErr := resp.RecvMsg(&s); recvErr != nil {
			return
		}
		connRespMu.Lock()
		cr := connResp
		connRespMu.Unlock()
		if cr != nil {
			_ = cr.SendMsg(&s)
		}
		session.Cancel()
	}()

	cr, connErr := cli.Connect(context.Background())
	if connErr != nil {
		return connErr
	}
	connRespMu.Lock()
	connResp = cr
	connRespMu.Unlock()
	err = connResp.Send(connReq)
	if err != nil {
		return err
	}
	var connectionID string
	err = grpcutil.CopyAndConvertGRPCStream[rpc.ConnectResponse, rpc.SyncResponse](
		connResp,
		resp,
		func(r *rpc.ConnectResponse) *rpc.SyncResponse {
			if r.ConnectionID != "" {
				connectionID = r.ConnectionID
			}
			// Forward the message (with any step sentinel) to the CLI for spinner
			// rendering, but strip the sentinel from the log-file copy.
			_, fileMsg := plog.DecodeStep(r.Message)
			_, _ = svr.LogFile.Write([]byte(fileMsg))
			return &rpc.SyncResponse{Message: r.Message}
		})
	if err != nil {
		return err
	}

	options := &handler.SyncOptions{
		WorkloadNamespace:    req.Namespace,
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
	// The inner Connect above already resolved the traffic manager namespace
	// (detectAndSetManagerNamespace). Thread that authoritative value into the
	// sync options so the cloned workload's nested `kubevpn proxy` pins
	// --manager-namespace instead of re-detecting it (which falls back to the
	// workload namespace when manager != workload).
	svr.connMu.RLock()
	if conn, _ := svr.findConnection(connectionID); conn != nil {
		options.ManagerNamespace = conn.GetManagerNamespace()
	}
	svr.connMu.RUnlock()

	logger.Debugf("Syncing workloads")
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
	svr.connMu.RLock()
	opt, _ := svr.findConnection(connectionID)
	svr.connMu.RUnlock()
	if opt == nil {
		return fmt.Errorf("cluster %s not found: %w", connectionID, config.ErrConnectionNotFound)
	}
	opt.SetSync(options)
	return nil
}
