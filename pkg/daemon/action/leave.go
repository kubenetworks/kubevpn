package action

import (
	"io"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
)

func (svr *Server) Leave(req *rpc.LeaveRequest, resp rpc.Daemon_LeaveServer) error {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	out := io.MultiWriter(newLeaveWarp(resp), svr.LogFile)
	log.SetOutput(out)
	log.SetLevel(log.InfoLevel)
	if svr.connect == nil {
		log.Infof("not proxy any resource in cluster")
		return errors.Errorf("not proxy any resource in cluster")
	}

	factory := svr.connect.GetFactory()
	namespace := svr.connect.Namespace
	maps := svr.connect.GetClientset().CoreV1().ConfigMaps(namespace)
	for _, workload := range req.GetWorkloads() {
		// add rollback func to remove envoy config
		log.Infof("leave workload %s", workload)
		err := handler.UnPatchContainer(factory, maps, namespace, workload, svr.connect.GetLocalTunIPv4())
		if err != nil {
			errors.LogErrorf("leave workload %s failed: %v", workload, err)
			continue
		}
		log.Infof("leave workload %s successfully", workload)
	}
	return nil
}

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
