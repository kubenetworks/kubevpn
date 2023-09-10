package action

import (
	"io"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Stop(req *rpc.QuitRequest, resp rpc.Daemon_QuitServer) error {
	if svr.connect == nil {
		return nil
	}

	out := newStopWarp(resp)
	origin := log.StandardLogger().Out
	defer func() {
		log.SetOutput(origin)
		log.SetLevel(log.DebugLevel)
	}()
	multiWriter := io.MultiWriter(origin, out)
	log.SetOutput(multiWriter)
	svr.connect.Cleanup()
	svr.t = time.Time{}
	svr.connect = nil
	return nil
}

type stopWarp struct {
	server rpc.Daemon_QuitServer
}

func (r *stopWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.QuitResponse{
		Message: string(p),
	})
	return len(p), err
}

func newStopWarp(server rpc.Daemon_QuitServer) io.Writer {
	return &stopWarp{server: server}
}
