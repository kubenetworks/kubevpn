package action

import (
	"context"
	"fmt"
	"io"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Disconnect(req *rpc.DisconnectRequest, resp rpc.Daemon_DisconnectServer) error {
	defer func() {
		util.InitLoggerForServer(true)
		log.SetOutput(svr.LogFile)
		config.Debug = false
	}()
	out := io.MultiWriter(newDisconnectWarp(resp), svr.LogFile)
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(out)
	switch {
	case req.GetAll():
		if svr.clone != nil {
			_ = svr.clone.Cleanup()
		}
		svr.clone = nil

		connects := handler.Connects(svr.secondaryConnect).Append(svr.connect)
		for _, connect := range connects.Sort() {
			if connect != nil {
				connect.Cleanup()
			}
		}
		svr.secondaryConnect = nil
		svr.connect = nil
		svr.t = time.Time{}
	case req.ID != nil && req.GetID() == 0:
		if svr.connect != nil {
			svr.connect.Cleanup()
		}
		svr.connect = nil
		svr.t = time.Time{}

		if svr.clone != nil {
			_ = svr.clone.Cleanup()
		}
		svr.clone = nil
	case req.ID != nil:
		index := req.GetID() - 1
		if index < int32(len(svr.secondaryConnect)) {
			svr.secondaryConnect[index].Cleanup()
			svr.secondaryConnect = append(svr.secondaryConnect[:index], svr.secondaryConnect[index+1:]...)
		} else {
			log.Errorf("Index %d out of range", req.GetID())
		}
	case req.KubeconfigBytes != nil && req.Namespace != nil:
		err := disconnectByKubeConfig(
			resp.Context(),
			svr,
			req.GetKubeconfigBytes(),
			req.GetNamespace(),
			req.GetSshJump(),
		)
		if err != nil {
			return err
		}
	case len(req.ClusterIDs) != 0:
		s := sets.New(req.ClusterIDs...)
		var connects = *new(handler.Connects)
		var foundModeFull bool
		if s.Has(svr.connect.GetClusterID()) {
			connects = connects.Append(svr.connect)
			foundModeFull = true
		}
		for i := 0; i < len(svr.secondaryConnect); i++ {
			if s.Has(svr.secondaryConnect[i].GetClusterID()) {
				connects = connects.Append(svr.secondaryConnect[i])
				svr.secondaryConnect = append(svr.secondaryConnect[:i], svr.secondaryConnect[i+1:]...)
				i--
			}
		}
		for _, connect := range connects.Sort() {
			if connect != nil {
				connect.Cleanup()
			}
		}
		if foundModeFull {
			svr.connect = nil
			svr.t = time.Time{}
			if svr.clone != nil {
				_ = svr.clone.Cleanup()
			}
			svr.clone = nil
		}
	}

	if svr.connect == nil && len(svr.secondaryConnect) == 0 {
		if svr.IsSudo {
			_ = dns.CleanupHosts()
		}
	}

	if !svr.IsSudo {
		cli := svr.GetClient(true)
		if cli == nil {
			return fmt.Errorf("sudo daemon not start")
		}
		connResp, err := cli.Disconnect(resp.Context(), req)
		if err != nil {
			return err
		}
		var recv *rpc.DisconnectResponse
		for {
			recv, err = connResp.Recv()
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
	}

	return nil
}

func disconnectByKubeConfig(ctx context.Context, svr *Server, kubeconfigBytes string, ns string, jump *rpc.SshJump) error {
	file, err := util.ConvertToTempKubeconfigFile([]byte(kubeconfigBytes))
	if err != nil {
		return err
	}
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	flags.AddFlag(&pflag.Flag{
		Name:     "kubeconfig",
		DefValue: file,
	})
	var sshConf = ssh.ParseSshFromRPC(jump)
	var path string
	path, err = ssh.SshJump(ctx, sshConf, flags, false)
	if err != nil {
		return err
	}
	connect := &handler.ConnectOptions{
		Namespace: ns,
	}
	err = connect.InitClient(util.InitFactoryByPath(path, ns))
	if err != nil {
		return err
	}
	disconnect(svr, connect)
	return nil
}

func disconnect(svr *Server, connect *handler.ConnectOptions) {
	client := svr.GetClient(false)
	if client == nil {
		return
	}
	if svr.connect != nil {
		isSameCluster, err := util.IsSameCluster(
			svr.connect.GetClientset().CoreV1().ConfigMaps(svr.connect.Namespace), svr.connect.Namespace,
			connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace), connect.Namespace,
		)
		if err == nil && isSameCluster {
			log.Infof("Disconnecting from the cluster...")
			svr.connect.Cleanup()
			svr.connect = nil
			svr.t = time.Time{}
		}
	}
	for i := 0; i < len(svr.secondaryConnect); i++ {
		options := svr.secondaryConnect[i]
		isSameCluster, err := util.IsSameCluster(
			options.GetClientset().CoreV1().ConfigMaps(options.Namespace), options.Namespace,
			connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace), connect.Namespace,
		)
		if err == nil && isSameCluster {
			log.Infof("Disconnecting from the cluster...")
			options.Cleanup()
			svr.secondaryConnect = append(svr.secondaryConnect[:i], svr.secondaryConnect[i+1:]...)
			i--
		}
	}

}

type disconnectWarp struct {
	server rpc.Daemon_DisconnectServer
}

func (r *disconnectWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.DisconnectResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newDisconnectWarp(server rpc.Daemon_DisconnectServer) io.Writer {
	return &disconnectWarp{server: server}
}
