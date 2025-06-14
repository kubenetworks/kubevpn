package action

import (
	"io"
	"os"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Reset(resp rpc.Daemon_ResetServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newResetWarp(resp), svr.LogFile))

	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	defer os.Remove(file)
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)
	var ctx = plog.WithLogger(resp.Context(), logger)
	if !sshConf.IsEmpty() {
		file, err = ssh.SshJump(ctx, sshConf, file, false)
		if err != nil {
			return err
		}
	}
	connect := &handler.ConnectOptions{}
	err = connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
	if err != nil {
		return err
	}
	connect.Namespace, err = util.DetectManagerNamespace(ctx, connect.GetFactory(), req.Namespace)
	if err != nil {
		return err
	}
	err = connect.Reset(ctx, req.Namespace, req.Workloads)
	if err != nil {
		return err
	}
	return nil
}

type resetWarp struct {
	server rpc.Daemon_ResetServer
}

func (r *resetWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.ResetResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newResetWarp(server rpc.Daemon_ResetServer) io.Writer {
	return &resetWarp{server: server}
}
