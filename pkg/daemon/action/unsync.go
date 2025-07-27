package action

import (
	"fmt"
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func (svr *Server) Unsync(resp rpc.Daemon_UnsyncServer) error {
	req, err := resp.Recv()
	if err != nil {
		return err
	}
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newRemoveWarp(resp), svr.LogFile))
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

type unsyncWarp struct {
	server rpc.Daemon_UnsyncServer
}

func (r *unsyncWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.UnsyncResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newRemoveWarp(server rpc.Daemon_UnsyncServer) io.Writer {
	return &unsyncWarp{server: server}
}
