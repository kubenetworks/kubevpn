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

	// Idempotency guard (root daemon): if a data-plane session already exists for this
	// ConnectionID, short-circuit instead of building a second one. This is hit when the
	// user daemon restarts and LoadFromConfig replays Connect while this (surviving) root
	// daemon still holds the connection — without the guard we would create a duplicate
	// DataSession, a second TUN/port-forward/route/DNS setup, and leak. Mirrors the
	// user-side guard in redirectConnectToSudoDaemon. ConnectionID is always forwarded by
	// the user daemon, so an empty value only happens in tests / direct callers.
	if req.ConnectionID != "" {
		svr.connMu.RLock()
		existing, _ := svr.findConnection(req.ConnectionID)
		svr.connMu.RUnlock()
		if existing != nil {
			plog.G(ctx).Infof("Data plane already established for connection %s", req.ConnectionID)
			return resp.Send(&rpc.ConnectResponse{ConnectionID: req.ConnectionID})
		}
	}

	// RequestRaw / proto.Marshal(req) is intentionally NOT done here: it is a control-plane
	// persistence field (user daemon only). The root daemon's DataSession is never persisted.
	ds := &handler.DataSession{
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

	// Serialize the allocation phase: two concurrent connects must not race their
	// TUN IP allocation with empty sibling snapshots.
	svr.connectMu.Lock()
	err = ds.DoConnect(session.Ctx)
	svr.connectMu.Unlock()
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
