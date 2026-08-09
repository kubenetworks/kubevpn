package action

import (
	"context"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Connect handles the Connect RPC, establishing a VPN tunnel to the cluster.
func (svr *Server) Connect(resp rpc.Daemon_ConnectServer) (err error) {
	if !svr.IsSudo {
		defer func() {
			if err == nil {
				_ = svr.OffloadToConfig()
			}
		}()
	}

	req, err := resp.Recv()
	if err != nil {
		return err
	}

	logger := newServerStreamLogger(svr.LogFile, req.Level, func(msg string) error {
		return resp.Send(&rpc.ConnectResponse{Message: msg})
	})
	if !svr.IsSudo {
		return svr.redirectConnectToSudoDaemon(req, resp, logger)
	}

	// RequestRaw / proto.Marshal(req) is intentionally NOT done here: it is a control-plane
	// persistence field (user daemon only). The root daemon's DataSession is never persisted.
	ds := &handler.DataSession{
		ManagerNamespace:     req.ManagerNamespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		WorkloadNamespace:    req.Namespace,
		Lock:                 &svr.Lock,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		OwnerID:              req.OwnerID,
		ConnectionID:         req.ConnectionID,
		ReservedTunIPs:       svr.siblingTunIPs,
	}
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes), "")
	if err != nil {
		return err
	}
	session := NewSessionLifecycle(logger)
	session.AddTempFile(&file)
	ds.AddRollbackFunc(func() error {
		session.RunCleanups()
		return nil
	})
	go grpcutil.ListenCancel(resp, session.Cancel)
	defer func() {
		if err != nil {
			ds.Cleanup(plog.WithLogger(context.Background(), logger))
			session.Cancel()
		}
	}()

	err = ds.InitClient(util.InitFactoryByPath(file, req.ManagerNamespace))
	if err != nil {
		return err
	}
	// Tag all downstream logs with the connection ID so concurrent connects can
	// be told apart in the shared root daemon log file.
	session.Ctx = plog.WithField(session.Ctx, LogFieldConnID, ds.ConnectionID)

	// Serialize the allocation phase: two concurrent connects must not race their
	// TUN IP allocation with empty sibling snapshots.
	svr.connectMu.Lock()
	err = ds.DoConnect(session.Ctx)
	svr.connectMu.Unlock()
	if err != nil {
		logger.Errorf("Failed to connect...")
		return err
	}

	if resp.Context().Err() != nil {
		return resp.Context().Err()
	}
	svr.connMu.Lock()
	svr.connections = append(svr.connections, ds)
	svr.connMu.Unlock()
	return nil
}
