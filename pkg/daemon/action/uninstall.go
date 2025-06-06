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

func (svr *Server) Uninstall(resp rpc.Daemon_UninstallServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newUninstallWarp(resp), svr.LogFile))

	connect := &handler.ConnectOptions{
		Namespace: req.Namespace,
		Lock:      &svr.Lock,
	}

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
	err = connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
	if err != nil {
		return err
	}
	err = connect.Uninstall(ctx)
	if err != nil {
		return err
	}
	return nil
}

type uninstallWarp struct {
	server rpc.Daemon_UninstallServer
}

func (r *uninstallWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.UninstallResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newUninstallWarp(server rpc.Daemon_UninstallServer) io.Writer {
	return &uninstallWarp{server: server}
}
