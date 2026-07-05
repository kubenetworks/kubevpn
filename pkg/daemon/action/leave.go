package action

import (
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// Leave handles the Leave RPC, removing proxy injection from the specified workloads on the active connection.
func (svr *Server) Leave(resp rpc.Daemon_LeaveServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}

	logger, ctx := svr.initStreamLogger(resp, int32(log.InfoLevel), func(msg string) error {
		return resp.Send(&rpc.LeaveResponse{Message: msg})
	})

	svr.connMu.RLock()
	conn, _ := svr.findConnection(svr.currentConnectionID)
	svr.connMu.RUnlock()
	if conn == nil {
		logger.Infof("No connection found")
		return config.ErrConnectionNotFound
	}

	var resources []handler.Resources
	for _, resource := range req.GetWorkloads() {
		resources = append(resources, handler.Resources{
			Namespace: req.Namespace,
			Workload:  resource,
		})
	}
	return conn.LeaveResource(ctx, resources, conn.GetOwnerID())
}
