package action

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

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
		OwnerID:              uuid.New().String()[:12],
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
	var connRespMu sync.Mutex
	go func() {
		var s rpc.Cancel
		if recvErr := resp.RecvMsg(&s); recvErr != nil {
			return
		}
		connRespMu.Lock()
		cr := connResp
		connRespMu.Unlock()
		if cr != nil {
			_ = cr.SendMsg(&s)
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

	svr.connMu.Lock()
	existing, _ := svr.findConnection(connectionID)
	if existing != nil {
		svr.currentConnectionID = connectionID
		svr.connMu.Unlock()
		session.Cancel()
		logger.Infof("Connected with cluster")
		return resp.Send(&rpc.ConnectResponse{
			ConnectionID: connectionID,
		})
	}
	svr.connMu.Unlock()

	return svr.forwardConnectToSudo(session.Ctx, req, connect, resp, cli, &connResp, &connRespMu, file, connectionID, logger)
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
	connRespMu *sync.Mutex,
	kubeconfigPath string,
	connectionID string,
	logger *log.Logger,
) error {
	ipCtx, err := connect.RentIP(resp.Context(), req.IPv4, req.IPv6)
	if err != nil {
		return err
	}

	content, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return err
	}
	req.KubeconfigBytes = string(content)
	cr, err := cli.Connect(ipCtx)
	if err != nil {
		return err
	}
	connRespMu.Lock()
	*connResp = cr
	connRespMu.Unlock()
	err = cr.Send(req)
	if err != nil {
		return err
	}
	err = grpcutil.CopyGRPCStream[rpc.ConnectResponse](cr, resp)
	if err != nil {
		return err
	}

	if resp.Context().Err() != nil {
		return resp.Context().Err()
	}
	connect.HealthCheckOnce(ctx, time.Second*10)
	go connect.HealthPeriod(ctx, time.Second*30)
	svr.connMu.Lock()
	svr.connections = append(svr.connections, connect)
	svr.currentConnectionID = connectionID
	svr.connMu.Unlock()
	return resp.Send(&rpc.ConnectResponse{
		ConnectionID: connectionID,
	})
}
