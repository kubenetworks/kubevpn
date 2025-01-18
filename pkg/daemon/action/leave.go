package action

import (
	"fmt"
	"io"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Leave(req *rpc.LeaveRequest, resp rpc.Daemon_LeaveServer) error {
	defer func() {
		util.InitLoggerForServer(true)
		log.SetOutput(svr.LogFile)
		config.Debug = false
	}()
	out := io.MultiWriter(newLeaveWarp(resp), svr.LogFile)
	util.InitLoggerForClient(config.Debug)
	log.SetOutput(out)
	if svr.connect == nil {
		log.Infof("Not proxy any resource in cluster")
		return fmt.Errorf("not proxy any resource in cluster")
	}

	factory := svr.connect.GetFactory()
	namespace := svr.connect.Namespace
	maps := svr.connect.GetClientset().CoreV1().ConfigMaps(namespace)
	v4, _ := svr.connect.GetLocalTunIP()
	for _, workload := range req.GetWorkloads() {
		object, err := util.GetUnstructuredObject(factory, namespace, workload)
		if err != nil {
			log.Errorf("Failed to get unstructured object: %v", err)
			return err
		}
		// add rollback func to remove envoy config
		err = inject.UnPatchContainer(factory, maps, object, func(isFargateMode bool, rule *controlplane.Rule) bool {
			if isFargateMode {
				return svr.connect.IsMe(util.ConvertWorkloadToUid(workload), rule.Headers)
			}
			return rule.LocalTunIPv4 == v4
		})
		if err != nil {
			log.Errorf("Leaving workload %s failed: %v", workload, err)
			continue
		}
		svr.connect.LeavePortMap(workload)
		err = util.RolloutStatus(resp.Context(), factory, namespace, workload, time.Minute*60)
	}
	return nil
}

type leaveWarp struct {
	server rpc.Daemon_LeaveServer
}

func (r *leaveWarp) Write(p []byte) (n int, err error) {
	_ = r.server.Send(&rpc.LeaveResponse{
		Message: string(p),
	})
	return len(p), nil
}

func newLeaveWarp(server rpc.Daemon_LeaveServer) io.Writer {
	return &leaveWarp{server: server}
}
