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
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) ConnectFork(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectForkServer) (err error) {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	config.Debug = req.Level == int32(log.DebugLevel)
	out := io.MultiWriter(newConnectForkWarp(resp), svr.LogFile)
	log.SetOutput(out)
	log.SetLevel(log.InfoLevel)
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
	var sshConf = util.ParseSshFromRPC(req.SshJump)
	var transferImage = req.TransferImage

	defaultlog.Default().SetOutput(io.Discard)
	if transferImage {
		err = util.TransferImage(ctx, sshConf, config.OriginImage, req.Image, out)
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
	path, err = util.SshJump(sshCtx, sshConf, flags, false)
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
	_, err = connect.RentInnerIP(ctx)
	if err != nil {
		return err
	}

	config.Image = req.Image
	err = connect.DoConnect(sshCtx, true)
	if err != nil {
		log.Errorf("do connect error: %v", err)
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
	var sshConf = util.ParseSshFromRPC(req.SshJump)
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
	path, err = util.SshJump(sshCtx, sshConf, flags, true)
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
			log.Infof("already connect to cluster")
			return nil
		}
	}

	ctx, err := connect.RentInnerIP(resp.Context())
	if err != nil {
		return err
	}

	connResp, err := cli.ConnectFork(ctx, req)
	if err != nil {
		return err
	}
	for {
		recv, err := connResp.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		err = resp.Send(recv)
		if err != nil {
			return err
		}
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
