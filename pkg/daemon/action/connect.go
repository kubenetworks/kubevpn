package action

import (
	"context"
	"fmt"
	"io"
	defaultlog "log"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Connect(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer) (e error) {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	config.Debug = req.Level == int32(log.DebugLevel)
	out := io.MultiWriter(newWarp(resp), svr.LogFile)
	log.SetOutput(out)
	log.SetLevel(log.InfoLevel)
	if !svr.IsSudo {
		return svr.redirectToSudoDaemon(req, resp)
	}

	ctx := resp.Context()
	if !svr.t.IsZero() {
		s := "already connected to one cluster, you can use options `--lite` to connect to another cluster"
		log.Debugf(s)
		// todo define already connect error?
		return status.Error(codes.AlreadyExists, s)
	}
	defer func() {
		if e != nil || ctx.Err() != nil {
			if svr.connect != nil {
				svr.connect.Cleanup()
				svr.connect = nil
			}
			svr.t = time.Time{}
		}
	}()
	svr.t = time.Now()
	svr.connect = &handler.ConnectOptions{
		Namespace:            req.Namespace,
		Headers:              req.Headers,
		PortMap:              req.PortMap,
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
		err := util.TransferImage(ctx, sshConf, config.OriginImage, req.Image, out)
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
	svr.connect.AddRolloutFunc(func() error {
		sshCancel()
		return nil
	})
	defer func() {
		if e != nil {
			sshCancel()
		}
	}()
	var path string
	path, err = util.SshJump(sshCtx, sshConf, flags, false)
	if err != nil {
		return err
	}
	err = svr.connect.InitClient(util.InitFactoryByPath(path, req.Namespace))
	if err != nil {
		return err
	}
	err = svr.connect.PreCheckResource()
	if err != nil {
		return err
	}
	_, err = svr.connect.RentInnerIP(ctx)
	if err != nil {
		return err
	}

	config.Image = req.Image
	err = svr.connect.DoConnect(sshCtx, false)
	if err != nil {
		log.Errorf("do connect error: %v", err)
		svr.connect.Cleanup()
		svr.connect = nil
		svr.t = time.Time{}
		return err
	}
	return nil
}

func (svr *Server) redirectToSudoDaemon(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer) (e error) {
	cli := svr.GetClient(true)
	if cli == nil {
		return fmt.Errorf("sudo daemon not start")
	}
	connect := &handler.ConnectOptions{
		Namespace:            req.Namespace,
		Headers:              req.Headers,
		PortMap:              req.PortMap,
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
		if e != nil {
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

	if svr.connect != nil {
		var isSameCluster bool
		isSameCluster, err = util.IsSameCluster(
			svr.connect.GetClientset().CoreV1().ConfigMaps(svr.connect.Namespace), svr.connect.Namespace,
			connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace), connect.Namespace,
		)
		if err == nil && isSameCluster && svr.connect.Equal(connect) {
			// same cluster, do nothing
			log.Infof("already connect to cluster")
			return nil
		}
	}

	ctx, err := connect.RentInnerIP(resp.Context())
	if err != nil {
		return err
	}

	connResp, err := cli.Connect(ctx, req)
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
