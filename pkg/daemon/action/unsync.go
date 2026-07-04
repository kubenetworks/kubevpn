package action

import (
	"fmt"
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Unsync handles the Unsync RPC, stopping file synchronization for the specified workloads on the current connection.
func (svr *Server) Unsync(resp rpc.Daemon_UnsyncServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newStreamWriter(func(msg string) error {
		return resp.Send(&rpc.UnsyncResponse{Message: msg})
	}), svr.LogFile))
	conn, _ := svr.findConnection(svr.currentConnectionID)
	if conn == nil {
		logger.Infof("No connection found")
		return fmt.Errorf("no connection found")
	}

	ctx := plog.WithLogger(resp.Context(), logger)
	if conn.Sync != nil {
		err = conn.Sync.Cleanup(ctx, req.Workloads...)
		conn.Sync = nil
	}
	return err
}

