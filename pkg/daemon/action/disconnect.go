package action

import (
	"context"
	"io"
	"os"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

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

	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newDisconnectWarp(resp), svr.LogFile))
	ctx := plog.WithLogger(resp.Context(), logger)

	// disconnect sudo daemon first
	// then disconnect from user daemon
	// because only ssh jump in user daemon
	if !svr.IsSudo {
		cli, err := svr.GetClient(true)
		if err != nil {
			return errors.Wrap(err, "sudo daemon not start")
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
			if connect != nil {
				if connect.Sync != nil {
					_ = connect.Sync.Cleanup(ctx)
				}
				connect.Cleanup(ctx)
			}
		}
		svr.connections = nil
		svr.currentConnectionID = ""
	case req.GetConnectionID() != "":
		var connects = *new(handler.Connects)
		for i := 0; i < len(svr.connections); i++ {
			if req.GetConnectionID() == svr.connections[i].GetConnectionID() {
				connects = connects.Append(svr.connections[i])
				svr.connections = append(svr.connections[:i], svr.connections[i+1:]...)
				i--
			}
		}
		for _, connect := range connects.Sort() {
			if connect != nil {
				if connect.Sync != nil {
					_ = connect.Sync.Cleanup(ctx)
				}
				connect.Cleanup(ctx)
			}
		}
		if svr.currentConnectionID == req.GetConnectionID() {
			for _, connection := range svr.connections {
				svr.currentConnectionID = connection.GetConnectionID()
			}
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

	if len(svr.connections) == 0 {
		if svr.IsSudo {
			_ = dns.CleanupHosts()
		}
	}
	return nil
}

func disconnectByKubeconfig(ctx context.Context, svr *Server, kubeconfigBytes string, ns string, jump *rpc.SshJump) error {
	file, err := util.ConvertToTempKubeconfigFile([]byte(kubeconfigBytes))
	if err != nil {
		return err
	}
	defer os.Remove(file)
	var sshConf = ssh.ParseSshFromRPC(jump)
	if !sshConf.IsEmpty() {
		file, err = ssh.SshJump(ctx, sshConf, file, false)
		if err != nil {
			return err
		}
	}
	connect := &handler.ConnectOptions{}
	err = connect.InitClient(util.InitFactoryByPath(file, ns))
	if err != nil {
		return err
	}
	connectionID, err := util.GetConnectionID(ctx, connect.GetClientset().CoreV1().Namespaces(), connect.Namespace)
	if err != nil {
		return err
	}
	disconnect(ctx, svr, connectionID)
	if svr.currentConnectionID == connectionID {
		for _, connection := range svr.connections {
			svr.currentConnectionID = connection.GetConnectionID()
		}
	}
	return nil
}

func disconnect(ctx context.Context, svr *Server, connectionID string) {
	for i := 0; i < len(svr.connections); i++ {
		options := svr.connections[i]
		id, _ := util.GetConnectionID(ctx, options.GetClientset().CoreV1().Namespaces(), options.OriginNamespace)
		if id == connectionID {
			plog.G(ctx).Infof("Disconnecting from the cluster...")
			options.Cleanup(ctx)
			svr.connections = append(svr.connections[:i], svr.connections[i+1:]...)
			i--
		}
	}
}

type disconnectWarp struct {
	server rpc.Daemon_DisconnectServer
}

func (r *disconnectWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.DisconnectResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newDisconnectWarp(server rpc.Daemon_DisconnectServer) io.Writer {
	return &disconnectWarp{server: server}
}
