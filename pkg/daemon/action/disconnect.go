package action

import (
	"context"
	"fmt"
	"io"
	"os"

	log "github.com/sirupsen/logrus"

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

	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newStreamWriter(func(msg string) error {
		return resp.Send(&rpc.DisconnectResponse{Message: msg})
	}), svr.LogFile))
	ctx := plog.WithLogger(resp.Context(), logger)

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
		err = util.CopyGRPCStream[rpc.DisconnectResponse](connResp, resp)
		if err != nil {
			return err
		}
	}

	switch {
	case req.GetAll():
		connects := handler.Connects(svr.connections)
		for _, connect := range connects.Sort() {
			cleanupConnection(ctx, connect)
		}
		svr.connections = nil
		svr.currentConnectionID = ""
	case req.GetConnectionID() != "":
		for _, connect := range svr.removeConnection(req.GetConnectionID()).Sort() {
			cleanupConnection(ctx, connect)
		}
		svr.resetCurrentConnection(req.GetConnectionID())
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

	if len(svr.connections) == 0 {
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
	connectionID, err := util.GetConnectionID(ctx, connect.GetClientset().CoreV1().Namespaces(), connect.ManagerNamespace)
	if err != nil {
		return err
	}
	disconnect(ctx, svr, connectionID)
	svr.resetCurrentConnection(connectionID)
	return nil
}

func disconnect(ctx context.Context, svr *Server, connectionID string) {
	for _, conn := range svr.removeConnection(connectionID) {
		plog.G(ctx).Infof("Disconnecting from the cluster...")
		cleanupConnection(ctx, conn)
	}
}

