package action

import (
	"fmt"
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
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
	if svr.connect == nil {
		return fmt.Errorf("not proxy any resource in cluster")
	}

	factory := svr.connect.GetFactory()
	namespace := svr.connect.Namespace
	maps := svr.connect.GetClientset().CoreV1().ConfigMaps(namespace)
	for _, workload := range req.GetWorkloads() {
		// add rollback func to remove envoy config
		err := handler.UnPatchContainer(factory, maps, namespace, workload, svr.connect.GetLocalTunIPv4())
		if err != nil {
			log.Error(err)
		}
	}
	return nil
}
