package action

import (
	"io"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Reset handles the Reset RPC, restoring the specified workloads to their original state before proxy injection.
func (svr *Server) Reset(resp rpc.Daemon_ResetServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newStreamWriter(func(msg string) error {
		return resp.Send(&rpc.ResetResponse{Message: msg})
	}), svr.LogFile))

	var ctx = plog.WithLogger(resp.Context(), logger)
	file, err := resolveKubeconfig(ctx, req.SshJump, req.KubeconfigBytes, false)
	if err != nil {
		return err
	}
	defer os.Remove(file)
	connect := &handler.ConnectOptions{}
	err = connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
	if err != nil {
		return err
	}
	connect.ManagerNamespace, err = util.DetectManagerNamespace(ctx, connect.GetFactory(), req.Namespace)
	if err != nil {
		return err
	}
	return connect.Reset(ctx, req.Namespace, req.Workloads)
}

