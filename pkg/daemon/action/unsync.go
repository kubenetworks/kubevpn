package action

import (
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// Unsync handles the Unsync RPC, stopping file synchronization for the specified workloads on the current connection.
func (svr *Server) Unsync(resp rpc.Daemon_UnsyncServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger, ctx := svr.initStreamLogger(resp, int32(log.InfoLevel), func(msg string) error {
		return resp.Send(&rpc.UnsyncResponse{Message: msg})
	})
	svr.connMu.RLock()
	conn, _ := svr.findConnection(svr.currentConnectionID)
	svr.connMu.RUnlock()
	if conn == nil {
		logger.Infof("No connection found")
		return fmt.Errorf("no connection found")
	}
	if conn.Sync != nil {
		err = conn.Sync.Cleanup(ctx, req.Workloads...)
		conn.Sync = nil
	}
	return err
}

