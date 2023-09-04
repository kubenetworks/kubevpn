package action

import (
	"io"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

type DisconnectWarp struct {
	server rpc.Daemon_DisconnectServer
}

func (r *DisconnectWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.DisconnectResponse{
		Message: string(p),
	})
	return len(p), err
}

func newDisconnectWarp(server rpc.Daemon_DisconnectServer) io.Writer {
	return &DisconnectWarp{server: server}
}

func (svr *Server) Disconnect(req *rpc.DisconnectRequest, resp rpc.Daemon_DisconnectServer) error {
	out := newDisconnectWarp(resp)
	origin := log.StandardLogger().Out
	defer func() {
		log.SetOutput(origin)
		svr.t = time.Time{}
		svr.connect = nil
	}()
	multiWriter := io.MultiWriter(origin, out)
	log.SetOutput(multiWriter)

	if svr.connect != nil {
		svr.connect.Cleanup()
	}
	return nil
}
