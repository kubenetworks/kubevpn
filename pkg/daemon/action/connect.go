package action

import (
	"context"
	"io"
	"os"

	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Connect(resp rpc.Daemon_ConnectServer) (err error) {
	if !svr.IsSudo {
		defer func() {
			if err == nil {
				_ = svr.OffloadToConfig()
			}
		}()
	}

	req, err := resp.Recv()
	if err != nil {
		return err
	}

	logger := plog.GetLoggerForClient(req.Level, io.MultiWriter(newConnectForkWarp(resp), svr.LogFile))
	if !svr.IsSudo {
		return svr.redirectConnectToSudoDaemon(req, resp, logger)
	}

	ctx := resp.Context()
	connect := &handler.ConnectOptions{
		Namespace:            req.ManagerNamespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		OriginNamespace:      req.Namespace,
		Lock:                 &svr.Lock,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		Request:              proto.Clone(req).(*rpc.ConnectRequest),
	}
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	sshCtx, sshCancel := context.WithCancel(context.Background())
	connect.AddRolloutFunc(func() error {
		sshCancel()
		_ = os.Remove(file)
		return nil
	})
	go util.ListenCancel(resp, sshCancel)
	sshCtx = plog.WithLogger(sshCtx, logger)
	defer plog.WithoutLogger(sshCtx)
	defer func() {
		if err != nil {
			connect.Cleanup(plog.WithLogger(context.Background(), logger))
			sshCancel()
		}
	}()

	err = connect.InitClient(util.InitFactoryByPath(file, req.ManagerNamespace))
	if err != nil {
		return err
	}
	err = connect.GetIPFromContext(ctx, logger)
	if err != nil {
		return err
	}

	err = connect.DoConnect(sshCtx)
	if err != nil {
		logger.Errorf("Failed to connect...")
		return err
	}

	if resp.Context().Err() != nil {
		return resp.Context().Err()
	}
	svr.connections = append(svr.connections, connect)
	return nil
}

func (svr *Server) redirectConnectToSudoDaemon(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer, logger *log.Logger) (err error) {
	cli, err := svr.GetClient(true)
	if err != nil {
		return errors.Wrap(err, "sudo daemon not start")
	}
	var sshConf = ssh.ParseSshFromRPC(req.SshJump)
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	sshCtx, sshCancel := context.WithCancel(context.Background())
	sshCtx = plog.WithLogger(sshCtx, logger)
	defer plog.WithoutLogger(sshCtx)
	connect := &handler.ConnectOptions{
		Namespace:            req.Namespace,
		OriginNamespace:      req.Namespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		Request:              proto.Clone(req).(*rpc.ConnectRequest),
	}
	connect.AddRolloutFunc(func() error {
		sshCancel()
		_ = os.Remove(file)
		return nil
	})
	defer func() {
		if err != nil {
			connect.Cleanup(plog.WithLogger(context.Background(), logger))
			sshCancel()
		}
	}()

	var connResp grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse]
	go func() {
		var s rpc.Cancel
		err = resp.RecvMsg(&s)
		if err != nil {
			return
		}
		if connResp != nil {
			_ = connResp.SendMsg(&s)
		} else {
			sshCancel()
		}
	}()

	if !sshConf.IsEmpty() {
		file, err = ssh.SshJump(sshCtx, sshConf, file, true)
		if err != nil {
			return err
		}
	}
	err = connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
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
		req.ManagerNamespace = req.Namespace
	}

	var connectionID string
	connectionID, err = util.GetConnectionID(sshCtx, connect.GetClientset().CoreV1().Namespaces(), connect.Namespace)
	if err != nil {
		return err
	}

	for _, options := range svr.connections {
		if options == nil {
			continue
		}
		var id string
		id, err = util.GetConnectionID(sshCtx, options.GetClientset().CoreV1().Namespaces(), options.Namespace)
		if err != nil {
			return err
		}
		if id == connectionID {
			sshCancel()
			// same cluster, do nothing
			logger.Infof("Connected with cluster")
			svr.currentConnectionID = connectionID
			return resp.Send(&rpc.ConnectResponse{
				ConnectionID: connectionID,
			})
		}
	}

	var ipCtx context.Context
	ipCtx, err = connect.RentIP(resp.Context(), req.IPv4, req.IPv6)
	if err != nil {
		return err
	}

	// only ssh jump in user daemon
	content, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	req.KubeconfigBytes = string(content)
	req.SshJump = ssh.SshConfig{}.ToRPC()
	connResp, err = cli.Connect(ipCtx)
	if err != nil {
		return err
	}
	err = connResp.Send(req)
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
	svr.connections = append(svr.connections, connect)
	svr.currentConnectionID = connectionID
	return resp.Send(&rpc.ConnectResponse{
		ConnectionID: connectionID,
	})
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

func newConnectForkWarp(server rpc.Daemon_ConnectServer) io.Writer {
	return &connectForkWarp{server: server}
}
