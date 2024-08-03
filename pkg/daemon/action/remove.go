package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Remove(req *rpc.RemoveRequest, resp rpc.Daemon_RemoveServer) error {
	defer func() {
		util.InitLoggerForServer(true)
		log.SetOutput(svr.LogFile)
		config.Debug = false
	}()
	out := io.MultiWriter(newRemoveWarp(resp), svr.LogFile)
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(out)

	if svr.clone != nil {
		err := svr.clone.Cleanup(req.Workloads...)
		svr.clone = nil
		return err
	} else {
		log.Info("No clone resource found")
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
