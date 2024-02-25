package action

import (
	"io"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Reset(req *rpc.ResetRequest, resp rpc.Daemon_ResetServer) error {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	out := io.MultiWriter(newResetWarp(resp), svr.LogFile)
	log.SetOutput(out)
	log.SetLevel(log.InfoLevel)

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
	var sshConf = util.ParseSshFromRPC(req.SshJump)
	var ctx = resp.Context()
	var path string
	path, err = handler.SshJump(ctx, sshConf, flags, false)
	if err != nil {
		return err
	}
	err = connect.InitClient(InitFactoryByPath(path, req.Namespace))
	if err != nil {
		return err
	}
	err = connect.Reset(ctx)
	if err != nil {
		return err
	}
	log.Debugf("Done")
	return nil
}

type resetWarp struct {
	server rpc.Daemon_ResetServer
}

func (r *resetWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.ResetResponse{
		Message: string(p),
	})
	return len(p), err
}

func newResetWarp(server rpc.Daemon_ResetServer) io.Writer {
	return &resetWarp{server: server}
}
