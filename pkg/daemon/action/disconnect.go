package action

import (
	"context"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"

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

	logger := newServerStreamLogger(svr.LogFile, int32(log.InfoLevel), func(msg string) error {
		return resp.Send(&rpc.DisconnectResponse{Message: msg})
	})
	ctx := plog.WithLogger(resp.Context(), logger)
	if id := req.GetConnectionID(); id != "" {
		ctx = plog.WithField(ctx, LogFieldConnID, id)
	}

	// disconnect sudo daemon first
	// then disconnect from user daemon
	// because only ssh jump in user daemon
	if !svr.IsSudo {
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
	file, err := resolveKubeconfig(ctx, jump, kubeconfigBytes, false)
	if err != nil {
		return err
	}
	defer os.Remove(file)
	connect := &handler.ConnectOptions{}
	err = connect.InitClient(util.InitFactoryByPath(file, ns))
	if err != nil {
		return err
	}
	connectionID, err := util.GetConnectionID(context.Background(), connect.GetClientset().CoreV1().Namespaces(), connect.ManagerNamespace)
	if err != nil {
		return err
	}
	disconnect(ctx, svr, connectionID)
	svr.connMu.Lock()
	svr.resetCurrentConnection(connectionID)
	svr.connMu.Unlock()
	return nil
}

func disconnect(ctx context.Context, svr *Server, connectionID string) {
	svr.connMu.Lock()
	removed := svr.removeConnection(connectionID)
	svr.connMu.Unlock()
	for _, conn := range removed {
		plog.G(ctx).Infof("Disconnecting from the cluster...")
		cleanupConnection(ctx, conn)
	}
}
