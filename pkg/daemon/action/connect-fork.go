package action

import (
	"context"
	"fmt"
	"io"
	defaultlog "log"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) ConnectFork(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectForkServer) (err error) {
	defer func() {
		util.InitLoggerForServer(true)
		log.SetOutput(svr.LogFile)
		config.Debug = false
	}()
	config.Debug = req.Level == int32(log.DebugLevel)
	out := io.MultiWriter(newConnectForkWarp(resp), svr.LogFile)
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(out)
	if !svr.IsSudo {
		return svr.redirectConnectForkToSudoDaemon(req, resp)
	}

	ctx := resp.Context()
	connect := &handler.ConnectOptions{
		Namespace:            req.Namespace,
		Headers:              req.Headers,
		Workloads:            req.Workloads,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		Lock:                 &svr.Lock,
	}
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)
	var transferImage = req.TransferImage

	defaultlog.Default().SetOutput(io.Discard)
	if transferImage {
		err = ssh.TransferImage(ctx, sshConf, config.OriginImage, req.Image, out)
		if err != nil {
			return err
		}
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

	sshCtx, sshCancel := context.WithCancel(context.Background())
	connect.AddRolloutFunc(func() error {
		sshCancel()
		return nil
	})
	defer func() {
		if err != nil {
			connect.Cleanup()
			sshCancel()
		}
	}()

	var path string
	path, err = ssh.SshJump(sshCtx, sshConf, flags, false)
	if err != nil {
		return err
	}
	err = connect.InitClient(util.InitFactoryByPath(path, req.Namespace))
	if err != nil {
		return err
	}
	err = connect.PreCheckResource()
	if err != nil {
		return err
	}
	err = connect.GetIPFromContext(ctx)
	if err != nil {
		return err
	}

	config.Image = req.Image
	err = connect.DoConnect(sshCtx, true)
	if err != nil {
		log.Errorf("Do connect error: %v", err)
		return err
	}

	if resp.Context().Err() != nil {
		return resp.Context().Err()
	}
	svr.secondaryConnect = append(svr.secondaryConnect, connect)
	return nil
}

func (svr *Server) redirectConnectForkToSudoDaemon(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer) (err error) {
	cli := svr.GetClient(true)
	if cli == nil {
		return fmt.Errorf("sudo daemon not start")
	}
	connect := &handler.ConnectOptions{
		Namespace:            req.Namespace,
		Headers:              req.Headers,
		Workloads:            req.Workloads,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
	}
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	flags.AddFlag(&pflag.Flag{
		Name:     "kubeconfig",
		DefValue: file,
	})
	sshCtx, sshCancel := context.WithCancel(context.Background())
	connect.AddRolloutFunc(func() error {
		sshCancel()
		return nil
	})
	defer func() {
		if err != nil {
			sshCancel()
		}
	}()
	var path string
	path, err = ssh.SshJump(sshCtx, sshConf, flags, true)
	if err != nil {
		return err
	}
	err = connect.InitClient(util.InitFactoryByPath(path, req.Namespace))
	if err != nil {
		return err
	}
	err = connect.PreCheckResource()
	if err != nil {
		return err
	}

	for _, options := range svr.secondaryConnect {
		var isSameCluster bool
		isSameCluster, err = util.IsSameCluster(
			options.GetClientset().CoreV1().ConfigMaps(options.Namespace), options.Namespace,
			connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace), connect.Namespace,
		)
		if err == nil && isSameCluster && options.Equal(connect) {
			// same cluster, do nothing
			log.Infof("Connected with cluster")
			return nil
		}
	}

	ctx, err := connect.RentIP(resp.Context())
	if err != nil {
		return err
	}

	connResp, err := cli.ConnectFork(ctx, req)
	if err != nil {
		return err
	}
	err = util.CopyGRPCStream[rpc.ConnectResponse](connResp, resp)
	if err != nil {
		return err
	}

	if resp.Context().Err() != nil {
		return resp.Context().Err()
	}
	svr.secondaryConnect = append(svr.secondaryConnect, connect)
	return nil
}

type connectForkWarp struct {
	server rpc.Daemon_ConnectServer
}

func (r *connectForkWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.ConnectResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newConnectForkWarp(server rpc.Daemon_ConnectForkServer) io.Writer {
	return &connectForkWarp{server: server}
}
