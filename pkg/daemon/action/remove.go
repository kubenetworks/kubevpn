package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func (svr *Server) Remove(req *rpc.RemoveRequest, resp rpc.Daemon_RemoveServer) error {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	out := io.MultiWriter(newRemoveWarp(resp), svr.LogFile)
	log.SetOutput(out)
	log.SetLevel(log.InfoLevel)

	if svr.clone != nil {
		err := svr.clone.Cleanup(req.Workloads...)
		svr.clone = nil
		return err
	} else {
		log.Info("remove: no clone resource found")
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
