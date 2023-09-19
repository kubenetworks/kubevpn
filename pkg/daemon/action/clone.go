package action

import (
	"fmt"
	"io"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func (svr *Server) Clone(req *rpc.CloneRequest, resp rpc.Daemon_CloneServer) error {
	var sshConf = util.ParseSshFromRPC(req.SshJump)
	origin := log.StandardLogger().Out
	defer func() {
		log.SetOutput(origin)
		log.SetLevel(log.DebugLevel)
	}()
	out := io.MultiWriter(newCloneWarp(resp), origin)
	log.SetOutput(out)
	util.InitLogger(false)
	connReq := &rpc.ConnectRequest{
		KubeconfigBytes: req.KubeconfigBytes,
		Namespace:       req.Namespace,
		ExtraCIDR:       req.ExtraCIDR,
		ExtraDomain:     req.ExtraDomain,
		UseLocalDNS:     req.UseLocalDNS,
		Engine:          req.Engine,
		SshJump:         req.SshJump,
		TransferImage:   req.TransferImage,
		Image:           req.Image,
		Level:           req.Level,
	}
	cli := svr.GetClient(false)
	connResp, err := cli.Connect(resp.Context(), connReq)
	if err != nil {
		return err
	}
	var msg *rpc.ConnectResponse
	for {
		msg, err = connResp.Recv()
		if err == io.EOF {
			break
		} else if err == nil {
			fmt.Fprint(out, msg.Message)
		} else if code := status.Code(err); code == codes.DeadlineExceeded || code == codes.Canceled {
			return nil
		} else {
			return err
		}
	}

	options := &handler.CloneOptions{
		Namespace:   req.Namespace,
		Headers:     req.Headers,
		Workloads:   req.Workloads,
		ExtraCIDR:   req.ExtraCIDR,
		ExtraDomain: req.ExtraDomain,
		UseLocalDNS: req.UseLocalDNS,
		Engine:      config.Engine(req.Engine),

		TargetKubeconfig:       req.TargetKubeconfig,
		TargetNamespace:        req.TargetNamespace,
		TargetContainer:        req.TargetContainer,
		TargetImage:            req.TargetImage,
		TargetRegistry:         req.TargetRegistry,
		IsChangeTargetRegistry: req.IsChangeTargetRegistry,
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
	err = handler.SshJump(resp.Context(), sshConf, flags, true)
	if err != nil {
		return err
	}
	f := InitFactory(req.KubeconfigBytes, req.Namespace)
	err = options.InitClient(f)
	if err != nil {
		return err
	}
	config.Image = req.Image
	err = options.DoClone(resp.Context())
	if err != nil {
		return err
	}
	svr.clone = options
	return nil
}

type cloneWarp struct {
	server rpc.Daemon_CloneServer
}

func (r *cloneWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.CloneResponse{
		Message: string(p),
	})
	return len(p), err
}

func newCloneWarp(server rpc.Daemon_CloneServer) io.Writer {
	return &cloneWarp{server: server}
}

//type daemonConnectServer struct {
//	out io.Writer
//	grpc.ServerStream
//}
//
//func (d *daemonConnectServer) Send(response *rpc.ConnectResponse) error {
//	_, err := d.out.Write([]byte(response.Message))
//	return err
//}
