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

	// Tag every root-daemon log for this connection with its ID from the moment the request
	// arrives (the user daemon always forwards it), so the whole lifecycle — idempotency guard,
	// setup, data plane, cleanup — is filterable by connID in the shared log file.
	ctx := plog.WithLogger(resp.Context(), logger)
	if req.ConnectionID != "" {
		ctx = plog.WithField(ctx, LogFieldConnID, req.ConnectionID)
	}

	// Serialize the decision, cleanup, allocation AND publication. Checking before
	// taking this lock lets concurrent requests both allocate for the same ID.
	svr.connectMu.Lock()
	defer svr.connectMu.Unlock()

	// Replays keep a live session, including one still reconnecting. A canceled
	// context is terminal and must be retired before reusing its ConnectionID.
	// Keep the nil-context fallback for tests / direct callers.
	if req.ConnectionID != "" {
		svr.connMu.RLock()
		existing, _ := svr.findConnection(req.ConnectionID)
		svr.connMu.RUnlock()
		if existing != nil {
			if existing.Context() == nil || existing.Context().Err() == nil {
				plog.G(ctx).Infof("Data plane already established for connection %s", req.ConnectionID)
				return resp.Send(&rpc.ConnectResponse{ConnectionID: req.ConnectionID})
			}
			svr.connMu.Lock()
			svr.removeConnectionInstance(existing)
			svr.connMu.Unlock()
			// Cleanup is a join barrier; never wait for network/DNS teardown with
			// connMu held, and never allocate the replacement before it finishes.
			existing.Cleanup(context.WithoutCancel(ctx))
		}
	}

	// RequestRaw / proto.Marshal(req) is intentionally NOT done here: it is a control-plane
	// persistence field (user daemon only). The root daemon's DataSession is never persisted.
	options := &handler.DataSession{
		ManagerNamespace:     req.ManagerNamespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		WorkloadNamespace:    req.Namespace,
		Lock:                 &svr.dnsMu,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		OwnerID:              req.OwnerID,
		ConnectionID:         req.ConnectionID,
		ReservedTunIPs:       svr.siblingTunIPs,
		SshConf:              parseSshFromRPC(req.SshJump),
	}
	var ds handler.DataPlane = options
	if svr.newDataSession != nil {
		ds = svr.newDataSession(options)
	}
	session := NewSessionLifecycle(logger)
	// Tag the data-plane session context with the connection ID up front so DoConnect and every
	// background goroutine it spawns (TUN, routes, DNS, per-packet) carry connID in the log file.
	if req.ConnectionID != "" {
		session.Ctx = plog.WithField(session.Ctx, LogFieldConnID, req.ConnectionID)
	}
	ds.AddRollbackFunc(func() error {
		session.Teardown()
		return nil
	})
	go grpcutil.ListenCancel(resp, session.Cancel)
	// Cleanup runs on a background context (must survive resp/session cancellation) but keeps the
	// connID tag so teardown logs stay filterable.
	cleanupCtx := plog.WithLogger(context.Background(), logger)
	if req.ConnectionID != "" {
		cleanupCtx = plog.WithField(cleanupCtx, LogFieldConnID, req.ConnectionID)
	}
	defer func() {
		if err != nil {
			ds.Cleanup(cleanupCtx)
			session.Cancel()
		}
	}()

	// Root daemon (data plane): the kubeconfig is consumed only by the in-process
	// kubectl Factory, so build it straight from bytes — no temp file to collide
	// with the user daemon's or to leak.
	err = ds.InitClient(util.InitFactoryByBytes([]byte(req.KubeconfigBytes), req.ManagerNamespace))
	if err != nil {
		return err
	}

	err = ds.DoConnect(session.Ctx)
	if err != nil {
		plog.G(ctx).Errorf("Failed to connect...")
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
