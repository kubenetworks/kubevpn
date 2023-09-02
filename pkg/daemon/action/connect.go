package action

import (
	"context"
	"io"
	defaultlog "log"
	"os"
	"runtime"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/dev"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

type warp struct {
	server rpc.Daemon_ConnectServer
}

func (r *warp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.ConnectResponse{
		Message: string(p),
	})
	return len(p), err
}

func newWarp(server rpc.Daemon_ConnectServer) io.Writer {
	return &warp{server: server}
}

func InitFactory(kubeconfig string, ns string) cmdutil.Factory {
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.WrapConfigFn = func(c *rest.Config) *rest.Config {
		if path, ok := os.LookupEnv(config.EnvSSHJump); ok {
			kubeconfigBytes, err := os.ReadFile(path)
			cmdutil.CheckErr(err)
			var conf *restclient.Config
			conf, err = clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
			cmdutil.CheckErr(err)
			return conf
		}
		return c
	}
	configFlags.KubeConfig = pointer.String(kubeconfig)
	configFlags.Namespace = pointer.String(ns)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	return cmdutil.NewFactory(matchVersionFlags)
}

func (svr *Server) Connect(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer) error {
	out := newWarp(resp)
	log.SetOutput(out)
	svr.timestamp = time.Now()
	var connect = &handler.ConnectOptions{
		Namespace:   req.Namespace,
		Headers:     req.Headers,
		Workloads:   req.Workloads,
		ExtraCIDR:   req.ExtraCIDR,
		ExtraDomain: req.ExtraDomain,
		UseLocalDNS: req.UseLocalDNS,
		Engine:      config.Engine(req.Engine),
	}
	var sshConf = &util.SshConfig{
		Addr:             req.Addr,
		User:             req.User,
		Password:         req.Password,
		Keyfile:          req.Keyfile,
		ConfigAlias:      req.ConfigAlias,
		RemoteKubeconfig: req.RemoteKubeconfig,
	}
	var transferImage = req.TransferImage

	go util.StartupPProf(config.PProfPort)
	util.InitLogger(config.Debug)
	defaultlog.Default().SetOutput(io.Discard)
	if transferImage {
		if err := dev.TransferImage(context.Background(), sshConf); err != nil {
			return err
		}
	}
	err := handler.SshJump(sshConf, nil)
	if err != nil {
		return err
	}
	runtime.GOMAXPROCS(0)
	err = connect.InitClient(InitFactory(req.Kubeconfig, req.Namespace))
	if err != nil {
		return err
	}
	if err = connect.DoConnect(); err != nil {
		log.Errorln(err)
		handler.Cleanup(syscall.SIGQUIT)
	} else {
		util.Print(out, "Now you can access resources in the kubernetes cluster, enjoy it :)")
	}
	return nil
}
