package action

import (
	"io"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func (svr *Server) Stop(resp rpc.Daemon_QuitServer) error {
	_, err := resp.Recv()
	if err != nil {
		return err
	}
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newStopWarp(resp), svr.LogFile))
	ctx := plog.WithLogger(resp.Context(), logger)
	if svr.connect == nil {
		plog.G(ctx).Info("No connect")
		return nil
	}

	svr.connect.Cleanup(ctx)
	svr.t = time.Time{}
	svr.connect = nil
	svr.OffloadToConfig()
	return nil
}

type stopWarp struct {
	server rpc.Daemon_QuitServer
}

func (r *stopWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.QuitResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newStopWarp(server rpc.Daemon_QuitServer) io.Writer {
	return &stopWarp{server: server}
}
