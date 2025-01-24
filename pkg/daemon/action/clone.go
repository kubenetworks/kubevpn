package action

import (
	"context"
	"io"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Clone(req *rpc.CloneRequest, resp rpc.Daemon_CloneServer) (err error) {
	defer func() {
		util.InitLoggerForServer(true)
		log.SetOutput(svr.LogFile)
		config.Debug = false
	}()
	config.Debug = req.Level == int32(log.DebugLevel)
	out := io.MultiWriter(newCloneWarp(resp), svr.LogFile)
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(out)
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)
	connReq := &rpc.ConnectRequest{
		KubeconfigBytes:      req.KubeconfigBytes,
		Namespace:            req.Namespace,
		ExtraRoute:           req.ExtraRoute,
		Engine:               req.Engine,
		SshJump:              req.SshJump,
		TransferImage:        req.TransferImage,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		Level:                req.Level,
		OriginKubeconfigPath: req.OriginKubeconfigPath,
	}
	cli := svr.GetClient(false)
	connResp, err := cli.Connect(resp.Context(), connReq)
	if err != nil {
		return err
	}
	err = util.PrintGRPCStream[rpc.ConnectResponse](connResp, out)
	if err != nil {
		return err
	}
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(out)

	options := &handler.CloneOptions{
		Namespace:            req.Namespace,
		Headers:              req.Headers,
		Workloads:            req.Workloads,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,

		TargetKubeconfig:       req.TargetKubeconfig,
		TargetNamespace:        req.TargetNamespace,
		TargetContainer:        req.TargetContainer,
		TargetImage:            req.TargetImage,
		TargetRegistry:         req.TargetRegistry,
		IsChangeTargetRegistry: req.IsChangeTargetRegistry,
		TargetWorkloadNames:    map[string]string{},
		LocalDir:               req.LocalDir,
		RemoteDir:              req.RemoteDir,
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
	sshCtx, sshFunc := context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			_ = options.Cleanup()
			sshFunc()
		}
	}()
	options.AddRollbackFunc(func() error {
		sshFunc()
		return nil
	})
	var path string
	path, err = ssh.SshJump(sshCtx, sshConf, flags, false)
	if err != nil {
		return err
	}
	f := util.InitFactoryByPath(path, req.Namespace)
	err = options.InitClient(f)
	if err != nil {
		log.Errorf("Failed to init client: %v", err)
		return err
	}
	config.Image = req.Image
	log.Infof("Clone workloads...")
	options.SetContext(sshCtx)
	err = options.DoClone(resp.Context(), []byte(req.KubeconfigBytes))
	if err != nil {
		log.Errorf("Clone workloads failed: %v", err)
		return err
	}
	svr.clone = options
	return nil
}

type cloneWarp struct {
	server rpc.Daemon_CloneServer
}

func (r *cloneWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.CloneResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newCloneWarp(server rpc.Daemon_CloneServer) io.Writer {
	return &cloneWarp{server: server}
}
