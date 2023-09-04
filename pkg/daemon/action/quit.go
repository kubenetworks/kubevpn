package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

type QuitWarp struct {
	server rpc.Daemon_QuitServer
}

func (r *QuitWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.QuitResponse{
		Message: string(p),
	})
	return len(p), err
}

func newQuitWarp(server rpc.Daemon_QuitServer) io.Writer {
	return &QuitWarp{server: server}
}

func (svr *Server) Quit(req *rpc.QuitRequest, resp rpc.Daemon_QuitServer) error {
	out := newQuitWarp(resp)
	origin := log.StandardLogger().Out
	defer func() {
		log.SetOutput(origin)
	}()
	multiWriter := io.MultiWriter(origin, out)
	log.SetOutput(multiWriter)

	if svr.connect != nil {
		svr.connect.Cleanup()
	}
	if svr.Cancel != nil {
		log.SetOutput(origin)
		svr.Cancel()
	}
	return nil
}
