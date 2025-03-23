package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func (svr *Server) Remove(req *rpc.RemoveRequest, resp rpc.Daemon_RemoveServer) error {
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newRemoveWarp(resp), svr.LogFile))
	ctx := plog.WithLogger(resp.Context(), logger)
	if svr.clone != nil {
		err := svr.clone.Cleanup(ctx, req.Workloads...)
		svr.clone = nil
		return err
	} else {
		logger.Info("No clone resource found")
	}
	return nil
}

type removeWarp struct {
	server rpc.Daemon_RemoveServer
}

func (r *removeWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.RemoveResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newRemoveWarp(server rpc.Daemon_RemoveServer) io.Writer {
	return &removeWarp{server: server}
}
