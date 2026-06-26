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
	var index = -1
	for i, connection := range svr.connections {
		if connection.GetConnectionID() == svr.currentConnectionID {
			index = i
			break
		}
	}
	if index == -1 {
		logger.Infof("No connection found")
		return fmt.Errorf("no connection found")
	}

	ctx := plog.WithLogger(resp.Context(), logger)
	if svr.connections[index].Sync != nil {
		err = svr.connections[index].Sync.Cleanup(ctx, req.Workloads...)
		svr.connections[index].Sync = nil
	}
	return err
}

