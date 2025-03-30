package action

import (
	"fmt"
	"io"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func (svr *Server) Leave(req *rpc.LeaveRequest, resp rpc.Daemon_LeaveServer) error {
	logger := plog.GetLoggerForClient(int32(log.InfoLevel), io.MultiWriter(newLeaveWarp(resp), svr.LogFile))
	if svr.connect == nil {
		logger.Infof("Not proxy any resource in cluster")
		return fmt.Errorf("not proxy any resource in cluster")
	}
	ctx := plog.WithLogger(resp.Context(), logger)

	factory := svr.connect.GetFactory()
	namespace := svr.connect.Namespace
	mapInterface := svr.connect.GetClientset().CoreV1().ConfigMaps(namespace)
	v4, _ := svr.connect.GetLocalTunIP()
	for _, workload := range req.GetWorkloads() {
		object, err := util.GetUnstructuredObject(factory, req.Namespace, workload)
		if err != nil {
			logger.Errorf("Failed to get unstructured object: %v", err)
			return err
		}
		u := object.Object.(*unstructured.Unstructured)
		templateSpec, _, err := util.GetPodTemplateSpecPath(u)
		if err != nil {
			logger.Errorf("Failed to get template spec path: %v", err)
			return err
		}
		// add rollback func to remove envoy config
		var empty bool
		empty, err = inject.UnPatchContainer(ctx, factory, mapInterface, object, func(isFargateMode bool, rule *controlplane.Rule) bool {
			if isFargateMode {
				return svr.connect.IsMe(req.Namespace, util.ConvertWorkloadToUid(workload), rule.Headers)
			}
			return rule.LocalTunIPv4 == v4
		})
		if err != nil {
			plog.G(ctx).Errorf("Leaving workload %s failed: %v", workload, err)
			continue
		}
		if empty {
			err = inject.ModifyServiceTargetPort(ctx, svr.connect.GetClientset(), req.Namespace, templateSpec.Labels, map[int32]int32{})
		}
		svr.connect.LeavePortMap(req.Namespace, workload)
		err = util.RolloutStatus(ctx, factory, req.Namespace, workload, time.Minute*60)
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
