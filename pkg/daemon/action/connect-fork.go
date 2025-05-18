package action

import (
	"context"
	"io"
	"os"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) ConnectFork(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectForkServer) (err error) {
	logger := plog.GetLoggerForClient(req.Level, io.MultiWriter(newConnectForkWarp(resp), svr.LogFile))
	if !svr.IsSudo {
		return svr.redirectConnectForkToSudoDaemon(req, resp, logger)
	}

	ctx := resp.Context()
	connect := &handler.ConnectOptions{
		Namespace:            req.ManagerNamespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		OriginNamespace:      req.Namespace,
		Lock:                 &svr.Lock,
		ImagePullSecretName:  req.ImagePullSecretName,
	}
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	sshCtx, sshCancel := context.WithCancel(context.Background())
	connect.AddRolloutFunc(func() error {
		sshCancel()
		os.Remove(file)
		return nil
	})
	sshCtx = plog.WithLogger(sshCtx, logger)
	defer plog.WithoutLogger(sshCtx)
	defer func() {
		if err != nil {
			connect.Cleanup(plog.WithLogger(context.Background(), logger))
			sshCancel()
		}
	}()

	err = connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
	if err != nil {
		return err
	}
	err = connect.GetIPFromContext(ctx, logger)
	if err != nil {
		return err
	}

	config.Image = req.Image
	err = connect.DoConnect(sshCtx, true, ctx.Done())
	if err != nil {
		logger.Errorf("Failed to connect...")
		return err
	}

	if resp.Context().Err() != nil {
		return resp.Context().Err()
	}
	svr.secondaryConnect = append(svr.secondaryConnect, connect)
	return nil
}

func (svr *Server) redirectConnectForkToSudoDaemon(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer, logger *log.Logger) (err error) {
	cli, err := svr.GetClient(true)
	if err != nil {
		return errors.Wrap(err, "sudo daemon not start")
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
	sshCtx = plog.WithLogger(sshCtx, logger)
	defer plog.WithoutLogger(sshCtx)
	connect := &handler.ConnectOptions{
		Namespace:            req.Namespace,
		OriginNamespace:      req.Namespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
	}
	connect.AddRolloutFunc(func() error {
		sshCancel()
		os.Remove(file)
		return nil
	})
	defer func() {
		if err != nil {
			connect.Cleanup(plog.WithLogger(context.Background(), logger))
			sshCancel()
		}
	}()
	var path string
	path, err = ssh.SshJump(sshCtx, sshConf, flags, true)
	if err != nil {
		return err
	}
	connect.AddRolloutFunc(func() error {
		os.Remove(path)
		return nil
	})
	err = connect.InitClient(util.InitFactoryByPath(path, req.Namespace))
	if err != nil {
		return err
	}

	if req.ManagerNamespace == "" {
		req.ManagerNamespace, err = util.DetectManagerNamespace(plog.WithLogger(sshCtx, logger), connect.GetFactory(), req.Namespace)
		if err != nil {
			return err
		}
	}
	if req.ManagerNamespace != "" {
		logger.Infof("Use manager namespace %s", req.ManagerNamespace)
		connect.Namespace = req.ManagerNamespace
	} else {
		logger.Infof("Use special namespace %s", req.Namespace)
	}

	for _, options := range svr.secondaryConnect {
		isSameCluster, _ := util.IsSameCluster(
			sshCtx,
			options.GetClientset().CoreV1(), options.Namespace,
			connect.GetClientset().CoreV1(), connect.Namespace,
		)
		if isSameCluster {
			sshCancel()
			os.Remove(file)
			os.Remove(path)
			// same cluster, do nothing
			logger.Infof("Connected with cluster")
			return nil
		}
	}

	ctx, err := connect.RentIP(resp.Context())
	if err != nil {
		return err
	}

	// only ssh jump in user daemon
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	req.KubeconfigBytes = string(content)
	req.SshJump = ssh.SshConfig{}.ToRPC()
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
