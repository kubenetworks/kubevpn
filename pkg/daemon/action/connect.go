package action

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Connect handles the Connect RPC, establishing a VPN tunnel to the cluster.
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

	logger := plog.GetLoggerForClient(req.Level, io.MultiWriter(newStreamWriter(func(msg string) error {
		return resp.Send(&rpc.ConnectResponse{Message: msg})
	}), svr.LogFile))
	if !svr.IsSudo {
		return svr.redirectConnectToSudoDaemon(req, resp, logger)
	}

	ctx := resp.Context()
	connect := &handler.ConnectOptions{
		ManagerNamespace:     req.ManagerNamespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		OriginNamespace:      req.Namespace,
		Lock:                 &svr.Lock,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		Request:              proto.Clone(req).(*rpc.ConnectRequest),
	}
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes), "")
	if err != nil {
		return err
	}
	session := NewSessionLifecycle(logger)
	session.AddTempFile(&file)
	connect.AddRollbackFunc(func() error {
		session.RunCleanups()
		return nil
	})
	go util.ListenCancel(resp, session.Cancel)
	defer func() {
		if err != nil {
			connect.Cleanup(plog.WithLogger(context.Background(), logger))
			session.Cancel()
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

	err = connect.DoConnect(session.Ctx)
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
		return fmt.Errorf("sudo daemon not start: %w", err)
	}
	session := NewSessionLifecycle(logger)
	connect := &handler.ConnectOptions{
		ManagerNamespace:     req.Namespace,
		OriginNamespace:      req.Namespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		Request:              proto.Clone(req).(*rpc.ConnectRequest),
	}
	var file string
	session.AddTempFile(&file)
	connect.AddRollbackFunc(func() error {
		session.RunCleanups()
		return nil
	})

	file, err = resolveKubeconfig(session.Ctx, req.SshJump, req.KubeconfigBytes, true)
	if err != nil {
		return err
	}
	if sshConf := ssh.ParseSshFromRPC(req.SshJump); !sshConf.IsEmpty() && sshConf.RemoteKubeconfig != "" {
		connect.OriginKubeconfigPath = file
	}

	defer func() {
		if err != nil {
			connect.Cleanup(plog.WithLogger(context.Background(), logger))
			session.Cancel()
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
			session.Cancel()
		}
	}()
	err = connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
	if err != nil {
		return err
	}

	if err = svr.detectAndSetManagerNamespace(session.Ctx, req, connect, logger); err != nil {
		return err
	}

	if err = connect.InitDHCP(session.Ctx); err != nil {
		return err
	}
	connectionID := connect.GetConnectionID()

	if existing, _ := svr.findConnection(connectionID); existing != nil {
		session.Cancel()
		logger.Infof("Connected with cluster")
		svr.currentConnectionID = connectionID
		return resp.Send(&rpc.ConnectResponse{
			ConnectionID: connectionID,
		})
	}

	return svr.forwardConnectToSudo(session.Ctx, req, connect, resp, cli, &connResp, file, connectionID, logger)
}

// detectAndSetManagerNamespace resolves the traffic manager namespace, falling
// back to the request namespace when no existing manager is found.
func (svr *Server) detectAndSetManagerNamespace(ctx context.Context, req *rpc.ConnectRequest, connect *handler.ConnectOptions, logger *log.Logger) error {
	if req.ManagerNamespace == "" {
		var err error
		req.ManagerNamespace, err = util.DetectManagerNamespace(plog.WithLogger(ctx, logger), connect.GetFactory(), req.Namespace)
		if err != nil {
			return err
		}
	}
	if req.ManagerNamespace != "" {
		logger.Infof("Use manager namespace %s", req.ManagerNamespace)
		connect.ManagerNamespace = req.ManagerNamespace
	} else {
		logger.Infof("Use special namespace %s", req.Namespace)
		req.ManagerNamespace = req.Namespace
	}
	return nil
}

// forwardConnectToSudo rents an IP, forwards the connect request to the sudo
// daemon, streams the response back to the caller, and starts health checks.
func (svr *Server) forwardConnectToSudo(
	ctx context.Context,
	req *rpc.ConnectRequest,
	connect *handler.ConnectOptions,
	resp rpc.Daemon_ConnectServer,
	cli rpc.DaemonClient,
	connResp *grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse],
	kubeconfigPath string,
	connectionID string,
	logger *log.Logger,
) error {
	ipCtx, err := connect.RentIP(resp.Context(), req.IPv4, req.IPv6)
	if err != nil {
		return err
	}

	// only ssh jump in user daemon
	content, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return err
	}
	req.KubeconfigBytes = string(content)
	*connResp, err = cli.Connect(ipCtx)
	if err != nil {
		return err
	}
	err = (*connResp).Send(req)
	if err != nil {
		return err
	}
	err = util.CopyGRPCStream[rpc.ConnectResponse](*connResp, resp)
	if err != nil {
		return err
	}

	if resp.Context().Err() != nil {
		return resp.Context().Err()
	}
	connect.HealthCheckOnce(ctx, time.Second*10)
	go connect.HealthPeriod(ctx, time.Second*30)
	svr.connections = append(svr.connections, connect)
	svr.currentConnectionID = connectionID
	return resp.Send(&rpc.ConnectResponse{
		ConnectionID: connectionID,
	})
}

