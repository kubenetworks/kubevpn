package action

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Connect(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer) (e error) {
	logger := plog.GetLoggerForClient(req.Level, io.MultiWriter(newWarp(resp), svr.LogFile))
	if !svr.IsSudo {
		return svr.redirectToSudoDaemon(req, resp, logger)
	}

	ctx := resp.Context()
	if !svr.t.IsZero() {
		s := "Only support one cluster connect with full mode, you can use options `--lite` to connect to another cluster"
		return status.Error(codes.AlreadyExists, s)
	}
	defer func() {
		if e != nil || ctx.Err() != nil {
			if svr.connect != nil {
				svr.connect.Cleanup(plog.WithLogger(context.Background(), logger))
				svr.connect = nil
			}
			svr.t = time.Time{}
		}
	}()
	svr.t = time.Now()
	svr.connect = &handler.ConnectOptions{
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
	svr.connect.AddRolloutFunc(func() error {
		sshCancel()
		os.Remove(file)
		return nil
	})
	sshCtx = plog.WithLogger(sshCtx, logger)
	defer plog.WithoutLogger(sshCtx)
	defer func() {
		if e != nil {
			svr.connect.Cleanup(sshCtx)
			svr.connect = nil
			svr.t = time.Time{}
			os.Remove(file)
			sshCancel()
		}
	}()
	err = svr.connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
	if err != nil {
		return err
	}
	err = svr.connect.GetIPFromContext(ctx, nil)
	if err != nil {
		return err
	}

	config.Image = req.Image
	err = svr.connect.DoConnect(sshCtx, false, ctx.Done())
	if err != nil {
		logger.Errorf("Failed to connect...")
		return err
	}
	return nil
}

func (svr *Server) redirectToSudoDaemon(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer, logger *log.Logger) (e error) {
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
		if e != nil {
			connect.Cleanup(plog.WithLogger(context.Background(), logger))
			sshCancel()
			os.Remove(file)
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

	if svr.connect != nil {
		isSameCluster, _ := util.IsSameCluster(
			sshCtx,
			svr.connect.GetClientset().CoreV1(), svr.connect.Namespace,
			connect.GetClientset().CoreV1(), connect.Namespace,
		)
		if isSameCluster {
			sshCancel()
			os.Remove(path)
			os.Remove(file)
			// same cluster, do nothing
			logger.Infof("Connected to cluster")
			return nil
		} else {
			s := "Only support one cluster connect with full mode, you can use options `--lite` to connect to another cluster"
			return status.Error(codes.AlreadyExists, s)
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
	connResp, err := cli.Connect(ctx, req)
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
	svr.t = time.Now()
	svr.connect = connect

	return nil
}

type warp struct {
	server rpc.Daemon_ConnectServer
}

func (r *warp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.ConnectResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newWarp(server rpc.Daemon_ConnectServer) io.Writer {
	return &warp{server: server}
}
