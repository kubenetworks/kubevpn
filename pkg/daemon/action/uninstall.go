package action

import (
	"io"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Uninstall(req *rpc.UninstallRequest, resp rpc.Daemon_UninstallServer) error {
	defer func() {
		util.InitLoggerForServer(true)
		log.SetOutput(svr.LogFile)
		config.Debug = false
	}()
	out := io.MultiWriter(newUninstallWarp(resp), svr.LogFile)
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(out)

	connect := &handler.ConnectOptions{
		Namespace: req.Namespace,
		Lock:      &svr.Lock,
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
	var ctx = resp.Context()
	var path string
	path, err = ssh.SshJump(ctx, sshConf, flags, false)
	if err != nil {
		return err
	}
	err = connect.InitClient(util.InitFactoryByPath(path, req.Namespace))
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
