package action

import (
	"context"
	"fmt"
	"os"
	"sync"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) redirectConnectToSudoDaemon(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer, logger *log.Logger) (err error) {
	cli, err := svr.GetClient(true)
	if err != nil {
		return fmt.Errorf("sudo daemon not start: %w", err)
	}
	session := NewSessionLifecycle(logger)
	reqBytes, _ := proto.Marshal(req)
	ownerID := req.OwnerID
	if ownerID == "" {
		// Stable per-(machine, OS user) ID so a client reconnects to the same
		// TUN IP instead of a fresh random one each time.
		ownerID = config.GetClientID()
	}
	connect := &handler.ConnectOptions{
		ManagerNamespace:     req.Namespace,
		WorkloadNamespace:    req.Namespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		RequestRaw:           reqBytes,
		OwnerID:              ownerID,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
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
	if sshConf := parseSshFromRPC(req.SshJump); !sshConf.IsEmpty() {
		connect.SshHosts = sshConf.Host()
		if sshConf.RemoteKubeconfig != "" {
			connect.OriginKubeconfigPath = file
		}
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

	connect.ConnectionID, err = util.GetConnectionID(context.Background(), connect.K8sClient.GetClientset().CoreV1().Namespaces(), connect.ManagerNamespace)
	if err != nil {
		return err
	}
	// Tag all downstream logs (forward-to-sudo, health checks) with the connection
	// ID so concurrent connects can be told apart in the shared user daemon log file.
	session.Ctx = plog.WithField(session.Ctx, LogFieldConnID, connect.ConnectionID)

	svr.connMu.Lock()
	existing, _ := svr.findConnection(connect.ConnectionID)
	if existing != nil {
		svr.currentConnectionID = connect.ConnectionID
		svr.connMu.Unlock()
		session.Cancel()
		logger.Infof("Connected with cluster")
		return resp.Send(&rpc.ConnectResponse{
			ConnectionID: connect.ConnectionID,
		})
	}
	svr.connMu.Unlock()

	return svr.forwardConnectToSudo(session.Ctx, req, connect, resp, cli, &connResp, &connRespMu, file, connect.ConnectionID, logger)
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
	if req.ManagerNamespace == "" {
		// No existing manager found anywhere: fall back to the request namespace.
		logger.Infof("Use special namespace %s", req.Namespace)
		req.ManagerNamespace = req.Namespace
	} else {
		logger.Infof("Use manager namespace %s", req.ManagerNamespace)
	}
	// Keep the user-daemon ConnectOptions and the request forwarded to the root
	// daemon in lock-step: both must carry the SAME resolved namespace. Assign it
	// explicitly here rather than relying on the struct's initial ManagerNamespace,
	// so the two daemons can never diverge if that initialization changes.
	connect.ManagerNamespace = req.ManagerNamespace
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
	if err := connect.CreateOutboundPod(ctx); err != nil {
		return err
	}
	if err := connect.UpgradeDeploy(ctx); err != nil {
		return err
	}

	content, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return err
	}
	req.KubeconfigBytes = string(content)
	req.OwnerID = connect.OwnerID
	req.ConnectionID = connectionID
	cr, err := cli.Connect(context.Background())
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
	svr.connMu.Lock()
	svr.connections = append(svr.connections, connect)
	svr.currentConnectionID = connectionID
	svr.connMu.Unlock()
	return resp.Send(&rpc.ConnectResponse{
		ConnectionID: connectionID,
	})
}
