package action

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Disconnect(resp rpc.Daemon_DisconnectServer) (err error) {
	if !svr.IsSudo {
		defer svr.OffloadToConfig()
	}

	req, err := resp.Recv()
	if err != nil {
		return err
	}

	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newDisconnectWarp(resp), svr.LogFile))
	ctx := plog.WithLogger(resp.Context(), logger)

	// disconnect sudo daemon first
	// then disconnect from user daemon
	// because only ssh jump in user daemon
	if !svr.IsSudo {
		cli, err := svr.GetClient(true)
		if err != nil {
			return errors.Wrap(err, "sudo daemon not start")
		}
		connResp, err := cli.Disconnect(resp.Context())
		if err != nil {
			return err
		}
		err = connResp.Send(req)
		if err != nil {
			return err
		}
		err = util.CopyGRPCStream[rpc.DisconnectResponse](connResp, resp)
		if err != nil {
			return err
		}
	}

	switch {
	case req.GetAll():
		if svr.clone != nil {
			_ = svr.clone.Cleanup(ctx)
		}
		svr.clone = nil

		connects := handler.Connects(svr.secondaryConnect).Append(svr.connect)
		for _, connect := range connects.Sort() {
			if connect != nil {
				connect.Cleanup(ctx)
			}
		}
		svr.secondaryConnect = nil
		svr.connect = nil
		svr.t = time.Time{}
	case req.ID != nil && req.GetID() == 0:
		if svr.connect != nil {
			svr.connect.Cleanup(ctx)
		}
		svr.connect = nil
		svr.t = time.Time{}

		if svr.clone != nil {
			_ = svr.clone.Cleanup(ctx)
		}
		svr.clone = nil
	case req.ID != nil:
		index := req.GetID() - 1
		if index < int32(len(svr.secondaryConnect)) {
			svr.secondaryConnect[index].Cleanup(ctx)
			svr.secondaryConnect = append(svr.secondaryConnect[:index], svr.secondaryConnect[index+1:]...)
		} else {
			plog.G(ctx).Errorf("Index %d out of range", req.GetID())
		}
	case req.KubeconfigBytes != nil && req.Namespace != nil:
		err = disconnectByKubeconfig(
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
				connect.Cleanup(ctx)
			}
		}
		if foundModeFull {
			svr.connect = nil
			svr.t = time.Time{}
			if svr.clone != nil {
				_ = svr.clone.Cleanup(ctx)
			}
			svr.clone = nil
		}
	}

	if svr.connect == nil && len(svr.secondaryConnect) == 0 {
		if svr.IsSudo {
			_ = dns.CleanupHosts()
		}
	}
	return nil
}

func disconnectByKubeconfig(ctx context.Context, svr *Server, kubeconfigBytes string, ns string, jump *rpc.SshJump) error {
	file, err := util.ConvertToTempKubeconfigFile([]byte(kubeconfigBytes))
	if err != nil {
		return err
	}
	defer os.Remove(file)
	var sshConf = ssh.ParseSshFromRPC(jump)
	if !sshConf.IsEmpty() {
		file, err = ssh.SshJump(ctx, sshConf, file, false)
		if err != nil {
			return err
		}
	}
	connect := &handler.ConnectOptions{}
	err = connect.InitClient(util.InitFactoryByPath(file, ns))
	if err != nil {
		return err
	}
	disconnect(ctx, svr, connect)
	return nil
}

func disconnect(ctx context.Context, svr *Server, connect *handler.ConnectOptions) {
	if svr.connect != nil {
		isSameCluster, _ := util.IsSameCluster(
			ctx,
			svr.connect.GetClientset().CoreV1(), svr.connect.OriginNamespace,
			connect.GetClientset().CoreV1(), connect.Namespace,
		)
		if isSameCluster {
			plog.G(ctx).Infof("Disconnecting from the cluster...")
			svr.connect.Cleanup(ctx)
			svr.connect = nil
			svr.t = time.Time{}
		}
	}
	for i := 0; i < len(svr.secondaryConnect); i++ {
		options := svr.secondaryConnect[i]
		isSameCluster, _ := util.IsSameCluster(
			ctx,
			options.GetClientset().CoreV1(), options.OriginNamespace,
			connect.GetClientset().CoreV1(), connect.Namespace,
		)
		if isSameCluster {
			plog.G(ctx).Infof("Disconnecting from the cluster...")
			options.Cleanup(ctx)
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
