package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

type logWarp struct {
	server rpc.Daemon_LogsServer
}

func (r *logWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.LogResponse{
		Message: string(p),
	})
	return len(p), err
}

func newLogWarp(server rpc.Daemon_LogsServer) io.Writer {
	return &logWarp{server: server}
}

func (svr *Server) Logs(req *rpc.LogRequest, resp rpc.Daemon_LogsServer) error {
	out := newLogWarp(resp)
	origin := log.StandardLogger().Out
	defer func() {
		log.SetOutput(origin)
	}()
	multiWriter := io.MultiWriter(origin, out)
	log.SetOutput(multiWriter)
	<-resp.Context().Done()
	return nil
}
