package action

import (
	"io"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Reset(req *rpc.ResetRequest, resp rpc.Daemon_ResetServer) error {
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newResetWarp(resp), svr.LogFile))
	connect := &handler.ConnectOptions{
		Namespace: req.Namespace,
	}

	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	flags.AddFlag(&pflag.Flag{
		Name:     "kubeconfig",
		DefValue: file,
	})
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)
	var ctx = plog.WithLogger(resp.Context(), logger)
	var path string
	path, err = ssh.SshJump(ctx, sshConf, flags, false)
	if err != nil {
		return err
	}
	err = connect.InitClient(util.InitFactoryByPath(path, req.Namespace))
	if err != nil {
		return err
	}
	err = connect.Reset(ctx, req.Workloads)
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
