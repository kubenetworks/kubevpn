package action

import (
	"io"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Stop(req *rpc.QuitRequest, resp rpc.Daemon_QuitServer) error {
	defer func() {
		util.InitLoggerForServer(true)
		log.SetOutput(svr.LogFile)
		config.Debug = false
	}()
	out := io.MultiWriter(newStopWarp(resp), svr.LogFile)
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(out)

	if svr.connect == nil {
		log.Info("No connect")
		return nil
	}

	svr.connect.Cleanup()
	svr.t = time.Time{}
	svr.connect = nil
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
