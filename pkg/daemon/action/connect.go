package action

import (
	"context"

	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
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

	logger := plog.GetLoggerForServer(req.Level, svr.LogFile)
	logger.AddHook(&plog.StreamHook{
		Writer: newStreamWriter(func(msg string) error {
			return resp.Send(&rpc.ConnectResponse{Message: msg})
		}),
		Level: log.InfoLevel,
	})
	if !svr.IsSudo {
		return svr.redirectConnectToSudoDaemon(req, resp, logger)
	}

	reqBytes, _ := proto.Marshal(req)
	connect := &handler.ConnectOptions{
		ManagerNamespace:     req.ManagerNamespace,
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		WorkloadNamespace:      req.Namespace,
		Lock:                 &svr.Lock,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		RequestRaw:           reqBytes,
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
	go grpcutil.ListenCancel(resp, session.Cancel)
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
	connect.OwnerID = req.OwnerID
	connect.ConnectionID = req.ConnectionID

	err = connect.DoConnect(session.Ctx)
	if err != nil {
		logger.Errorf("Failed to connect...")
		return err
	}

	if resp.Context().Err() != nil {
		return resp.Context().Err()
	}
	svr.connMu.Lock()
	svr.connections = append(svr.connections, connect)
	svr.connMu.Unlock()
	return nil
}
