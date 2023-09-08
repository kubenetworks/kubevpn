package action

import (
	log "github.com/sirupsen/logrus"
	"io"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
)

type leaveWarp struct {
	server rpc.Daemon_LeaveServer
}

func (r *leaveWarp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.LeaveResponse{
		Message: string(p),
	})
	return len(p), err
}

func newLeaveWarp(server rpc.Daemon_LeaveServer) io.Writer {
	return &leaveWarp{server: server}
}

func (svr *Server) Leave(req *rpc.LeaveRequest, resp rpc.Daemon_LeaveServer) error {
	out := newLeaveWarp(resp)
	origin := log.StandardLogger().Out
	defer func() {
		log.SetOutput(origin)
	}()
	multiWriter := io.MultiWriter(origin, out)
	log.SetOutput(multiWriter)

	for _, workload := range req.GetWorkloads() {
		println(workload)
	}
	return nil
}
