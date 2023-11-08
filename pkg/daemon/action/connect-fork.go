package action

import (
	"context"
	"io"
	defaultlog "log"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func (svr *Server) ConnectFork(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectForkServer) error {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
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
		ExtraCIDR:            req.ExtraCIDR,
		ExtraDomain:          req.ExtraDomain,
		UseLocalDNS:          req.UseLocalDNS,
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
	}
	var sshConf = util.ParseSshFromRPC(req.SshJump)
	var transferImage = req.TransferImage

	go util.StartupPProf(config.PProfPort)
	defaultlog.Default().SetOutput(io.Discard)
	if transferImage {
		err := util.TransferImage(ctx, sshConf, config.OriginImage, req.Image, out)
		if err != nil {
			err = errors.Wrap(err, "util.TransferImage(ctx, sshConf, config.OriginImage, req.Image, out): ")
			return err
		}
	}
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		err = errors.Wrap(err, "util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes)): ")
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
	var path string
	path, err = handler.SshJump(sshCtx, sshConf, flags, false)
	if err != nil {
		err = errors.Wrap(err, "handler.SshJump(sshCtx, sshConf, flags, false): ")
		return err
	}
	err = connect.InitClient(InitFactoryByPath(path, req.Namespace))
	if err != nil {
		err = errors.Wrap(err, "connect.InitClient(InitFactoryByPath(path, req.Namespace)): ")
		return err
	}
	err = connect.PreCheckResource()
	if err != nil {
		err = errors.Wrap(err, "connect.PreCheckResource(): ")
		return err
	}
	_, err = connect.RentInnerIP(ctx)
	if err != nil {
		err = errors.Wrap(err, "connect.RentInnerIP(ctx): ")
		return err
	}

	config.Image = req.Image
	err = connect.DoConnect(sshCtx, true)
	if err != nil {
		errors.LogErrorf("do connect error: %v", err)
		connect.Cleanup()
		return err
	}
	svr.secondaryConnect = append(svr.secondaryConnect, connect)

	return nil
}

func (svr *Server) redirectConnectForkToSudoDaemon(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer) error {
	cli := svr.GetClient(true)
	if cli == nil {
		return errors.Errorf("sudo daemon not start")
	}
	connect := &handler.ConnectOptions{
		Namespace:            req.Namespace,
		Headers:              req.Headers,
		Workloads:            req.Workloads,
		ExtraCIDR:            req.ExtraCIDR,
		ExtraDomain:          req.ExtraDomain,
		UseLocalDNS:          req.UseLocalDNS,
		Engine:               config.Engine(req.Engine),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
	}
	var sshConf = util.ParseSshFromRPC(req.SshJump)
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		err = errors.Wrap(err, "util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes)): ")
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
	var path string
	path, err = handler.SshJump(sshCtx, sshConf, flags, true)
	if err != nil {
		err = errors.Wrap(err, "handler.SshJump(sshCtx, sshConf, flags, true): ")
		return err
	}
	err = connect.InitClient(InitFactoryByPath(path, req.Namespace))
	if err != nil {
		err = errors.Wrap(err, "connect.InitClient(InitFactoryByPath(path, req.Namespace)): ")
		return err
	}
	err = connect.PreCheckResource()
	if err != nil {
		err = errors.Wrap(err, "connect.PreCheckResource(): ")
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
		err = errors.Wrap(err, "connect.RentInnerIP(resp.Context()): ")
		return err
	}

	connResp, err := cli.ConnectFork(ctx, req)
	if err != nil {
		err = errors.Wrap(err, "cli.ConnectFork(ctx, req): ")
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
			err = errors.Wrap(err, "resp.Send(recv): ")
			return err
		}
	}

	svr.secondaryConnect = append(svr.secondaryConnect, connect)

	if req.Foreground {
		<-resp.Context().Done()
		for i := 0; i < len(svr.secondaryConnect); i++ {
			if svr.secondaryConnect[i] == connect {
				cli := svr.GetClient(false)
				if cli == nil {
					return errors.Errorf("sudo daemon not start")
				}
				disconnect, err := cli.Disconnect(context.Background(), &rpc.DisconnectRequest{
					ID: pointer.Int32(int32(i)),
				})
				if err != nil {
					errors.LogErrorf("disconnect error: %v", err)
					return err
				}
				for {
					recv, err := disconnect.Recv()
					if err == io.EOF {
						break
					} else if err != nil {
						return err
					}
					log.Info(recv.Message)
				}
				break
			}
		}
	}

	return nil
}

type connectForkWarp struct {
	server rpc.Daemon_ConnectServer
}

func (r *connectForkWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.ConnectResponse{
		Message: string(p),
	})
	return len(p), err
}

func newConnectForkWarp(server rpc.Daemon_ConnectForkServer) io.Writer {
	return &connectForkWarp{server: server}
}
