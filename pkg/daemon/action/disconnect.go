package action

import (
	"context"
	"fmt"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Disconnect handles the Disconnect RPC, tearing down VPN tunnel(s).
func (svr *Server) Disconnect(resp rpc.Daemon_DisconnectServer) (err error) {
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
		return resp.Send(&rpc.DisconnectResponse{Message: msg})
	})
	ctx := plog.WithLogger(resp.Context(), logger)
	if id := req.GetConnectionID(); id != "" {
		ctx = plog.WithField(ctx, LogFieldConnID, id)
	}

	// Order matters. Server-side leave-all removes proxy sidecars/rules by asking the
	// traffic manager over the VPN (see LeaveAllProxyResources), so it must run while the
	// data plane is still up — i.e. BEFORE the sudo daemon drops the TUN below. The sudo
	// daemon is disconnected first (only the user daemon holds the ssh jump); the user
	// daemon's own connection cleanup (cleanupConnection) runs last and re-attempts leave
	// as a harmless idempotent no-op.
	if !svr.IsSudo {
		svr.leaveProxiesBeforeTeardown(ctx, req)

		cli, err := svr.GetClient(true)
		if err != nil {
			return fmt.Errorf("sudo daemon not start: %w", err)
		}
		connResp, err := cli.Disconnect(resp.Context())
		if err != nil {
			return err
		}
		err = connResp.Send(req)
		if err != nil {
			return err
		}
		err = grpcutil.CopyGRPCStream[rpc.DisconnectResponse](connResp, resp)
		if err != nil {
			return err
		}
	}

	// Emit the progress step only from the user daemon (the orchestrator). The
	// sudo daemon's stream is copied verbatim above, so guarding on !IsSudo keeps
	// the CLI from rendering the step twice. On an early error return the step is
	// left unfinished, so the renderer marks it failed (✗).
	if !svr.IsSudo {
		plog.StepStart(ctx, "Disconnecting")
	}

	switch {
	case req.GetAll():
		svr.connMu.Lock()
		connects := handler.Connects(svr.connections)
		svr.connections = nil
		svr.currentConnectionID = ""
		svr.connMu.Unlock()
		for _, connect := range connects.Sort() {
			cleanupConnection(ctx, connect)
		}
	case req.GetConnectionID() != "":
		svr.connMu.Lock()
		removed := svr.removeConnection(req.GetConnectionID())
		svr.resetCurrentConnection(req.GetConnectionID())
		svr.connMu.Unlock()
		for _, connect := range removed.Sort() {
			cleanupConnection(ctx, connect)
		}
	case req.KubeconfigBytes != nil && req.Namespace != nil:
		err = disconnectByKubeconfig(
			resp.Context(),
			svr,
			req.GetKubeconfigBytes(),
			req.GetNamespace(),
			req.GetSshJump(),
		)
		if err != nil {
			return err
		}
	}

	if !svr.IsSudo {
		plog.StepDone(ctx, "Disconnected from the cluster")
	}

	svr.connMu.RLock()
	empty := len(svr.connections) == 0
	svr.connMu.RUnlock()
	if empty {
		if svr.IsSudo {
			_ = dns.CleanupHosts()
		}
	}
	return nil
}

func disconnectByKubeconfig(ctx context.Context, svr *Server, kubeconfigBytes string, ns string, jump *rpc.SshJump) error {
	connectionID, err := resolveConnectionIDByKubeconfig(ctx, jump, kubeconfigBytes, ns)
	if err != nil {
		return err
	}
	disconnect(ctx, svr, connectionID)
	svr.connMu.Lock()
	svr.resetCurrentConnection(connectionID)
	svr.connMu.Unlock()
	return nil
}

// resolveConnectionIDByKubeconfig derives the stored connection ID for a kubeconfig +
// namespace. Connections are keyed by the ID derived from the *manager* namespace (see
// connect_elevate.go detectAndSetManagerNamespace + GetConnectionID), which may differ
// from the workload namespace, so it is resolved the same way — otherwise the lookup
// misses and the proxy/VPN state leaks when the manager lives in its own namespace.
func resolveConnectionIDByKubeconfig(ctx context.Context, jump *rpc.SshJump, kubeconfigBytes, ns string) (string, error) {
	resolvedBytes, err := resolveKubeconfigBytes(ctx, jump, kubeconfigBytes, false)
	if err != nil {
		return "", err
	}
	connect := &handler.ConnectOptions{}
	if err = connect.InitClient(util.InitFactoryByBytes(resolvedBytes, ns)); err != nil {
		return "", err
	}
	managerNamespace, err := util.DetectManagerNamespace(ctx, connect.GetFactory(), ns)
	if err != nil {
		return "", err
	}
	if managerNamespace == "" {
		managerNamespace = ns
	}
	return util.GetConnectionID(context.Background(), connect.GetClientset().CoreV1().Namespaces(), managerNamespace)
}

// leaveProxiesBeforeTeardown resolves the connections targeted by a Disconnect request
// and asks the traffic manager to remove their proxy sidecars/rules while the VPN data
// plane is still up. Server-side leave reaches the manager over the VPN, so it MUST run
// before the sudo daemon drops the TUN. User daemon only (DataSession's leave is a no-op);
// best-effort — errors are logged and teardown proceeds (the manager's lease reaper
// reclaims any rule left behind).
func (svr *Server) leaveProxiesBeforeTeardown(ctx context.Context, req *rpc.DisconnectRequest) {
	var targets handler.Connects
	switch {
	case req.GetAll():
		svr.connMu.RLock()
		targets = append(targets, svr.connections...)
		svr.connMu.RUnlock()
	case req.GetConnectionID() != "":
		svr.connMu.RLock()
		if c, i := svr.findConnection(req.GetConnectionID()); i != -1 {
			targets = targets.Append(c)
		}
		svr.connMu.RUnlock()
	case req.KubeconfigBytes != nil && req.Namespace != nil:
		id, err := resolveConnectionIDByKubeconfig(ctx, req.GetSshJump(), req.GetKubeconfigBytes(), req.GetNamespace())
		if err != nil {
			plog.G(ctx).Warnf("Resolve connection before leaving proxies failed: %v", err)
			return
		}
		svr.connMu.RLock()
		if c, i := svr.findConnection(id); i != -1 {
			targets = targets.Append(c)
		}
		svr.connMu.RUnlock()
	}
	for _, conn := range targets {
		// LeaveAllProxyResources is control-plane-only (ProxyController). In the
		// root daemon connections are *DataSession (not ProxyController), so the
		// assertion fails and we skip — correct, the root daemon holds no proxy
		// resources. Mirrors the old data-plane no-op stub.
		if pc, ok := conn.(handler.ProxyController); ok {
			if err := pc.LeaveAllProxyResources(ctx); err != nil {
				plog.G(ctx).Warnf("Leave proxy resources before VPN teardown failed (lease reaper will reclaim): %v", err)
			}
		}
	}
}

func disconnect(ctx context.Context, svr *Server, connectionID string) {
	svr.connMu.Lock()
	removed := svr.removeConnection(connectionID)
	svr.connMu.Unlock()
	for _, conn := range removed {
		plog.G(ctx).Debugf("Disconnecting connection %q", connectionID)
		cleanupConnection(ctx, conn)
	}
}
