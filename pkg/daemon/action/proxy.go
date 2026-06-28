package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	v1 "k8s.io/api/core/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Proxy
//  1. if not connect to cluster
//     1.1 connect to cluster
//     1.2 proxy workloads
//  2. if already connect to cluster
//     2.1 disconnect from cluster
//     2.2 same as step 1
func (svr *Server) Proxy(resp rpc.Daemon_ProxyServer) (err error) {
	var req *rpc.ProxyRequest
	req, err = resp.Recv()
	if err != nil {
		return err
	}

	logger := newServerStreamLogger(svr.LogFile, req.Level, func(msg string) error {
		return resp.Send(&rpc.ProxyResponse{Message: msg})
	})
	ctx := plog.WithLogger(resp.Context(), logger)
	file, err := resolveKubeconfig(ctx, req.SshJump, req.KubeconfigBytes, false)
	if err != nil {
		return err
	}
	defer os.Remove(file)
	connectReq := convert(req)
	reqBytes, _ := proto.Marshal(connectReq)
	connect := &handler.ConnectOptions{
		ExtraRouteInfo:       *handler.ParseExtraRouteFromRPC(req.ExtraRoute),
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		RequestRaw:           reqBytes,
	}
	err = connect.InitClient(util.InitFactoryByPath(file, req.Namespace))
	if err != nil {
		return err
	}

	var workloads []string
	workloads, _, err = util.NormalizedResource(connect.GetFactory(), req.Namespace, req.Workloads)
	if err != nil {
		return err
	}

	var cli rpc.DaemonClient
	cli, err = svr.GetClient(false)
	if err != nil {
		return fmt.Errorf("daemon is not available: %w", err)
	}

	var connResp grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse]
	var connRespMu sync.Mutex
	cancel, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
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
		}
		cancelFunc()
	}()

	plog.G(ctx).Debugf("Connecting to cluster")
	cr, connErr := cli.Connect(context.Background())
	if connErr != nil {
		return connErr
	}
	connRespMu.Lock()
	connResp = cr
	connRespMu.Unlock()
	err = connResp.Send(convert(req))
	if err != nil {
		return err
	}
	var connectionID string
	connectionID, err = grpcutil.CopyGRPCConnStream(connResp, resp)
	if err != nil {
		return err
	}
	// Tag subsequent logs with the connection ID for concurrent-op isolation.
	ctx = plog.WithField(ctx, LogFieldConnID, connectionID)

	svr.connMu.RLock()
	options, _ := svr.findConnection(connectionID)
	svr.connMu.RUnlock()
	if options == nil {
		return errors.New("failed to connect to cluster")
	}

	defer func() {
		if err != nil {
			cleanupCtx := plog.WithLogger(context.Background(), logger)
			_ = options.LeaveAllProxyResources(cleanupCtx)
			disconnect(cleanupCtx, svr, connectionID)
			svr.connMu.Lock()
			svr.resetCurrentConnection(connectionID)
			svr.connMu.Unlock()
		}
	}()

	ips := svr.getSudoTunIPs(ctx)
	tunV4, tunV6 := resolveTunIP(options, ips)
	if tunV4 == "" {
		return fmt.Errorf("no TUN IP found for connection %s", connectionID)
	}
	var podList []v1.Pod
	podList, err = options.GetRunningPodList(cancel)
	if err != nil {
		return err
	}
	image := podList[0].Spec.Containers[0].Image
	err = options.CreateRemoteInboundPod(plog.WithLogger(cancel, logger), req.Namespace, workloads, req.Headers, req.PortMap, image, tunV4, tunV6)
	if err != nil {
		plog.G(ctx).Errorf("Failed to inject inbound sidecar: %v", err)
		return err
	}
	svr.connMu.Lock()
	svr.currentConnectionID = connectionID
	svr.connMu.Unlock()
	options.HealthCheckOnce(cancel, config.HealthCheckTimeout)
	return nil
}


func convert(req *rpc.ProxyRequest) *rpc.ConnectRequest {
	return &rpc.ConnectRequest{
		KubeconfigBytes:      req.KubeconfigBytes,
		Namespace:            req.Namespace,
		ExtraRoute:           req.ExtraRoute,
		SshJump:              req.SshJump,
		TransferImage:        req.TransferImage,
		Image:                req.Image,
		ImagePullSecretName:  req.ImagePullSecretName,
		Foreground:           req.Foreground,
		Level:                req.Level,
		OriginKubeconfigPath: req.OriginKubeconfigPath,
		ManagerNamespace:     req.ManagerNamespace,
	}
}
