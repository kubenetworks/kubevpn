package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func (svr *Server) Remove(req *rpc.RemoveRequest, resp rpc.Daemon_RemoveServer) error {
	origin := log.StandardLogger().Out
	defer func() {
		log.SetOutput(origin)
		log.SetLevel(log.DebugLevel)
	}()
	out := io.MultiWriter(newRemoveWarp(resp), origin)
	log.SetOutput(out)
	util.InitLogger(false)

	if svr.clone != nil {
		err := svr.clone.Cleanup(req.Workloads)
		svr.clone = nil
		return err
	}
	return nil
}

type removeWarp struct {
	server rpc.Daemon_RemoveServer
}

func (r *removeWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.RemoveResponse{
		Message: string(p),
	})
	return len(p), err
}

func newRemoveWarp(server rpc.Daemon_RemoveServer) io.Writer {
	return &removeWarp{server: server}
}
