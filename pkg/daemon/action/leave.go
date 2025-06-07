package action

import (
	"fmt"
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func (svr *Server) Leave(resp rpc.Daemon_LeaveServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}

	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newLeaveWarp(resp), svr.LogFile))
	if svr.connect == nil {
		logger.Infof("No proxy resource found")
		return fmt.Errorf("no proxy resource found")
	}
	ctx := plog.WithLogger(resp.Context(), logger)

	v4, _ := svr.connect.GetLocalTunIP()
	var resources []handler.Resources
	for _, resource := range req.GetWorkloads() {
		resources = append(resources, handler.Resources{
			Namespace: req.Namespace,
			Workload:  resource,
		})
	}
	return svr.connect.LeaveResource(ctx, resources, v4)
}

type leaveWarp struct {
	server rpc.Daemon_LeaveServer
}

func (r *leaveWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.LeaveResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newLeaveWarp(server rpc.Daemon_LeaveServer) io.Writer {
	return &leaveWarp{server: server}
}
