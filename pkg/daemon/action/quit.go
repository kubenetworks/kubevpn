package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

func (svr *Server) Quit(req *rpc.QuitRequest, resp rpc.Daemon_QuitServer) error {
	origin := log.StandardLogger().Out
	defer func() {
		log.SetOutput(origin)
		log.SetLevel(log.DebugLevel)
	}()
	log.SetOutput(io.MultiWriter(origin, newQuitWarp(resp)))

	if svr.connect != nil {
		svr.connect.Cleanup()
	}
	if svr.Cancel != nil {
		svr.Cancel()
	}
	return nil
}

type quitWarp struct {
	server rpc.Daemon_QuitServer
}

func (r *quitWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.QuitResponse{
		Message: string(p),
	})
	return len(p), err
}

func newQuitWarp(server rpc.Daemon_QuitServer) io.Writer {
	return &quitWarp{server: server}
}
